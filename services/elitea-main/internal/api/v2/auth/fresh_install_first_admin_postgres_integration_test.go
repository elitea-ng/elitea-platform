package auth

// The first administrator of a FRESH install, made through single sign-on.
//
// Form (username/password) sign-in is off by default (ELITEA_FORM_LOGIN_ENABLED),
// so on a new deployment the ONLY way to obtain an administrator is an OIDC or
// SAML login named by `initial_global_admins` / ELITEA_INITIAL_GLOBAL_ADMINS.
// newFirstLoginPool hand-builds the handful of tables the grants write, which
// is exactly the kind of fixture that let a write to a pylon-only table
// (auth_core__user_group) roll back every first login on a real fresh install
// while the suite stayed green. These tests build the schema the way
// cmd/elitea-migrate does on an empty database (Bootstrap + ApplyShared) and
// assert the administration role and the actor PAT ROWS.

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/identity"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	bootstrapschema "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/identityrepo"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

func TestAFreshInstallMakesItsFirstAdministratorThroughOIDCFromTheEnvironment(t *testing.T) {
	pool := newFreshInstallFirstLoginPool(t)
	// The SSO-only shape: no authentication document, so the list comes from
	// the environment (cmd/elitea-main singleSignOnFirstLoginPolicy).
	t.Setenv("ELITEA_INITIAL_GLOBAL_ADMINS", "email:founder@corp.com")
	handler := (&OIDCHandler{pool: pool}).WithFirstLoginPolicy(
		FirstLoginPolicy{InitialGlobalAdmins: InitialGlobalAdminsFromEnv()})

	userID := signInWithVerifiedOIDC(t, handler, "founder-sub", "founder@corp.com", true)

	require.Equal(t, []string{identity.InitialAdministrationRole},
		administrationRoles(t, pool, userID),
		"a fresh install's first OIDC login named by ELITEA_INITIAL_GLOBAL_ADMINS got no administration role")
	tokens := ownedTokens(t, pool, userID)
	require.Len(t, tokens, 1, "the first administrator has no actor PAT")
	require.Equal(t, identityrepo.ActorPATName, tokens[0].name)

	// Somebody else signing in afterwards is an ordinary user.
	otherID := signInWithVerifiedOIDC(t, handler, "someone-sub", "someone@corp.com", true)
	require.Empty(t, administrationRoles(t, pool, otherID))
}

func TestAFreshInstallMakesItsFirstAdministratorThroughSAML(t *testing.T) {
	pool := newFreshInstallFirstLoginPool(t)
	handler := (&SAMLHandler{pool: pool}).WithFirstLoginPolicy(
		FirstLoginPolicy{InitialGlobalAdmins: []string{SAMLProviderRefPrefix + "founder-nameid"}})

	userID := signInWithSAML(t, handler, "founder-nameid", "founder@corp.com")

	require.Equal(t, []string{identity.InitialAdministrationRole},
		administrationRoles(t, pool, userID),
		"a fresh install's first SAML login named by initial_global_admins got no administration role")
	require.Len(t, ownedTokens(t, pool, userID), 1, "the first administrator has no actor PAT")
}

// newFreshInstallFirstLoginPool creates a private empty database and runs the
// two calls cmd/elitea-migrate makes on a first install.
func newFreshInstallFirstLoginPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := os.Getenv(environment)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL integration test", environment)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	require.NoError(t, err)
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	require.NoError(t, err)

	databaseName := fmt.Sprintf("elitea_fresh_sso_%d_%d", os.Getpid(), time.Now().UnixNano())
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
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated database: %v", err)
		}
		adminPool.Close()
	})

	_, err = pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`)
	require.NoError(t, err)
	applied, err := migrate.Bootstrap(ctx, pool, bootstrapschema.Initial)
	require.NoError(t, err)
	require.True(t, applied, "Bootstrap reported nothing to do on an empty database")
	require.NoError(t, migrate.New(pool, platformmigrations.Files).ApplyShared(ctx))
	return pool
}
