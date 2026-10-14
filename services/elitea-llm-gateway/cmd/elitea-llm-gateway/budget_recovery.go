package main

// budget_recovery.go — issue #315: budget enforcement comes back when NATS
// becomes reachable.
//
// Issue #304 made the failure VISIBLE: a gateway that starts while NATS is
// unreachable reports not ready instead of serving unmetered. It did not make
// the gateway RECOVER. server.New dials once, and nats.go only resurrects a
// connection that succeeded at least once, so a failed initial dial left
// enforcement off until an operator restarted the pod — which they can only do
// after NATS is back.
//
// Two things had to become safe before recovery could be written:
//
//  1. The gate itself. It was a plain field the money path read with no
//     synchronisation, so installing it late was a data race on billing.
//     llmproxy publishes it atomically now (internal/llmproxy/budget_plane.go).
//  2. The governance store. /readyz reads it on request goroutines and the
//     shutdown path reads it on the main goroutine, so a late write to a plain
//     variable is the same hazard one layer up. enforcementPlane below is the
//     atomic holder for it.

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/config"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/cost"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/governance"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/llmproxy"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/policy"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/server"
)

// budgetRecoveryInterval is how often a gateway with no enforcement re-dials
// NATS. It is a constant and not an operator setting: the value trades a bounded
// amount of log noise against how long a recovered NATS takes to return the pod
// to service, and neither side of that trade belongs to a deployment.
const budgetRecoveryInterval = 15 * time.Second

// enforcementPlane holds the governance store three goroutines read after
// startup: the /readyz handler, the shutdown drain, and the recovery loop that
// installs it. The pointer is atomic for exactly that reason.
//
// It is a POINTER to the concrete store, never an interface. The caller's
// variable is a typed *governance.GovernanceStore, and holding it as an
// interface reintroduces the typed-nil-in-non-nil-interface trap that already
// produced a /readyz panic once.
type enforcementPlane struct {
	cfg   config.Config
	store atomic.Pointer[governance.GovernanceStore]
}

// Ping answers the /readyz dependency check for the store that is in force now.
// A plane with no store answers success: either NATS is deliberately off, or
// unwired reports the fault and /readyz never reaches this call.
func (e *enforcementPlane) Ping(ctx context.Context) error {
	gs := e.store.Load()
	if gs == nil {
		return nil
	}
	return gs.Ping(ctx)
}

// unwired reports the issue #304 state: NATS is configured, and enforcement is
// not wired. It is a FUNCTION and not a bool because a re-dial can clear it
// while the process runs — which is the whole of issue #315. A pod that
// recovers must return to the load-balancer rotation without a restart.
func (e *enforcementPlane) unwired() bool {
	return budgetEnforcementUnwired(e.cfg, e.store.Load())
}

// install publishes a store. It reports false when a store is already
// published, so a caller that loses a race never replaces one.
func (e *enforcementPlane) install(gs *governance.GovernanceStore) bool {
	return gs != nil && e.store.CompareAndSwap(nil, gs)
}

// current returns the published store for the shutdown drain. It is nil when
// enforcement was never wired.
func (e *enforcementPlane) current() *governance.GovernanceStore { return e.store.Load() }

// budgetRecovery is everything the recovery loop needs. It is a struct because
// the loop takes seven collaborators and a positional list of them is a defect
// waiting for its next reader.
type budgetRecovery struct {
	cfg   config.Config
	srv   *server.Server
	pool  *pgxpool.Pool
	plane *enforcementPlane
	// handler is the RUNNING /llm handler. InstallBudgetEnforcement is safe to
	// call while it serves traffic; that is what issue #315 built.
	handler *llmproxy.Handler
	// policy supplies the fallback budget ceiling an operator authored. nil
	// when the gateway has no database, in which case recovery cannot run at
	// all — governance needs the pool.
	policy *policy.Store
	logger *slog.Logger
	// hub, when set, is the single NATS recovery loop; the budget recovery
	// subscribes to it instead of running a second poller. nil (tests) falls
	// back to a private loop.
	hub *rateLimitMonitor
	// build assembles the governance engine. It is nil in production, where
	// attempt uses buildGovernance. It is a TEST SEAM: buildGovernance needs a
	// live database pool, and without this seam the install half of the loop —
	// the half issue #315 is about — could only be read, never executed.
	build governanceBuilder
}

// governanceBuilder is buildGovernance's signature.
type governanceBuilder func(
	context.Context, config.Config, server.NATSClient, *pgxpool.Pool, *slog.Logger,
) (*governance.GovernanceStore, *cost.Calculator, error)

// startBudgetRecovery re-dials NATS in the background and installs budget
// enforcement on the running gateway when the dial succeeds.
//
// It starts ONLY when enforcement is configured and unwired, and it stops on
// the first successful install or when ctx is cancelled. A gateway that already
// enforces, one with no GATEWAY_NATS_URL, and one with no database each get no
// goroutine at all: the first needs nothing, and the other two can never
// succeed.
func startBudgetRecovery(ctx context.Context, r budgetRecovery) {
	if !r.plane.unwired() {
		return
	}
	if r.pool == nil {
		r.logger.Error("BUDGET ENFORCEMENT CANNOT RECOVER: NATS is configured but there is no database pool, " +
			"and the governance engine needs both. This pod stays not-ready until it is restarted with a working " +
			"DATABASE_URL")
		return
	}
	r.logger.Warn("BUDGET ENFORCEMENT RECOVERY ARMED: NATS is configured but was not reachable at startup. "+
		"The gateway re-dials it and installs enforcement without a restart; /readyz reports not_ready until then",
		"interval", budgetRecoveryInterval)
	if r.hub != nil {
		r.hub.subscribe(r.attempt)
		return
	}
	go r.run(ctx)
}

// run is the loop. It is a method so a test can drive one attempt.
func (r budgetRecovery) run(ctx context.Context) {
	ticker := time.NewTicker(budgetRecoveryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if r.attempt(ctx) {
			return
		}
	}
}

// attempt performs one re-dial and, when it succeeds, installs enforcement. It
// reports whether the loop is finished — either because enforcement is now in
// force, or because no further attempt can succeed.
func (r budgetRecovery) attempt(ctx context.Context) bool {
	if r.handler.BudgetEnforcementInstalled() {
		return true
	}
	nc, err := r.srv.RedialNATS(ctx)
	if err != nil {
		if errors.Is(err, server.ErrNATSNotConfigured) || errors.Is(err, server.ErrServerClosed) {
			return true
		}
		r.logger.Warn("budget enforcement recovery: NATS is still unreachable; the gateway stays not-ready",
			"err", err, "retry_in", budgetRecoveryInterval)
		return false
	}

	build := r.build
	if build == nil {
		build = buildGovernance
	}
	govStore, calc, gerr := build(ctx, r.cfg, nc, r.pool, r.logger)
	if gerr != nil {
		r.logger.Error("budget enforcement recovery: NATS is reachable but governance assembly failed",
			"err", gerr, "retry_in", budgetRecoveryInterval)
		return false
	}

	// The authored fallback ceiling is set BEFORE the gate goes live, so the
	// first request the gate admits already sees an operator's budget row.
	if r.policy != nil {
		govStore.SetBudgetDefaults(policyBudgetDefaults{store: r.policy})
	}

	// The money path first. nc carries both event publishers and govStore is
	// the budget_used reader, so one install publishes the whole budget plane.
	installed := r.handler.InstallBudgetEnforcement(llmproxy.BudgetEnforcement{
		Gate:   govStore,
		Calc:   calc,
		Alerts: nc,
		Ops:    nc,
		Usage:  govStore,
	})
	if !installed {
		// Another install won. Do not publish a second store: the drain would
		// then wait on one the handler never billed through.
		r.logger.Warn("budget enforcement recovery: a gate was installed by another path; this attempt is dropped")
		govStore.Drain()
		return true
	}

	// Then readiness and the shutdown drain.
	r.plane.install(govStore)
	recordBudgetEnforcementEnabled(true)
	r.logger.Info("BUDGET ENFORCEMENT RECOVERED: NATS is reachable again and the gate is installed on the "+
		"running gateway; /readyz reports ready again",
		"nats_url", r.cfg.NATSURL)
	return true
}
