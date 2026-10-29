package repos

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// Shared 0204 (ADR-0040 decisions 1, 3, 5 and 6, issue #1497): the
// registry, change requests with maker-checker approvals and the
// recertification pass. CI-only: skipped without the integration database.
// The fixture is the agent releases fixture: project 1, agent 31, draft
// version 41.

func newLifecycleTestStore(t *testing.T, pool *pgxpool.Pool, policy agentlifecycle.Policy) *AgentLifecycle {
	t.Helper()
	appender, err := securityaudit.NewPostgresAppender("elitea-main-test")
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewAgentLifecycle(pool, appender, func(context.Context) (agentlifecycle.Policy, error) {
		return policy, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func lifecycleAuditActions(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
SELECT event->'data'->>'action' FROM elitea_runtime.security_audit_outbox
WHERE event_type = $1 ORDER BY seq`, string(securityaudit.TypeAgentLifecycle))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var actions []string
	for rows.Next() {
		var action string
		if err := rows.Scan(&action); err != nil {
			t.Fatal(err)
		}
		actions = append(actions, action)
	}
	return actions
}

func requirePgCode(t *testing.T, err error, code, what string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("%s: got %v, want SQLSTATE %s", what, err, code)
	}
}

func requireRefusal(t *testing.T, err error, code string) {
	t.Helper()
	refusal, ok := agentlifecycle.AsRefusal(err)
	if !ok || refusal.Code != code {
		t.Fatalf("got %v, want the refusal %s", err, code)
	}
}

// TestAgentLifecycle_DatabaseRefusesSelfApproval proves the maker-checker
// rule holds when code is bypassed: an approval row whose approver is the
// proposer violates the CHECK, and a row that names another proposer to
// dodge it violates the foreign key to the change request's own proposer.
func TestAgentLifecycle_DatabaseRefusesSelfApproval(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentReleaseFixture(t, pool)
	store := newLifecycleTestStore(t, pool, agentlifecycle.DefaultPolicy())
	if _, err := store.Registry(t.Context(), 1, 31); err != nil {
		t.Fatalf("lazy registry row: %v", err)
	}
	var changeID int64
	if err := pool.QueryRow(t.Context(), `
INSERT INTO elitea_lifecycle.change_requests
    (tenant_project_id, application_id, kind, state, risk_tier, candidate_version_id, version_name,
     summary, proposed_by, required_approvals)
VALUES (1, 31, 'release', 'submitted', 'medium', 41, 'v2', 'A change.', 7, 1)
RETURNING id`).Scan(&changeID); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO elitea_lifecycle.change_request_approvals (change_request_id, proposed_by, approver_id)
VALUES ($1, $2, $3)`
	_, err := pool.Exec(t.Context(), insert, changeID, 7, 7)
	requirePgCode(t, err, "23514", "the proposer approving their own request")
	_, err = pool.Exec(t.Context(), insert, changeID, 8, 7)
	requirePgCode(t, err, "23503", "an approval that names a forged proposer")
	if _, err := pool.Exec(t.Context(), insert, changeID, 7, 8); err != nil {
		t.Fatalf("an approval by another user: %v", err)
	}
	_, err = pool.Exec(t.Context(), insert, changeID, 7, 8)
	requirePgCode(t, err, "23505", "a second approval by the same approver")
}

// TestAgentLifecycle_RecertificationMakerChecker walks a recertification
// change request of a medium-tier agent through the store: the author's
// approval is refused, a direct publish is refused, and another user's
// approval certifies the published release.
func TestAgentLifecycle_RecertificationMakerChecker(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentReleaseFixture(t, pool)
	releases, _ := newReleaseTestStores(t, pool)
	release := releaseTestPublish(t, pool, releases, 41, 141, "v1")
	store := newLifecycleTestStore(t, pool, agentlifecycle.DefaultPolicy())
	ctx := t.Context()

	tier := agentlifecycle.TierMedium
	if _, err := store.UpdateRegistry(ctx, 1, 31, RegistryActor{UserID: 11}, agentlifecycle.RegistryUpdate{Tier: &tier}); err != nil {
		t.Fatalf("raise the tier: %v", err)
	}
	low := agentlifecycle.TierLow
	_, err := store.UpdateRegistry(ctx, 1, 31, RegistryActor{UserID: 11}, agentlifecycle.RegistryUpdate{Tier: &low})
	requireRefusal(t, err, agentlifecycle.TierLowerCode)
	requireRefusal(t, store.CheckDirectPublish(ctx, 1, 31), agentlifecycle.ChangeRequiredCode)

	change, err := store.CreateChangeRequest(ctx, agentlifecycle.NewChangeRequest{
		ProjectID: 1, ApplicationID: 31, Kind: agentlifecycle.KindRecertification,
		Summary: "Annual review.", ProposedBy: 7,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = store.CreateChangeRequest(ctx, agentlifecycle.NewChangeRequest{
		ProjectID: 1, ApplicationID: 31, Kind: agentlifecycle.KindRecertification,
		Summary: "A second one.", ProposedBy: 8,
	})
	requireRefusal(t, err, agentlifecycle.OpenChangeCode)
	if _, err := store.SubmitChangeRequest(ctx, 1, 31, change.ID, 8); err == nil {
		t.Fatal("a non-author submitted the change request")
	}
	if _, err := store.SubmitChangeRequest(ctx, 1, 31, change.ID, 7); err != nil {
		t.Fatalf("submit: %v", err)
	}
	_, err = store.ApproveChangeRequest(ctx, Approval{ProjectID: 1, ApplicationID: 31, ChangeID: change.ID, ApproverID: 7})
	requireRefusal(t, err, agentlifecycle.SelfApprovalCode)

	outcome, err := store.ApproveChangeRequest(ctx, Approval{ProjectID: 1, ApplicationID: 31, ChangeID: change.ID, ApproverID: 8})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if !outcome.Complete || outcome.Change.State != agentlifecycle.ChangeApproved || outcome.Change.ReleaseID != release.ID {
		t.Fatalf("outcome = %+v", outcome)
	}
	registry, err := store.Registry(ctx, 1, 31)
	if err != nil {
		t.Fatal(err)
	}
	if registry.State != agentlifecycle.StateCertified || registry.CertifiedReleaseID != release.ID ||
		registry.RecertifyBy == nil || registry.RecertifyBy.Before(time.Now().Add(364*24*time.Hour)) {
		t.Fatalf("registry = %+v", registry)
	}
	want := []string{agentlifecycle.ActionRegistryTierChanged, agentlifecycle.ActionChangeSubmitted,
		agentlifecycle.ActionChangeApproved, agentlifecycle.ActionCertificationCert}
	got := lifecycleAuditActions(t, pool)
	if len(got) != len(want) {
		t.Fatalf("audit actions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("audit actions = %v, want %v", got, want)
		}
	}
}

// TestAgentLifecycle_LapseSuspendRefusesRuns proves lapse_action suspend:
// a certification past recertify_by lapses, the pass writes an agent
// suspension of source certification, and the admission gate refuses the
// agent with agent_certification_lapsed. Switching back to warn resumes it.
func TestAgentLifecycle_LapseSuspendRefusesRuns(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentReleaseFixture(t, pool)
	releases, _ := newReleaseTestStores(t, pool)
	release := releaseTestPublish(t, pool, releases, 41, 141, "v1")
	policy := agentlifecycle.DefaultPolicy()
	policy.LapseAction = agentlifecycle.LapseSuspend
	store := newLifecycleTestStore(t, pool, policy)
	ctx := t.Context()
	if _, err := store.Registry(ctx, 1, 31); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE elitea_lifecycle.agent_registry
SET certification_state = 'certified', certified_release_id = $1,
    certified_at = clock_timestamp() - interval '400 days', recertify_by = clock_timestamp() - interval '1 day'
WHERE tenant_project_id = 1 AND application_id = 31`, release.ID); err != nil {
		t.Fatal(err)
	}
	suspensions, err := NewExecutionSuspensionRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := safety.NewGate(suspensions, safety.GateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	deps := RecertificationDeps{Drift: releases, Suspender: suspensions, Notify: gate.Notify}

	summary, err := store.RecertificationPass(ctx, deps)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if summary.Lapsed != 1 || summary.Suspended != 1 {
		t.Fatalf("summary = %s", summary)
	}
	registry, err := store.Registry(ctx, 1, 31)
	if err != nil {
		t.Fatal(err)
	}
	if registry.State != agentlifecycle.StateSuspended || registry.SuspensionID == 0 {
		t.Fatalf("registry = %+v", registry)
	}
	gate.Invalidate()
	err = gate.Check(ctx, safety.Subject{ProjectID: 1, ApplicationID: 31})
	suspended, ok := safety.AsSuspended(err)
	if !ok || suspended.Code() != safety.CertificationLapsedCode {
		t.Fatalf("admission = %v, want %s", err, safety.CertificationLapsedCode)
	}
	if err := gate.Check(ctx, safety.Subject{ProjectID: 1, ApplicationID: 32}); err != nil {
		t.Fatalf("another agent is refused: %v", err)
	}

	// A second pass changes nothing: the suspension is already there.
	summary, err = store.RecertificationPass(ctx, deps)
	if err != nil || summary.Suspended != 0 || summary.Lapsed != 0 {
		t.Fatalf("second pass = %s, %v", summary, err)
	}

	// lapse_action back to warn: the agent runs again, still lapsed.
	warn := newLifecycleTestStore(t, pool, agentlifecycle.DefaultPolicy())
	summary, err = warn.RecertificationPass(ctx, deps)
	if err != nil || summary.Unsuspended != 1 {
		t.Fatalf("warn pass = %s, %v", summary, err)
	}
	gate.Invalidate()
	if err := gate.Check(ctx, safety.Subject{ProjectID: 1, ApplicationID: 31}); err != nil {
		t.Fatalf("admission after warn: %v", err)
	}
	if registry, err = store.Registry(ctx, 1, 31); err != nil || registry.State != agentlifecycle.StateLapsed {
		t.Fatalf("registry after warn = %+v, %v", registry, err)
	}
}
