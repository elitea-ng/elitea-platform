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
	for i, step := range steps {
		// step is "<tool>:<status>".
		tool, status, _ := strings.Cut(step, ":")
		if _, err := pool.Exec(t.Context(), `
INSERT INTO elitea_runtime.run_steps (run_id, seq, step_id, generation, project_id, application_id, kind, actor,
    name, toolkit_id, summary, started_at, ended_at, prev_hash, hash)
VALUES ($1, $2, gen_random_uuid(), 1, $3, $4, 'tool_call', 'user:11', $5, '51', jsonb_build_object('status', $6::text),
    now(), now(), decode(repeat('00', 32), 'hex'), decode(repeat('11', 32), 'hex'))`,
			runID, i+1, projectID, applicationID, tool, status); err != nil {
			t.Fatalf("seed step %d: %v", i+1, err)
		}
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
