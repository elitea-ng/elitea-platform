package delegationstore

// Shared 0185 (issue #1117): sign-in retention, delegation from the session,
// Load scoped to tenant project and binding, revocation wiping the
// ciphertext and bumping the epoch atomically with its audit event, logout
// revocation, tampered rows failing to open, and the authorization
// requirement round trip. CI-only: skipped without the integration database.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/dbtest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

const (
	custodyURLEnv    = "ELITEA_TEST_DATABASE_URL"
	custodyBootstrap = "../../../db/migrations/001_initial.sql"
	custodyRevision  = "rev-1117"
	custodyTenant    = "aaaaaaaa-0000-4000-8000-000000000001"
	custodyUser      = "cccccccc-0000-4000-8000-000000000003"
	middleTier       = "11111111-1111-1111-1111-111111111111"
	custodyBinding   = "0192f5e2-7c3a-7b1e-8a9d-00000000b001"
	otherBinding     = "0192f5e2-7c3a-7b1e-8a9d-00000000b002"
)

var (
	custodyTemplateOnce sync.Once
	custodyTemplate     string
	custodyTemplateErr  error
)

func newCustodyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := testpg.DSN(os.Getenv(custodyURLEnv))
	if databaseURL == "" {
		t.Skipf("set %s to run the delegation custody integration test", custodyURLEnv)
	}
	custodyTemplateOnce.Do(func() {
		bootstrap, err := os.ReadFile(custodyBootstrap)
		if err != nil {
			custodyTemplateErr = err
			return
		}
		ctx, cancel := dbtest.BuildContext(context.Background())
		defer cancel()
		admin, err := pgxpool.New(ctx, databaseURL)
		if err != nil {
			custodyTemplateErr = err
			return
		}
		defer admin.Close()
		custodyTemplate, custodyTemplateErr = dbtest.EnsureTemplate(ctx, admin, dbtest.Spec{
			Files: platformmigrations.Files, Seed: string(bootstrap),
		})
	})
	require.NoError(t, custodyTemplateErr)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, databaseURL)
	require.NoError(t, err)
	defer admin.Close()
	name := fmt.Sprintf("elitea_custody_%d_%d", os.Getpid(), time.Now().UnixNano())
	require.NoError(t, dbtest.CreateFromTemplate(ctx, admin, custodyTemplate, name))
	config, err := pgxpool.ParseConfig(databaseURL)
	require.NoError(t, err)
	config.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, config)
	require.NoError(t, err)
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer dropCancel()
		dropPool, dropErr := pgxpool.New(dropCtx, databaseURL)
		if dropErr != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(dropCtx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	})
	seedCustodyBindings(t, pool)
	return pool
}

func seedCustodyBindings(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	canonical := []byte(`{"schema":"elitea.credential-policy.v1"}`)
	digest := sha256.Sum256(canonical)
	_, err := pool.Exec(ctx, `
INSERT INTO elitea_identity.credential_policy_revisions (id, digest, canonical, document, created_by)
VALUES ($1, $2, $3, $4::jsonb, 'integration-test')`, custodyRevision, digest[:], canonical, string(canonical))
	require.NoError(t, err)
	for index, id := range []string{custodyBinding, otherBinding} {
		_, err := pool.Exec(ctx, `
INSERT INTO elitea_identity.agent_identity_bindings
    (id, tenant_project_id, application_id, application_version_id, definition_digest, mode,
     policy_revision_id, proposed_by, approved_by, last_transition_by, state)
VALUES ($1::uuid, 7, 3, $2, $3, 'dedicated_agent', $4, '100', '200', '200', 'active')`,
			id, index+1, bytes.Repeat([]byte{byte(index + 1)}, 32), custodyRevision)
		require.NoError(t, err)
	}
}

func newTestStore(t *testing.T, pool *pgxpool.Pool) *Store {
	t.Helper()
	audit, err := securityaudit.NewPostgresAppender("elitea-main/integration-test")
	require.NoError(t, err)
	store, err := New(pool, bytes.Repeat([]byte{9}, 32), audit)
	require.NoError(t, err)
	return store
}

func signInFor(session string) SignIn {
	return SignIn{
		SessionID: session, UserID: 42, Issuer: "https://login.example/" + custodyTenant + "/v2.0",
		TenantID: custodyTenant, ObjectID: custodyUser, Audience: middleTier, Scopes: []string{"access_as_user"},
		Token: "header.payload-for-" + session + ".signature", ExpiresAt: time.Now().Add(time.Hour),
	}
}

func delegateFor(session string) DelegateRequest {
	return DelegateRequest{
		SessionID: session, UserID: 42, TenantProjectID: 7, AgentBindingID: custodyBinding,
		Audience: middleTier, Scopes: []string{"https://graph.microsoft.com/Sites.Selected"},
	}
}

func countAudit(t *testing.T, pool *pgxpool.Pool, action, resourceID string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `
SELECT count(*) FROM elitea_runtime.security_audit_outbox
WHERE event->>'type' = $1 AND event->'data'->>'action' = $2 AND event->'data'->>'resource_id' = $3`,
		string(securityaudit.TypeDelegationChanged), action, resourceID).Scan(&n))
	return n
}

func TestSignInAssertionBecomesALoadableDelegation(t *testing.T) {
	pool := newCustodyPool(t)
	store := newTestStore(t, pool)
	ctx := context.Background()

	signIn := signInFor("session-1")
	_, err := store.RetainSignIn(ctx, signIn)
	require.NoError(t, err)

	// The stored ciphertext holds no token text.
	var stored []byte
	require.NoError(t, pool.QueryRow(ctx, `SELECT assertion_ciphertext FROM elitea_identity.signin_assertions`).Scan(&stored))
	require.NotContains(t, string(stored), signIn.Token)

	delegation, err := store.Delegate(ctx, delegateFor("session-1"))
	require.NoError(t, err)
	require.Equal(t, 1, countAudit(t, pool, "delegation.create", delegation.ID))

	assertion, loaded, err := store.Load(ctx, LoadRequest{DelegationID: delegation.ID, TenantProjectID: 7, AgentBindingID: custodyBinding})
	require.NoError(t, err)
	require.Equal(t, signIn.Token, assertion.Token)
	require.Equal(t, custodyTenant, assertion.TenantID)
	require.Equal(t, custodyUser, assertion.ObjectID)
	require.Equal(t, middleTier, assertion.Audience)
	require.Equal(t, delegation.ID, loaded.ID)

	// Out of scope: another binding or tenant project sees nothing.
	_, _, err = store.Load(ctx, LoadRequest{DelegationID: delegation.ID, TenantProjectID: 7, AgentBindingID: otherBinding})
	require.ErrorIs(t, err, ErrDelegationNotFound)
	_, _, err = store.Load(ctx, LoadRequest{DelegationID: delegation.ID, TenantProjectID: 8, AgentBindingID: custodyBinding})
	require.ErrorIs(t, err, ErrDelegationNotFound)

	// Another session, user or audience has no assertion to delegate.
	for _, request := range []DelegateRequest{
		delegateFor("session-2"),
		func() DelegateRequest { r := delegateFor("session-1"); r.UserID = 43; return r }(),
		func() DelegateRequest {
			r := delegateFor("session-1")
			r.Audience = "api://blueprint/access_agent"
			return r
		}(),
	} {
		_, err := store.Delegate(ctx, request)
		require.ErrorIs(t, err, ErrNoSignInAssertion)
	}
}

func TestATamperedDelegationRowDoesNotOpen(t *testing.T) {
	pool := newCustodyPool(t)
	store := newTestStore(t, pool)
	ctx := context.Background()
	_, err := store.RetainSignIn(ctx, signInFor("session-1"))
	require.NoError(t, err)
	delegation, err := store.Delegate(ctx, delegateFor("session-1"))
	require.NoError(t, err)

	for column, value := range map[string]string{
		"subject_object_id":    "'dddddddd-0000-4000-8000-000000000004'",
		"assertion_audience":   "'api://other'",
		"assertion_expires_at": "assertion_expires_at + interval '1 second'",
	} {
		t.Run(column, func(t *testing.T) {
			tx, err := pool.Begin(ctx)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback(ctx) }()
			_, err = tx.Exec(ctx, "UPDATE elitea_identity.delegations SET "+column+" = "+value+" WHERE id = $1::uuid", delegation.ID)
			require.NoError(t, err)
			// A store over the transaction would need a seam; read through
			// the same query Load runs instead.
			var (
				ciphertext []byte
				d          Delegation
				expires    time.Time
			)
			require.NoError(t, tx.QueryRow(ctx, `
SELECT id::text, tenant_project_id, agent_binding_id::text, tenant_id, subject_object_id, assertion_ciphertext,
       assertion_expires_at, assertion_audience
FROM elitea_identity.delegations WHERE id = $1::uuid`, delegation.ID).
				Scan(&d.ID, &d.TenantProjectID, &d.AgentBindingID, &d.TenantID, &d.ObjectID, &ciphertext, &expires, &d.Audience))
			d.ExpiresAt = expires.UTC()
			_, err = store.sealer.open(ciphertext, d.aad())
			require.ErrorIs(t, err, ErrCustodyIntegrity)
		})
	}
	_, _, err = store.Load(ctx, LoadRequest{DelegationID: delegation.ID, TenantProjectID: 7, AgentBindingID: custodyBinding})
	require.NoError(t, err, "the rolled-back edits left the row intact")
}

func TestRevocationWipesTheCiphertextAndBumpsTheEpochAtomically(t *testing.T) {
	pool := newCustodyPool(t)
	store := newTestStore(t, pool)
	ctx := context.Background()
	_, err := store.RetainSignIn(ctx, signInFor("session-1"))
	require.NoError(t, err)
	delegation, err := store.Delegate(ctx, delegateFor("session-1"))
	require.NoError(t, err)

	// An audit append failure rolls the revocation back: state, ciphertext
	// and epoch are unchanged.
	failing := *store
	failing.audit = securityaudit.FailingAppender{}
	_, err = failing.Revoke(ctx, delegation.ID, "100")
	require.Error(t, err)
	var state string
	var epoch int64
	var wiped bool
	read := func() {
		require.NoError(t, pool.QueryRow(ctx, `
SELECT state, revocation_epoch, assertion_ciphertext IS NULL FROM elitea_identity.delegations WHERE id = $1::uuid`,
			delegation.ID).Scan(&state, &epoch, &wiped))
	}
	read()
	require.Equal(t, "active", state)
	require.EqualValues(t, 0, epoch)
	require.False(t, wiped)

	got, err := store.Revoke(ctx, delegation.ID, "100")
	require.NoError(t, err)
	require.EqualValues(t, 1, got)
	read()
	require.Equal(t, "revoked", state)
	require.EqualValues(t, 1, epoch)
	require.True(t, wiped)
	require.Equal(t, 1, countAudit(t, pool, "delegation.revoke", delegation.ID))

	// Idempotent, and a revoked delegation never loads.
	got, err = store.Revoke(ctx, delegation.ID, "100")
	require.NoError(t, err)
	require.EqualValues(t, 1, got)
	_, _, err = store.Load(ctx, LoadRequest{DelegationID: delegation.ID, TenantProjectID: 7, AgentBindingID: custodyBinding})
	require.ErrorIs(t, err, ErrDelegationNotUsable)

	// The table refuses a revoked row that still holds ciphertext.
	_, err = pool.Exec(ctx, `UPDATE elitea_identity.delegations SET assertion_ciphertext = '\x00' WHERE id = $1::uuid`, delegation.ID)
	require.Error(t, err)
}

func TestConcurrentRevocationsBumpTheEpochOnce(t *testing.T) {
	pool := newCustodyPool(t)
	store := newTestStore(t, pool)
	ctx := context.Background()
	_, err := store.RetainSignIn(ctx, signInFor("session-1"))
	require.NoError(t, err)
	delegation, err := store.Delegate(ctx, delegateFor("session-1"))
	require.NoError(t, err)

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.Revoke(ctx, delegation.ID, "100")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	var epoch int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT revocation_epoch FROM elitea_identity.delegations WHERE id = $1::uuid`, delegation.ID).Scan(&epoch))
	require.EqualValues(t, 1, epoch)
	require.Equal(t, 1, countAudit(t, pool, "delegation.revoke", delegation.ID))
}

func TestLogoutRevokesWhatTheSessionCreated(t *testing.T) {
	pool := newCustodyPool(t)
	store := newTestStore(t, pool)
	ctx := context.Background()
	for _, session := range []string{"session-1", "session-2"} {
		_, err := store.RetainSignIn(ctx, signInFor(session))
		require.NoError(t, err)
	}
	mine, err := store.Delegate(ctx, delegateFor("session-1"))
	require.NoError(t, err)
	theirs, err := store.Delegate(ctx, delegateFor("session-2"))
	require.NoError(t, err)

	revoked, err := store.RevokeSession(ctx, "session-1")
	require.NoError(t, err)
	require.Equal(t, 1, revoked)

	_, _, err = store.Load(ctx, LoadRequest{DelegationID: mine.ID, TenantProjectID: 7, AgentBindingID: custodyBinding})
	require.ErrorIs(t, err, ErrDelegationNotUsable)
	_, _, err = store.Load(ctx, LoadRequest{DelegationID: theirs.ID, TenantProjectID: 7, AgentBindingID: custodyBinding})
	require.NoError(t, err, "another session's delegation is untouched")
	_, err = store.Delegate(ctx, delegateFor("session-1"))
	require.ErrorIs(t, err, ErrNoSignInAssertion, "the session's sign-in assertion is revoked too")

	var live int
	require.NoError(t, pool.QueryRow(ctx, `
SELECT count(*) FROM elitea_identity.signin_assertions WHERE assertion_ciphertext IS NOT NULL`).Scan(&live))
	require.Equal(t, 1, live)
}

// A delegation is minted only for a binding that is active in its tenant
// project: another tenant's binding, or a suspended one, is refused.
func TestDelegateNeedsAnActiveBindingInTheTenantProject(t *testing.T) {
	pool := newCustodyPool(t)
	store := newTestStore(t, pool)
	ctx := context.Background()
	_, err := store.RetainSignIn(ctx, signInFor("session-1"))
	require.NoError(t, err)
	other := delegateFor("session-1")
	other.TenantProjectID = 8
	_, err = store.Delegate(ctx, other)
	require.ErrorIs(t, err, ErrBindingNotActive)
	_, err = pool.Exec(ctx, `UPDATE elitea_identity.agent_identity_bindings SET state = 'suspended' WHERE id = $1::uuid`, custodyBinding)
	require.NoError(t, err)
	_, err = store.Delegate(ctx, delegateFor("session-1"))
	require.ErrorIs(t, err, ErrBindingNotActive)
}

func TestAuthorizationRequirementRoundTrip(t *testing.T) {
	pool := newCustodyPool(t)
	store := newTestStore(t, pool)
	ctx := context.Background()
	for _, session := range []string{"session-1", "session-2"} {
		_, err := store.RetainSignIn(ctx, signInFor(session))
		require.NoError(t, err)
	}
	paused, err := store.Delegate(ctx, delegateFor("session-1"))
	require.NoError(t, err)
	answer, err := store.Delegate(ctx, delegateFor("session-2"))
	require.NoError(t, err)
	requirement := Requirement{
		TenantProjectID: 7, ActorUserID: 42,
		ConversationUUID: "0192f5e2-7c3a-7b1e-8a9d-0000000000c1", ResponseMessageID: "0192f5e2-7c3a-7b1e-8a9d-0000000000c2",
		ExecutionID: "exec-1", ExecutionGeneration: "3", DelegationID: paused.ID,
		Scopes: []string{"https://graph.microsoft.com/Sites.Selected"}, Reason: ReasonClaimsChallenge,
		Claims: `{"access_token":{"acrs":{"essential":true,"value":"c1"}}}`,
	}
	recorded, err := store.RecordRequirement(ctx, requirement)
	require.NoError(t, err)
	require.NotEmpty(t, recorded.Reference)

	_, err = store.RequirementFor(ctx, recorded.Reference, 7, 43)
	require.ErrorIs(t, err, ErrRequirementNotFound, "another user cannot read it")

	// A delegation that does not exist, or no longer authorizes, is no answer.
	_, err = store.ResolveRequirement(ctx, recorded.Reference, 7, 42, "0192f5e2-7c3a-7b1e-8a9d-0000000000d9")
	require.ErrorIs(t, err, ErrInvalid)
	_, err = store.RevokeSession(ctx, "session-1")
	require.NoError(t, err)
	_, err = store.ResolveRequirement(ctx, recorded.Reference, 7, 42, paused.ID)
	require.ErrorIs(t, err, ErrInvalid, "a revoked delegation does not resolve the pause")

	resolved, err := store.ResolveRequirement(ctx, recorded.Reference, 7, 42, answer.ID)
	require.NoError(t, err)
	require.True(t, resolved.Resolved)
	// A retry of the same answer (its continuation failed) resolves again.
	again, err := store.ResolveRequirement(ctx, recorded.Reference, 7, 42, answer.ID)
	require.NoError(t, err)
	require.Equal(t, answer.ID, again.ResolvedDelegationID)
	_, err = store.ResolveRequirement(ctx, recorded.Reference, 7, 42, paused.ID)
	require.True(t, errors.Is(err, ErrRequirementResolved), "a second, different answer is refused")
}
