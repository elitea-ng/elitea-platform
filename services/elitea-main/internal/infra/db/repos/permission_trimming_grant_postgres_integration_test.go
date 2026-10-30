package repos

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

const (
	trimGrantExternal  = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1501"
	trimGrantBinding   = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1502"
	trimGrantTarget    = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1503"
	trimGrantReference = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1504"
	trimGrantID        = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1505"
)

// A credential use grant is dated by its last change: one changed yesterday
// is too new to judge (not a candidate, its removal refused); backdated past
// the window and unused it is a candidate, and Remove revokes it.
func TestPermissionTrimming_GrantIsDatedByItsLastChange(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCredentialReviewFixture(t, pool)
	ctx := t.Context()
	governance, err := NewCredentialGovernanceStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	revision := putIntegrationRevision(t, ctx, governance, "it-trim-grant")
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`
INSERT INTO elitea_identity.external_agent_identities
    (id, tenant_project_id, application_id, mode, provider, provider_tenant, kind, external_id, state, created_by, last_transition_by)
VALUES ($1::uuid, 1, 31, 'dedicated_agent', 'fake', 't1', 'fake.agent', 'obj-trim', 'active', '11', '11')`, trimGrantExternal)
	exec(`
INSERT INTO elitea_identity.agent_identity_bindings
    (id, tenant_project_id, application_id, application_version_id, definition_digest, mode,
     external_agent_identity_id, policy_revision_id, proposed_by, approved_by, last_transition_by, state)
VALUES ($1::uuid, 1, 31, 41, $2, 'dedicated_agent', $3::uuid, $4, '11', '12', '12', 'active')`,
		trimGrantBinding, bytes.Repeat([]byte{9}, 32), trimGrantExternal, revision.ID)
	exec(`
INSERT INTO elitea_identity.target_bindings (id, tenant_project_id, provider_type, target_identity, audience)
VALUES ($1::uuid, 1, 'jira', 'https://jira.corp.example', '')`, trimGrantTarget)
	exec(`
INSERT INTO elitea_identity.credential_references
    (id, tenant_project_id, class, backend, locator, provider_type, target_binding_id, generation, state)
VALUES ($1::uuid, 1, 'static_secret', 'pgvault', 'p1/jira', 'jira', $2::uuid, 1, 'active')`,
		trimGrantReference, trimGrantTarget)
	exec(`INSERT INTO elitea_identity.agent_toolkit_bindings (agent_binding_id, toolkit_id, tenant_project_id, toolkit_digest)
VALUES ($1::uuid, 51, 1, $2)`, trimGrantBinding, bytes.Repeat([]byte{5}, 32))
	exec(`
INSERT INTO elitea_identity.credential_use_grants
    (id, tenant_project_id, agent_binding_id, toolkit_id, credential_reference_id, target_binding_id,
     capabilities, operation_scopes, state, epoch, proposed_by, approved_by, last_transition_by, updated_at)
VALUES ($1::uuid, 1, $2::uuid, 51, $3::uuid, $4::uuid, ARRAY['use'], ARRAY['jira.read'], 'active', 1, '11', '12', '12',
        now() - interval '1 day')`,
		trimGrantID, trimGrantBinding, trimGrantReference, trimGrantTarget)
	key := trimGrantBinding + "/51/" + trimGrantReference

	trimming, err := NewPermissionTrimming(newCredentialReviewTestStore(t, pool, time.Now().UTC(),
		platformconfig.DefaultCredentialReviewPolicy()))
	if err != nil {
		t.Fatal(err)
	}
	report, err := trimming.Report(ctx, TrimmingFilter{ProjectID: 1, ApplicationID: 31})
	if err != nil {
		t.Fatal(err)
	}
	grant := findTrim(report.Items, TrimCredentialGrant, key)
	if grant == nil || grant.Candidate || !grant.TooNew || grant.GrantedAt == nil {
		t.Fatalf("grant changed yesterday = %+v, want too new", grant)
	}
	var refusal *TrimRefusal
	if _, err := trimming.Remove(ctx, RemoveRequest{ProjectID: 1, ApplicationID: 31, Kind: TrimCredentialGrant,
		Key: key, ActorID: 11}); !errors.As(err, &refusal) || refusal.Code != TrimRefusalTooNew {
		t.Fatalf("removing a too-new grant = %v, want %s", err, TrimRefusalTooNew)
	}

	exec(`UPDATE elitea_identity.credential_use_grants SET updated_at = now() - interval '40 days' WHERE id = $1::uuid`, trimGrantID)
	report, err = trimming.Report(ctx, TrimmingFilter{ProjectID: 1, ApplicationID: 31})
	if err != nil {
		t.Fatal(err)
	}
	grant = findTrim(report.Items, TrimCredentialGrant, key)
	if grant == nil || !grant.Candidate || !grant.Removable || grant.TooNew {
		t.Fatalf("grant unchanged for 40 days = %+v, want a removable candidate", grant)
	}
	if _, err := trimming.Remove(ctx, RemoveRequest{ProjectID: 1, ApplicationID: 31, Kind: TrimCredentialGrant,
		Key: key, ActorID: 11}); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM elitea_identity.credential_use_grants WHERE id = $1::uuid`, trimGrantID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "revoked" {
		t.Fatalf("grant state = %s, want revoked", state)
	}
}
