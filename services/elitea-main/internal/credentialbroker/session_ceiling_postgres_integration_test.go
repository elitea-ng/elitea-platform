package credentialbroker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// The session ceiling is the targets of the binding's active, unexpired
// grants: audience when set, else the canonical identity; never a
// suspended, revoked or expired grant's, nor another binding's.
func TestPostgresSessionCeiling(t *testing.T) {
	pool := newBrokerPool(t)
	ctx := context.Background()
	audit, err := securityaudit.NewPostgresAppender("elitea-main/integration-test")
	require.NoError(t, err)
	store, err := NewPostgresStore(pool, audit)
	require.NoError(t, err)
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, sql, args...)
		require.NoError(t, err)
	}
	const (
		binding = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e2b01"
		other   = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e2b02"
	)
	exec(`
INSERT INTO elitea_identity.credential_policy_revisions (id, digest, canonical, document, created_by)
VALUES ('it-ceiling', decode(repeat('ab', 32), 'hex'), '{}'::bytea, '{}'::jsonb, 'it')`)
	for i, id := range []string{binding, other} {
		exec(`
INSERT INTO elitea_identity.agent_identity_bindings
    (id, tenant_project_id, application_id, application_version_id, definition_digest, mode,
     policy_revision_id, proposed_by, approved_by, last_transition_by, state)
VALUES ($1::uuid, 7, 3, $2, decode(repeat('01', 32), 'hex'), 'dedicated_agent', 'it-ceiling', '100', '200', '200', 'active')`, id, i+1)
	}
	grant := func(n int, bindingID, identity, audience, state, expires string) {
		t.Helper()
		target := "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e3c0" + string(rune('0'+n))
		reference := "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e4d0" + string(rune('0'+n))
		exec(`INSERT INTO elitea_identity.target_bindings (id, tenant_project_id, provider_type, target_identity, audience)
VALUES ($1::uuid, 7, 'jira', $2, $3)`, target, identity, audience)
		exec(`INSERT INTO elitea_identity.credential_references
    (id, tenant_project_id, class, backend, locator, provider_type, target_binding_id)
VALUES ($1::uuid, 7, 'static_secret', 'pgvault', 'p7/jira', 'jira', $2::uuid)`, reference, target)
		exec(`INSERT INTO elitea_identity.credential_use_grants
    (id, tenant_project_id, agent_binding_id, toolkit_id, credential_reference_id, target_binding_id,
     capabilities, operation_scopes, state, expires_at, proposed_by, approved_by, last_transition_by)
VALUES (gen_random_uuid(), 7, $1::uuid, 11, $2::uuid, $3::uuid, ARRAY['use'], ARRAY['jira.issue.read'],
        $4, NULLIF($5, '')::timestamptz, '100', '200', '200')`, bindingID, reference, target, state, expires)
	}
	grant(1, binding, "https://a.example", "api://jira-a", "active", "")
	grant(2, binding, "https://b.example", "", "active", "2999-01-01T00:00:00Z")
	grant(3, binding, "https://expired.example", "", "active", "2000-01-01T00:00:00Z")
	grant(4, binding, "https://suspended.example", "", "suspended", "")
	grant(5, other, "https://other.example", "", "active", "")

	ceiling, err := store.SessionCeiling(ctx, 7, binding)
	require.NoError(t, err)
	require.Equal(t, []string{"api://jira-a", "https://b.example"}, ceiling)
	empty, err := store.SessionCeiling(ctx, 8, binding)
	require.NoError(t, err)
	require.Empty(t, empty)
}
