package repos

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runmandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runmandate/runmandatetest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/epoch"
	mandatedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

const (
	unattendedExternal   = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1b01"
	unattendedBinding    = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1b02"
	unattendedBackground = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1b03"
	unattendedAutonomous = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1b04"
)

// A mandate-bound run (#1122) records the mandate its fire checked, not the
// one admission would infer: the context names it, its subject, its binding
// and the webhook caller. A mandate that moved since the check refuses the
// admission, and a revoke afterwards makes the run's epoch stale, which is
// what cancels it at the next lease renewal (#1121).
func TestPostgresAdmissionRecordsTheFireTimeMandate(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := NewCredentialGovernanceStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	revision := putIntegrationRevision(t, ctx, store, "it-unattended")
	now := time.Now().UTC()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`
INSERT INTO elitea_identity.external_agent_identities
    (id, tenant_project_id, application_id, mode, provider, provider_tenant, kind, external_id, state, created_by, last_transition_by)
VALUES ($1::uuid, 7, 3, 'dedicated_agent', 'fake', 't1', 'fake.agent', 'obj-u1', 'active', '100', '200')`, unattendedExternal)
	exec(`
INSERT INTO elitea_identity.agent_identity_bindings
    (id, tenant_project_id, application_id, application_version_id, definition_digest, mode,
     external_agent_identity_id, policy_revision_id, proposed_by, approved_by, last_transition_by, state)
VALUES ($1::uuid, 7, 3, 4, $2, 'dedicated_agent', $3::uuid, $4, '100', '200', '200', 'active')`,
		unattendedBinding, bytes.Repeat([]byte{8}, 32), unattendedExternal, revision.ID)
	for _, m := range []struct {
		id, kind string
		subject  any
	}{{unattendedBackground, "delegated_background", int64(5)}, {unattendedAutonomous, "autonomous", nil}} {
		exec(`
INSERT INTO elitea_identity.mandates
    (id, tenant_project_id, kind, agent_binding_id, subject_user_id, operation_scopes, not_before, expires_at,
     state, revocation_epoch, policy_revision_id, issued_by, approved_by, last_transition_by)
VALUES ($1::uuid, 7, $2, $3::uuid, $4, ARRAY['jira.issue.read'], $5, $6, 'active', 3, $7, '100', '200', '200')`,
			m.id, m.kind, unattendedBinding, m.subject, now.Add(-time.Hour), now.Add(time.Hour), revision.ID)
	}

	admit := func(executionID, actor string, authority securitycontext.RunAuthority) (*securitycontext.Reference, error) {
		t.Helper()
		exec(`
INSERT INTO elitea_runtime.execution_admission_profiles (execution_id, profile, policy_revision_id, policy_digest)
VALUES ($1, 'elitea.credential-profile.governed.v1', $2, $3)`, executionID, revision.ID, revision.Digest[:])
		runCtx := securitycontext.WithRunAuthority(ctx, authority)
		var ref securitycontext.Reference
		err := pgx.BeginFunc(runCtx, pool, func(tx pgx.Tx) error {
			var err error
			ref, err = recordSecurityContext(runCtx, tx, pgxRows(tx), executionID, securityContextFacts{
				tenantProjectID: 7, actorID: actor, applicationVersionID: 4, admittedAt: now,
			})
			return err
		})
		if err != nil {
			return nil, err
		}
		return &ref, nil
	}
	read := func(executionID string, ref *securitycontext.Reference) securitycontext.Context {
		t.Helper()
		var stored []byte
		if err := pool.QueryRow(ctx, `SELECT context_bytes FROM elitea_runtime.execution_security_contexts WHERE execution_id = $1`, executionID).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		decoded, err := securitycontext.Verify(stored, *ref)
		if err != nil {
			t.Fatal(err)
		}
		return securitycontext.Context{
			SubjectPrincipalRef: decoded.GetSubjectPrincipalRef(),
			AgentBindingID:      decoded.GetAgentBindingId(),
			MandateID:           decoded.GetMandateId(), MandateEpoch: decoded.GetMandateEpoch(),
			Caller: securitycontext.Caller{
				AuthenticationSource: decoded.GetCaller().GetAuthenticationSource(),
				CallingApplicationID: decoded.GetCaller().GetCallingApplicationId(),
			},
		}
	}

	// A delegated_background schedule: the subject is the mandate's.
	ref, err := admit("unattended-background", "5", securitycontext.RunAuthority{
		MandateID: unattendedBackground, MandateEpoch: 3, SubjectUserID: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := read("unattended-background", ref); got.MandateID != unattendedBackground || got.MandateEpoch != 3 ||
		got.SubjectPrincipalRef != "5" || got.AgentBindingID != unattendedBinding {
		t.Fatalf("background context: %+v", got)
	}

	// An autonomous trigger: no subject, the webhook caller recorded, even
	// though the attributed actor is the trigger's creator. Inference would
	// have bound no mandate to a user run.
	caller := securitycontext.Caller{AuthenticationSource: "pipeline_trigger.bearer", CallingApplicationID: "pipeline_trigger:9"}
	ref, err = admit("unattended-autonomous", "41", securitycontext.RunAuthority{
		MandateID: unattendedAutonomous, MandateEpoch: 3, Caller: caller,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := read("unattended-autonomous", ref)
	if got.MandateID != unattendedAutonomous || got.SubjectPrincipalRef != "" || got.Caller != caller {
		t.Fatalf("autonomous context: %+v", got)
	}

	// After a rollback (ADR-0040 decision 2) the run executes the release's
	// version row (9) while the mandate binds the trigger's version (4): a
	// typed refusal, not "not usable", because the mandate is fine. A run
	// whose release version IS the mandate's binding version is admitted.
	admitRelease := func(executionID string, versionID, bindingVersionID int64) error {
		t.Helper()
		exec(`
INSERT INTO elitea_runtime.execution_admission_profiles (execution_id, profile, policy_revision_id, policy_digest)
VALUES ($1, 'elitea.credential-profile.governed.v1', $2, $3)`, executionID, revision.ID, revision.Digest[:])
		runCtx := securitycontext.WithRunAuthority(ctx, securitycontext.RunAuthority{MandateID: unattendedAutonomous, MandateEpoch: 3})
		return pgx.BeginFunc(runCtx, pool, func(tx pgx.Tx) error {
			_, err := recordSecurityContext(runCtx, tx, pgxRows(tx), executionID, securityContextFacts{
				tenantProjectID: 7, applicationVersionID: versionID, bindingVersionID: bindingVersionID, admittedAt: now,
			})
			return err
		})
	}
	if err := admitRelease("unattended-rolled-back", 4, 9); !errors.Is(err, ErrRunAuthorityReleaseRolledBack) {
		t.Fatalf("a rolled-back release under the trigger version's mandate: %v", err)
	}
	if errors.Is(ErrRunAuthorityReleaseRolledBack, ErrRunAuthorityNotUsable) {
		t.Fatal("the rollback refusal must be its own error")
	}
	if err := admitRelease("unattended-release-version", 9, 4); err != nil {
		t.Fatalf("a run whose release version is the mandate's binding version: %v", err)
	}

	// A mandate that moved since the fire-time check refuses the admission.
	if _, err := admit("unattended-stale", "", securitycontext.RunAuthority{
		MandateID: unattendedAutonomous, MandateEpoch: 2,
	}); !errors.Is(err, ErrRunAuthorityNotUsable) {
		t.Fatalf("stale fire-time epoch: %v", err)
	}
	// A subject the mandate does not name refuses it too.
	if _, err := admit("unattended-subject", "6", securitycontext.RunAuthority{
		MandateID: unattendedBackground, MandateEpoch: 3, SubjectUserID: 6,
	}); !errors.Is(err, ErrRunAuthorityNotUsable) {
		t.Fatalf("another subject: %v", err)
	}

	// Revoked after admission: the run's pinned epoch is stale, so the
	// lease renewal check cancels it.
	epochs, err := epoch.NewPostgresStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	loader, err := epoch.NewPostgresContextLoader(pool)
	if err != nil {
		t.Fatal(err)
	}
	checker, err := epoch.NewRunChecker(loader, epochs)
	if err != nil {
		t.Fatal(err)
	}
	if governed, stale, err := checker.Check(ctx, "unattended-autonomous"); err != nil || !governed || len(stale) != 0 {
		t.Fatalf("before revoke: governed=%v stale=%v err=%v", governed, stale, err)
	}
	exec(`UPDATE elitea_identity.mandates SET state = 'revoked', revocation_epoch = revocation_epoch + 1, last_transition_by = '200' WHERE id = $1::uuid`, unattendedAutonomous)
	governed, stale, err := checker.Check(ctx, "unattended-autonomous")
	if err != nil || !governed || len(stale) != 1 || stale[0].Kind != epoch.KindMandate || stale[0].ID != unattendedAutonomous {
		t.Fatalf("after revoke: governed=%v stale=%v err=%v", governed, stale, err)
	}
	// And the next fire is refused at admission.
	if _, err := admit("unattended-after-revoke", "", securitycontext.RunAuthority{
		MandateID: unattendedAutonomous, MandateEpoch: 4,
	}); !errors.Is(err, ErrRunAuthorityNotUsable) {
		t.Fatalf("revoked mandate admitted: %v", err)
	}
}

// An index schedule's attach target carries every cron of the index's stored
// schedules, so an attach whose mandate does not cover one of them answers
// MANDATE_SCOPE_MISMATCH up front instead of being refused at every
// occurrence (#1122).
func TestPostgresIndexScheduleTargetReadsItsCrons(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	appender, err := securityaudit.NewPostgresAppender("elitea-main/integration-test")
	if err != nil {
		t.Fatal(err)
	}
	// elitea_tools is created with the tenant schema itself (001_initial.sql),
	// not by the ledgered corpus this template is built from.
	if _, err := pool.Exec(ctx, `
CREATE TABLE p_1.elitea_tools (
    id SERIAL PRIMARY KEY,
    name VARCHAR(128) NOT NULL,
    type VARCHAR(64) NOT NULL,
    description TEXT,
    owner_id INTEGER NOT NULL,
    author_id INTEGER NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT now(),
    meta JSONB DEFAULT '{}'::jsonb,
    settings JSONB DEFAULT '{}'::jsonb
);
INSERT INTO p_1.elitea_tools (id, name, type, owner_id, author_id, meta, created_at) VALUES
    (941, 'repo', 'github', 1, 1,
     '{"indexes_meta":{"docs":{"schedules":{"-1":{"cron":"0 3 * * *","enabled":true},"17":{"cron":"30 2 * * *","enabled":true}}},"empty":{}}}',
     now() AT TIME ZONE 'UTC')`); err != nil {
		t.Fatal(err)
	}
	mandates := NewUnattendedRunMandates(pool, appender)
	attachment := MandateAttachment{Origin: runmandate.OriginIndexSchedule, ProjectID: 1, ToolkitID: 941, IndexName: "docs", ActorID: 1}
	target, err := mandates.Target(ctx, attachment)
	if err != nil || target.ResourceID != "941:docs" || len(target.Crons) != 2 ||
		target.Crons[0] != "0 3 * * *" || target.Crons[1] != "30 2 * * *" {
		t.Fatalf("target %+v err=%v", target, err)
	}
	empty := attachment
	empty.IndexName = "empty"
	emptyTarget, err := mandates.Target(ctx, empty)
	if err != nil || len(emptyTarget.Crons) != 0 {
		t.Fatalf("index with no schedules: %+v err=%v", emptyTarget, err)
	}

	store := runmandatetest.New()
	store.PutBinding("binding-1", 1, 0)
	store.PutMandate("one-cron", 1, mandatedomain.KindAutonomous, "binding-1", 0, runmandate.CronScope("0 3 * * *"))
	store.PutMandate("both-crons", 1, mandatedomain.KindAutonomous, "binding-1", 0,
		runmandate.CronScope("0 3 * * *"), runmandate.CronScope("30 2 * * *"))
	service := store.Service()
	if _, err := service.CheckAttach(ctx, "one-cron", target); !errors.Is(err, runmandate.ErrScopeMismatch) {
		t.Fatalf("a mandate missing one cron attached: %v", err)
	}
	if _, err := service.CheckAttach(ctx, "both-crons", target); err != nil {
		t.Fatalf("a mandate covering every cron: %v", err)
	}
	if _, err := service.CheckAttach(ctx, "one-cron", emptyTarget); !errors.Is(err, runmandate.ErrScopeMismatch) {
		t.Fatalf("a cron-scoped mandate attached to an index with no schedule: %v", err)
	}
}

// Attach, detach and a refused run each append a security audit decision
// (#1122), and tenant 0158 is in the shipped chain.
func TestPostgresUnattendedRunMandatesAudit(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	appender, err := securityaudit.NewPostgresAppender("elitea-main/integration-test")
	if err != nil {
		t.Fatal(err)
	}
	// The template's application_versions holds only what the tenant history
	// alters; the schedule's foreign key needs nothing more than the id.
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.application_versions (id) VALUES (932);
INSERT INTO p_1.pipeline_schedules (application_id, version_id, cron, active, user_input, author_id)
VALUES (931, 932, '0 * * * *', true, 'scheduled', 41);`); err != nil {
		t.Fatal(err)
	}
	var indexTable bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('p_1.index_schedule_mandates') IS NOT NULL`).Scan(&indexTable); err != nil || !indexTable {
		t.Fatalf("tenant 0158 not applied: %v %v", indexTable, err)
	}

	mandates := NewUnattendedRunMandates(pool, appender)
	attachment := MandateAttachment{Origin: runmandate.OriginPipelineSchedule, ProjectID: 1, VersionID: 932, MandateID: unattendedAutonomous, ActorID: 41}
	target, err := mandates.Target(ctx, attachment)
	if err != nil || target.Cron != "0 * * * *" || target.Input == nil || *target.Input != "scheduled" {
		t.Fatalf("target %+v err=%v", target, err)
	}
	if err := mandates.Attach(ctx, attachment, target); err != nil {
		t.Fatal(err)
	}
	var attached *string
	if err := pool.QueryRow(ctx, `SELECT mandate_id FROM p_1.pipeline_schedules WHERE version_id = 932`).Scan(&attached); err != nil ||
		attached == nil || *attached != unattendedAutonomous {
		t.Fatalf("mandate_id = %v err=%v", attached, err)
	}
	if err := mandates.Detach(ctx, attachment, target); err != nil {
		t.Fatal(err)
	}
	if err := mandates.RecordRunRefusal(ctx, runmandate.Refusal{
		Target: target, Code: runmandate.CodeMandateRequired, At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	missing := attachment
	missing.VersionID = 999
	if err := mandates.Attach(ctx, missing, target); !errors.Is(err, ErrAttachmentTargetNotFound) {
		t.Fatalf("attach to no schedule: %v", err)
	}

	rows, err := pool.Query(ctx, `
SELECT event->'data'->>'action', event->'data'->>'effect',
       ARRAY(SELECT jsonb_array_elements_text(event->'data'->'reasons'))
FROM elitea_runtime.security_audit_outbox
WHERE event_type = $1 AND event->'data'->>'resource_id' = $2
ORDER BY seq`, string(securityaudit.TypeDecision), target.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		Action, Effect string
		Reasons        []string
	}
	events, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var out row
		err := r.Scan(&out.Action, &out.Effect, &out.Reasons)
		return out, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 ||
		events[0].Action != "unattended_run.mandate.attach" ||
		events[1].Action != "unattended_run.mandate.detach" ||
		events[2].Action != "unattended_run.admit" || events[2].Effect != "deny" ||
		len(events[2].Reasons) != 2 || events[2].Reasons[0] != "mandate_required" {
		t.Fatalf("audit events = %+v", events)
	}
}
