package browserauth

// What a FORM login leaves behind, driven over real HTTP and a real PostgreSQL.
//
// # Why this test is at the HTTP boundary and not below it
//
// TestFormLifecycleAcrossRealHTTPAndApplicationBoundaries already drives these
// two requests, but against componentProvisioner — a stub that returns a user
// id and writes nothing. It therefore proves the redirect and the cookie
// rotation and says nothing at all about the rows a login owes. Those rows
// were the defect (#620): the Form plane, which is the ONLY browser plane a
// deployment without single sign-on has, issued no actor personal access
// token, so a form-only install signed a person in who then could not complete
// a chat turn. The cure was hand-written SQL in
// deploy/scripts/standalone-stack.sh.
//
// So this composes the production provisioner — identityrepo.PostgresRepository
// behind identity.ProvisionService, exactly as internal/authcomposition
// NewFormGraph does — and asserts the ROWS, then asks the runtime's own issuer
// whether stage `actor_pat_issuance` would now pass.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	browserapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/browserauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/identity"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browserflow"
	sessionstate "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/session"
	dbschema "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/schema"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/authsvc"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/identityrepo"
)

// formLoginPATSigningKey is deployment material in production. Its only job
// here is to let LocalIssuer sign; nothing verifies the signature.
var formLoginPATSigningKey = []byte("form-login-integration-test-signing-key")

// The whole of what a first Form login owes a fresh install.
func TestAFormLoginLeavesTheActorPATAndTheInitialAdminGrant(t *testing.T) {
	ctx := context.Background()
	pool := newFormGrantsPool(t)
	handler, sessions := newFormLoginHandler(t, pool, []string{"admin"})

	userID := completeFormLogin(t, handler, sessions, "admin", "highly-sensitive-password")

	// 1. The grant. `initial_global_admins` is the only way a fresh install
	//    makes its first administrator without INSERT INTO auth_core__user_role.
	if roles := administrationRoleNames(t, ctx, pool, userID); len(roles) != 1 ||
		roles[0] != identity.InitialAdministrationRole {
		t.Fatalf("administration roles = %v, want [%s]", roles, identity.InitialAdministrationRole)
	}

	// 2. The credential. Assert the ROW, not the absence of an error.
	tokens := ownedTokenRows(t, ctx, pool, userID)
	if len(tokens) != 1 {
		t.Fatalf("tokens = %d, want 1: a form-provisioned user was left with none", len(tokens))
	}
	if tokens[0].name != identityrepo.ActorPATName {
		t.Fatalf("token name = %q, want %q", tokens[0].name, identityrepo.ActorPATName)
	}
	if tokens[0].expires {
		t.Fatal("the auto-issued token must not expire")
	}

	// 3. The stage that used to fail. LocalIssuer.IssueToken is exactly what
	//    internal/infra/storage.index_runtime_context calls for a user-initiated
	//    execution; its failure is reported as
	//    runtimeContextUnavailable("actor_pat_issuance"). A token back from it
	//    is the execution being admitted, with no seeded SQL anywhere.
	issued, err := authsvc.NewLocalIssuerBytes(pool, formLoginPATSigningKey).
		IssueToken(ctx, int64(userID))
	if err != nil {
		t.Fatalf("actor PAT issuance: %v", err)
	}
	if issued == "" {
		t.Fatal("actor PAT issuance returned an empty bearer")
	}
}

// The grant is a named list. Everybody else signs in as themselves — and still
// gets the credential, because the credential is not a privilege.
func TestAnUnlistedFormLoginGetsTheActorPATButNoAdministrationRole(t *testing.T) {
	ctx := context.Background()
	pool := newFormGrantsPool(t)
	handler, sessions := newFormLoginHandler(t, pool, []string{"somebody-else"})

	userID := completeFormLogin(t, handler, sessions, "admin", "highly-sensitive-password")

	if roles := administrationRoleNames(t, ctx, pool, userID); len(roles) != 0 {
		t.Fatalf("administration roles = %v, want none", roles)
	}
	if tokens := ownedTokenRows(t, ctx, pool, userID); len(tokens) != 1 {
		t.Fatalf("tokens = %d, want 1", len(tokens))
	}
}

// Every login runs the same provisioning. It must not accumulate a key per
// login, and a second sign-in must not re-promote a demoted account either.
func TestRepeatedFormLoginsWriteNothingNew(t *testing.T) {
	ctx := context.Background()
	pool := newFormGrantsPool(t)
	handler, sessions := newFormLoginHandler(t, pool, []string{"admin"})

	first := completeFormLogin(t, handler, sessions, "admin", "highly-sensitive-password")
	second := completeFormLogin(t, handler, sessions, "admin", "highly-sensitive-password")
	if first != second {
		t.Fatalf("user ids = %d and %d", first, second)
	}
	if tokens := ownedTokenRows(t, ctx, pool, first); len(tokens) != 1 {
		t.Fatalf("tokens = %d, want 1", len(tokens))
	}
	if roles := administrationRoleNames(t, ctx, pool, first); len(roles) != 1 {
		t.Fatalf("administration roles = %v, want one", roles)
	}
}

/* ── the two requests a browser makes ──────────────────────────────────── */

// completeFormLogin performs the real pair: GET /forward-auth/login, then POST
// /forward-auth/auth_form/authorize with the cookie and transaction the first
// response handed out. It returns the user id the rotated session carries, so
// the id under test is the one the login itself authenticated.
func completeFormLogin(
	t *testing.T,
	handler *Handler,
	sessions *componentSessionStore,
	login string,
	password string,
) int64 {
	t.Helper()

	beginRecorder := httptest.NewRecorder()
	mount(handler).ServeHTTP(beginRecorder, httptest.NewRequest(
		http.MethodGet, BasePath+LoginPath+"?target_to=%2Fafter", nil))
	if beginRecorder.Code != http.StatusFound {
		t.Fatalf("begin status = %d, want %d", beginRecorder.Code, http.StatusFound)
	}
	beginCookies := beginRecorder.Result().Cookies()
	if len(beginCookies) != 1 {
		t.Fatalf("begin cookies = %d, want 1", len(beginCookies))
	}
	location, err := url.Parse(beginRecorder.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	transactionID := location.Query().Get("target_to")
	if browserflow.ValidateTransactionID(transactionID) != nil {
		t.Fatalf("transaction ID = %q", transactionID)
	}

	body := url.Values{
		"target": {transactionID}, "login": {login}, "password": {password},
	}.Encode()
	authorizeRequest := httptest.NewRequest(
		http.MethodPost, BasePath+FormAuthorizePath, strings.NewReader(body))
	authorizeRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	authorizeRequest.AddCookie(beginCookies[0])
	authorizeRecorder := httptest.NewRecorder()
	mount(handler).ServeHTTP(authorizeRecorder, authorizeRequest)

	if authorizeRecorder.Code != http.StatusFound {
		t.Fatalf("authorize status = %d, want %d (body %q)",
			authorizeRecorder.Code, http.StatusFound, authorizeRecorder.Body.String())
	}
	if target := authorizeRecorder.Header().Get("Location"); target != "/after" {
		t.Fatalf("location = %q, want /after", target)
	}
	authorized := authorizeRecorder.Result().Cookies()
	if len(authorized) != 1 {
		t.Fatalf("authorize cookies = %d, want 1", len(authorized))
	}
	rotated, found := strings.CutPrefix(authorized[0].Value, CookieValuePrefix)
	if !found {
		t.Fatalf("rotated cookie = %q", authorized[0].Value)
	}
	state, err := sessions.Read(context.Background(), rotated)
	if err != nil {
		t.Fatal(err)
	}
	if state.UserID == nil || *state.UserID <= 0 {
		t.Fatalf("authenticated session carries user id %v", state.UserID)
	}
	return *state.UserID
}

/* ── production composition, with the real provisioner ─────────────────── */

func newFormLoginHandler(
	t *testing.T,
	pool *pgxpool.Pool,
	initialGlobalAdmins []string,
) (*Handler, *componentSessionStore) {
	t.Helper()

	repository, err := identityrepo.NewPostgresRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	provisioner, err := identity.NewProvisionService(repository, identity.ProvisioningPolicy{
		InitialGlobalAdmins: initialGlobalAdmins,
	})
	if err != nil {
		t.Fatal(err)
	}
	sessions := &componentSessionStore{records: make(map[string]sessionstate.State)}
	flow, err := browserapp.NewService(
		sessions,
		&componentTransactionStore{records: make(map[string]browserflow.Transaction)},
		provisioner,
		authsvc.NewPrincipalValidator(pool),
		5*time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := browserapp.NewFormProvider(
		[]byte(`{"users":[{"login":"admin","password":"highly-sensitive-password"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	cookies, err := NewCookiePolicy(CookieConfig{
		Name: "centry_auth_session", Secure: true,
		SameSite: http.SameSiteLaxMode, Lifetime: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	events := make([]string, 0, 4)
	handler, err := NewHandler(
		flow,
		provider,
		&attemptAdmitterStub{events: &events},
		&clientKeyResolverStub{events: &events, key: "client-7"},
		cookies,
		Config{DefaultLoginTarget: "/", DefaultLogoutTarget: "/"},
	)
	if err != nil {
		t.Fatal(err)
	}
	return handler, sessions
}

/* ── reading the rows back ─────────────────────────────────────────────── */

func administrationRoleNames(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, userID int64,
) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `
SELECT role.name
FROM public.auth_core__user_role AS assignment
JOIN public.auth_core__role AS role ON role.id = assignment.role_id
WHERE assignment.user_id = $1 AND role.mode = $2
ORDER BY role.name`, userID, identity.InitialAdministrationMode)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

type ownedTokenRow struct {
	name    string
	expires bool
}

func ownedTokenRows(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, userID int64,
) []ownedTokenRow {
	t.Helper()
	rows, err := pool.Query(ctx, `
SELECT COALESCE(name, ''), expires IS NOT NULL
FROM public.auth_core__token WHERE user_id = $1 ORDER BY id`, userID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var tokens []ownedTokenRow
	for rows.Next() {
		var token ownedTokenRow
		if err := rows.Scan(&token.name, &token.expires); err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, token)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return tokens
}

/* ── database bootstrap ────────────────────────────────────────────────── */

// newFormGrantsPool builds one isolated database per test, in the shape
// internal/db/schema/auth_core_baseline.sql creates, with the root group and
// the administration roles a real deployment's migration seeds.
func newFormGrantsPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := os.Getenv(environment)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		t.Fatal(err)
	}

	databaseName := fmt.Sprintf("elitea_form_grants_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		t.Fatal(err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated database: %v", err)
		}
		adminPool.Close()
	})

	if _, err := pool.Exec(ctx, dbschema.AuthCoreBaselineSQLCProjection); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__group (id, name) VALUES (1, 'Root');
INSERT INTO public.auth_core__role (name, mode) VALUES
    ('super_admin', 'administration'),
    ('admin', 'administration'),
    ('user', 'administration'),
    ('viewer', 'default');`); err != nil {
		t.Fatal(err)
	}
	return pool
}
