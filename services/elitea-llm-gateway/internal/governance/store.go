// Package governance composes the NATS JetStream client and the tiered-hybrid
// fail-mode FSM into a budget-enforcement engine (design §8.5, BF0.9a).
//
// GovernanceStore is the Elitea-native budget engine (design Option E). Bifrost
// does not expose an InitFromStore entrypoint in any imported package path; the
// governance plugin coupling is therefore intentionally absent. The
// GovernanceStore owns the full CheckBudget/UpdateUsage loop, and the handler
// layer (BF0.9b/c) will wire it in via the llmproxy.BudgetChecker port.
package governance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/sony/gobreaker/v2"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/failmode"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/infra/nats"
)

// natsClient is the minimal NATS surface the GovernanceStore needs. *nats.Client
// satisfies it; tests inject a fake so no live NATS is required.
type natsClient interface {
	IncrBudget(ctx context.Context, subject string, deltaNano int64) (int64, error)
	IncrBudgetIdempotent(ctx context.Context, subject, eventID string, deltaNano int64) (total int64, applied bool, err error)
	ReadBudget(ctx context.Context, subject string) (int64, error)
	PublishDelta(ctx context.Context, eventID string, payload []byte) error
	TryAlertCooldown(ctx context.Context, key string) (bool, error)
	OnBreakerStateChange(fn func(from, to gobreaker.State))
	BreakerState() gobreaker.State
}

// GovernanceStore is the budget-enforcement engine. It composes:
//   - A hardened NATS JetStream client (authoritative counter + write-behind).
//   - A failmode.Store (Postgres snapshot tier).
//   - A failmode.DegradedCounters (per-replica overspend cap while NATS is down).
//   - A failmode.Reconciler (recovery reconciliation on breaker→CLOSED).
//   - failmode.Params (the resolved enforcement configuration).
//
// All money amounts are int64 nano-USD. GovernanceStore is safe for concurrent use.
type GovernanceStore struct {
	nc       natsClient
	store    *failmode.Store
	degraded *failmode.DegradedCounters
	rec      *failmode.Reconciler
	params   failmode.Params
	log      *slog.Logger

	// persistWg tracks in-flight PersistOutageDelta goroutines (Fix #2). Drain()
	// blocks until all in-flight persists complete so the server can call it
	// before closing the pool on graceful shutdown.
	persistWg sync.WaitGroup

	// FIX #8: track when the circuit breaker transitioned to open so
	// CheckBudget can detect when the outage has exceeded params.DegradedMaxDuration
	// and set Params.OutageExceededMax = true to force FORCED_CLOSED.
	brkMu         sync.RWMutex
	breakerOpenAt time.Time // zero means breaker is not open

	// defaults supplies an AUTHORED fallback ceiling for a project that has no
	// gateway.project_budget row (issue #218). nil means the old behaviour: no
	// row is unlimited.
	//
	// It can only make the decision STRICTER. The branch it feeds is the one
	// that used to return "unlimited, always allow", so a project gains a
	// ceiling it did not have and never loses one it did — the fallback cannot
	// override a real project_budget row, because that branch is only reached
	// when there is none.
	defaults BudgetDefaults
}

// BudgetDefaults supplies the authored global budget for a project with no
// per-project row. *policy.Snapshot answers this through an adapter in the
// composition root; the interface keeps internal/governance free of a
// dependency on the definition plane.
type BudgetDefaults interface {
	// DefaultBudgetNano returns the ceiling in nano-USD, the soft-alert
	// percentage, and the per-project fail-mode override. ok is false when no
	// authored budget applies, which restores the unlimited behaviour.
	DefaultBudgetNano(projectID int) (limitNano int64, softAlertPct int, failMode string, ok bool)
}

// SetBudgetDefaults arms the authored fallback ceiling. It is called once from
// the composition root, before the gateway serves, so no request observes a
// half-wired store.
func (g *GovernanceStore) SetBudgetDefaults(d BudgetDefaults) {
	if g == nil {
		return
	}
	g.defaults = d
}

// NewGovernanceStore assembles a GovernanceStore from the four pre-built
// primitives and a resolved Params. Call Start before serving to bind the
// service-lifetime context to the reconciler.
func NewGovernanceStore(
	nc natsClient,
	store *failmode.Store,
	degraded *failmode.DegradedCounters,
	rec *failmode.Reconciler,
	params failmode.Params,
	log *slog.Logger,
) *GovernanceStore {
	if log == nil {
		log = slog.Default()
	}
	gs := &GovernanceStore{
		nc:       nc,
		store:    store,
		degraded: degraded,
		rec:      rec,
		params:   params,
		log:      log,
	}
	// Wire the breaker→CLOSED edge to the reconciler's recovery pass.
	// FIX #8: also record the breaker-open timestamp so CheckBudget can
	// compute the continuous-outage duration and set OutageExceededMax.
	nc.OnBreakerStateChange(func(from, to gobreaker.State) {
		gs.log.Info("governance: NATS breaker state change",
			slog.String("from", from.String()),
			slog.String("to", to.String()),
		)
		gs.brkMu.Lock()
		switch to {
		case gobreaker.StateOpen:
			// Record when the outage started (only on first open — don't reset
			// the clock on a flap between open and half-open).
			if gs.breakerOpenAt.IsZero() {
				gs.breakerOpenAt = time.Now()
			}
		case gobreaker.StateClosed:
			// Breaker recovered: reset the outage-start clock.
			gs.breakerOpenAt = time.Time{}
		}
		gs.brkMu.Unlock()
		gs.rec.HandleBreakerChange(from, to)
	})
	// Issue #515: the breaker edge is not the only way an outage row is cleared
	// any more — the reconciler also sweeps on a timer, because a single failed
	// counter operation marks a row outage-owned without ever opening the
	// breaker, so no edge follows to clear it. Give that sweep the breaker
	// state, so it attempts recovery only while the authoritative counter is
	// reachable. A project that is genuinely in outage keeps its row, which is
	// the point: the flag must never be cleared unconditionally.
	rec.SetHealthCheck(func() bool { return nc.BreakerState() == gobreaker.StateClosed })
	return gs
}

// Start binds the service-lifetime context to the recovery reconciler. It
// MUST be called before the gateway starts serving (i.e. before concurrent
// CheckBudget / UpdateUsage calls), so that the reconciler can derive a
// properly-bounded context for its recovery passes.
func (g *GovernanceStore) Start(ctx context.Context) {
	g.rec.Start(ctx)
}

// CheckBudget is the pre-LLM admission check (design §8.5). It:
//  1. Attempts to read the authoritative counter from NATS.
//  2. Reads the Postgres snapshot (for the FSM's fallback tier or HEALTHY snap).
//  3. Invokes Decide with the resolved inputs and returns the Decision.
//
// When the NATS breaker is open or the read fails, natsUp=false and the FSM
// falls back to the Postgres snapshot (tiered_hybrid / fail_open / fail_closed).
// ErrNoBudgetRow from the snapshot is treated as an unlimited project (no
// config row ⇒ nothing to enforce).
//
// periodStartUnix is the current billing-period start (Unix seconds). reqCostNano
// is the pre-estimated cost of the request in nano-USD (may be 0 for a pure check).
func (g *GovernanceStore) CheckBudget(
	ctx context.Context,
	projectID int,
	scope, scopeID string,
	periodStartUnix, reqCostNano int64,
) (failmode.Decision, error) {
	subject := nats.BudgetSubject(scope, scopeID, periodStartUnix)

	// Attempt the authoritative NATS read (breaker-guarded, OpTimeout-bounded).
	authoritativeNano, natsErr := g.nc.ReadBudget(ctx, subject)
	natsUp := natsErr == nil

	if natsErr != nil && !errors.Is(natsErr, nats.ErrUnavailable) {
		// A non-infrastructure error (parse/config) is worth logging but still
		// falls back via the FSM rather than hard-failing the request.
		g.log.Warn("governance: ReadBudget non-infra error; treating as unavailable",
			slog.String("subject", subject),
			slog.Any("err", natsErr),
		)
	}

	// Read the Postgres snapshot. A missing budget row is treated as unlimited.
	snap, snapErr := g.store.ReadSnapshot(ctx, projectID, scope, scopeID, periodStartUnix)
	if snapErr != nil {
		if errors.Is(snapErr, failmode.ErrNoBudgetRow) {
			// No per-project budget row. Before an authored fallback exists this
			// is unlimited; with one, the operator's global ceiling applies.
			//
			// AccumulatedNano stays 0 and Found stays false, which is what the
			// FSM already assumes for a budgeted project with no accumulator
			// row: no spend recorded this period. The authoritative NATS
			// counter, read above, is what actually gates the request on the
			// healthy path.
			snap = failmode.Snapshot{IsUnlimited: true}
			if g.defaults != nil {
				if limitNano, softPct, failMode, ok := g.defaults.DefaultBudgetNano(projectID); ok && limitNano > 0 {
					snap = failmode.Snapshot{
						IsUnlimited:   false,
						HardLimitNano: limitNano,
						SoftAlertPct:  softPct,
						NatsFailMode:  failmode.FailMode(failMode),
					}
				}
			}
		} else {
			// PG read failed and NATS is also down ⇒ Block503 (no data).
			if !natsUp {
				g.log.Error("governance: snapshot read failed and NATS unavailable",
					slog.Int("project_id", projectID),
					slog.Any("err", snapErr),
				)
				return failmode.Decision{
					Verdict:  failmode.Block503,
					State:    failmode.StateDownPGStale,
					Degraded: true,
				}, nil
			}
			// Fix #1 (fail-closed): NATS is up but PG snapshot read failed.
			// A zero Snapshot{} would give HardLimitNano=0 which makes
			// fsm.Decide's NATS_HEALTHY path evaluate (HardLimitNano > 0) as
			// false and silently allow unlimited requests. Propagate the error
			// so the caller (handler) can return 503 — enforcement must never
			// be silently disabled by a transient DB failure.
			g.log.Error("governance: snapshot read failed; failing closed to deny request",
				slog.Int("project_id", projectID),
				slog.Any("err", snapErr),
			)
			return failmode.Decision{
				Verdict:  failmode.Block503,
				State:    failmode.StateDownPGStale,
				Degraded: false,
			}, fmt.Errorf("governance: snapshot unavailable: %w", snapErr)
		}
	}

	degradedKey := nats.BudgetSubject(scope, scopeID, periodStartUnix)
	replicaDegradedNano := g.degraded.Get(degradedKey)

	// Apply the per-project fail-mode override when present: the snapshot carries
	// the nats_fail_mode column from gateway.project_budget (read by ReadSnapshot
	// via snapshotSQL). A non-empty override replaces the platform baseline in
	// Params so Decide uses the per-project policy.
	params := g.params
	if snap.NatsFailMode != "" {
		params.Mode = failmode.ResolveFailMode(string(snap.NatsFailMode), g.params.Mode)
	}

	// FIX #8: derive OutageExceededMax from the breaker-open timestamp. When
	// the breaker has been open longer than params.DegradedMaxDuration the FSM
	// forces FORCED_CLOSED (§8.5). A zero DegradedMaxDuration disables the
	// ceiling (dev/test; caller did not configure it).
	if !natsUp && params.DegradedMaxDuration > 0 {
		g.brkMu.RLock()
		openAt := g.breakerOpenAt
		g.brkMu.RUnlock()
		if !openAt.IsZero() && time.Since(openAt) > params.DegradedMaxDuration {
			params.OutageExceededMax = true
		}
	}

	dec := failmode.Decide(natsUp, authoritativeNano, replicaDegradedNano, snap, reqCostNano, params)
	dec.Budgeted = !snap.IsUnlimited && snap.HardLimitNano > 0
	return dec, nil
}

// deltaPayload is the minimal JSON structure written to the GATEWAY_BUDGET_DELTAS
// write-behind stream (design §8.6). The scheduler drains this stream into the
// durable Postgres accumulator. JSON keys MUST match the scheduler consumer's
// BudgetDelta struct exactly (services/elitea-scheduler/internal/budgetwriteback/types.go).
type deltaPayload struct {
	EventID      string `json:"event_id"`
	Scope        string `json:"scope"`
	ScopeID      string `json:"scope_id"`
	ProjectID    int    `json:"project_id"`
	OrgID        *int   `json:"org_id,omitempty"`
	PeriodStart  int64  `json:"period_start"`
	PeriodEnd    int64  `json:"period_end"`
	DeltaNanoUSD int64  `json:"delta_nano_usd"`
	// Usage carries the request's reporting dimensions (issue #320). It is
	// OMITTED, not zero-filled, when the caller has none to report: the
	// consumer appends a usage-ledger row only when this object is present, so
	// an absent object means "this delta is money only", never "this request
	// used no tokens".
	//
	// It rides at most ONE of a request's deltas. A request that bills both the
	// project and the member scope publishes two deltas, and only the project
	// one carries Usage — the ledger row already names the member in its
	// user_id column, and a second row would double every token and request
	// count the per-model table reports.
	Usage *usageDimsPayload `json:"usage,omitempty"`
}

// usageDimsPayload is the wire form of failmode.UsageDimensions. Its JSON keys
// MUST match the scheduler consumer's UsageDimensions struct exactly
// (services/elitea-scheduler/internal/budgetwriteback/types.go).
type usageDimsPayload struct {
	UserID           *int   `json:"user_id,omitempty"`
	Provider         string `json:"provider,omitempty"`
	Model            string `json:"model,omitempty"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	OccurredAtUnix   int64  `json:"occurred_at"`
	// ExecutionID is omitempty because it is absent for most requests, and an
	// older consumer that does not know the key ignores it — so this field can
	// ship before the scheduler that reads it without changing what any
	// existing delta means.
	ExecutionID string `json:"execution_id,omitempty"`
}

// usageDimsFor converts the internal dimensions to the wire form, preserving
// nil so an absent object stays absent.
func usageDimsFor(dims *failmode.UsageDimensions) *usageDimsPayload {
	if dims == nil {
		return nil
	}
	return &usageDimsPayload{
		UserID:           dims.UserID,
		Provider:         dims.Provider,
		Model:            dims.Model,
		PromptTokens:     dims.PromptTokens,
		CompletionTokens: dims.CompletionTokens,
		OccurredAtUnix:   dims.OccurredAtUnix,
		ExecutionID:      dims.ExecutionID,
	}
}

// UpdateUsage records a billed increment onto the authoritative NATS counter and
// publishes a write-behind delta for durable accumulation (design §8.6).
//
// It calls IncrBudgetIdempotent (idempotent via Nats-Msg-Id=eventID) so a retry
// after a crash between the increment and the delta publish does not double-count
// the counter. If the increment was a suppressed duplicate (applied=false) the
// delta publish is still attempted so the write-behind consumer can reconcile.
//
// periodEndUnix is the current period's end (Unix seconds) used only by the
// delta payload; it is not required for counter enforcement.
//
// dims carries the request's reporting dimensions (issue #320) and may be nil.
// It never affects the counter or the accumulator — only whether a row is
// appended to the per-request usage ledger.
func (g *GovernanceStore) UpdateUsage(
	ctx context.Context,
	projectID int,
	scope, scopeID, eventID string,
	costNano int64,
	periodStartUnix, periodEndUnix int64,
	dims *failmode.UsageDimensions,
) error {
	subject := nats.BudgetSubject(scope, scopeID, periodStartUnix)

	_, _, incrErr := g.nc.IncrBudgetIdempotent(ctx, subject, eventID, costNano)
	if incrErr != nil && !errors.Is(incrErr, nats.ErrUnavailable) {
		return fmt.Errorf("governance: IncrBudgetIdempotent: %w", incrErr)
	}
	if incrErr != nil {
		// NATS is down; accumulate onto the per-replica degraded counter so the
		// FSM cap keeps gating further requests during the outage window.
		g.degraded.Add(subject, costNano)
		g.log.Warn("governance: NATS unavailable during UpdateUsage; degraded counter updated",
			slog.String("subject", subject),
			slog.Int64("cost_nano", costNano),
		)
		// Persist a durable outage-window delta so spend is not lost if this
		// replica restarts before the breaker recovers. Run off the request path
		// (bounded goroutine) so a slow Postgres write does not stall /llm.
		outageDelta := failmode.OutageDelta{
			ProjectID: projectID,
			OrgID:     nil,
			Scope:     scope,
			ScopeID:   scopeID,
			// The event id and the dimensions travel with the outage write so
			// the usage ledger keeps its row for a request billed while NATS is
			// down (issue #320). The id makes that row idempotent against a
			// later redelivery of the same delta through the consumer.
			EventID:      eventID,
			PeriodStart:  periodStartUnix,
			PeriodEnd:    periodEndUnix,
			DeltaNanoUSD: costNano,
			Usage:        dims,
		}
		// Fix #2: use the WaitGroup so Drain() can wait for in-flight persists
		// to complete before pool.Close() on graceful shutdown.
		g.persistWg.Add(1)
		go func() {
			defer g.persistWg.Done()
			ctx2, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := g.store.PersistOutageDelta(ctx2, outageDelta); err != nil {
				g.log.Warn("governance: PersistOutageDelta failed during NATS outage",
					slog.String("event_id", eventID),
					slog.Any("err", err),
				)
			}
		}()
	}

	// Publish a write-behind delta regardless of whether the counter increment
	// was applied or suppressed; the scheduler is the durable accounting ground-truth.
	payload, err := json.Marshal(deltaPayload{
		EventID:      eventID,
		Scope:        scope,
		ScopeID:      scopeID,
		ProjectID:    projectID,
		OrgID:        nil, // org_id not available at this call site; omitted (omitempty)
		PeriodStart:  periodStartUnix,
		PeriodEnd:    periodEndUnix,
		DeltaNanoUSD: costNano,
		Usage:        usageDimsFor(dims),
	})
	if err != nil {
		// JSON marshal of a fixed struct should never fail.
		return fmt.Errorf("governance: marshal delta payload: %w", err)
	}
	if pubErr := g.nc.PublishDelta(ctx, eventID, payload); pubErr != nil {
		// A failed delta publish is logged but not fatal: the NATS counter was
		// already incremented (or the degraded counter was updated), so the
		// immediate enforcement decision is correct. The write-behind consumer
		// operates on an at-least-once basis; missing deltas are reconciled via
		// the outage-recovery path on the next breaker→CLOSED edge.
		g.log.Warn("governance: PublishDelta failed; delta will be reconciled on recovery",
			slog.String("event_id", eventID),
			slog.Any("err", pubErr),
		)
	}

	return nil
}

// Drain blocks until all in-flight PersistOutageDelta goroutines complete (Fix
// #2). The server MUST call this before closing the database pool on graceful
// shutdown so no goroutine races with pool.Close().
func (g *GovernanceStore) Drain() {
	g.persistWg.Wait()
}

// DumpTotal returns the current in-process degraded total for a scope/period.
// When NATS is healthy this is 0 (the authoritative counter is the ground-truth);
// during an outage it reflects this replica's billed spend since NATS went down.
func (g *GovernanceStore) DumpTotal(scope, scopeID string, periodStartUnix int64) int64 {
	return g.degraded.Get(nats.BudgetSubject(scope, scopeID, periodStartUnix))
}

// ResetExpired is a no-op stub. Period rollover is handled automatically: the
// NATS counter subjects are keyed by period_start (BudgetSubject includes the
// unix timestamp), so a new period produces a new subject and the old one is
// subject to the GATEWAY_BUDGET stream's MaxMsgsPerSubject=1 retention. The
// per-replica DegradedCounters are reset on the breaker→CLOSED reconciliation
// edge (see recovery.go); they otherwise reset on pod restart.
//
// If a future design requires explicit TTL-driven reset (e.g., for very long-lived
// replicas spanning multiple billing periods), the caller should invoke
// DegradedCounters.Reset with the old subject.
func (g *GovernanceStore) ResetExpired(_ context.Context) error {
	// No-op: period rollover is handled by counter TTL (subject-keyed periods).
	return nil
}

// alertCooldownKey builds the KV key for the 80% soft-alert cooldown for a given
// scope. The key is scoped by subject so different scopes have independent
// cooldowns.
func alertCooldownKey(subject string) string {
	return "alert." + subject
}

// TryAlertCooldown is a thin forward to the NATS client's TryAlertCooldown for
// the 80% soft-alert path (§8.3). It is exposed so the handler layer can call it
// without holding a reference to the raw NATS client.
func (g *GovernanceStore) TryAlertCooldown(ctx context.Context, scope, scopeID string, periodStartUnix int64) (bool, error) {
	key := alertCooldownKey(nats.BudgetSubject(scope, scopeID, periodStartUnix))
	return g.nc.TryAlertCooldown(ctx, key)
}

// Ping reports whether the GovernanceStore's NATS dependency is reachable. It
// returns nil when the circuit breaker is closed (healthy) and ErrUnavailable
// when the breaker is open or half-open. Callers (e.g. /readyz probes) use this
// to include budget-enforcement health in readiness checks.
// A nil receiver means enforcement is disabled, which is a healthy state, not a
// fault — defence in depth for callers that convert a typed nil into an
// interface before the guard runs.
func (g *GovernanceStore) Ping(_ context.Context) error {
	if g == nil || g.nc == nil {
		return nil
	}
	if g.nc.BreakerState() != gobreaker.StateClosed {
		return nats.ErrUnavailable
	}
	return nil
}

// NATSUnavailable reports whether the NATS circuit breaker is currently open
// (i.e., the last several operations failed). Exposed for observability.
func (g *GovernanceStore) NATSUnavailable() bool {
	return g.nc.BreakerState() != gobreaker.StateClosed
}

// pgFreshnessDefault is the failmode.Params.PGFreshness default used by
// DefaultParams if the caller does not supply one.
const pgFreshnessDefault = 5 * time.Minute

// DefaultParams returns a sensible Params baseline for production use.
// The caller should override individual fields (e.g. DegradedCapNano from config)
// rather than using this verbatim.
func DefaultParams() failmode.Params {
	return failmode.Params{
		Mode:             failmode.ModeTieredHybrid,
		PGFreshness:      pgFreshnessDefault,
		ExpectedReplicas: 1,
		DegradedCapNano:  0, // 0 ⇒ FSM derives 10% of HardLimitNano
	}
}
