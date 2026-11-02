package repos

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
)

const (
	indexMandateExternal = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1c01"
	indexMandateBinding  = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1c02"
	indexMandateID       = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1c03"
	indexMandateRef      = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1c04"
)

// A governed scheduled index run (#1473) runs no agent version: its
// security context names the agent its mandate binds, and the toolkit
// credential its fire froze under that binding. A freeze made under
// another binding refuses the admission.
func TestPostgresIndexRunRecordsTheMandateAgentAndTheFrozenToolkit(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := NewCredentialGovernanceStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	revision := putIntegrationRevision(t, ctx, store, "it-index-mandate")
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
VALUES ($1::uuid, 7, 3, 'dedicated_agent', 'fake', 't1', 'fake.agent', 'obj-i1', 'active', '100', '200')`, indexMandateExternal)
	exec(`
INSERT INTO elitea_identity.agent_identity_bindings
    (id, tenant_project_id, application_id, application_version_id, definition_digest, mode,
     external_agent_identity_id, policy_revision_id, proposed_by, approved_by, last_transition_by, state)
VALUES ($1::uuid, 7, 3, 4, $2, 'dedicated_agent', $3::uuid, $4, '100', '200', '200', 'active')`,
		indexMandateBinding, bytes.Repeat([]byte{8}, 32), indexMandateExternal, revision.ID)
	exec(`
INSERT INTO elitea_identity.mandates
    (id, tenant_project_id, kind, agent_binding_id, subject_user_id, operation_scopes, not_before, expires_at,
     state, revocation_epoch, policy_revision_id, issued_by, approved_by, last_transition_by)
VALUES ($1::uuid, 7, 'autonomous', $2::uuid, NULL, ARRAY['jira.issue.search'], $3, $4, 'active', 3, $5, '100', '200', '200')`,
		indexMandateID, indexMandateBinding, now.Add(-time.Hour), now.Add(time.Hour), revision.ID)

	authority := securitycontext.RunAuthority{MandateID: indexMandateID, MandateEpoch: 3, AgentBindingID: indexMandateBinding}
	admit := func(executionID string, freeze *agentexecutionapp.GovernedCredentialFreeze) (securitycontext.Reference, error) {
		t.Helper()
		exec(`
INSERT INTO elitea_runtime.execution_admission_profiles (execution_id, profile, policy_revision_id, policy_digest)
VALUES ($1, 'elitea.credential-profile.governed.v1', $2, $3)`, executionID, revision.ID, revision.Digest[:])
		runCtx := agentexecutionapp.WithUnattendedCredentialFreeze(securitycontext.WithRunAuthority(ctx, authority), freeze)
		var ref securitycontext.Reference
		err := pgx.BeginFunc(runCtx, pool, func(tx pgx.Tx) error {
			// The index admission's facts: no application version.
			facts := securityContextFacts{tenantProjectID: 7, actorID: "41", admittedAt: now}
			facts.governed = agentexecutionapp.UnattendedCredentialFreezeFrom(runCtx)
			var err error
			ref, err = recordSecurityContext(runCtx, tx, pgxRows(tx), executionID, facts)
			return err
		})
		return ref, err
	}

	digest := [32]byte{9}
	ref, err := admit("index-governed", &agentexecutionapp.GovernedCredentialFreeze{
		AgentBindingID: indexMandateBinding,
		References: []agentexecutionapp.GovernedCredentialReference{{
			ToolkitID: 21, CredentialReferenceID: indexMandateRef, TargetBindingID: indexMandateExternal, Generation: 2, ToolkitDigest: digest,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var stored []byte
	if err := pool.QueryRow(ctx, `SELECT context_bytes FROM elitea_runtime.execution_security_contexts WHERE execution_id = 'index-governed'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	decoded, err := securitycontext.Verify(stored, ref)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.GetMandateId() != indexMandateID || decoded.GetAgentBindingId() != indexMandateBinding ||
		decoded.GetSubjectPrincipalRef() != "" || decoded.GetApplicationId() != 3 || decoded.GetApplicationVersionId() != 4 {
		t.Fatalf("index context: mandate=%q binding=%q subject=%q application=%d version=%d",
			decoded.GetMandateId(), decoded.GetAgentBindingId(), decoded.GetSubjectPrincipalRef(),
			decoded.GetApplicationId(), decoded.GetApplicationVersionId())
	}
	refs := decoded.GetCredentialReferences()
	if len(refs) != 1 || refs[0].GetToolkitId() != 21 || refs[0].GetCredentialReferenceId() != indexMandateRef ||
		!bytes.Equal(refs[0].GetToolkitDigest().GetValue(), digest[:]) {
		t.Fatalf("index credential references: %v", refs)
	}

	// A freeze made under another binding than the mandate's is refused.
	if _, err := admit("index-other-binding", &agentexecutionapp.GovernedCredentialFreeze{AgentBindingID: "another"}); !errors.Is(err, ErrGovernedBindingChanged) {
		t.Fatalf("a freeze under another binding: %v", err)
	}
}
