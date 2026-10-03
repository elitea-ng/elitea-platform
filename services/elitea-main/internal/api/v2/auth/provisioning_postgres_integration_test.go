package auth

// The email fallback's adoption guard, against a real PostgreSQL.
//
// # Why these need a database
//
// The guard is one `NOT EXISTS` sub-select inside an `ON CONFLICT ... WHERE`
// clause. A scripted transaction can assert the ORDER of the statements — which
// is what oidc_test.go does — but it cannot assert what that clause MATCHES,
// because the matching is done by PostgreSQL. The defect this file exists to
// stop was invisible to the scripted tests for exactly that reason: the guard
// read `LIKE 'oidc:%'`, the SAML path wrote `saml:`, and every scripted test
// went on passing while a second SAML subject could take over an account.

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// resolveOnce runs one login's resolution in its own committed transaction.
func resolveOnce(t *testing.T, pool *pgxpool.Pool, providerRef, email string) (int, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	userID, err := resolveProvisionedUser(ctx, tx, providerRef, email, "Alice", nil, false)
	if err != nil {
		return 0, err
	}
	return userID, tx.Commit(ctx)
}

// A second SAML subject must NOT take over an account the first one owns.
//
// This is the takeover the OIDC path documents as failure 1, reintroduced for
// SAML by a guard that named one namespace. The identity provider re-issuing a
// persistent NameID is enough to trigger it; so is any other account at that
// provider asserting the address.
func TestASecondSAMLSubjectCannotAdoptTheFirstsAccount(t *testing.T) {
	pool := newProvisioningPool(t)

	first, err := resolveOnce(t, pool, SAMLProviderRefPrefix+"alice-nameid-1", "alice@corp.com")
	require.NoError(t, err)

	_, err = resolveOnce(t, pool, SAMLProviderRefPrefix+"alice-nameid-2", "alice@corp.com")
	require.ErrorIs(t, err, errIdentityConflict,
		"a second SAML subject adopted account %d by email alone", first)
}

// And the cross-protocol direction. A deployment may run one OIDC and one SAML
// provider at once, and they may be different identity providers.
func TestAnOIDCSubjectCannotAdoptASAMLOwnedAccount(t *testing.T) {
	pool := newProvisioningPool(t)

	_, err := resolveOnce(t, pool, SAMLProviderRefPrefix+"alice-nameid-1", "alice@corp.com")
	require.NoError(t, err)

	_, err = resolveOnce(t, pool, OIDCProviderRefPrefix+"mallory-sub", "alice@corp.com")
	require.ErrorIs(t, err, errIdentityConflict)
}

func TestASAMLSubjectCannotAdoptAnOIDCOwnedAccount(t *testing.T) {
	pool := newProvisioningPool(t)

	_, err := resolveOnce(t, pool, OIDCProviderRefPrefix+"alice-sub", "alice@corp.com")
	require.NoError(t, err)

	_, err = resolveOnce(t, pool, SAMLProviderRefPrefix+"mallory-nameid", "alice@corp.com")
	require.ErrorIs(t, err, errIdentityConflict)
}

// The same subject signing in twice still resolves its own account. Without
// this, a guard that refused everything would pass every test above.
func TestTheSameSubjectKeepsItsAccount(t *testing.T) {
	pool := newProvisioningPool(t)

	first, err := resolveOnce(t, pool, SAMLProviderRefPrefix+"alice-nameid-1", "alice@corp.com")
	require.NoError(t, err)

	again, err := resolveOnce(t, pool, SAMLProviderRefPrefix+"alice-nameid-1", "alice.smith@corp.com")
	require.NoError(t, err)
	require.Equal(t, first, again, "a changed address must not orphan the account")
}

/* ── what must STAY adoptable ──────────────────────────────────────────── */

// A pylon-created account holds a BARE ref: the raw subject with no prefix
// (legacy/plugins/auth_init/rpc/processor.py:55). A namespace-blind guard would
// refuse every such account with 409 on its first login here.
func TestAPylonBareRefAccountIsStillAdoptable(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx := context.Background()

	var existing int
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO auth_core__user (email, name) VALUES ('alice@corp.com', 'Alice') RETURNING id`,
	).Scan(&existing))
	_, err := pool.Exec(ctx,
		`INSERT INTO auth_core__user_provider (user_id, provider_ref) VALUES ($1, 'alice-raw-subject')`,
		existing)
	require.NoError(t, err)

	adopted, err := resolveOnce(t, pool, OIDCProviderRefPrefix+"alice-sub", "alice@corp.com")
	require.NoError(t, err)
	require.Equal(t, existing, adopted)
}

// An invited person holds `cirro:invite:token:<token>`
// (legacy/plugins/admin/api/v2/user_invite.py:83). That is an invite receipt,
// not a federated identity, and it contains colons — so a guard written as
// "any ref with a colon" would lock every invited person out.
func TestAnInvitedAccountIsStillAdoptable(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx := context.Background()

	var existing int
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO auth_core__user (email, name) VALUES ('bob@corp.com', 'Bob') RETURNING id`,
	).Scan(&existing))
	_, err := pool.Exec(ctx,
		`INSERT INTO auth_core__user_provider (user_id, provider_ref)
		 VALUES ($1, 'cirro:invite:token:abc123')`, existing)
	require.NoError(t, err)

	adopted, err := resolveOnce(t, pool, SAMLProviderRefPrefix+"bob-nameid", "bob@corp.com")
	require.NoError(t, err)
	require.Equal(t, existing, adopted)
}

// An account nobody holds is adopted, which is the whole point of the fallback.
func TestAnUnlinkedAccountIsAdopted(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx := context.Background()

	var existing int
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO auth_core__user (email, name) VALUES ('carol@corp.com', 'Carol') RETURNING id`,
	).Scan(&existing))

	adopted, err := resolveOnce(t, pool, OIDCProviderRefPrefix+"carol-sub", "carol@corp.com")
	require.NoError(t, err)
	require.Equal(t, existing, adopted)
}

// A suspended account is refused as suspended, not as a conflict. The two are
// different operator actions and the login reports them differently.
func TestASuspendedAccountIsReportedAsSuspended(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx,
		`INSERT INTO auth_core__user (email, name, suspended) VALUES ('dave@corp.com', 'Dave', true)`)
	require.NoError(t, err)

	_, err = resolveOnce(t, pool, SAMLProviderRefPrefix+"dave-nameid", "dave@corp.com")
	require.ErrorIs(t, err, errUserSuspended)
}

/* ── database bootstrap ────────────────────────────────────────────────── */

func newProvisioningPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := os.Getenv(environment)
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

	databaseName := fmt.Sprintf("elitea_prov_it_%d_%d", os.Getpid(), time.Now().UnixNano())
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

	// The two tables in the shape 001_initial.sql creates them. They are
	// restated rather than read from that file because it is a thousand
	// statements of unrelated schema; these columns are the whole of what the
	// resolution path touches.
	_, err = pool.Exec(ctx, `
		CREATE TABLE auth_core__user (
			id SERIAL PRIMARY KEY,
			email TEXT UNIQUE,
			name TEXT,
			last_login TIMESTAMP,
			suspended BOOLEAN NOT NULL DEFAULT false
		);
		CREATE TABLE auth_core__user_provider (
			user_id INTEGER REFERENCES auth_core__user(id) ON DELETE CASCADE,
			provider_ref TEXT UNIQUE,
			PRIMARY KEY (user_id, provider_ref)
		)`)
	require.NoError(t, err)

	// The SCIM side table the adoption guard reads, from the migration's own
	// file rather than a copy of it.
	migration, err := os.ReadFile("../../../../migrations/shared/0096_scim_provisioning.sql")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(migration))
	require.NoError(t, err)

	return pool
}

/* ── M5: a SCIM-provisioned account is adopted only by a paired provider ── */

// seedSCIMAccount is an account a SCIM client provisioned: an address, a SCIM
// record, and no federated link.
func seedSCIMAccount(t *testing.T, pool *pgxpool.Pool, email string) int {
	t.Helper()
	ctx := context.Background()
	var id int
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO auth_core__user (email, name) VALUES ($1, 'Provisioned') RETURNING id`, email,
	).Scan(&id))
	_, err := pool.Exec(ctx,
		`INSERT INTO elitea_auth.scim_users (user_id, external_id) VALUES ($1, 'entra-object-id')`, id)
	require.NoError(t, err)
	return id
}

func resolveOnceFor(t *testing.T, pool *pgxpool.Pool, providerRef, email string, adoptSCIMUsers bool) (int, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	userID, err := resolveProvisionedUserFor(ctx, tx, providerRef, email, "Alice", nil, false, adoptSCIMUsers)
	if err != nil {
		return 0, err
	}
	return userID, tx.Commit(ctx)
}

// A provider NOT paired with SCIM — a GitHub connector behind Dex, any OIDC or
// SAML provider without `adopt_scim_users` — asserting a directory-provisioned
// address is refused, and nothing is linked.
func TestAnUnpairedProviderCannotAdoptASCIMProvisionedAccount(t *testing.T) {
	pool := newProvisioningPool(t)
	provisioned := seedSCIMAccount(t, pool, "alice@corp.com")

	_, err := resolveOnceFor(t, pool, OIDCProviderRefPrefix+"github|mallory", "alice@corp.com", false)
	require.ErrorIs(t, err, errIdentityConflict)
	// The default resolution (no provider setting known) refuses too.
	_, err = resolveOnce(t, pool, SAMLProviderRefPrefix+"some-nameid", "Alice@Corp.com")
	require.ErrorIs(t, err, errIdentityConflict)

	var links int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM auth_core__user_provider WHERE user_id = $1`, provisioned).Scan(&links))
	require.Zero(t, links)
}

// The provider PAIRED with the SCIM client (Entra ID's SAML app, with
// `adopt_scim_users` on) adopts the provisioned account on its first login, and
// the next login resolves by subject.
func TestThePairedProviderAdoptsTheSCIMProvisionedAccount(t *testing.T) {
	pool := newProvisioningPool(t)
	provisioned := seedSCIMAccount(t, pool, "alice@corp.com")

	adopted, err := resolveOnceFor(t, pool, SAMLProviderRefPrefix+"entra-nameid", "Alice@Corp.com", true)
	require.NoError(t, err)
	require.Equal(t, provisioned, adopted)

	again, err := resolveOnce(t, pool, SAMLProviderRefPrefix+"entra-nameid", "alice@corp.com")
	require.NoError(t, err)
	require.Equal(t, provisioned, again)

	// Once linked, a DIFFERENT provider is refused even with the setting on:
	// the federated-link guard still applies.
	_, err = resolveOnceFor(t, pool, OIDCProviderRefPrefix+"other-sub", "alice@corp.com", true)
	require.ErrorIs(t, err, errIdentityConflict)
}

// An account with no SCIM record keeps the old rule: adoptable whatever the
// setting says, so turning SCIM on does not lock out existing users.
func TestAnAccountWithoutASCIMRecordIsStillAdoptedWithTheSettingOff(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx := context.Background()

	var existing int
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO auth_core__user (email, name) VALUES ('erin@corp.com', 'Erin') RETURNING id`,
	).Scan(&existing))

	adopted, err := resolveOnceFor(t, pool, OIDCProviderRefPrefix+"erin-sub", "erin@corp.com", false)
	require.NoError(t, err)
	require.Equal(t, existing, adopted)
}
