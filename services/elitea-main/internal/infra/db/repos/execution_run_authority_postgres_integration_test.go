package repos

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
)

const (
	authorityExternal   = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1a01"
	authorityBinding    = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1a02"
	authorityDelegation = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1a03"
	authorityBackground = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1a04"
	authorityAutonomous = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1a05"
	authoritySecondAuto = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e1a06"
)

// Admission names the run's one authority source (ADR-0039 decision 3):
// the user's delegation, else their delegated_background mandate; an
// autonomous mandate only for a run with no user; ambiguity records none.
func TestPostgresAdmissionBindsTheRunAuthority(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := NewCredentialGovernanceStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	revision := putIntegrationRevision(t, ctx, store, "it-authority")
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
VALUES ($1::uuid, 7, 3, 'dedicated_agent', 'fake', 't1', 'fake.agent', 'obj-1', 'active', '100', '200')`, authorityExternal)
	exec(`
INSERT INTO elitea_identity.agent_identity_bindings
    (id, tenant_project_id, application_id, application_version_id, definition_digest, mode,
     external_agent_identity_id, policy_revision_id, proposed_by, approved_by, last_transition_by, state)
VALUES ($1::uuid, 7, 3, 4, $2, 'dedicated_agent', $3::uuid, $4, '100', '200', '200', 'active')`,
		authorityBinding, bytes.Repeat([]byte{9}, 32), authorityExternal, revision.ID)

	run := func(t *testing.T, executionID, actor string) securitycontext.Context {
		t.Helper()
		exec(`
INSERT INTO elitea_runtime.execution_admission_profiles (execution_id, profile, policy_revision_id, policy_digest)
VALUES ($1, 'elitea.credential-profile.governed.v1', $2, $3)`, executionID, revision.ID, revision.Digest[:])
		var ref securitycontext.Reference
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			var err error
			ref, err = recordSecurityContext(ctx, tx, pgxRows(tx), executionID, securityContextFacts{
				tenantProjectID: 7, actorID: actor, applicationVersionID: 4, admittedAt: now,
			})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		var stored []byte
		if err := pool.QueryRow(ctx, `SELECT context_bytes FROM elitea_runtime.execution_security_contexts WHERE execution_id = $1`, executionID).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		decoded, err := securitycontext.Verify(stored, ref)
		if err != nil || decoded.GetAgentBindingId() != authorityBinding {
			t.Fatalf("context %+v err=%v", decoded, err)
		}
		return securitycontext.Context{
			MandateID: decoded.GetMandateId(), MandateEpoch: decoded.GetMandateEpoch(),
			DelegationID: decoded.GetDelegationId(), DelegationEpoch: decoded.GetDelegationEpoch(),
		}
	}
	mandate := func(id, kind string, subject any) {
		t.Helper()
		exec(`
INSERT INTO elitea_identity.mandates
    (id, tenant_project_id, kind, agent_binding_id, subject_user_id, operation_scopes, not_before, expires_at,
     state, revocation_epoch, policy_revision_id, issued_by, approved_by, last_transition_by)
VALUES ($1::uuid, 7, $2, $3::uuid, $4, ARRAY['jira.issue.read'], $5, $6, 'active', 3, $7, '100', '200', '200')`,
			id, kind, authorityBinding, subject, now.Add(-time.Hour), now.Add(time.Hour), revision.ID)
	}

	// No authority at all records none.
	if got := run(t, "authority-none", "5"); got.MandateID != "" || got.DelegationID != "" {
		t.Fatalf("no authority: %+v", got)
	}

	mandate(authorityBackground, "delegated_background", int64(5))
	mandate(authorityAutonomous, "autonomous", nil)
	if got := run(t, "authority-background", "5"); got.MandateID != authorityBackground || got.MandateEpoch != 3 || got.DelegationID != "" {
		t.Fatalf("user run without a delegation: %+v", got)
	}
	if got := run(t, "authority-autonomous", ""); got.MandateID != authorityAutonomous {
		t.Fatalf("run with no user: %+v", got)
	}
	// An autonomous mandate never serves another user's run.
	if got := run(t, "authority-other-user", "6"); got.MandateID != "" || got.DelegationID != "" {
		t.Fatalf("another user's run: %+v", got)
	}

	// The user's delegation, linked through their sign-in assertion, wins.
	digest := bytes.Repeat([]byte{4}, 32)
	exec(`
INSERT INTO elitea_identity.signin_assertions
    (id, browser_session_digest, user_id, issuer, tenant_id, subject_object_id, audience, assertion_ciphertext,
     assertion_expires_at, key_label)
VALUES (gen_random_uuid(), $1, 5, 'https://login.example', 'tid-1', 'oid-1', 'api://mid', '\x00', $2, 'k')`, digest, now.Add(time.Hour))
	exec(`
INSERT INTO elitea_identity.delegations
    (id, tenant_project_id, issuer, tenant_id, subject_object_id, agent_binding_id, consent_recorded_at, expires_at,
     state, revocation_epoch, browser_session_digest)
VALUES ($1::uuid, 7, 'https://login.example', 'tid-1', 'oid-1', $2::uuid, $3, $4, 'active', 2, $5)`,
		authorityDelegation, authorityBinding, now, now.Add(time.Hour), digest)
	if got := run(t, "authority-delegation", "5"); got.DelegationID != authorityDelegation || got.DelegationEpoch != 2 || got.MandateID != "" {
		t.Fatalf("user run with a delegation: %+v", got)
	}

	// Two autonomous mandates are ambiguous: none is recorded.
	mandate(authoritySecondAuto, "autonomous", nil)
	if got := run(t, "authority-ambiguous", ""); got.MandateID != "" {
		t.Fatalf("ambiguous mandates: %+v", got)
	}
}
