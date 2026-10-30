package repos

// The agent circuit breaker against a REAL migrated database (shared 0213,
// ADR-0041 decision 2, #1498): synthetic failures past a rule trip the
// breaker (a breaker suspension at agent scope that cancels the run in
// flight, its security audit event and the owners' notifications); a
// suspended agent is not tripped again; a resume by the wrong role is
// refused; the window restarts after a resume. CI-only: skipped without the
// integration database.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/breaker"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

func newBreakerTestStore(t *testing.T, pool *pgxpool.Pool) *AgentBreaker {
	t.Helper()
	appender, err := securityaudit.NewPostgresAppender("elitea-main-test")
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewAgentBreaker(pool, appender)
	if err != nil {
		t.Fatal(err)
	}
	// The runs these tests seed settle now; TestAgentBreaker_IngestComputes
	// EachRunOnce covers the grace.
	store.settleGrace = 0
	return store
}

// seedEndedAgentRun writes an agent run of agent (project, application)
// that ended in state.
func seedEndedAgentRun(t *testing.T, pool *pgxpool.Pool, executionID string, projectID, applicationID int64, state string) {
	t.Helper()
	seedAgentRun(t, pool, executionID, projectID, applicationID)
	if _, err := pool.Exec(t.Context(), `
UPDATE elitea_runtime.execution_jobs SET state = $2, settled_at = clock_timestamp() WHERE execution_id = $1`,
		executionID, state); err != nil {
		t.Fatalf("settle %s: %v", executionID, err)
	}
}

func seedToolSteps(t *testing.T, pool *pgxpool.Pool, runID string, projectID, applicationID int64, steps ...string) {
	t.Helper()
	base := len(existingSteps(t, pool, runID))
	for i, step := range steps {
		// step is "<tool>:<status>"; status "capped" is a call the worker's
		// run caps meter refused (an error step with the meter's text).
		tool, status, _ := strings.Cut(step, ":")
		errorText := ""
		if status == "capped" {
			status, errorText = "error", "guardrail.forbidden: "+capRefusalText
		}
		if _, err := pool.Exec(t.Context(), `
INSERT INTO elitea_runtime.run_steps (run_id, seq, step_id, generation, project_id, application_id, kind, actor,
    name, toolkit_id, summary, started_at, ended_at, prev_hash, hash)
VALUES ($1, $2, gen_random_uuid(), 1, $3, $4, 'tool_call', 'user:11', $5, '51',
    jsonb_strip_nulls(jsonb_build_object('status', $6::text, 'error', NULLIF($7::text, ''))),
    now(), now(), decode(repeat('00', 32), 'hex'), decode(repeat('11', 32), 'hex'))`,
			runID, base+i+1, projectID, applicationID, tool, status, errorText); err != nil {
			t.Fatalf("seed step %d: %v", i+1, err)
		}
	}
}

func existingSteps(t *testing.T, pool *pgxpool.Pool, runID string) []int64 {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT seq FROM elitea_runtime.run_steps WHERE run_id = $1`, runID)
	if err != nil {
		t.Fatal(err)
	}
	seqs, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		t.Fatal(err)
	}
	return seqs
}

// seedWorkerCapFailure writes the failure event Main projects for a run the
// worker stopped at a run cap (runtime_failure.go: code CAP_REACHED).
func seedWorkerCapFailure(t *testing.T, pool *pgxpool.Pool, executionID string, projectID int64) {
	t.Helper()
	event := `{"code":"CAP_REACHED","cap":"max_tool_calls","safe_message":"The run stopped.","retryable":false}`
	if _, err := pool.Exec(t.Context(), `
INSERT INTO elitea_runtime.execution_replay_events
    (event_id, execution_id, generation, projection_project_id, event_type, event_bytes, event_digest)
VALUES ('failed-' || $1, $1, 1, $2, 'execution.failed', convert_to($3, 'UTF8'), sha256(convert_to($3, 'UTF8')))`,
		executionID, projectID, event); err != nil {
		t.Fatalf("seed the cap failure of %s: %v", executionID, err)
	}
}

func breakerPass(store *AgentBreaker, metrics *breaker.Metrics) *breaker.Pass {
	return &breaker.Pass{Store: store, Metrics: metrics,
		Settings: func(context.Context) (breaker.Settings, error) { return breaker.DefaultSettings(), nil }}
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func TestAgentBreaker_FailuresTripTheBreakerAndTheWrongRoleCannotResume(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentReleaseFixture(t, pool)
	lifecycle := newLifecycleTestStore(t, pool, agentlifecycle.DefaultPolicy())
	ctx := t.Context()
	high := agentlifecycle.TierHigh
	if _, err := lifecycle.UpdateRegistry(ctx, 1, 31, RegistryActor{UserID: 11}, agentlifecycle.RegistryUpdate{Tier: &high}); err != nil {
		t.Fatalf("set the tier: %v", err)
	}

	// 11 of 20 runs failed (55 % > 50 %), and cancelled runs do not count.
	for i := range 20 {
		state := "SUCCEEDED"
		if i < 11 {
			state = "FAILED"
		}
		seedEndedAgentRun(t, pool, fmt.Sprintf("brk-a-%02d", i), 1, 31, state)
	}
	seedEndedAgentRun(t, pool, "brk-a-cancelled", 1, 31, "CANCELLED")
	// Another agent with a healthy window.
	for i := range 20 {
		seedEndedAgentRun(t, pool, fmt.Sprintf("brk-b-%02d", i), 1, 32, "SUCCEEDED")
	}
	seedAgentRun(t, pool, "brk-a-inflight", 1, 31)

	store := newBreakerTestStore(t, pool)
	metrics := breaker.NewMetrics()
	summary, err := breakerPass(store, metrics).Run(ctx)
	if err != nil {
		t.Fatalf("pass: %v (%s)", err, summary.String())
	}
	if summary.Tripped != 1 || summary.Evaluated != 2 {
		t.Fatalf("summary = %s", summary.String())
	}
	if metrics.Trips(breaker.RuleErrorRate) != 1 {
		t.Fatalf("trips metric = %d", metrics.Trips(breaker.RuleErrorRate))
	}

	repo, err := NewExecutionSuspensionRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	active, err := repo.LoadActiveSuspensions(ctx)
	if err != nil || len(active) != 1 {
		t.Fatalf("active = %+v, %v", active, err)
	}
	trip := active[0]
	if trip.Source != safety.SourceBreaker || trip.Target.Scope != safety.ScopeAgent || trip.Target.ApplicationID != 31 ||
		trip.ResumeRequires != safety.ResumeGovernanceRole || trip.ActorID != 0 {
		t.Fatalf("suspension = %+v", trip)
	}
	if trip.Detail["rule"] != "error_rate" || trip.Detail["observed"] != float64(55) || trip.Detail["failed_runs"] != float64(11) {
		t.Fatalf("detail = %+v", trip.Detail)
	}
	if got := desiredState(t, pool, "brk-a-inflight"); got != "CANCELLED" {
		t.Fatalf("the run in flight was not cancelled: %s", got)
	}
	if n := countRows(t, pool, `
SELECT count(*) FROM elitea_runtime.security_audit_outbox
WHERE event_type = $1 AND event->'data'->>'action' = 'agent.breaker.tripped'`, string(securityaudit.TypeAgentBreaker)); n != 1 {
		t.Fatalf("audit events = %d, want 1", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM centry.notifications WHERE event_type = $1`, BreakerNotificationEventType); n < 1 {
		t.Fatalf("no owner was notified")
	}

	// Suspended: a later failure does not trip it again.
	seedEndedAgentRun(t, pool, "brk-a-late", 1, 31, "FAILED")
	if summary, err := breakerPass(store, metrics).Run(ctx); err != nil || summary.Tripped != 0 {
		t.Fatalf("second pass = %s, %v", summary.String(), err)
	}

	// The wrong role: a project admin may not resume a governance_role stop.
	service, err := safety.NewService(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	projectAdmin := safety.Actor{UserID: 5, ProjectID: 1}
	if _, err := service.Resume(ctx, projectAdmin, safety.ResumeRequest{ID: trip.ID}); !errors.Is(err, safety.ErrResumeForbidden) {
		t.Fatalf("project admin resume: %v, want ErrResumeForbidden", err)
	}
	resumed, err := service.Resume(ctx, safety.Actor{UserID: 9, PlatformAdmin: true}, safety.ResumeRequest{ID: trip.ID, Reason: "fixed the prompt"})
	if err != nil || resumed.Active() {
		t.Fatalf("platform admin resume = %+v, %v", resumed, err)
	}

	// The window restarts: the runs before the resume do not count.
	seedEndedAgentRun(t, pool, "brk-a-after", 1, 31, "FAILED")
	if summary, err := breakerPass(store, metrics).Run(ctx); err != nil || summary.Tripped != 0 {
		t.Fatalf("pass after the resume = %s, %v", summary.String(), err)
	}
	observation, err := store.Observe(ctx, breaker.AgentKey{ProjectID: 1, ApplicationID: 31}, 20)
	if err != nil || len(observation.Outcomes) != 1 || observation.Suspended {
		t.Fatalf("observation after the resume = %+v, %v", observation, err)
	}
}

// A critical agent's breaker stop needs two different platform
// administrators: the first records an approval, the same one cannot
// complete it, a second one resumes. A project admin is refused.
func TestAgentBreaker_ToolStreakTripsACriticalAgentAndNeedsTwoPeople(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentReleaseFixture(t, pool)
	lifecycle := newLifecycleTestStore(t, pool, agentlifecycle.DefaultPolicy())
	ctx := t.Context()
	critical := agentlifecycle.TierCritical
	if _, err := lifecycle.UpdateRegistry(ctx, 1, 31, RegistryActor{UserID: 11}, agentlifecycle.RegistryUpdate{Tier: &critical}); err != nil {
		t.Fatalf("set the tier: %v", err)
	}
	// search fails 5 times in a row; another tool between the failures does
	// not break the streak, and a success of search before them does not count.
	seedToolSteps(t, pool, "brk-tool", 1, 31,
		"search:ok", "search:error", "search:error", "fetch:ok", "search:error", "search:error", "search:error")
	seedEndedAgentRun(t, pool, "brk-tool", 1, 31, "SUCCEEDED")

	store := newBreakerTestStore(t, pool)
	summary, err := breakerPass(store, breaker.NewMetrics()).Run(ctx)
	if err != nil || summary.Tripped != 1 {
		t.Fatalf("pass = %s, %v", summary.String(), err)
	}
	repo, err := NewExecutionSuspensionRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	active, err := repo.LoadActiveSuspensions(ctx)
	if err != nil || len(active) != 1 {
		t.Fatalf("active = %+v, %v", active, err)
	}
	trip := active[0]
	if trip.ResumeRequires != safety.ResumeTwoPerson || trip.Detail["rule"] != "tool_failure_streak" ||
		trip.Detail["tool_name"] != "search" || trip.Detail["execution_id"] != "brk-tool" {
		t.Fatalf("suspension = %+v", trip)
	}

	service, err := safety.NewService(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Resume(ctx, safety.Actor{UserID: 5, ProjectID: 1}, safety.ResumeRequest{ID: trip.ID}); !errors.Is(err, safety.ErrResumeForbidden) {
		t.Fatalf("project admin: %v", err)
	}
	first := safety.Actor{UserID: 8, PlatformAdmin: true}
	approved, err := service.Resume(ctx, first, safety.ResumeRequest{ID: trip.ID})
	if !errors.Is(err, safety.ErrResumeApprovalRecorded) || !approved.Active() || approved.ResumeApprovedBy == nil {
		t.Fatalf("first admin = %+v, %v", approved, err)
	}
	if _, err := service.Resume(ctx, first, safety.ResumeRequest{ID: trip.ID}); !errors.Is(err, safety.ErrSecondPersonRequired) {
		t.Fatalf("same admin again: %v", err)
	}
	resumed, err := service.Resume(ctx, safety.Actor{UserID: 9, PlatformAdmin: true}, safety.ResumeRequest{ID: trip.ID, Reason: "tool fixed"})
	if err != nil || resumed.Active() {
		t.Fatalf("second admin = %+v, %v", resumed, err)
	}
	// The database holds the rule too: no resume of a system two-person stop
	// without an approval by another administrator.
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.execution_suspensions (scope, project_id, scope_key, reason, actor_id, source, resume_requires)
VALUES ('agent', 1, '77', 'x', 0, 'breaker', 'two_person')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_suspensions SET resumed_at = now(), resumed_by = 9
WHERE scope_key = '77' AND resumed_at IS NULL`); err == nil {
		t.Fatal("a system two-person stop resumed without an approval")
	}
}

// The registry override only tightens: a looser value is refused with 422
// circuit_breaker_override_loosens; a tighter one is stored and audited.
func TestAgentBreaker_RegistryOverrideOnlyTightens(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentReleaseFixture(t, pool)
	lifecycle := newLifecycleTestStore(t, pool, agentlifecycle.DefaultPolicy())
	ctx := t.Context()

	_, err := lifecycle.UpdateRegistry(ctx, 1, 31, RegistryActor{UserID: 11},
		agentlifecycle.RegistryUpdate{CircuitBreaker: []byte(`{"error_rate_percent":80}`)})
	requireRefusal(t, err, agentlifecycle.BreakerOverrideLoosensCode)
	_, err = lifecycle.UpdateRegistry(ctx, 1, 31, RegistryActor{UserID: 11},
		agentlifecycle.RegistryUpdate{CircuitBreaker: []byte(`{"error_rate":30}`)})
	requireRefusal(t, err, agentlifecycle.InvalidCode)

	registry, err := lifecycle.UpdateRegistry(ctx, 1, 31, RegistryActor{UserID: 11},
		agentlifecycle.RegistryUpdate{CircuitBreaker: []byte(`{"error_rate_percent":30,"spend_per_hour_usd":2.5}`)})
	if err != nil {
		t.Fatal(err)
	}
	override, err := breaker.ParseOverride(registry.CircuitBreaker)
	if err != nil || override.ErrorRatePercent == nil || *override.ErrorRatePercent != 30 || *override.SpendPerHourUSD != 2.5 {
		t.Fatalf("stored = %s, %v", registry.CircuitBreaker, err)
	}
	found := false
	for _, action := range lifecycleAuditActions(t, pool) {
		found = found || action == agentlifecycle.ActionRegistryBreakerOverride
	}
	if !found {
		t.Fatal("the override change was not audited")
	}
	store := newBreakerTestStore(t, pool)
	if any, err := store.AnySpendOverride(ctx); err != nil || !any {
		t.Fatalf("AnySpendOverride = %v, %v", any, err)
	}
	observation, err := store.Observe(ctx, breaker.AgentKey{ProjectID: 1, ApplicationID: 31}, 20)
	if err != nil || observation.Override.ErrorRatePercent == nil || *observation.Override.ErrorRatePercent != 30 {
		t.Fatalf("observation = %+v, %v", observation, err)
	}
}

// Run cap stops are not failures of the agent (ADR-0041 decisions 2 and 3):
// a run the worker stopped at a cap (FAILED, CAP_REACHED) and a run Main
// cancelled at a cap are left out of the error rate, and the tool calls the
// run caps meter refused are not in a tool's failure streak. Genuine
// failures still trip.
func TestAgentBreaker_CapStopsAreNotFailures(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentReleaseFixture(t, pool)
	ctx := t.Context()
	store := newBreakerTestStore(t, pool)
	metrics := breaker.NewMetrics()

	// 20 ended runs, 16 of them FAILED at the worker's tool call cap and 4
	// genuine failures; one more run Main cancelled at its wall cap. Counted
	// as failures, that is 20 of 20.
	for i := range 20 {
		id := fmt.Sprintf("brk-cap-%02d", i)
		seedAgentRun(t, pool, id, 1, 31)
		if i < 16 {
			// search failed 4 times, then the meter refused 3 more calls:
			// the streak is 4, under the default 5.
			seedToolSteps(t, pool, id, 1, 31, "search:error", "search:error", "search:error", "search:error",
				"search:capped", "search:capped", "search:capped")
			seedWorkerCapFailure(t, pool, id, 1)
		}
		if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs SET state = 'FAILED', settled_at = clock_timestamp() WHERE execution_id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
	seedAgentRun(t, pool, "brk-cap-main", 1, 31)
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs SET state = 'CANCELLED', cancel_reason = 'cap_reached', settled_at = clock_timestamp()
WHERE execution_id = 'brk-cap-main'`); err != nil {
		t.Fatal(err)
	}

	summary, err := breakerPass(store, metrics).Run(ctx)
	if err != nil || summary.Tripped != 0 || summary.Evaluated != 1 {
		t.Fatalf("pass with cap stops = %s, %v", summary.String(), err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM elitea_runtime.agent_breaker_runs WHERE cap_reached`); n != 17 {
		t.Fatalf("cap stops in the window = %d, want 17", n)
	}
	observation, err := store.Observe(ctx, breaker.AgentKey{ProjectID: 1, ApplicationID: 31}, 20)
	if err != nil || len(observation.Outcomes) != 4 || observation.ToolStreak != 4 {
		t.Fatalf("observation = %+v, %v", observation, err)
	}

	// 16 more genuine failures fill the window: 20 of 20 failed.
	for i := range 16 {
		seedEndedAgentRun(t, pool, fmt.Sprintf("brk-cap-real-%02d", i), 1, 31, "FAILED")
	}
	summary, err = breakerPass(store, metrics).Run(ctx)
	if err != nil || summary.Tripped != 1 || metrics.Trips(breaker.RuleErrorRate) != 1 {
		t.Fatalf("pass with genuine failures = %s, %v", summary.String(), err)
	}
}

// IngestRuns computes each run once: a second pass copies nothing and does
// not read the run again (a step written after the copy does not change the
// row), and a run inside the settle grace is copied only after it.
func TestAgentBreaker_IngestComputesEachRunOnce(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentReleaseFixture(t, pool)
	ctx := t.Context()
	store := newBreakerTestStore(t, pool)
	store.settleGrace = breaker.IngestSettleGrace

	seedToolSteps(t, pool, "brk-once", 1, 31, "search:error", "search:error")
	seedAgentRun(t, pool, "brk-once", 1, 31)
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs SET state = 'SUCCEEDED', settled_at = clock_timestamp() - interval '1 minute'
WHERE execution_id = 'brk-once'`); err != nil {
		t.Fatal(err)
	}
	// Settled now: inside the grace.
	seedEndedAgentRun(t, pool, "brk-fresh", 1, 32, "SUCCEEDED")

	touched, err := store.IngestRuns(ctx, breaker.IngestLookback)
	if err != nil || len(touched) != 1 || touched[0] != (breaker.AgentKey{ProjectID: 1, ApplicationID: 31}) {
		t.Fatalf("first ingest = %v, %v", touched, err)
	}
	var recorded string
	if err := pool.QueryRow(ctx, `SELECT recorded_at::text FROM elitea_runtime.agent_breaker_runs WHERE execution_id = 'brk-once'`).Scan(&recorded); err != nil {
		t.Fatal(err)
	}

	// A step that (against the claim fence) lands after the copy: the run is
	// not computed again.
	seedToolSteps(t, pool, "brk-once", 1, 31, "search:error", "search:error", "search:error")
	touched, err = store.IngestRuns(ctx, breaker.IngestLookback)
	if err != nil || len(touched) != 0 {
		t.Fatalf("second ingest = %v, %v", touched, err)
	}
	var (
		streak int
		again  string
	)
	if err := pool.QueryRow(ctx, `
SELECT tool_failure_streak, recorded_at::text FROM elitea_runtime.agent_breaker_runs WHERE execution_id = 'brk-once'`).Scan(&streak, &again); err != nil {
		t.Fatal(err)
	}
	if streak != 2 || again != recorded {
		t.Fatalf("the run was computed again: streak %d recorded %s then %s", streak, recorded, again)
	}

	// Past the grace, the fresh run is copied, once.
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs SET settled_at = clock_timestamp() - interval '1 minute' WHERE execution_id = 'brk-fresh'`); err != nil {
		t.Fatal(err)
	}
	touched, err = store.IngestRuns(ctx, breaker.IngestLookback)
	if err != nil || len(touched) != 1 || touched[0] != (breaker.AgentKey{ProjectID: 1, ApplicationID: 32}) {
		t.Fatalf("ingest after the grace = %v, %v", touched, err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM elitea_runtime.agent_breaker_runs`); n != 2 {
		t.Fatalf("window rows = %d, want 2", n)
	}
}

// SpendOverLimit answers only the agents whose spend in the last hour is
// over their own limit: the tier default tightened by the override.
func TestAgentBreaker_SpendOverLimitSelectsOnlyAgentsOverTheirLimit(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentReleaseFixture(t, pool)
	lifecycle := newLifecycleTestStore(t, pool, agentlifecycle.DefaultPolicy())
	ctx := t.Context()
	store := newBreakerTestStore(t, pool)

	// 31: high tier, override 1 USD, spends 2. 33: no registry record (low
	// tier, no override), spends 50.
	high := agentlifecycle.TierHigh
	if _, err := lifecycle.UpdateRegistry(ctx, 1, 31, RegistryActor{UserID: 11},
		agentlifecycle.RegistryUpdate{Tier: &high, CircuitBreaker: []byte(`{"spend_per_hour_usd":1}`)}); err != nil {
		t.Fatal(err)
	}
	for app, usd := range map[int64]float64{31: 2, 33: 50} {
		id := fmt.Sprintf("brk-spend-%d", app)
		seedAgentRun(t, pool, id, 1, app)
		if _, err := pool.Exec(ctx, `
INSERT INTO gateway.llm_request_logs
    (occurred_at, project_id, user_id, route, method, status, duration_ms, provider, model, execution_id, operation, cost_usd)
VALUES (now(), 1, 7, '/tools', 'POST', 200, 10, 'firecrawl', 'scrape', $1, 'tool', $2)`, id, usd); err != nil {
			t.Fatalf("seed the spend of %d: %v", app, err)
		}
	}
	check := func(limits map[agentlifecycle.Tier]float64, want map[breaker.AgentKey]float64) {
		t.Helper()
		got, err := store.SpendOverLimit(ctx, limits)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("over the limit = %v, want %v", got, want)
		}
		for key, usd := range want {
			if got[key] != usd {
				t.Fatalf("over the limit = %v, want %v", got, want)
			}
		}
	}
	over, noRecord := breaker.AgentKey{ProjectID: 1, ApplicationID: 31}, breaker.AgentKey{ProjectID: 1, ApplicationID: 33}
	// No tier limit: 31 is over its override; 33 has no limit.
	check(map[agentlifecycle.Tier]float64{}, map[breaker.AgentKey]float64{over: 2})
	// A low tier limit applies to the agent with no registry record.
	check(map[agentlifecycle.Tier]float64{agentlifecycle.TierLow: 40}, map[breaker.AgentKey]float64{over: 2, noRecord: 50})
	// 31's override raised to 5 under a high tier limit of 10: the tighter
	// is 5 and 31 spends 2, so nobody is over.
	if _, err := lifecycle.UpdateRegistry(ctx, 1, 31, RegistryActor{UserID: 11},
		agentlifecycle.RegistryUpdate{CircuitBreaker: []byte(`{"spend_per_hour_usd":5}`)}); err != nil {
		t.Fatal(err)
	}
	check(map[agentlifecycle.Tier]float64{agentlifecycle.TierHigh: 10}, map[breaker.AgentKey]float64{})
	// A high tier limit tighter than the override applies.
	check(map[agentlifecycle.Tier]float64{agentlifecycle.TierHigh: 1.5}, map[breaker.AgentKey]float64{over: 2})
}
