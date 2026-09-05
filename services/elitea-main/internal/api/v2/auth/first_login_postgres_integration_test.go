package auth

// What a first single-sign-on login must leave behind, against a real
// PostgreSQL.
//
// # Why these need a database
//
// Both defects these tests pin are ROWS THAT WERE NEVER WRITTEN, and absence is
// exactly what a scripted transaction cannot see: every scripted provisioning
// test went on passing while an SSO-only deployment could produce neither an
// administrator nor a chat turn.
//
//   - The administration role. `initial_global_admins` reached only the
//     browserauth plane, and internal/api/production_router.go does not mount
//     that plane when single sign-on is configured. The mounted plane is this
//     one, and it assigned no role at all, so the first administrator could only
//     be made with INSERT INTO auth_core__user_role.
//   - The actor personal access token. internal/infra/authsvc.LocalIssuer
//     re-signs an existing row and never creates one, so a person who had just
//     signed in had no PAT, and their first chat turn failed as
//     runtimeContextUnavailable(runtimeContextStageActorPATIssuance) — an error
//     naming a database stage for what was really missing setup.
//
// Assert the ROWS. A login that merely returns no error proves nothing here.

import (
	"context"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/identity"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/identityrepo"
)

/* ── the administration role ───────────────────────────────────────────── */

// The bare spelling, which is what the OTHER provisioning plane matches and
// therefore what an operator who has read the configuration reference writes.
func TestABareInitialAdminEntryGrantsTheRoleOnTheMountedOIDCPlane(t *testing.T) {
	pool := newFirstLoginPool(t)
	handler := (&OIDCHandler{pool: pool}).WithFirstLoginPolicy(
		FirstLoginPolicy{InitialGlobalAdmins: []string{"alice-sub"}})

	userID := signInWithOIDC(t, handler, "alice-sub", "alice@corp.com")

	require.Equal(t, []string{identity.InitialAdministrationRole},
		administrationRoles(t, pool, userID),
		"the configured initial global administrator was provisioned with no role")
}

// The prefixed spelling, which is what this plane actually stores. An operator
// reading auth_core__user_provider to find the reference sees `oidc:<sub>` and
// will paste that.
func TestAPrefixedInitialAdminEntryGrantsTheRoleToo(t *testing.T) {
	pool := newFirstLoginPool(t)
	handler := (&OIDCHandler{pool: pool}).WithFirstLoginPolicy(
		FirstLoginPolicy{InitialGlobalAdmins: []string{OIDCProviderRefPrefix + "alice-sub"}})

	userID := signInWithOIDC(t, handler, "alice-sub", "alice@corp.com")

	require.Equal(t, []string{identity.InitialAdministrationRole},
		administrationRoles(t, pool, userID))
}

func TestTheSAMLPlaneGrantsTheRoleInBothSpellings(t *testing.T) {
	for name, entry := range map[string]string{
		"bare":     "alice-nameid",
		"prefixed": SAMLProviderRefPrefix + "alice-nameid",
	} {
		t.Run(name, func(t *testing.T) {
			pool := newFirstLoginPool(t)
			handler := (&SAMLHandler{pool: pool}).WithFirstLoginPolicy(
				FirstLoginPolicy{InitialGlobalAdmins: []string{entry}})

			userID := signInWithSAML(t, handler, "alice-nameid", "alice@corp.com")

			require.Equal(t, []string{identity.InitialAdministrationRole},
				administrationRoles(t, pool, userID))
		})
	}
}

// Everybody else signs in as themselves. The grant is a named list, not a
// welcome gift.
func TestAnUnlistedSubjectGetsNoAdministrationRole(t *testing.T) {
	pool := newFirstLoginPool(t)
	handler := (&OIDCHandler{pool: pool}).WithFirstLoginPolicy(
		FirstLoginPolicy{InitialGlobalAdmins: []string{"alice-sub"}})

	userID := signInWithOIDC(t, handler, "mallory-sub", "mallory@corp.com")

	require.Empty(t, administrationRoles(t, pool, userID))
}

// A person the operator has since DEMOTED must stay demoted. The grant is
// one-shot per account: it looks at whether the account holds any
// administration-mode role at all, not at whether it holds this one.
func TestADemotedInitialAdminIsNotRepromotedByTheNextLogin(t *testing.T) {
	pool := newFirstLoginPool(t)
	handler := (&OIDCHandler{pool: pool}).WithFirstLoginPolicy(
		FirstLoginPolicy{InitialGlobalAdmins: []string{"alice-sub"}})
	ctx := context.Background()

	userID := signInWithOIDC(t, handler, "alice-sub", "alice@corp.com")
	_, err := pool.Exec(ctx, `DELETE FROM auth_core__user_role WHERE user_id = $1`, userID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO auth_core__user_role (user_id, role_id)
		 SELECT $1, id FROM auth_core__role WHERE name = 'user' AND mode = 'administration'`,
		userID)
	require.NoError(t, err)

	require.Equal(t, userID, signInWithOIDC(t, handler, "alice-sub", "alice@corp.com"))
	require.Equal(t, []string{"user"}, administrationRoles(t, pool, userID))
}

/* ── the `email:` spelling ─────────────────────────────────────────────── */

// The shape that makes a fresh Azure AD or Okta deployment administrable at
// all. The OIDC subject there is an opaque identifier nobody knows before the
// first sign-in, so a reference entry cannot be written in advance. The address
// can.
func TestAnEmailEntryGrantsTheRoleToAVerifiedAddress(t *testing.T) {
	pool := newFirstLoginPool(t)
	handler := (&OIDCHandler{pool: pool}).WithFirstLoginPolicy(
		FirstLoginPolicy{InitialGlobalAdmins: []string{"email:Alice@Corp.com"}})

	userID := signInWithVerifiedOIDC(t, handler,
		"00000000-1111-2222-3333-444444444444", "alice@corp.com", true)

	require.Equal(t, []string{identity.InitialAdministrationRole},
		administrationRoles(t, pool, userID),
		"a verified address named by an email: entry was provisioned with no role")
}

// THE ROW THAT MUST NOT EXIST. Without a stated verification the address is a
// claim, and an identity provider that can assert an address must not be able
// to assert its way into the administration role.
func TestAnEmailEntryGrantsNothingWithoutAStatedVerification(t *testing.T) {
	unverified := false
	for name, emailVerified := range map[string]*bool{
		// The common shape: many identity providers never send the claim.
		"the claim is absent": nil,
		// Callback refuses this login before provisioning, so this case is
		// defence in depth on the layer below it.
		"the claim states false": &unverified,
	} {
		t.Run(name, func(t *testing.T) {
			pool := newFirstLoginPool(t)
			handler := (&OIDCHandler{pool: pool}).WithFirstLoginPolicy(
				FirstLoginPolicy{InitialGlobalAdmins: []string{"email:alice@corp.com"}})

			id, err := handler.provisionUser(context.Background(),
				"mallory-sub", "alice@corp.com", "Mallory", emailVerified, false)
			require.NoError(t, err)

			require.Empty(t, administrationRoles(t, pool, atoiUserID(t, id)))
		})
	}
}

// The SAML plane states nothing about the address it carries, so an `email:`
// entry can never name a SAML login. The operator uses `saml:<nameid>`.
func TestAnEmailEntryGrantsNothingOnTheSAMLPlane(t *testing.T) {
	pool := newFirstLoginPool(t)
	handler := (&SAMLHandler{pool: pool}).WithFirstLoginPolicy(
		FirstLoginPolicy{InitialGlobalAdmins: []string{"email:alice@corp.com"}})

	userID := signInWithSAML(t, handler, "alice-nameid", "alice@corp.com")

	require.Empty(t, administrationRoles(t, pool, userID))
}

/* ── the actor personal access token ───────────────────────────────────── */

// The token the agent runtime signs its calls with. Without this row the
// person's first chat turn fails inside the runtime.
func TestAFirstLoginIssuesAVisibleRevocableActorPAT(t *testing.T) {
	pool := newFirstLoginPool(t)
	handler := &OIDCHandler{pool: pool}

	userID := signInWithOIDC(t, handler, "alice-sub", "alice@corp.com")

	tokens := ownedTokens(t, pool, userID)
	require.Len(t, tokens, 1, "a signed-in user was left with no personal access token")
	require.Equal(t, identityrepo.ActorPATName, tokens[0].name,
		"the auto-issued token must be distinguishable from a key the user made")
	require.False(t, tokens[0].expires,
		"the auto-issued token must not expire; expiry breaks chat with a database-stage error")
	require.NotEmpty(t, tokens[0].uuid, "the bearer is signed from the uuid")

	// And it is listed on /settings/tokens, so its owner can see and revoke it.
	// ListOwnedPATs is the query that screen reads.
	listed, err := newPostgresTokenRepository(pool).List(context.Background(), int64(userID))
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.NotNil(t, listed[0].Name)
	require.Equal(t, identityrepo.ActorPATName, *listed[0].Name)
}

// The SAML plane is the same provisioning, so it owes the same credential.
func TestASAMLFirstLoginAlsoIssuesTheActorPAT(t *testing.T) {
	pool := newFirstLoginPool(t)
	handler := &SAMLHandler{pool: pool}

	userID := signInWithSAML(t, handler, "alice-nameid", "alice@corp.com")

	require.Len(t, ownedTokens(t, pool, userID), 1)
}

// Every login runs the same code. It must not accumulate a key per login.
func TestRepeatedLoginsIssueOnlyOneActorPAT(t *testing.T) {
	pool := newFirstLoginPool(t)
	handler := &OIDCHandler{pool: pool}

	userID := signInWithOIDC(t, handler, "alice-sub", "alice@corp.com")
	require.Equal(t, userID, signInWithOIDC(t, handler, "alice-sub", "alice@corp.com"))
	require.Equal(t, userID, signInWithOIDC(t, handler, "alice-sub", "alice@corp.com"))

	require.Len(t, ownedTokens(t, pool, userID), 1)
}

// A person who already has a working key gets nothing extra: the condition is
// "no active PAT", read through the query the issuer itself reads.
func TestAnAccountThatAlreadyHasAPATIsLeftAlone(t *testing.T) {
	pool := newFirstLoginPool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx,
		`INSERT INTO auth_core__user (email, name) VALUES ('alice@corp.com', 'Alice')`)
	require.NoError(t, err)
	var existingID int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id FROM auth_core__user WHERE email = 'alice@corp.com'`).Scan(&existingID))
	_, err = pool.Exec(ctx,
		`INSERT INTO auth_core__token (uuid, user_id, name) VALUES ('own-key-uuid', $1, 'my laptop')`,
		existingID)
	require.NoError(t, err)

	userID := signInWithOIDC(t, &OIDCHandler{pool: pool}, "alice-sub", "alice@corp.com")
	require.Equal(t, existingID, userID)

	tokens := ownedTokens(t, pool, userID)
	require.Len(t, tokens, 1)
	require.Equal(t, "my laptop", tokens[0].name)
}

// Revoking every key must not leave the account permanently unable to chat.
func TestRevokingEveryKeyIsHealedByTheNextLogin(t *testing.T) {
	pool := newFirstLoginPool(t)
	handler := &OIDCHandler{pool: pool}
	ctx := context.Background()

	userID := signInWithOIDC(t, handler, "alice-sub", "alice@corp.com")
	_, err := pool.Exec(ctx, `DELETE FROM auth_core__token WHERE user_id = $1`, userID)
	require.NoError(t, err)

	require.Equal(t, userID, signInWithOIDC(t, handler, "alice-sub", "alice@corp.com"))
	require.Len(t, ownedTokens(t, pool, userID), 1)
}

/* ── helpers ───────────────────────────────────────────────────────────── */

func signInWithOIDC(t *testing.T, handler *OIDCHandler, sub, email string) int {
	t.Helper()
	id, err := handler.provisionUser(context.Background(), sub, email, "Alice", nil, false)
	require.NoError(t, err)
	return atoiUserID(t, id)
}

// signInWithVerifiedOIDC is signInWithOIDC with the `email_verified` claim the
// identity provider sent. Only an explicit `true` opens the `email:` spelling
// of `initial_global_admins`.
func signInWithVerifiedOIDC(
	t *testing.T, handler *OIDCHandler, sub, email string, emailVerified bool,
) int {
	t.Helper()
	id, err := handler.provisionUser(
		context.Background(), sub, email, "Alice", &emailVerified, false)
	require.NoError(t, err)
	return atoiUserID(t, id)
}

func signInWithSAML(t *testing.T, handler *SAMLHandler, nameID, email string) int {
	t.Helper()
	id, err := handler.provisionUser(context.Background(), nameID, email, "Alice")
	require.NoError(t, err)
	return atoiUserID(t, id)
}

func atoiUserID(t *testing.T, id string) int {
	t.Helper()
	userID, err := strconv.Atoi(id)
	require.NoError(t, err)
	require.Positive(t, userID)
	return userID
}

// administrationRoles is what the administration mode says this account is.
func administrationRoles(t *testing.T, pool *pgxpool.Pool, userID int) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT role.name
		 FROM auth_core__user_role AS assignment
		 JOIN auth_core__role AS role ON role.id = assignment.role_id
		 WHERE assignment.user_id = $1 AND role.mode = $2
		 ORDER BY role.name`,
		userID, identity.InitialAdministrationMode)
	require.NoError(t, err)
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		names = append(names, name)
	}
	require.NoError(t, rows.Err())
	return names
}

type ownedToken struct {
	name    string
	uuid    string
	expires bool
}

func ownedTokens(t *testing.T, pool *pgxpool.Pool, userID int) []ownedToken {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT COALESCE(name, ''), COALESCE(uuid, ''), expires IS NOT NULL
		 FROM auth_core__token WHERE user_id = $1 ORDER BY id`, userID)
	require.NoError(t, err)
	defer rows.Close()

	var tokens []ownedToken
	for rows.Next() {
		var token ownedToken
		require.NoError(t, rows.Scan(&token.name, &token.uuid, &token.expires))
		tokens = append(tokens, token)
	}
	require.NoError(t, rows.Err())
	return tokens
}

/* ── database bootstrap ────────────────────────────────────────────────── */

// newFirstLoginPool extends the provisioning fixture with the three tables the
// grants write, in the shape internal/db/schema/auth_core_baseline.sql creates
// them, and seeds the roles a real deployment's migration seeds.
func newFirstLoginPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := newProvisioningPool(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx, `
		CREATE TABLE auth_core__token (
			id serial PRIMARY KEY,
			uuid varchar(36),
			expires timestamp without time zone,
			user_id integer REFERENCES auth_core__user (id)
				ON UPDATE CASCADE ON DELETE CASCADE,
			name text
		);
		CREATE UNIQUE INDEX ix_auth_core__token_uuid ON auth_core__token (uuid);
		CREATE TABLE auth_core__role (
			id serial PRIMARY KEY,
			name varchar(64) NOT NULL,
			mode varchar(64) NOT NULL,
			UNIQUE (name, mode)
		);
		CREATE TABLE auth_core__user_role (
			id serial PRIMARY KEY,
			user_id integer NOT NULL REFERENCES auth_core__user (id) ON DELETE CASCADE,
			role_id integer NOT NULL REFERENCES auth_core__role (id) ON DELETE CASCADE,
			UNIQUE (user_id, role_id)
		);
		CREATE SCHEMA elitea_identity;
		CREATE TABLE elitea_identity.token_project_binding (
			token_id integer PRIMARY KEY REFERENCES auth_core__token (id) ON DELETE CASCADE,
			project_id integer NOT NULL
		);
		INSERT INTO auth_core__role (name, mode) VALUES
			('super_admin', 'administration'),
			('admin', 'administration'),
			('user', 'administration'),
			('viewer', 'default');`)
	require.NoError(t, err)

	return pool
}
