package repos

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// Shared 0184 (issue #1116): the approver CHECK, the one-live-grant index,
// the grant-target = credential-target foreign key, the transition with its
// audit event, the exact-key read and the readers the decision gates use.
// CI-only: skipped without the integration database.

const (
	grantTestBinding    = "0192f5e2-7c3a-7b1e-8a9d-000000001161"
	grantTestTargetA    = "0192f5e2-7c3a-7b1e-8a9d-000000001162"
	grantTestTargetB    = "0192f5e2-7c3a-7b1e-8a9d-000000001163"
	grantTestCredential = "0192f5e2-7c3a-7b1e-8a9d-000000001164"
)

func seedGrantWorld(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedAgentIdentityPolicyRevision(t, pool)
	ctx := context.Background()
	bindings := NewAgentIdentityBindings(pool, agentIdentityAudit(t))
	_, err := bindings.CreateBinding(ctx, proposedTestBinding(t, grantTestBinding, 1161, 0x61))
	require.NoError(t, err)
	for _, target := range []string{grantTestTargetA, grantTestTargetB} {
		_, err := pool.Exec(ctx, `
INSERT INTO elitea_identity.target_bindings (id, tenant_project_id, provider_type, target_identity)
VALUES ($1::uuid, 7, 'jira', $2)`, target, "https://"+target[len(target)-4:]+".example.test")
		require.NoError(t, err)
	}
	_, err = pool.Exec(ctx, `
INSERT INTO elitea_identity.credential_references
    (id, tenant_project_id, class, backend, locator, provider_type, target_binding_id, generation, state)
VALUES ($1::uuid, 7, 'static_secret', 'pgvault', 'p7/jira', 'jira', $2::uuid, 1, 'active')`,
		grantTestCredential, grantTestTargetA)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
INSERT INTO elitea_identity.agent_toolkit_bindings (agent_binding_id, toolkit_id, tenant_project_id, toolkit_digest)
VALUES ($1::uuid, 11, 7, $2)`, grantTestBinding, make([]byte, 32))
	require.NoError(t, err)
}

func proposedTestGrant(t *testing.T, id, target string) credentialgrant.CredentialUseGrant {
	t.Helper()
	g, err := credentialgrant.NewGrant(id, credentialgrant.Proposal{
		TenantProjectID: 7, AgentBindingID: grantTestBinding, ToolkitID: 11,
		CredentialReferenceID: grantTestCredential, TargetBindingID: target,
		Capabilities:    []credentialgrant.Capability{credentialgrant.CapUse},
		OperationScopes: []string{"jira.issue.read"},
	}, "100", time.Now().UTC().Truncate(time.Microsecond))
	require.NoError(t, err)
	return g
}

func grantTransitionBy(action credentialgrant.Action, actor string) func(credentialgrant.CredentialUseGrant) (credentialgrant.CredentialUseGrant, error) {
	return func(current credentialgrant.CredentialUseGrant) (credentialgrant.CredentialUseGrant, error) {
		return current.Transition(action, actor, time.Now().UTC())
	}
}

func TestCredentialGrantApproverCheckIsEnforcedByTheDatabase(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedGrantWorld(t, pool)
	store := NewCredentialGrants(pool, agentIdentityAudit(t))
	ctx := context.Background()

	created, err := store.CreateGrant(ctx, proposedTestGrant(t, "0192f5e2-7c3a-7b1e-8a9d-000000001171", grantTestTargetA))
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `
UPDATE elitea_identity.credential_use_grants SET approved_by = proposed_by, state = 'active' WHERE id = $1::uuid`, created.ID)
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "want a constraint violation, got %v", err)
	require.Equal(t, "23514", pgErr.Code)
	require.Equal(t, credentialGrantApproverCheck, pgErr.ConstraintName)

	// A transition whose apply skipped the domain check maps to the typed error.
	_, err = store.TransitionGrant(ctx, 7, created.ID, func(current credentialgrant.CredentialUseGrant) (credentialgrant.CredentialUseGrant, error) {
		next := current
		next.State, next.ApprovedBy, next.Epoch = credentialgrant.GrantActive, current.ProposedBy, current.Epoch+1
		return next, nil
	})
	require.ErrorIs(t, err, credentialgrant.ErrSelfApproval)

	// Active without an approver is refused too.
	_, err = pool.Exec(ctx, `UPDATE elitea_identity.credential_use_grants SET state = 'active' WHERE id = $1::uuid`, created.ID)
	require.Error(t, err)
}

func TestCredentialGrantKeyTargetAndLifecycle(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedGrantWorld(t, pool)
	store := NewCredentialGrants(pool, agentIdentityAudit(t))
	ctx := context.Background()

	// The credential is bound to target A: a grant naming B is refused.
	_, err := store.CreateGrant(ctx, proposedTestGrant(t, "0192f5e2-7c3a-7b1e-8a9d-000000001181", grantTestTargetB))
	require.ErrorIs(t, err, credentialgrant.ErrReferenceNotUsable)

	created, err := store.CreateGrant(ctx, proposedTestGrant(t, "0192f5e2-7c3a-7b1e-8a9d-000000001182", grantTestTargetA))
	require.NoError(t, err)
	_, err = store.CreateGrant(ctx, proposedTestGrant(t, "0192f5e2-7c3a-7b1e-8a9d-000000001183", grantTestTargetA))
	require.ErrorIs(t, err, credentialgrant.ErrDuplicateGrant)

	// Another tenant cannot reach the row.
	_, err = store.TransitionGrant(ctx, 8, created.ID, grantTransitionBy(credentialgrant.ActionApprove, "200"))
	require.ErrorIs(t, err, credentialgrant.ErrGrantNotFound)

	active, err := store.TransitionGrant(ctx, 7, created.ID, grantTransitionBy(credentialgrant.ActionApprove, "200"))
	require.NoError(t, err)
	require.Equal(t, credentialgrant.GrantActive, active.State)
	require.EqualValues(t, 1, active.Epoch)

	key := credentialgrant.GrantKey{
		TenantProjectID: 7, AgentBindingID: grantTestBinding, ToolkitID: 11,
		CredentialReferenceID: grantTestCredential, TargetBindingID: grantTestTargetA,
	}
	read, err := store.GrantForKey(ctx, key)
	require.NoError(t, err)
	require.Equal(t, active.ID, read.ID)
	key.TargetBindingID = grantTestTargetB
	_, err = store.GrantForKey(ctx, key)
	require.ErrorIs(t, err, credentialgrant.ErrGrantNotFound)

	ref, err := store.CredentialReference(ctx, 7, grantTestCredential)
	require.NoError(t, err)
	require.Equal(t, grantTestTargetA, ref.TargetBindingID)
	_, err = store.CredentialReference(ctx, 8, grantTestCredential)
	require.ErrorIs(t, err, credentialgrant.ErrReferenceNotFound)
	toolkit, err := store.AgentToolkitBinding(ctx, grantTestBinding, 11)
	require.NoError(t, err)
	require.EqualValues(t, 7, toolkit.TenantProjectID)
	_, err = store.AgentToolkitBinding(ctx, grantTestBinding, 12)
	require.ErrorIs(t, err, credentialgrant.ErrToolkitNotBound)

	revoked, err := store.TransitionGrant(ctx, 7, created.ID, grantTransitionBy(credentialgrant.ActionRevoke, "100"))
	require.NoError(t, err)
	require.Equal(t, credentialgrant.GrantRevoked, revoked.State)
	// A revoked grant frees the key for a new proposal.
	_, err = store.CreateGrant(ctx, proposedTestGrant(t, "0192f5e2-7c3a-7b1e-8a9d-000000001184", grantTestTargetA))
	require.NoError(t, err)

	rows, err := store.ListGrants(ctx, credentialgrant.ListFilter{TenantProjectID: 7, State: credentialgrant.GrantRevoked})
	require.NoError(t, err)
	require.Len(t, rows, 1)

	events := auditEvents(t, pool, securityaudit.TypeGrantChanged, created.ID)
	require.Len(t, events, 3)
	require.Equal(t, []string{"grant.propose", "grant.approve", "grant.revoke"},
		[]string{events[0].Action, events[1].Action, events[2].Action})
	require.Equal(t, "user:200", events[1].Principal)
	require.EqualValues(t, 2, events[2].Epoch)
}

func TestCredentialGrantChangeRollsBackWhenItsAuditAppendFails(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedGrantWorld(t, pool)
	failing := NewCredentialGrants(pool, securityaudit.FailingAppender{Err: errors.New("outbox down")})
	_, err := failing.CreateGrant(context.Background(), proposedTestGrant(t, "0192f5e2-7c3a-7b1e-8a9d-000000001191", grantTestTargetA))
	require.Error(t, err)
	var count int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM elitea_identity.credential_use_grants`).Scan(&count))
	require.Zero(t, count)
}

// The entity access query runs against a migrated database with the
// bootstrap auth tables: a member sees an application of the project, a
// non-member does not, and an application that does not exist is refused.
func TestCredentialGrantEntityAccessRequiresMembershipAndTheApplication(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAuthCoreTables(t, pool)
	_, err := pool.Exec(t.Context(), `
CREATE TABLE IF NOT EXISTS public.auth_core__role (id SERIAL PRIMARY KEY, name TEXT NOT NULL, mode TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS public.auth_core__user_role (user_id INTEGER NOT NULL, role_id INTEGER NOT NULL);
CREATE SCHEMA IF NOT EXISTS centry;
CREATE TABLE IF NOT EXISTS centry.project (id INTEGER PRIMARY KEY, suspended BOOLEAN NOT NULL DEFAULT false);
INSERT INTO centry.project (id) VALUES (7) ON CONFLICT DO NOTHING;
INSERT INTO public.auth_core__user (id, email) VALUES (41, 'member@example.test'), (42, 'other@example.test');
CREATE SCHEMA IF NOT EXISTS p_7;
CREATE TABLE IF NOT EXISTS p_7.applications (id INTEGER PRIMARY KEY);
INSERT INTO p_7.applications (id) VALUES (3) ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	seedProjectAdmin(t, pool, 7, 41)

	access := NewCredentialGrantEntityAccess(pool)
	for _, tc := range []struct {
		name               string
		user, project, app int64
		want               bool
	}{
		{"member", 41, 7, 3, true},
		{"non-member", 42, 7, 3, false},
		{"missing application", 41, 7, 4, false},
		{"project without a tenant schema", 41, 8, 3, false},
		{"no user", 0, 7, 3, false},
	} {
		ok, err := access.AgentAccessible(t.Context(), tc.user, tc.project, tc.app)
		require.NoError(t, err, tc.name)
		require.Equal(t, tc.want, ok, tc.name)
	}
}
