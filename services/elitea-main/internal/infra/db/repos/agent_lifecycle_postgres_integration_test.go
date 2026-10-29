package repos

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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

func newLifecycleTestStore(t *testing.T, pool *pgxpool.Pool, policy agentlifecycle.Policy, options ...AgentLifecycleOption) *AgentLifecycle {
	t.Helper()
	appender, err := securityaudit.NewPostgresAppender("elitea-main-test")
	if err != nil {
		t.Fatal(err)
	}
	releases, _ := newReleaseTestStores(t, pool)
	options = append([]AgentLifecycleOption{WithCandidateDigester(releases)}, options...)
	store, err := NewAgentLifecycle(pool, appender, func(context.Context) (agentlifecycle.Policy, error) {
		return policy, nil
	}, options...)
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
SET certification_state = 'certified', certified_release_id = $1, risk_tier = 'medium',
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

// certifyForTest sets agent 31 certified on release with the given
// deadline offset (negative: passed).
func certifyForTest(t *testing.T, pool *pgxpool.Pool, releaseID int64, state string, recertifyIn time.Duration) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
UPDATE elitea_lifecycle.agent_registry
SET certification_state = $2, certified_release_id = $1,
    certified_at = clock_timestamp() - interval '400 days', recertify_by = clock_timestamp() + $3::interval
WHERE tenant_project_id = 1 AND application_id = 31`, releaseID, state, recertifyIn.String()); err != nil {
		t.Fatal(err)
	}
}

func countAction(actions []string, action string) int {
	n := 0
	for _, a := range actions {
		if a == action {
			n++
		}
	}
	return n
}

// TestAgentLifecycle_SuspendedEventForCoveredAgent proves a lapsed row
// promoted to suspended while a manual suspension already covers it still
// gets its agent.certification.suspended event, and that the pass writes
// no second suspension for it.
func TestAgentLifecycle_SuspendedEventForCoveredAgent(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentReleaseFixture(t, pool)
	releases, _ := newReleaseTestStores(t, pool)
	release := releaseTestPublish(t, pool, releases, 41, 141, "v1")
	ctx := t.Context()
	warn := newLifecycleTestStore(t, pool, agentlifecycle.DefaultPolicy())
	if _, err := warn.Registry(ctx, 1, 31); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE elitea_lifecycle.agent_registry SET risk_tier = 'medium'
WHERE tenant_project_id = 1 AND application_id = 31`); err != nil {
		t.Fatal(err)
	}
	certifyForTest(t, pool, release.ID, "certified", -24*time.Hour)
	suspensions, err := NewExecutionSuspensionRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	deps := RecertificationDeps{Drift: releases, Suspender: suspensions}
	if summary, err := warn.RecertificationPass(ctx, deps); err != nil || summary.Lapsed != 1 {
		t.Fatalf("warn pass = %s, %v", summary, err)
	}
	manual, err := suspensions.CreateSuspension(ctx, safety.NewSuspension{
		Target: safety.Target{Scope: safety.ScopeAgent, ProjectID: 1, ApplicationID: 31},
		Reason: "Incident.", Source: safety.SourceManual, ResumeRequires: safety.ResumeAnyAdmin,
	})
	if err != nil {
		t.Fatal(err)
	}
	policy := agentlifecycle.DefaultPolicy()
	policy.LapseAction = agentlifecycle.LapseSuspend
	suspend := newLifecycleTestStore(t, pool, policy)
	summary, err := suspend.RecertificationPass(ctx, deps)
	if err != nil || summary.Suspended != 0 {
		t.Fatalf("suspend pass = %s, %v", summary, err)
	}
	if got := countAction(lifecycleAuditActions(t, pool), agentlifecycle.ActionCertificationSuspended); got != 1 {
		t.Fatalf("suspended events = %d, want 1", got)
	}
	var active int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.execution_suspensions
WHERE scope = 'agent' AND scope_key = '31' AND resumed_at IS NULL`).Scan(&active); err != nil || active != 1 {
		t.Fatalf("active suspensions = %d, %v (manual %d)", active, err, manual.ID)
	}
}

// TestAgentLifecycle_LowerTierWithoutCadenceRecertifies proves a suspended
// agent lowered to a tier with no cadence returns to certified with no
// deadline and its certification suspension resumed, both audited; and
// that a tier change moves recertify_by to the new tier's cadence.
func TestAgentLifecycle_LowerTierWithoutCadenceRecertifies(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentReleaseFixture(t, pool)
	releases, _ := newReleaseTestStores(t, pool)
	release := releaseTestPublish(t, pool, releases, 41, 141, "v1")
	ctx := t.Context()
	policy := agentlifecycle.DefaultPolicy()
	policy.LapseAction = agentlifecycle.LapseSuspend
	store := newLifecycleTestStore(t, pool, policy)
	governance := RegistryActor{UserID: 11, Governance: true}
	medium := agentlifecycle.TierMedium
	if _, err := store.UpdateRegistry(ctx, 1, 31, governance, agentlifecycle.RegistryUpdate{Tier: &medium}); err != nil {
		t.Fatal(err)
	}
	certifyForTest(t, pool, release.ID, "certified", 300*24*time.Hour)

	// medium → high: recertify_by is certified_at + 180 days, already past.
	high := agentlifecycle.TierHigh
	registry, err := store.UpdateRegistry(ctx, 1, 31, governance, agentlifecycle.RegistryUpdate{Tier: &high})
	if err != nil {
		t.Fatal(err)
	}
	if registry.RecertifyBy == nil || registry.CertifiedAt == nil ||
		!registry.RecertifyBy.Equal(registry.CertifiedAt.Add(180*agentlifecycle.Day)) {
		t.Fatalf("recertify_by = %v, certified_at = %v", registry.RecertifyBy, registry.CertifiedAt)
	}
	suspensions, err := NewExecutionSuspensionRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	deps := RecertificationDeps{Drift: releases, Suspender: suspensions}
	if summary, err := store.RecertificationPass(ctx, deps); err != nil || summary.Suspended != 1 {
		t.Fatalf("pass = %s, %v", summary, err)
	}

	// A recertification is open to a suspended agent at any tier.
	low := agentlifecycle.TierLow
	registry, err = store.UpdateRegistry(ctx, 1, 31, governance, agentlifecycle.RegistryUpdate{Tier: &low})
	if err != nil {
		t.Fatal(err)
	}
	if registry.State != agentlifecycle.StateCertified || registry.RecertifyBy != nil {
		t.Fatalf("registry after low = %+v", registry)
	}
	var active int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.execution_suspensions
WHERE scope = 'agent' AND scope_key = '31' AND resumed_at IS NULL`).Scan(&active); err != nil || active != 0 {
		t.Fatalf("active suspensions = %d, %v", active, err)
	}
	if got := countAction(lifecycleAuditActions(t, pool), agentlifecycle.ActionCertificationCert); got != 2 {
		t.Fatalf("certified events = %d, want 2 (state and resume)", got)
	}

	// A low-tier agent is never lapsed by time, even with a stale deadline.
	certifyForTest(t, pool, release.ID, "certified", -24*time.Hour)
	if summary, err := store.RecertificationPass(ctx, deps); err != nil || summary.Lapsed != 0 || summary.Due != 0 {
		t.Fatalf("pass at low = %s, %v", summary, err)
	}

	// A due low-tier agent can still be recertified (one approval).
	if _, err := store.MarkDue(ctx, 1, 31, 11, "External review."); err != nil {
		t.Fatal(err)
	}
	change, err := store.CreateChangeRequest(ctx, agentlifecycle.NewChangeRequest{
		ProjectID: 1, ApplicationID: 31, Kind: agentlifecycle.KindRecertification, Summary: "Review.", ProposedBy: 7,
	})
	if err != nil {
		t.Fatalf("create at low: %v", err)
	}
	if _, err := store.SubmitChangeRequest(ctx, 1, 31, change.ID, 7); err != nil {
		t.Fatalf("submit at low: %v", err)
	}
	outcome, err := store.ApproveChangeRequest(ctx, Approval{ProjectID: 1, ApplicationID: 31, ChangeID: change.ID, ApproverID: 8})
	if err != nil || !outcome.Complete {
		t.Fatalf("approve at low = %+v, %v", outcome, err)
	}
}

// lifecycleTestPublisher is the publish path of a release change request
// in these tests: the checks pass (after onPrecheck), and the publish
// copies the candidate into version 141 and records its release.
type lifecycleTestPublisher struct {
	t          *testing.T
	pool       *pgxpool.Pool
	releases   *AgentReleases
	onPrecheck func()
}

func (p *lifecycleTestPublisher) PrecheckCandidate(context.Context, agentlifecycle.PublishCandidate) error {
	if p.onPrecheck != nil {
		p.onPrecheck()
	}
	return nil
}

func (p *lifecycleTestPublisher) PublishCandidateTx(ctx context.Context, tx pgx.Tx, candidate agentlifecycle.PublishCandidate) (agentlifecycle.PublishedCandidate, error) {
	if _, err := tx.Exec(ctx, `
INSERT INTO p_1.application_versions (
    id, application_id, name, status, author_id, uuid, llm_settings, instructions,
    conversation_starters, welcome_message, agent_type, meta, pipeline_settings, created_at)
SELECT 141, application_id, $2, 'published', author_id, gen_random_uuid(), llm_settings, instructions,
       conversation_starters, welcome_message, agent_type, meta, pipeline_settings, created_at
FROM p_1.application_versions WHERE id = $1`, candidate.VersionID, candidate.VersionName); err != nil {
		return agentlifecycle.PublishedCandidate{}, err
	}
	releaseID, err := p.releases.RecordPublishedTx(ctx, tx, 1, 31, 141, candidate.VersionID, candidate.VersionName, 7)
	if err != nil {
		return agentlifecycle.PublishedCandidate{}, err
	}
	return agentlifecycle.PublishedCandidate{ApplicationID: 31, PublishedVersionID: 141,
		SourceVersionID: candidate.VersionID, ReleaseID: releaseID, VersionName: candidate.VersionName}, nil
}

func (p *lifecycleTestPublisher) AnnouncePublished(context.Context, int64, agentlifecycle.PublishedCandidate) {}

func raiseToMedium(t *testing.T, store *AgentLifecycle) {
	t.Helper()
	medium := agentlifecycle.TierMedium
	if _, err := store.UpdateRegistry(t.Context(), 1, 31, RegistryActor{UserID: 11}, agentlifecycle.RegistryUpdate{Tier: &medium}); err != nil {
		t.Fatalf("raise the tier: %v", err)
	}
}

// editAttachment changes the toolkit selection of draft version 41: an
// attachment edit that writes no version revision.
func editAttachment(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
UPDATE p_1.entity_tool_mapping SET selected_tools = '["list_products","list_features"]'::jsonb
WHERE entity_version_id = 41 AND tool_id = 51`); err != nil {
		t.Fatal(err)
	}
}

// TestAgentLifecycle_AttachmentEditAfterSubmitIsStale proves the final
// approval refuses a candidate whose attachments changed after submit,
// although its version row and revisions did not, and that an unchanged
// candidate publishes.
func TestAgentLifecycle_AttachmentEditAfterSubmitIsStale(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentReleaseFixture(t, pool)
	releases, _ := newReleaseTestStores(t, pool)
	publisher := &lifecycleTestPublisher{t: t, pool: pool, releases: releases}
	store := newLifecycleTestStore(t, pool, agentlifecycle.DefaultPolicy(), WithLifecyclePublisher(publisher))
	ctx := t.Context()
	raiseToMedium(t, store)
	create := func() agentlifecycle.ChangeRequest {
		change, err := store.CreateChangeRequest(ctx, agentlifecycle.NewChangeRequest{
			ProjectID: 1, ApplicationID: 31, Kind: agentlifecycle.KindRelease, CandidateVersionID: 41,
			VersionName: "v1", Summary: "First release.", ProposedBy: 7,
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := store.SubmitChangeRequest(ctx, 1, 31, change.ID, 7); err != nil {
			t.Fatalf("submit: %v", err)
		}
		return change
	}
	change := create()
	var digest []byte
	if err := pool.QueryRow(ctx, `SELECT candidate_digest FROM elitea_lifecycle.change_requests WHERE id = $1`,
		change.ID).Scan(&digest); err != nil || len(digest) != 32 {
		t.Fatalf("candidate digest = %x, %v", digest, err)
	}
	editAttachment(t, pool)
	_, err := store.ApproveChangeRequest(ctx, Approval{ProjectID: 1, ApplicationID: 31, ChangeID: change.ID, ApproverID: 8})
	requireRefusal(t, err, agentlifecycle.StaleCode)

	if _, err := store.WithdrawChangeRequest(ctx, 1, 31, change.ID, 7, false, ""); err != nil {
		t.Fatal(err)
	}
	change = create()
	outcome, err := store.ApproveChangeRequest(ctx, Approval{ProjectID: 1, ApplicationID: 31, ChangeID: change.ID, ApproverID: 8})
	if err != nil || outcome.Published == nil {
		t.Fatalf("approve an unchanged candidate = %+v, %v", outcome, err)
	}
}

// TestAgentLifecycle_EditDuringSubmitIsStale proves the candidate is
// pinned before the publish checks run: an attachment edit while they run
// answers change_request_stale and leaves the request a draft.
func TestAgentLifecycle_EditDuringSubmitIsStale(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentReleaseFixture(t, pool)
	releases, _ := newReleaseTestStores(t, pool)
	publisher := &lifecycleTestPublisher{t: t, pool: pool, releases: releases}
	publisher.onPrecheck = func() { editAttachment(t, pool) }
	store := newLifecycleTestStore(t, pool, agentlifecycle.DefaultPolicy(), WithLifecyclePublisher(publisher))
	ctx := t.Context()
	raiseToMedium(t, store)
	change, err := store.CreateChangeRequest(ctx, agentlifecycle.NewChangeRequest{
		ProjectID: 1, ApplicationID: 31, Kind: agentlifecycle.KindRelease, CandidateVersionID: 41,
		VersionName: "v1", Summary: "First release.", ProposedBy: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.SubmitChangeRequest(ctx, 1, 31, change.ID, 7)
	requireRefusal(t, err, agentlifecycle.StaleCode)
	got, err := store.GetChangeRequest(ctx, 1, 31, change.ID)
	if err != nil || got.State != agentlifecycle.ChangeDraft || got.GateID != 0 {
		t.Fatalf("after a stale submit = %+v, %v", got, err)
	}
}

// TestAgentLifecycle_RecertificationPinsHeadAtSubmit proves a
// recertification certifies the release published at submit, not the one
// published when it was created.
func TestAgentLifecycle_RecertificationPinsHeadAtSubmit(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentReleaseFixture(t, pool)
	releases, _ := newReleaseTestStores(t, pool)
	releaseTestPublish(t, pool, releases, 41, 141, "v1")
	store := newLifecycleTestStore(t, pool, agentlifecycle.DefaultPolicy())
	ctx := t.Context()
	raiseToMedium(t, store)
	change, err := store.CreateChangeRequest(ctx, agentlifecycle.NewChangeRequest{
		ProjectID: 1, ApplicationID: 31, Kind: agentlifecycle.KindRecertification, Summary: "Review.", ProposedBy: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	second := releaseTestPublish(t, pool, releases, 41, 142, "v2")
	submitted, err := store.SubmitChangeRequest(ctx, 1, 31, change.ID, 7)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if submitted.CandidateVersionID != 142 || submitted.CurrentReleaseID != second.ID {
		t.Fatalf("submitted candidate = %d, release %d; want 142, %d",
			submitted.CandidateVersionID, submitted.CurrentReleaseID, second.ID)
	}
	outcome, err := store.ApproveChangeRequest(ctx, Approval{ProjectID: 1, ApplicationID: 31, ChangeID: change.ID, ApproverID: 8})
	if err != nil || outcome.Change.ReleaseID != second.ID {
		t.Fatalf("approve = %+v, %v", outcome, err)
	}
}
