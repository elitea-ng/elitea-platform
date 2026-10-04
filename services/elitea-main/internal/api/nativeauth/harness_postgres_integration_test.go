package nativeauth_test

// The PostgreSQL harness for the native authorization tests: a throwaway
// database holding what a real deployment holds — the bootstrap schema
// (001_initial.sql) plus the whole shared migration corpus, 0141 included — so
// every query runs against the real tables, foreign keys and checks.
//
// The corpus is applied ONCE per package run into a template database; each
// test clones it (CREATE DATABASE ... TEMPLATE), which is what keeps a dozen
// row-state tests affordable.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/browserauth"
	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	nativeapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/nativeauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/authsvc"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/nativeauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

const (
	testDatabaseEnv = "ELITEA_TEST_DATABASE_URL"
	testOrigin      = "https://elitea.example.test"
	testClientID    = "dev.elitea.conformance"
	testRedirect    = "dev.elitea.conformance:/oauth/callback"
	testSecret      = "native-auth-test-session-secret"
	testVerifier    = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	testChallenge   = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
)

var (
	templateOnce sync.Once
	templateName string
	templateErr  error
)

func adminPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv(testDatabaseEnv)
	if databaseURL == "" {
		t.Skipf("set %s to run the native authorization PostgreSQL tests", testDatabaseEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", testDatabaseEnv, err)
	}
	config.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	return pool
}

func buildTemplate(t *testing.T, admin *pgxpool.Pool) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	name := fmt.Sprintf("elitea_native_tpl_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		return "", err
	}
	config := admin.Config().Copy()
	config.ConnConfig.Database = name
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return "", err
	}
	defer pool.Close()
	initial, err := os.ReadFile(filepath.Join("..", "..", "infra", "db", "migrations", "001_initial.sql"))
	if err != nil {
		return "", err
	}
	if _, err := pool.Exec(ctx, string(initial)); err != nil {
		return "", fmt.Errorf("apply 001_initial.sql: %w", err)
	}
	if err := migrate.New(pool, migrations.Files).ApplyShared(ctx); err != nil {
		return "", fmt.Errorf("apply shared migrations: %w", err)
	}
	return name, nil
}

// newPool clones the template into a database owned by this test.
func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := adminPool(t)
	templateOnce.Do(func() { templateName, templateErr = buildTemplate(t, admin) })
	if templateErr != nil {
		admin.Close()
		t.Fatalf("build the template database: %v", templateErr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	name := fmt.Sprintf("elitea_native_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoted+" TEMPLATE "+pgx.Identifier{templateName}.Sanitize()); err != nil {
		admin.Close()
		t.Fatalf("clone the template database: %v", err)
	}
	config := admin.Config().Copy()
	config.ConnConfig.Database = name
	config.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		admin.Close()
		t.Fatalf("open the test database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), time.Minute)
		defer dropCancel()
		if _, err := admin.Exec(dropCtx, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("drop test database: %v", err)
		}
		admin.Close()
	})
	return pool
}

// TestMain drops the template after the package's tests.
func TestMain(m *testing.M) {
	code := m.Run()
	if templateName != "" {
		if databaseURL := os.Getenv(testDatabaseEnv); databaseURL != "" {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			if pool, err := pgxpool.New(ctx, databaseURL); err == nil {
				_, _ = pool.Exec(ctx, "DROP DATABASE "+pgx.Identifier{templateName}.Sanitize()+" WITH (FORCE)")
				pool.Close()
			}
			cancel()
		}
	}
	os.Exit(code)
}

/* ── the stack under test ───────────────────────────────────────────────── */

type recordingAudit struct {
	mu     sync.Mutex
	events []audit.Event
}

func (r *recordingAudit) Record(_ context.Context, event audit.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recordingAudit) actions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.events))
	for _, event := range r.events {
		out = append(out, event.Action)
	}
	return out
}

type stack struct {
	t         *testing.T
	pool      *pgxpool.Pool
	store     *domain.Store
	registry  *domain.Registry
	validator *domain.AccessValidator
	handler   *nativeapi.Handler
	audit     *recordingAudit
	router    http.Handler
	now       time.Time
	clockMu   sync.Mutex
}

type stackOption func(*nativeapi.Config, *domain.Config)

func withRedelivery(window time.Duration) stackOption {
	return func(_ *nativeapi.Config, cfg *domain.Config) { cfg.RedeliveryWindow = window }
}

func withAuthenticate(authenticate func(http.Handler) http.Handler) stackOption {
	return func(cfg *nativeapi.Config, _ *domain.Config) { cfg.Authenticate = authenticate }
}

func withSessionMaxLifetime(lifetime time.Duration) stackOption {
	return func(_ *nativeapi.Config, cfg *domain.Config) { cfg.SessionMaxLifetime = lifetime }
}

// fileClient is the conformance client from the plan's file layer.
func fileClient() domain.Client {
	return domain.Client{
		ClientID: testClientID, DisplayName: "Conformance", Enabled: true, Source: domain.SourceFile,
		RedirectURIs: []string{testRedirect, "http://127.0.0.1/callback"},
	}
}

func newStack(t *testing.T, options ...stackOption) *stack {
	t.Helper()
	pool := newPool(t)
	s := &stack{t: t, pool: pool, audit: &recordingAudit{}, now: time.Now().UTC()}
	storeConfig := domain.DefaultConfig()
	authConfig := apimw.AuthConfig{
		Validator:          authsvc.NewNativeAwareValidator(nil, domain.NewAccessValidator(pool)),
		PrincipalValidator: authsvc.NewPrincipalValidator(pool),
		SessionSecret:      testSecret,
	}
	handlerConfig := nativeapi.Config{
		PublicOrigin: testOrigin,
		Pages:        browserauth.NewNativePages(nil),
		Authenticate: apimw.Auth(authConfig),
		Audit:        s.audit,
	}
	for _, option := range options {
		option(&handlerConfig, &storeConfig)
	}
	s.registry = domain.NewRegistry([]domain.Client{fileClient()}, pool)
	s.store = domain.NewStore(pool, storeConfig)
	s.store.SetClock(s.clock)
	s.validator = domain.NewAccessValidator(pool)
	s.validator.SetClock(s.clock)
	handlerConfig.Registry, handlerConfig.Store = s.registry, s.store
	s.handler = nativeapi.New(handlerConfig)

	// The protected route a native client calls: the REAL Auth middleware
	// with the native-aware validator and the real principal re-check.
	authConfig.Validator = authsvc.NewNativeAwareValidator(nil, s.validator)
	router := chi.NewRouter()
	s.handler.Mount(router)
	// The device registry as router.go mounts it (minus the admin RBAC
	// gate, which router tests cover): behind the group's Auth.
	authenticated := router.With(apimw.Auth(authConfig))
	authenticated.With(s.handler.RegisteredOnly).Get(nativeapi.DevicesPath, s.handler.ListDevices)
	authenticated.With(s.handler.RegisteredOnly).Delete(nativeapi.DevicesPath+"/{deviceID}", s.handler.RevokeDevice)
	authenticated.Get("/api/v2/admin/native_devices/administration", s.handler.AdminListDevices)
	authenticated.Delete("/api/v2/admin/native_devices/administration/{deviceID}", s.handler.AdminRevokeDevice)
	router.With(apimw.Auth(authConfig)).Get("/api/v2/whoami", func(w http.ResponseWriter, r *http.Request) {
		user, _ := auth.UserFromContext(r.Context())
		_ = json.NewEncoder(w).Encode(user)
	})
	s.router = router
	return s
}

func (s *stack) clock() time.Time {
	s.clockMu.Lock()
	defer s.clockMu.Unlock()
	return s.now
}

func (s *stack) advance(d time.Duration) {
	s.clockMu.Lock()
	defer s.clockMu.Unlock()
	s.now = s.now.Add(d)
}

func (s *stack) seedUser(email string) int64 {
	s.t.Helper()
	var id int64
	if err := s.pool.QueryRow(context.Background(),
		`INSERT INTO public.auth_core__user (email, name) VALUES ($1, $1) RETURNING id`, email).Scan(&id); err != nil {
		s.t.Fatalf("seed user: %v", err)
	}
	return id
}

// sessionCookie mints the legacy signed `elitea_session` cookie the real Auth
// middleware verifies with SessionSecret.
func sessionCookie(userID int64, email string) *http.Cookie {
	payload, _ := json.Marshal(map[string]any{
		"uid": strconv.FormatInt(userID, 10), "email": email, "exp": time.Now().Add(time.Hour).Unix(),
	})
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(encoded))
	return &http.Cookie{Name: "elitea_session", Value: encoded + "." + hex.EncodeToString(mac.Sum(nil))}
}

type requestOption func(*http.Request)

func withCookies(cookies ...*http.Cookie) requestOption {
	return func(r *http.Request) {
		for _, cookie := range cookies {
			if cookie != nil {
				r.AddCookie(cookie)
			}
		}
	}
}

func withHeader(name, value string) requestOption {
	return func(r *http.Request) { r.Header.Set(name, value) }
}

func (s *stack) do(method, target string, form url.Values, options ...requestOption) *httptest.ResponseRecorder {
	var request *http.Request
	if form != nil {
		request = httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		request = httptest.NewRequest(method, target, nil)
	}
	for _, option := range options {
		option(request)
	}
	recorder := httptest.NewRecorder()
	s.router.ServeHTTP(recorder, request)
	return recorder
}

func authorizeQuery(overrides map[string]string) string {
	values := url.Values{
		"response_type": {"code"}, "client_id": {testClientID}, "redirect_uri": {testRedirect},
		"code_challenge": {testChallenge}, "code_challenge_method": {"S256"}, "state": {"state-123"},
		"device_name": {"Alex's Phone"}, "platform": {"ios"}, "client_version": {"1.2.3"},
	}
	for key, value := range overrides {
		if value == "" {
			values.Del(key)
			continue
		}
		values.Set(key, value)
	}
	return nativeapi.AuthorizePath + "?" + values.Encode()
}

func binderFrom(t *testing.T, recorder *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, cookie := range recorder.Result().Cookies() {
		if strings.Contains(cookie.Name, "elitea_native_authz") && cookie.Value != "" {
			return cookie
		}
	}
	t.Fatalf("no binder cookie in %v", recorder.Result().Header)
	return nil
}

var (
	hiddenRequest = regexp.MustCompile(`name="request" value="([^"]+)"`)
	hiddenUID     = regexp.MustCompile(`name="uid" value="([^"]+)"`)
)

// signIn runs authorize -> continue (signed in) -> consent -> allow and
// returns the code from the app redirect.
func (s *stack) signIn(userID int64, email string) string {
	s.t.Helper()
	start := s.do(http.MethodGet, authorizeQuery(nil), nil)
	if start.Code != http.StatusFound {
		s.t.Fatalf("authorize = %d %s", start.Code, start.Body.String())
	}
	binder := binderFrom(s.t, start)
	session := sessionCookie(userID, email)
	consent := s.do(http.MethodGet, start.Header().Get("Location"), nil, withCookies(binder, session))
	if consent.Code != http.StatusOK {
		s.t.Fatalf("continue = %d %s", consent.Code, consent.Body.String())
	}
	request := hiddenRequest.FindStringSubmatch(consent.Body.String())
	uid := hiddenUID.FindStringSubmatch(consent.Body.String())
	if request == nil || uid == nil {
		s.t.Fatalf("consent page has no form: %s", consent.Body.String())
	}
	decision := s.do(http.MethodPost, nativeapi.DecisionPath,
		url.Values{"request": {request[1]}, "uid": {uid[1]}, "decision": {"allow"}},
		withCookies(binder, session), withHeader("Origin", testOrigin))
	if decision.Code != http.StatusFound {
		s.t.Fatalf("decision = %d %s", decision.Code, decision.Body.String())
	}
	location, err := url.Parse(decision.Header().Get("Location"))
	if err != nil {
		s.t.Fatalf("decision location: %v", err)
	}
	if got := location.Query().Get("iss"); got != testOrigin {
		s.t.Fatalf("iss = %q, want %q", got, testOrigin)
	}
	if got := location.Query().Get("state"); got != "state-123" {
		s.t.Fatalf("state = %q", got)
	}
	code := location.Query().Get("code")
	if !strings.HasPrefix(code, domain.PrefixCode) {
		s.t.Fatalf("no code in %s", location)
	}
	return code
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	DeviceID     string `json:"device_id"`
	Error        string `json:"error"`
}

func (s *stack) token(form url.Values) (*httptest.ResponseRecorder, tokenResponse) {
	s.t.Helper()
	recorder := s.do(http.MethodPost, nativeapi.TokenPath, form)
	var body tokenResponse
	_ = json.Unmarshal(recorder.Body.Bytes(), &body)
	return recorder, body
}

func (s *stack) exchange(code string) tokenResponse {
	s.t.Helper()
	recorder, body := s.token(url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {testRedirect},
		"client_id": {testClientID}, "code_verifier": {testVerifier},
	})
	if recorder.Code != http.StatusOK {
		s.t.Fatalf("exchange = %d %s", recorder.Code, recorder.Body.String())
	}
	return body
}

func (s *stack) refresh(refreshToken string) (*httptest.ResponseRecorder, tokenResponse) {
	return s.token(url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}, "client_id": {testClientID},
	})
}

func (s *stack) whoami(accessToken string) *httptest.ResponseRecorder {
	return s.do(http.MethodGet, "/api/v2/whoami", nil, withHeader("Authorization", "Bearer "+accessToken))
}

type familyState struct {
	revokedAt     *time.Time
	revokeReason  *string
	tokenID       *int64
	accessTokens  int
	refreshTokens int
}

func (s *stack) family(deviceID string) familyState {
	s.t.Helper()
	ctx := context.Background()
	var state familyState
	if err := s.pool.QueryRow(ctx, `
		SELECT revoked_at, revoke_reason, token_id FROM elitea_auth.native_sessions WHERE id = $1`,
		deviceID).Scan(&state.revokedAt, &state.revokeReason, &state.tokenID); err != nil {
		s.t.Fatalf("read family: %v", err)
	}
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM elitea_auth.native_access_tokens WHERE session_id = $1`,
		deviceID).Scan(&state.accessTokens)
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM elitea_auth.native_refresh_tokens WHERE session_id = $1`,
		deviceID).Scan(&state.refreshTokens)
	return state
}

func (s *stack) anchorOf(deviceID string) int64 {
	s.t.Helper()
	var anchor *int64
	if err := s.pool.QueryRow(context.Background(),
		`SELECT token_id FROM elitea_auth.native_sessions WHERE id = $1`, deviceID).Scan(&anchor); err != nil || anchor == nil {
		s.t.Fatalf("anchor of %s: %v", deviceID, err)
	}
	return *anchor
}

func (s *stack) anchorExists(anchor int64) bool {
	var exists bool
	_ = s.pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM public.auth_core__token WHERE id = $1)`, anchor).Scan(&exists)
	return exists
}

func reason(state familyState) string {
	if state.revokeReason == nil {
		return ""
	}
	return *state.revokeReason
}

func strconvFormat(v int64) string { return strconv.FormatInt(v, 10) }

func errNoRows() error { return pgx.ErrNoRows }

// newBareRouter mounts a handler without the stack's whoami route.
func newBareRouter(handler *nativeapi.Handler) http.Handler {
	router := chi.NewRouter()
	handler.Mount(router)
	return router
}

func doOn(router http.Handler, method, target string, form url.Values) *httptest.ResponseRecorder {
	var request *http.Request
	if form != nil {
		request = httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		request = httptest.NewRequest(method, target, nil)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func doOnWithBearer(handler http.Handler, accessToken string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, nativeapi.DevicesPath, nil)
	request.Header.Set("Authorization", "Bearer "+accessToken)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}
