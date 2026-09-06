package main

// budget_startup_gate.go — issue #304: a gateway that starts without NATS runs
// with budget enforcement off for its whole lifetime.
//
// Three states share one symptom (the budget gate is not wired) and they are
// NOT the same fault:
//
//  1. GATEWAY_NATS_URL is set and the dial failed. /readyz already reports not
//     ready (main.go, budgetEnforcementUnwired) and the recovery loop already
//     re-dials and installs enforcement on the running process (issue #315).
//     The pod drains, comes back on its own, and an operator can see both.
//  2. GATEWAY_NATS_URL is UNSET while the database holds enforcing budget rows.
//     Nothing re-dials a URL that was never given, and /readyz answers ready by
//     design — the NATS-less posture is a supported one. So this pod serves
//     /llm with every authored ceiling ignored, bills nothing, reports healthy,
//     and no probe anywhere says so. This is the state that had no control.
//  3. Nothing is configured and nothing is authored. That is the bootstrap and
//     local-development posture and it must keep working.
//
// The gate below refuses to start in state 2, and in state 1 as well when the
// operator asks for it with LLM_BUDGET_REQUIRE_ENFORCEMENT=on. It refuses
// BEFORE the listener opens, so a refused process serves no request.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/config"
)

// budgetProbeTimeout bounds the startup probe. A database that does not answer
// must not hold the process before its listener: the probe reports "cannot
// tell", the gate says so out loud, and startup continues. A refusal to start
// is for a state this gateway can PROVE, never for one it failed to read.
const budgetProbeTimeout = 3 * time.Second

// errNoBudgetPool is the probe answer for a gateway with no database pool.
var errNoBudgetPool = errors.New("no database pool: the gateway cannot read gateway.project_budget or gateway.user_budget")

// budgetEvidence is what the startup probe learned about authored budgets.
//
// found and err are separate fields, and the separation is the point. "No rows"
// and "could not ask" are different answers to the same question, and reading
// the second as the first is how a gate stops gating: an unreachable database
// would report zero budgets and every deployment would pass.
type budgetEvidence struct {
	// found reports at least one ENFORCING budget row — a row that would refuse
	// a request if the gate were wired. An unlimited or disabled row authors no
	// ceiling and is not evidence.
	found bool
	// err is non-nil when the probe could not answer at all.
	err error
}

// budgetRowQuerier is the read half of *pgxpool.Pool the probe needs. It is a
// small interface so the probe can be tested without a live database.
type budgetRowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// budgetTableProbeSQL asks whether the two budget tables exist before either is
// named in a query.
//
// It is a SEPARATE round trip on purpose. PostgreSQL resolves every relation in
// a statement at PARSE time, so a `CASE WHEN to_regclass(...) IS NULL` guard
// wrapped around a SELECT from the same table still raises 42P01 on a database
// that has not run shared migration 0067. The guard has to run in its own
// statement to guard anything.
const budgetTableProbeSQL = `SELECT to_regclass('gateway.project_budget') IS NOT NULL,
       to_regclass('gateway.user_budget') IS NOT NULL`

// projectBudgetProbeSQL and userBudgetProbeSQL count only the rows that would
// change an admission decision.
//
// The predicates match what the gateway's own snapshot reads treat as a
// ceiling (internal/failmode/store.go): a project row is unlimited when
// is_unlimited is set or hard_limit_usd is NULL, and gateway.user_budget
// carries no is_unlimited column at all — a member cap bites when it is
// enabled and names an amount. A row that authors no ceiling is not evidence
// that this deployment enforces budgets.
const projectBudgetProbeSQL = `SELECT EXISTS (
	SELECT 1 FROM gateway.project_budget
	WHERE enabled AND NOT is_unlimited AND hard_limit_usd IS NOT NULL)`

const userBudgetProbeSQL = `SELECT EXISTS (
	SELECT 1 FROM gateway.user_budget
	WHERE enabled AND hard_limit_usd IS NOT NULL)`

// probeAuthoredBudgets answers whether this deployment authored any budget that
// the gateway would enforce.
//
// It takes the CONCRETE pool type and nil-checks it here rather than accepting
// the interface directly: a nil *pgxpool.Pool boxed into a non-nil
// budgetRowQuerier passes an `q != nil` test and then panics on the first call.
// That exact shape already produced a /readyz panic in this binary once.
func probeAuthoredBudgets(ctx context.Context, pool *pgxpool.Pool) budgetEvidence {
	if pool == nil {
		return budgetEvidence{err: errNoBudgetPool}
	}
	return probeAuthoredBudgetsWith(ctx, pool)
}

// probeAuthoredBudgetsWith is probeAuthoredBudgets over any querier.
func probeAuthoredBudgetsWith(ctx context.Context, q budgetRowQuerier) budgetEvidence {
	ctx, cancel := context.WithTimeout(ctx, budgetProbeTimeout)
	defer cancel()

	var projectTable, userTable bool
	if err := q.QueryRow(ctx, budgetTableProbeSQL).Scan(&projectTable, &userTable); err != nil {
		return budgetEvidence{err: fmt.Errorf("read the budget tables: %w", err)}
	}
	if !projectTable && !userTable {
		// Shared migration 0067 has not run. Nothing can have authored a budget
		// yet, so this is a real "no" and not a failure to read.
		return budgetEvidence{}
	}
	for _, probe := range []struct {
		table string
		sql   string
		on    bool
	}{
		{"gateway.project_budget", projectBudgetProbeSQL, projectTable},
		{"gateway.user_budget", userBudgetProbeSQL, userTable},
	} {
		if !probe.on {
			continue
		}
		var found bool
		if err := q.QueryRow(ctx, probe.sql).Scan(&found); err != nil {
			return budgetEvidence{err: fmt.Errorf("read %s: %w", probe.table, err)}
		}
		if found {
			return budgetEvidence{found: true}
		}
	}
	return budgetEvidence{}
}

// budgetStartupRefusal is the whole decision, as a pure function of the three
// inputs, so the matrix can be executed rather than read.
//
// wired reports that the budget gate IS attached to the handler. Every mode
// admits a wired gateway; the modes only differ over what an unwired one does.
func budgetStartupRefusal(cfg config.Config, wired bool, budgets budgetEvidence) error {
	if wired {
		return nil
	}
	switch cfg.RequireBudgetEnforcement {
	case config.RequireEnforcementOff:
		// The documented opt-out. main logs it; nothing here refuses.
		return nil
	case config.RequireEnforcementOn:
		return errors.New("LLM_BUDGET_REQUIRE_ENFORCEMENT=on and the budget gate is not wired. " +
			"This process would serve every /llm request with no ceiling and bill nothing, for as long as it " +
			"runs. Restore NATS and the database, or set LLM_BUDGET_REQUIRE_ENFORCEMENT=auto to accept the " +
			"readiness gate and the background recovery instead, or =off to serve unmetered on purpose")
	}
	// auto.
	if cfg.NATSURL != "" {
		// State 1. /readyz reports not ready and startBudgetRecovery re-dials.
		// Exiting here would replace a pod that returns to service on its own
		// with a CrashLoopBackOff that cannot start until NATS does.
		return nil
	}
	if budgets.err != nil {
		// Cannot prove state 2. main logs the failure to read; it does not
		// refuse on an unread answer.
		return nil
	}
	if !budgets.found {
		return nil
	}
	return errors.New("GATEWAY_NATS_URL is empty and this database holds enforcing budget rows " +
		"(gateway.project_budget or gateway.user_budget). The budget counters live in NATS, so this process " +
		"would ignore every authored ceiling, bill nothing, and still report ready — nothing re-dials a URL " +
		"that was never given. Set GATEWAY_NATS_URL, or set LLM_BUDGET_REQUIRE_ENFORCEMENT=off to run " +
		"unmetered on purpose")
}

// budgetStartupGate probes for authored budgets, states the posture in the log,
// and returns the refusal, if any. main calls it BEFORE the listener opens.
//
// It always logs what it decided. The state this issue is about was invisible,
// so a gate that refuses in one case and says nothing in the others would leave
// most of the same hole.
func budgetStartupGate(ctx context.Context, cfg config.Config, logger *slog.Logger, wired bool, pool *pgxpool.Pool) error {
	if wired {
		return nil
	}
	budgets := budgetEvidence{}
	if cfg.RequireBudgetEnforcement != config.RequireEnforcementOn {
		// Mode "on" refuses on the unwired gate alone, so it needs no evidence;
		// skipping the query keeps a database outage from delaying a startup
		// whose answer is already decided.
		budgets = probeAuthoredBudgets(ctx, pool)
	}
	return reportBudgetStartupPosture(cfg, logger, wired, budgets)
}

// reportBudgetStartupPosture is budgetStartupGate with the evidence already in
// hand: it writes the log lines and returns the refusal. It is separate so the
// posture reporting can be executed against every evidence shape without a
// database.
func reportBudgetStartupPosture(cfg config.Config, logger *slog.Logger, wired bool, budgets budgetEvidence) error {
	switch {
	case budgets.err != nil:
		logger.Error("BUDGET EVIDENCE UNREADABLE: cannot tell whether this deployment authored any budget, "+
			"so the startup gate cannot refuse on it (issue #304)",
			"err", budgets.err, "require_enforcement", cfg.RequireBudgetEnforcement)
	case budgets.found:
		logger.Warn("AUTHORED BUDGETS FOUND while budget enforcement is NOT wired: every ceiling in "+
			"gateway.project_budget / gateway.user_budget is ignored by this process (issue #304)",
			"require_enforcement", cfg.RequireBudgetEnforcement, "nats_url_set", cfg.NATSURL != "")
	}
	if err := budgetStartupRefusal(cfg, wired, budgets); err != nil {
		return err
	}
	if cfg.RequireBudgetEnforcement == config.RequireEnforcementOff {
		logger.Warn("BUDGET STARTUP GATE DISABLED: LLM_BUDGET_REQUIRE_ENFORCEMENT=off, so this process starts " +
			"and serves /llm even though the budget gate is not wired (issue #304)")
	}
	return nil
}
