package legacyrbac_test

// The credential decides the administration set (ADR-0059 decision 7, the
// credentials table; story R1.2a, issue 2506), against the REAL resolver and
// the token rows shared 0265 shaped:
//
//   - a personal access token by its admin_access: `none` resolves nothing,
//     `inherit` the owner's set, `scoped` the owner's CURRENT set intersected
//     with admin_scopes, so a scope the owner loses is gone at the next request;
//   - a token whose owner held nothing at migration time (`none`) resolves
//     nothing after the owner is given `admin` (the second half of R1.1's
//     criterion);
//   - a run token of an administrator resolves nothing, also when it arrives in
//     X-API-Key, through the real authentication middleware;
//   - a native device session keeps only the registry's native_client names;
//   - a project system token resolves nothing;
//   - the developer mode does not read the credential.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/runtoken"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
)

// The fixture's administration grants. The admin role holds both in-application
// names a native session keeps and one name it does not.
var credentialFixtureAdminSet = []string{
	adminaccess.AdminAuthUsers,
	adminaccess.RuntimeAdminPublishedAgents,
	adminaccess.RuntimePlugins,
}

const credentialFixtureDeveloperPermission = "credential.fixture.developer"

type credentialFixture struct {
	pool     *pgxpool.Pool
	resolver *legacyrbac.PostgresResolver
}

// newCredentialFixture seeds user 1 (holds `admin` and a developer role) and
// user 2 (holds nothing).
func newCredentialFixture(t *testing.T) credentialFixture {
	t.Helper()
	pool := newGrantPool(t)
	ctx := context.Background()
	statements := []string{
		`INSERT INTO public.auth_core__user (id, email, name) VALUES
			(1, 'ada@example.com', 'Ada'), (2, 'plain@example.com', 'Plain'),
			(3, 'system_user_7@centry.user', 'System')`,
		`INSERT INTO public.auth_core__role (id, name, mode) VALUES
			(5, 'admin', 'administration'), (6, 'editor', 'developer')`,
		`INSERT INTO public.auth_core__user_role (user_id, role_id) VALUES (1, 5), (1, 6), (3, 5)`,
		`INSERT INTO public.auth_core__role_permission (role_id, permission) VALUES
			(6, '` + credentialFixtureDeveloperPermission + `')`,
	}
	for _, name := range credentialFixtureAdminSet {
		statements = append(statements,
			`INSERT INTO public.auth_core__role_permission (role_id, permission) VALUES (5, '`+name+`')`)
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	return credentialFixture{pool: pool, resolver: legacyrbac.NewPostgresResolver(pool)}
}

// token inserts a token row of owner with access and scopes and returns its id.
func (f credentialFixture) token(t *testing.T, owner int, access string, scopes ...string) string {
	t.Helper()
	if scopes == nil {
		scopes = []string{}
	}
	var id int
	if err := f.pool.QueryRow(context.Background(), `
INSERT INTO public.auth_core__token (uuid, user_id, name, admin_access, admin_scopes)
VALUES (gen_random_uuid()::text, $1, 'credential-kind', $2, $3) RETURNING id`,
		owner, access, scopes).Scan(&id); err != nil {
		t.Fatalf("seed a %s token of user %d: %v", access, owner, err)
	}
	return strconv.Itoa(id)
}

func credentialPrincipal(kind auth.CredentialKind, owner int, tokenID string) auth.User {
	id := strconv.Itoa(owner)
	authType := "token"
	if tokenID == "" {
		authType = "session"
	}
	return auth.User{ID: id, UserID: id, TokenID: tokenID, AuthType: authType, CredentialKind: kind}
}

func (f credentialFixture) administration(t *testing.T, principal auth.User) auth.PermissionResolution {
	t.Helper()
	resolution, err := f.resolver.ResolvePermissions(
		context.Background(), principal, auth.PermissionModeAdministration, "")
	if err != nil {
		t.Fatalf("resolve the administration set of %+v: %v", principal, err)
	}
	return resolution
}

func wantSet(t *testing.T, what string, got, want []string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%s resolves %v, want %v", what, got, want)
	}
}

func TestAPersonalAccessTokenResolvesByItsAdminAccess(t *testing.T) {
	f := newCredentialFixture(t)
	session := f.administration(t, credentialPrincipal(auth.CredentialKindSession, 1, ""))
	wantSet(t, "the owner's browser session", session.Permissions, credentialFixtureAdminSet)

	none := f.token(t, 1, legacyrbac.TokenAdminAccessNone)
	wantSet(t, "a none token of an admin",
		f.administration(t, credentialPrincipal(auth.CredentialKindToken, 1, none)).Permissions, nil)

	inherit := f.token(t, 1, legacyrbac.TokenAdminAccessInherit)
	wantSet(t, "an inherit token",
		f.administration(t, credentialPrincipal(auth.CredentialKindToken, 1, inherit)).Permissions,
		credentialFixtureAdminSet)

	// A scope the owner does not hold is never resolved.
	scoped := f.token(t, 1, legacyrbac.TokenAdminAccessScoped, adminaccess.AdminAuthUsers, "not.held.by.owner")
	wantSet(t, "a scoped token",
		f.administration(t, credentialPrincipal(auth.CredentialKindToken, 1, scoped)).Permissions,
		[]string{adminaccess.AdminAuthUsers})
}

// The intersection is taken at request time: no write to the token row is
// needed for it to lose what its owner lost.
func TestAScopedTokenLosesAScopeItsOwnerLoses(t *testing.T) {
	f := newCredentialFixture(t)
	scoped := f.token(t, 1, legacyrbac.TokenAdminAccessScoped, adminaccess.AdminAuthUsers, adminaccess.RuntimePlugins)
	principal := credentialPrincipal(auth.CredentialKindToken, 1, scoped)
	wantSet(t, "the scoped token before", f.administration(t, principal).Permissions,
		[]string{adminaccess.AdminAuthUsers, adminaccess.RuntimePlugins})

	if _, err := f.pool.Exec(context.Background(),
		`DELETE FROM public.auth_core__user_role WHERE user_id = 1 AND role_id = 5`); err != nil {
		t.Fatalf("take the admin role away: %v", err)
	}
	wantSet(t, "the scoped token after its owner lost admin", f.administration(t, principal).Permissions, nil)
}

// R1.1's backfill gave `inherit` only to tokens of owners who held an
// administration permission then. A `none` token stays empty when its owner
// is given `admin` later.
func TestANoneTokenGainsNothingWhenItsOwnerBecomesAdmin(t *testing.T) {
	f := newCredentialFixture(t)
	token := f.token(t, 2, legacyrbac.TokenAdminAccessNone)
	principal := credentialPrincipal(auth.CredentialKindToken, 2, token)
	wantSet(t, "the plain user's token", f.administration(t, principal).Permissions, nil)

	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO public.auth_core__user_role (user_id, role_id) VALUES (2, 5)`); err != nil {
		t.Fatalf("give the owner admin: %v", err)
	}
	wantSet(t, "the owner's session after it became admin",
		f.administration(t, credentialPrincipal(auth.CredentialKindSession, 2, "")).Permissions,
		credentialFixtureAdminSet)
	wantSet(t, "the plain user's token after its owner became admin",
		f.administration(t, principal).Permissions, nil)
}

func TestANativeSessionKeepsOnlyTheNativeClientNames(t *testing.T) {
	f := newCredentialFixture(t)
	// The device anchor: a token row with no uuid (ADR-0025), `none` as 0265
	// leaves it. The native rule does not read admin_access.
	var anchor int
	if err := f.pool.QueryRow(context.Background(), `
INSERT INTO public.auth_core__token (user_id, name) VALUES (1, 'device') RETURNING id`).Scan(&anchor); err != nil {
		t.Fatalf("seed the device anchor: %v", err)
	}
	principal := credentialPrincipal(auth.CredentialKindNative, 1, strconv.Itoa(anchor))
	principal.NativeClientID = "desktop"
	wantSet(t, "a native session of an admin", f.administration(t, principal).Permissions,
		[]string{adminaccess.RuntimeAdminPublishedAgents, adminaccess.RuntimePlugins})
}

func TestARunTokenAndASystemTokenResolveNothing(t *testing.T) {
	f := newCredentialFixture(t)
	anchor := f.token(t, 1, legacyrbac.TokenAdminAccessInherit)
	wantSet(t, "a run token of an admin (its anchor is inherit)",
		f.administration(t, credentialPrincipal(auth.CredentialKindRun, 1, anchor)).Permissions, nil)

	system := f.token(t, 3, legacyrbac.TokenAdminAccessInherit)
	wantSet(t, "a project system token whose identity holds admin",
		f.administration(t, credentialPrincipal(auth.CredentialKindSystem, 3, system)).Permissions, nil)
}

func TestTheZeroKindResolvesNothingAndTheDeveloperModeIsUnchanged(t *testing.T) {
	f := newCredentialFixture(t)
	wantSet(t, "a principal no authenticating site built",
		f.administration(t, auth.User{ID: "1", UserID: "1"}).Permissions, nil)

	none := f.token(t, 1, legacyrbac.TokenAdminAccessNone)
	for _, principal := range []auth.User{
		{ID: "1", UserID: "1"},
		credentialPrincipal(auth.CredentialKindSession, 1, ""),
		credentialPrincipal(auth.CredentialKindToken, 1, none),
		credentialPrincipal(auth.CredentialKindRun, 1, none),
	} {
		resolution, err := f.resolver.ResolvePermissions(
			context.Background(), principal, auth.PermissionModeDeveloper, "")
		if err != nil {
			t.Fatal(err)
		}
		wantSet(t, "the developer mode of "+string(principal.CredentialKind),
			resolution.Permissions, []string{credentialFixtureDeveloperPermission})
	}
}

func TestOnlyABrowserSessionWithARowCanHoldAnElevation(t *testing.T) {
	f := newCredentialFixture(t)
	withRow := credentialPrincipal(auth.CredentialKindSession, 1, "")
	withRow.BrowserSessionRef = 41
	if got := f.administration(t, withRow).ElevationSessionRef; got != 41 {
		t.Fatalf("a server-side session exposes elevation ref %d, want 41", got)
	}
	form := credentialPrincipal(auth.CredentialKindForm, 1, "")
	form.BrowserSessionRef = 42
	if got := f.administration(t, form).ElevationSessionRef; got != 42 {
		t.Fatalf("a Form session exposes elevation ref %d, want 42", got)
	}
	legacy := credentialPrincipal(auth.CredentialKindSession, 1, "")
	if got := f.administration(t, legacy).ElevationSessionRef; got != 0 {
		t.Fatalf("a session without a row exposes elevation ref %d, want 0", got)
	}
	inherit := credentialPrincipal(auth.CredentialKindToken, 1, f.token(t, 1, legacyrbac.TokenAdminAccessInherit))
	inherit.BrowserSessionRef = 43
	if got := f.administration(t, inherit).ElevationSessionRef; got != 0 {
		t.Fatalf("a token exposes elevation ref %d, want 0", got)
	}
}

// runTokenOf answers every run token with the actor's anchor token row, the
// principal shape runtoken.Validator returns.
type runTokenOf struct{ anchor string }

func (v runTokenOf) ValidateRunToken(context.Context, string, string) (auth.User, runtoken.Binding, error) {
	return auth.User{ID: "1", UserID: "1", TokenID: v.anchor, AuthType: "token",
			CredentialKind: auth.CredentialKindRun},
		runtoken.Binding{ProjectID: 1, UserID: 1}, nil
}

type passThroughPrincipals struct{}

func (passThroughPrincipals) ValidatePrincipal(_ context.Context, user auth.User) (auth.User, error) {
	// Rebuild the principal the way a validator that reads rows does, so the
	// kind must survive validation by the middleware, not by this stub.
	user.CredentialKind = auth.CredentialKindNone
	return user, nil
}

// A run token of an admin resolves nothing whichever header carries it: the
// middleware marks the principal `run` after validation.
func TestARunTokenInXAPIKeyResolvesNothing(t *testing.T) {
	f := newCredentialFixture(t)
	anchor := f.token(t, 1, legacyrbac.TokenAdminAccessInherit)
	var seen []auth.PermissionResolution
	handler := apimw.Auth(apimw.AuthConfig{
		PrincipalValidator: passThroughPrincipals{},
		RunTokens:          &apimw.RunTokenScope{Audience: "fixture", Validator: runTokenOf{anchor: anchor}},
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, _ := auth.UserFromContext(r.Context())
		if principal.CredentialKind != auth.CredentialKindRun {
			t.Errorf("the run token reached the handler as kind %q", principal.CredentialKind)
		}
		seen = append(seen, f.administration(t, principal))
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, header := range [][2]string{
		{"X-API-Key", runtoken.Prefix + "fixture"},
		{"Authorization", "Bearer " + runtoken.Prefix + "fixture"},
	} {
		request := httptest.NewRequest(http.MethodGet, "/fixture", nil)
		request.Header.Set(header[0], header[1])
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("%s: status %d (%s)", header[0], response.Code, response.Body.String())
		}
	}
	if len(seen) != 2 {
		t.Fatalf("the handler ran %d times, want 2", len(seen))
	}
	for _, resolution := range seen {
		wantSet(t, "a run token of an admin", resolution.Permissions, nil)
	}
}
