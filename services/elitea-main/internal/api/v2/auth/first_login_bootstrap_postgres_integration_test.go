package auth

// identity.initial_global_admins (ELITEA_INITIAL_GLOBAL_ADMINS) is a
// bootstrap only (issue 2589, ADR-0059 R0.12b, decision 5): it grants
// super_admin at first sign-in only while no unsuspended account holds
// admin.auth.users.super_admin. Once one does, the entry is skipped with a
// warning that names the entry's shape, never the address.

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/identity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identityrepo"
)

const bootstrapAdminAddress = "alice@corp.com"

// captureWarnings sends slog's default logger to a buffer for the test.
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buffer bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buffer
}

// existingSuperAdmin creates an account that holds super_admin, suspended or
// not, and returns its id.
func existingSuperAdmin(t *testing.T, pool *pgxpool.Pool, suspended bool) int {
	t.Helper()
	ctx := context.Background()
	var id int
	require.NoError(t, pool.QueryRow(ctx, `
INSERT INTO auth_core__user (email, name, suspended) VALUES ('root@corp.com', 'Root', $1)
RETURNING id`, suspended).Scan(&id))
	_, err := pool.Exec(ctx, `
INSERT INTO auth_core__user_role (user_id, role_id)
SELECT $1, id FROM auth_core__role WHERE name = 'super_admin' AND mode = 'administration'`, id)
	require.NoError(t, err)
	return id
}

func requireSkipWarning(t *testing.T, logs *bytes.Buffer, shape string) {
	t.Helper()
	text := logs.String()
	require.Contains(t, text, "identity.initial_global_admins entry ignored")
	require.Contains(t, text, "elitea-main admin-role grant")
	require.Contains(t, text, "entry_shape="+shape)
	require.NotContains(t, strings.ToLower(text), bootstrapAdminAddress,
		"the warning names the address")
}

func TestAListedAddressGetsNothingOnceAGlobalAdministratorExists(t *testing.T) {
	pool := newFirstLoginPool(t)
	existingSuperAdmin(t, pool, false)
	logs := captureWarnings(t)
	handler := (&OIDCHandler{pool: pool}).WithFirstLoginPolicy(
		FirstLoginPolicy{InitialGlobalAdmins: []string{"email:" + bootstrapAdminAddress}})

	userID := signInWithVerifiedOIDC(t, handler,
		"00000000-1111-2222-3333-666666666666", bootstrapAdminAddress, true)

	require.Empty(t, administrationRoles(t, pool, userID),
		"the list granted a role while a global administrator exists")
	requireSkipWarning(t, logs, identity.InitialGlobalAdminShapeEmail)
	var history int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM elitea_auth.admin_role_grants WHERE user_id = $1`, userID).Scan(&history))
	require.Zero(t, history)
}

func TestAListedAddressIsGrantedSuperAdminWhileThereIsNoGlobalAdministrator(t *testing.T) {
	pool := newFirstLoginPool(t)
	logs := captureWarnings(t)
	handler := (&OIDCHandler{pool: pool}).WithFirstLoginPolicy(
		FirstLoginPolicy{InitialGlobalAdmins: []string{"email:" + bootstrapAdminAddress}})

	userID := signInWithVerifiedOIDC(t, handler,
		"00000000-1111-2222-3333-777777777777", bootstrapAdminAddress, true)

	require.Equal(t, []string{identity.InitialAdministrationRole}, administrationRoles(t, pool, userID))
	require.NotContains(t, logs.String(), "identity.initial_global_admins entry ignored")

	// The first administrator ends the bootstrap: the next listed account
	// gets nothing.
	second := signInWithOIDC(t, handler, "bob-sub", "bob@corp.com")
	require.Empty(t, administrationRoles(t, pool, second))
	ignored, err := identityrepo.InitialGlobalAdminsIgnored(context.Background(), pool,
		[]string{"email:" + bootstrapAdminAddress})
	require.NoError(t, err)
	require.True(t, ignored, "the posture predicate misses a list configured after the bootstrap")
}

func TestASuspendedGlobalAdministratorDoesNotEndTheBootstrap(t *testing.T) {
	pool := newFirstLoginPool(t)
	existingSuperAdmin(t, pool, true)
	handler := (&OIDCHandler{pool: pool}).WithFirstLoginPolicy(
		FirstLoginPolicy{InitialGlobalAdmins: []string{"alice-sub"}})

	ignored, err := identityrepo.InitialGlobalAdminsIgnored(context.Background(), pool, []string{"alice-sub"})
	require.NoError(t, err)
	require.False(t, ignored)

	userID := signInWithOIDC(t, handler, "alice-sub", bootstrapAdminAddress)
	require.Equal(t, []string{identity.InitialAdministrationRole}, administrationRoles(t, pool, userID),
		"a suspended super_admin blocked the bootstrap")
}

// The provisioning plane (identityrepo.Provision, a new account through the
// form and the identity service) follows the same rule.
func TestTheProvisioningPathFollowsTheBootstrapRule(t *testing.T) {
	for name, tc := range map[string]struct {
		superAdmin bool
		want       []string
	}{
		"no global administrator": {want: []string{identity.InitialAdministrationRole}},
		"a global administrator":  {superAdmin: true},
	} {
		t.Run(name, func(t *testing.T) {
			pool := newFirstLoginPool(t)
			if tc.superAdmin {
				existingSuperAdmin(t, pool, false)
			}
			logs := captureWarnings(t)
			repository, err := identityrepo.NewPostgresRepository(pool)
			require.NoError(t, err)
			result, err := repository.Provision(context.Background(), identity.ProvisionCommand{
				Provider:                  "form",
				ProviderReference:         "alice",
				Email:                     bootstrapAdminAddress,
				Name:                      "Alice",
				InitialAdministrationMode: identity.InitialAdministrationMode,
				InitialAdministrationRole: identity.InitialAdministrationRole,
			})
			require.NoError(t, err)
			roles := administrationRoles(t, pool, int(result.UserID))
			if tc.want == nil {
				require.Empty(t, roles)
				requireSkipWarning(t, logs, identity.InitialGlobalAdminShapeReference)
				return
			}
			require.Equal(t, tc.want, roles)
		})
	}
}
