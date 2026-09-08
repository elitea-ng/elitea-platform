package auth

// EIGHT ACCOUNTS SIGNING IN AT ONCE MUST END WITH EIGHT PERSONAL PROJECTS.
//
// THE DEFECT (issue 843). The provisioner ran one attempt at a time and DROPPED
// every caller beyond that slot, and sign-in only asked. A burst of first
// logins — a team onboarded together, an identity-provider group mapped onto
// one shared project — therefore produced ONE personal project between them.
// The rest were told by `GET /social/author` that the shared project was their
// private one, so nothing ever asked again. It was found with three E2E
// personas signing in in parallel: exactly one got a project, per run.
//
// WHY THIS TEST GOES THROUGH THE REAL CALLBACK. Every layer between the
// identity provider and the provisioner is part of the defect: the callback
// decides which account the login names, sign-in decides whether to ask and
// how long to wait, and the ensurer decides who is served and who is
// discarded. A test that called EnsureStarted eight times would have passed
// against the broken code as easily as against this one — the drop is invisible
// from there, because a dropped caller is simply told "nothing to wait for".
// So this drives the whole path: a fake identity provider serving discovery,
// keys and tokens; the handler's own Login for the state, nonce and PKCE
// cookies; and a real PostgreSQL behind it, because the thing being counted is
// ROWS.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/personalproject"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/dbtest"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

// concurrentSignIns is the burst. Eight is well past the one slot the
// provisioner runs, so every account but one has to be QUEUED to be served at
// all — which is the property under test.
const concurrentSignIns = 8

func TestEightConcurrentFirstSignInsGetEightPersonalProjects(t *testing.T) {
	pool := newSignInProvisioningPool(t)
	provider := newFakeIdentityProvider(t)

	ensurer, err := personalproject.NewEnsurer(pool, newSignInProvisioner(pool))
	require.NoError(t, err)

	handler := provider.handler(t, pool).WithPersonalProjectEnsurer(ensurer)

	// Sent together, as a team onboarded together sends them.
	var logins sync.WaitGroup
	failures := make(chan string, concurrentSignIns)
	begin := make(chan struct{})
	for index := range concurrentSignIns {
		logins.Add(1)
		go func() {
			defer logins.Done()
			<-begin
			email := fmt.Sprintf("burst-%d@autotest.local", index)
			if status, body := provider.signIn(handler, fmt.Sprintf("sub-%d", index), email); status != http.StatusFound {
				// A test goroutine may not call Fatalf.
				failures <- fmt.Sprintf("%s: callback answered %d (%s)", email, status, body)
			}
		}()
	}
	close(begin)
	logins.Wait()
	close(failures)
	for failure := range failures {
		t.Fatal(failure)
	}

	// The sign-in wait is bounded, so the queue may still be draining. Poll for
	// the rows rather than assuming the last login waited for everybody.
	accounts := signedInAccounts(t, pool, concurrentSignIns)
	require.Len(t, accounts, concurrentSignIns,
		"the burst did not create one account per login")

	awaitPersonalProjects(t, pool, accounts)

	// ONE PROJECT PER ACCOUNT, OWNED BY THAT ACCOUNT. Counting projects alone
	// would pass on a run that gave one user eight of them.
	for email, userID := range accounts {
		name := personalproject.Name(userID)
		rows, err := pool.Query(context.Background(),
			`SELECT id, owner_id, create_success FROM centry.project WHERE name = $1`, name)
		require.NoError(t, err)
		var owned int
		for rows.Next() {
			var projectID, ownerID int64
			var created bool
			require.NoError(t, rows.Scan(&projectID, &ownerID, &created))
			require.Equal(t, userID, ownerID, "%s is owned by user %d, not by %s", name, ownerID, email)
			require.True(t, created, "%s was left half-provisioned", name)
			owned++
		}
		require.NoError(t, rows.Err())
		require.Equal(t, 1, owned, "%s exists %d times, want exactly one", name, owned)

		// And it is a project its owner can actually use: the resolver both
		// this endpoint and the `/llm` hop apply is membership-checked, so a
		// project with no role assignment resolves to nothing.
		var member bool
		require.NoError(t, pool.QueryRow(context.Background(), `
			SELECT EXISTS (
			    SELECT 1
			    FROM public.auth_core__project_user_role AS assignment
			    JOIN centry.project AS project ON project.id = assignment.project_id
			    WHERE project.name = $1 AND assignment.user_id = $2
			)`, name, userID).Scan(&member))
		require.True(t, member, "%s has no role assignment, so every reader resolves it to nothing", name)
	}
}

// THE SIGN-IN WAIT IS A BOUND ON THE WAIT, NEVER ON THE LOGIN.
//
// The wait added for issue 843 sits between the identity provider and the
// redirect, and a provisioning run that applies the whole tenant migration
// corpus takes far longer than any login may be held open for. So a slow — or
// permanently stuck — ensurer must cost the bound and no more. Without this,
// the fix for a burst of first logins would be an outage on the login path.
func TestSignInAnswersWithinItsDeadlineWhenProvisioningIsSlow(t *testing.T) {
	pool := newSignInProvisioningPool(t)
	provider := newFakeIdentityProvider(t)

	stuck := &neverFinishingEnsurer{done: make(chan struct{})}
	t.Cleanup(func() { close(stuck.done) })

	const bound = 300 * time.Millisecond
	handler := provider.handler(t, pool).
		WithPersonalProjectEnsurer(stuck).
		WithPersonalProjectWait(bound)

	started := time.Now()
	status, body := provider.signIn(handler, "patient-sub", "patient@autotest.local")
	elapsed := time.Since(started)

	require.Equal(t, http.StatusFound, status,
		"a login that waited for a stuck provisioner did not complete (%s)", body)
	require.True(t, stuck.asked(), "the login did not ask for a personal project at all")
	// Generously above the bound and far below the ten-minute provisioning
	// deadline: what is being measured is that the wait ENDS, not how precisely.
	require.Less(t, elapsed, 30*bound,
		"the login was held for %s against a %s bound, so it waits for the work "+
			"rather than for the bound", elapsed, bound)
	require.GreaterOrEqual(t, elapsed, bound,
		"the login answered before its own bound, so nothing was waited for")
}

// The production bound is a constant, so no deployment can be configured into
// holding its login callback open. Pin the value the doc comment justifies.
func TestTheSignInWaitIsFiveSeconds(t *testing.T) {
	require.Equal(t, 5*time.Second, defaultSignInProvisionWait)
}

/* ── the stuck ensurer ─────────────────────────────────────────────────── */

// neverFinishingEnsurer accepts every caller and finishes none, which is what a
// provisioning run applying a tenant corpus looks like from the login's side.
type neverFinishingEnsurer struct {
	done chan struct{}

	mu    sync.Mutex
	calls int
}

func (e *neverFinishingEnsurer) EnsureAsync(userID int64) { _ = e.EnsureStarted(userID) }

func (e *neverFinishingEnsurer) EnsureStarted(int64) <-chan struct{} {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	return e.done
}

func (e *neverFinishingEnsurer) asked() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls > 0
}

/* ── the fake identity provider ────────────────────────────────────────── */

// fakeIdentityProvider serves the three endpoints a browser OIDC login needs:
// discovery, the signing keys, and the token exchange. It is here rather than
// stubbed at the handler because the callback's own verification — issuer,
// audience, signature, nonce — is part of the path this test is about.
type fakeIdentityProvider struct {
	server   *httptest.Server
	key      *rsa.PrivateKey
	clientID string

	mu    sync.Mutex
	codes map[string]fakeAuthorization
}

// fakeAuthorization is one redeemed authorization code.
type fakeAuthorization struct {
	subject string
	email   string
	nonce   string
}

func newFakeIdentityProvider(t *testing.T) *fakeIdentityProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	provider := &fakeIdentityProvider{
		key:      key,
		clientID: "elitea-test-client",
		codes:    map[string]fakeAuthorization{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		issuer := provider.server.URL
		writeFakeJSON(w, map[string]any{
			"issuer":                                issuer,
			"authorization_endpoint":                issuer + "/authorize",
			"token_endpoint":                        issuer + "/token",
			"jwks_uri":                              issuer + "/jwks",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		public := provider.key.Public().(*rsa.PublicKey)
		writeFakeJSON(w, map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "fake-key",
			"n": base64.RawURLEncoding.EncodeToString(public.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(public.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		provider.mu.Lock()
		authorization, known := provider.codes[r.Form.Get("code")]
		delete(provider.codes, r.Form.Get("code"))
		provider.mu.Unlock()
		if !known {
			http.Error(w, "unknown code", http.StatusBadRequest)
			return
		}
		writeFakeJSON(w, map[string]any{
			"access_token": "fake-access-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
			"id_token":     provider.idToken(authorization),
		})
	})

	provider.server = httptest.NewServer(mux)
	t.Cleanup(provider.server.Close)
	return provider
}

// idToken mints the assertion the callback verifies.
func (p *fakeIdentityProvider) idToken(authorization fakeAuthorization) string {
	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss":            p.server.URL,
		"aud":            p.clientID,
		"sub":            authorization.subject,
		"email":          authorization.email,
		"email_verified": true,
		"name":           authorization.email,
		"nonce":          authorization.nonce,
		"iat":            now.Unix(),
		"exp":            now.Add(time.Hour).Unix(),
	})
	token.Header["kid"] = "fake-key"
	signed, err := token.SignedString(p.key)
	if err != nil {
		panic(err)
	}
	return signed
}

// handler builds an OIDC handler federated to this provider.
func (p *fakeIdentityProvider) handler(t *testing.T, pool *pgxpool.Pool) *OIDCHandler {
	t.Helper()
	handler, err := NewOIDCHandler(context.Background(), &OIDCConfig{
		IssuerURL:   p.server.URL,
		ClientID:    p.clientID,
		RedirectURI: "http://elitea.test/forward-auth/auth_oidc/callback",
	}, pool, "test-secret-key")
	require.NoError(t, err)
	return handler
}

// signIn drives ONE complete browser login: Login for the per-attempt cookies,
// then Callback with the code the provider issues for them.
//
// The cookies are carried by hand because there is no browser here, and they
// are read from Login's own response rather than forged: the state cookie is
// signed with the handler's secret and the nonce is echoed into the id_token,
// so a forged pair would fail the callback's checks for reasons this test is
// not about.
func (p *fakeIdentityProvider) signIn(handler *OIDCHandler, subject, email string) (int, string) {
	loginRecorder := httptest.NewRecorder()
	handler.Login(loginRecorder, httptest.NewRequest(http.MethodGet, "/forward-auth/auth_oidc/login", nil))
	if loginRecorder.Code != http.StatusFound {
		return loginRecorder.Code, "login did not redirect: " + loginRecorder.Body.String()
	}

	authorizeURL, err := url.Parse(loginRecorder.Header().Get("Location"))
	if err != nil {
		return 0, "unparsable authorize URL: " + err.Error()
	}
	state := authorizeURL.Query().Get("state")
	nonce := authorizeURL.Query().Get("nonce")

	code := fmt.Sprintf("code-%s-%d", subject, time.Now().UnixNano())
	p.mu.Lock()
	p.codes[code] = fakeAuthorization{subject: subject, email: email, nonce: nonce}
	p.mu.Unlock()

	callback := httptest.NewRequest(http.MethodGet,
		"/forward-auth/auth_oidc/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), nil)
	for _, cookie := range loginRecorder.Result().Cookies() {
		callback.AddCookie(cookie)
	}
	callbackRecorder := httptest.NewRecorder()
	handler.Callback(callbackRecorder, callback)
	return callbackRecorder.Code, callbackRecorder.Body.String()
}

func writeFakeJSON(w http.ResponseWriter, document map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(document)
}

/* ── fixture ───────────────────────────────────────────────────────────── */

// newSignInProvisioner builds the real provisioning pipeline. A stub would not
// do: what the ensurer produces is a project row, a role assignment and a
// `p_<id>` schema, and the assertions above read all three.
func newSignInProvisioner(pool *pgxpool.Pool) *projectprovisioning.Provisioner {
	return projectprovisioning.New(
		pool,
		migrate.New(pool, platformmigrations.Files),
		nil,
		projectprovisioning.WithProjectVault(v2secrets.NewHandler(pool)),
	)
}

// signedInAccounts reads the accounts the burst created, by email.
func signedInAccounts(t *testing.T, pool *pgxpool.Pool, want int) map[string]int64 {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx,
		`SELECT id, email FROM public.auth_core__user WHERE email LIKE 'burst-%@autotest.local' ORDER BY id`)
	require.NoError(t, err)
	accounts := map[string]int64{}
	for rows.Next() {
		var userID int64
		var email string
		require.NoError(t, rows.Scan(&userID, &email))
		accounts[email] = userID
	}
	require.NoError(t, rows.Err())
	require.Len(t, accounts, want)
	return accounts
}

// awaitPersonalProjects waits for the queue to drain.
//
// POLLING IS THE POINT, not a workaround. The sign-in wait is bounded, so the
// last logins in a burst return before their own attempts run — that is the
// design. What this asserts is that the work still HAPPENS, which is exactly
// what the dropped callers never did.
func awaitPersonalProjects(t *testing.T, pool *pgxpool.Pool, accounts map[string]int64) {
	t.Helper()
	names := make([]string, 0, len(accounts))
	for _, userID := range accounts {
		names = append(names, personalproject.Name(userID))
	}
	deadline := time.Now().Add(240 * time.Second)
	var provisioned int
	for time.Now().Before(deadline) {
		require.NoError(t, pool.QueryRow(context.Background(),
			`SELECT count(*) FROM centry.project WHERE name = ANY($1) AND create_success`,
			names).Scan(&provisioned))
		if provisioned == len(names) {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("%d of %d accounts have a personal project; the callers beyond the "+
		"one provisioning slot were discarded", provisioned, len(names))
}

const (
	signInProvisioningURLEnv    = "ELITEA_TEST_DATABASE_URL"
	signInProvisioningBootstrap = "../../../infra/db/migrations/001_initial.sql"
)

// The template carries the ledgered TENANT history, because the provisioner
// applies it to every project it creates. Built on first use rather than in a
// TestMain, so a failure here cannot take down the suites in this package that
// touch no database at all.
var (
	signInProvisioningTemplateOnce sync.Once
	signInProvisioningTemplate     string
	signInProvisioningTemplateErr  error
)

func signInProvisioningTemplateName(databaseURL string) (string, error) {
	signInProvisioningTemplateOnce.Do(func() {
		bootstrap, err := os.ReadFile(signInProvisioningBootstrap)
		if err != nil {
			signInProvisioningTemplateErr = fmt.Errorf("read bootstrap schema: %w", err)
			return
		}
		ctx, cancel := dbtest.BuildContext(context.Background())
		defer cancel()
		adminPool, err := pgxpool.New(ctx, databaseURL)
		if err != nil {
			signInProvisioningTemplateErr = fmt.Errorf("open admin pool: %w", err)
			return
		}
		defer adminPool.Close()
		signInProvisioningTemplate, signInProvisioningTemplateErr = dbtest.EnsureTemplate(
			ctx, adminPool, dbtest.Spec{
				Files:   platformmigrations.Files,
				Seed:    string(bootstrap),
				Tenants: []int64{1},
			})
	})
	return signInProvisioningTemplate, signInProvisioningTemplateErr
}

func newSignInProvisioningPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv(signInProvisioningURLEnv)
	if databaseURL == "" {
		t.Skipf("set %s to run the concurrent sign-in provisioning test", signInProvisioningURLEnv)
	}
	template, err := signInProvisioningTemplateName(databaseURL)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	adminPool, err := pgxpool.New(ctx, databaseURL)
	require.NoError(t, err)
	defer adminPool.Close()

	databaseName := fmt.Sprintf("elitea_signin_pp_%d_%d", os.Getpid(), time.Now().UnixNano())
	require.NoError(t, dbtest.CreateFromTemplate(ctx, adminPool, template, databaseName))

	config, err := pgxpool.ParseConfig(databaseURL)
	require.NoError(t, err)
	config.ConnConfig.Database = databaseName
	// Above the burst: eight callbacks provision accounts concurrently, and the
	// one provisioning attempt underneath takes several connections of its own.
	config.MaxConns = 12
	pool, err := pgxpool.NewWithConfig(ctx, config)
	require.NoError(t, err)

	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		dropPool, dropErr := pgxpool.New(dropCtx, databaseURL)
		if dropErr != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(dropCtx,
			"DROP DATABASE IF EXISTS "+pgx.Identifier{databaseName}.Sanitize()+" WITH (FORCE)")
	})
	return pool
}
