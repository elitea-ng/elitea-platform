package identityproviders

// The store against a real PostgreSQL, applying shared migration 0095.
//
// The DDL is executed from the migration FILE rather than restated here, so
// this test cannot pass against a schema the migration does not create. That is
// the same rule the pre-built MCP catalogue's integration test states, and it is
// what keeps a constraint from being asserted in a copy nobody deploys.

import (
	"context"
	"fmt"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

func TestUpsertBumpsTheRevisionSoAReaderCacheInvalidates(t *testing.T) {
	pool := newProviderPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	first, err := store.Upsert(ctx, validOIDC())
	require.NoError(t, err)
	require.Equal(t, 1, first.Revision)

	changed := validOIDC()
	changed.OIDC.Scopes = []string{"profile", "email", "groups"}
	second, err := store.Upsert(ctx, changed)
	require.NoError(t, err)

	// The revision is the login path's cache key. If a save did not move it,
	// a running process would go on using the previous document forever, and
	// the admin page's save would look like it did nothing.
	require.Equal(t, 2, second.Revision,
		"a save that does not move the revision leaves every reader on the old document")
	require.Contains(t, second.OIDC.Scopes, "groups")
}

// A save REPLACES the document. A merge would make removing a scope impossible:
// the operator deletes it from the form, and it comes back.
func TestUpsertReplacesTheDocumentRatherThanMergingIt(t *testing.T) {
	pool := newProviderPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	wide := validOIDC()
	wide.OIDC.Scopes = []string{"profile", "email", "groups"}
	_, err := store.Upsert(ctx, wide)
	require.NoError(t, err)

	narrow := validOIDC()
	narrow.OIDC.Scopes = []string{"email"}
	stored, err := store.Upsert(ctx, narrow)
	require.NoError(t, err)

	require.NotContains(t, stored.OIDC.Scopes, "groups")
	require.NotContains(t, stored.OIDC.Scopes, "profile")
}

// Enabling one provider disables the others of its kind IN THE SAME
// transaction. Leaving the partial unique index to reject the save would make
// the operator disable the old provider first, and between those two saves the
// deployment federates no logins at all.
func TestEnablingAProviderRetiresTheOtherOfItsKind(t *testing.T) {
	pool := newProviderPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	outgoing := validOIDC()
	outgoing.Enabled = true
	_, err := store.Upsert(ctx, outgoing)
	require.NoError(t, err)

	incoming := validOIDC()
	incoming.Key = "replacement"
	incoming.Enabled = true
	_, err = store.Upsert(ctx, incoming)
	require.NoError(t, err, "enabling a replacement must not be refused by the unique index")

	live, err := store.Enabled(ctx, KindOIDC)
	require.NoError(t, err)
	require.Equal(t, "replacement", live.Key)

	retired, err := store.Lookup(ctx, "corporate")
	require.NoError(t, err)
	require.False(t, retired.Enabled)
	require.NotNil(t, retired.OIDC, "a retired row keeps its document, so restoring it is one save")
}

// The two protocols mount different route sets, so one enabled definition of
// each is legitimate and must not be treated as a collision.
func TestOneProviderOfEachKindCanBeEnabledAtOnce(t *testing.T) {
	pool := newProviderPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	oidcProvider := validOIDC()
	oidcProvider.Enabled = true
	_, err := store.Upsert(ctx, oidcProvider)
	require.NoError(t, err)

	samlProvider := validSAML(t)
	samlProvider.Enabled = true
	_, err = store.Upsert(ctx, samlProvider)
	require.NoError(t, err)

	live, err := store.Enabled(ctx, KindSAML)
	require.NoError(t, err)
	require.Equal(t, "corporate_saml", live.Key)
	require.NotNil(t, live.SAML)
}

// "No provider is configured" and "the table could not be read" are different
// facts. The login path falls back to the environment on the first and refuses
// on the second, so the store must not collapse them.
func TestNoEnabledProviderIsNotFoundRatherThanAnError(t *testing.T) {
	pool := newProviderPool(t)
	store := NewStore(pool)

	_, err := store.Enabled(context.Background(), KindOIDC)
	require.ErrorIs(t, err, ErrNotFound)
}

// Delete returns the vault reference so the caller can remove the sealed
// secret. Losing it would leave a credential in the vault that nothing names.
func TestDeleteReturnsTheVaultReferenceItHeld(t *testing.T) {
	pool := newProviderPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	provider := validOIDC()
	provider.SecretRef = "identity_provider__corporate_abcd1234__secret"
	_, err := store.Upsert(ctx, provider)
	require.NoError(t, err)

	reference, err := store.Delete(ctx, "corporate")
	require.NoError(t, err)
	require.Equal(t, "identity_provider__corporate_abcd1234__secret", reference)

	_, err = store.Lookup(ctx, "corporate")
	require.ErrorIs(t, err, ErrNotFound)
}

// The migration's constraints are load-bearing: they are what stops a row that
// no reader in this service can open.
func TestTheMigrationRefusesARowNoReaderCanOpen(t *testing.T) {
	pool := newProviderPool(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx,
		`INSERT INTO elitea_auth.identity_providers (provider_key, kind, display_name)
		 VALUES ('ldap', 'ldap', 'LDAP')`)
	require.Error(t, err, "a kind no login path implements must not be storable")

	_, err = pool.Exec(ctx,
		`INSERT INTO elitea_auth.identity_providers (provider_key, kind, display_name, config)
		 VALUES ('array', 'oidc', 'Array config', '[]'::jsonb)`)
	require.Error(t, err, "a config that is not an object cannot be decoded")
}

/* ── database bootstrap ────────────────────────────────────────────────── */

func newProviderPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	require.NoError(t, err)
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	require.NoError(t, err)
	require.NoError(t, adminPool.Ping(ctx))

	databaseName := fmt.Sprintf("elitea_idp_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{databaseName}.Sanitize()
	_, err = adminPool.Exec(ctx, "CREATE DATABASE "+quoted)
	require.NoError(t, err)

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	require.NoError(t, err)

	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated database: %v", err)
		}
		adminPool.Close()
	})

	migration, err := os.ReadFile("../../migrations/shared/0095_identity_providers.sql")
	require.NoError(t, err, "the migration file must be readable: this test proves IT, not a copy of it")
	_, err = pool.Exec(ctx, string(migration))
	require.NoError(t, err)

	// Applying it twice must be a no-op. Every file in this corpus is expected
	// to be idempotent, and a re-run is what a partially-applied deployment
	// does.
	_, err = pool.Exec(ctx, string(migration))
	require.NoError(t, err, "migration 0095 must be idempotent")

	return pool
}
