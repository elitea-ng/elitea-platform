package authcomposition

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	browserapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/browserauth"
	forwardapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/forwardauth"
)

func TestNewFormGraphComposesSeparateDirectAndMainPolicies(t *testing.T) {
	config := writeMaterialFixture(t)
	pool := newUnconnectedPool(t)
	var client *redis.Client
	var temporaryPAT []byte
	graph, err := newFormGraph(
		context.Background(),
		config,
		FormGraphDependencies{
			PostgreSQL: pool,
			MainRoutePublicRules: []forwardapp.PublicRule{{
				Name: "route.health",
				Conditions: []forwardapp.RuleCondition{{
					Field: forwardapp.SourceURI, Pattern: `/health`,
				}},
			}},
		},
		func(_ context.Context, _ Config, material *materializedFiles) (*redis.Client, error) {
			temporaryPAT = material.patSigningKey
			client = redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
			return client, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if graph.Routes() == nil || graph.BrowserRoutes() == nil || graph.MainForwardAuth() == nil ||
		graph.ForwardedIdentityVerifier() == nil || !allZero(temporaryPAT) {
		t.Fatalf("graph=%+v temporary PAT cleared=%v", graph, allZero(temporaryPAT))
	}
	if _, err := graph.ValidateToken(context.Background(), "not-a-token"); err == nil || errors.Is(err, ErrInvalidGraph) {
		t.Fatalf("runtime PAT bridge did not reuse the composed validator: %v", err)
	}
	if _, err := graph.IssueToken(context.Background(), 7); err == nil || errors.Is(err, ErrInvalidGraph) {
		t.Fatalf("runtime PAT bridge did not reuse the composed issuer: %v", err)
	}

	for _, uri := range []string{"/forward-auth/login", "/health"} {
		decision, err := graph.AuthorizeMain(context.Background(), publicMainRequest(uri))
		if err != nil {
			t.Fatal(err)
		}
		if decision.Kind != forwardapp.DecisionAllow ||
			decision.Authentication.Type != forwardapp.AuthenticationPublic {
			t.Fatalf("Main decision for %s = %+v", uri, decision)
		}
	}

	request := httptest.NewRequest(http.MethodGet, "http://auth-internal/auth", nil)
	request.RemoteAddr = "10.1.2.3:1234"
	request.Header.Set("X-Forwarded-For", "203.0.113.7")
	request.Header.Set("X-Forwarded-Method", http.MethodGet)
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("X-Forwarded-Host", "elitea.example")
	request.Header.Set("X-Forwarded-Uri", "/forward-auth/login")
	response := httptest.NewRecorder()
	graph.Routes().ServeHTTP(response, request)
	if response.Code != http.StatusFound ||
		response.Header().Get("Location") != "/auth/login?target_to=%2Fforward-auth%2Flogin" {
		t.Fatalf("Direct response = %d location=%q body=%q", response.Code, response.Header().Get("Location"), response.Body.String())
	}

	mainRequest := httptest.NewRequest(http.MethodGet, "http://auth-internal/internal/forward-auth/main", nil)
	mainRequest.RemoteAddr = "10.1.2.3:1234"
	mainRequest.Header.Set("X-Forwarded-For", "203.0.113.7")
	mainRequest.Header.Set("X-Forwarded-Method", http.MethodGet)
	mainRequest.Header.Set("X-Forwarded-Proto", "https")
	mainRequest.Header.Set("X-Forwarded-Host", "elitea.example")
	mainRequest.Header.Set("X-Forwarded-Uri", "/health")
	mainResponse := httptest.NewRecorder()
	graph.MainForwardAuth().ServeHTTP(mainResponse, mainRequest)
	if mainResponse.Code != http.StatusOK || mainResponse.Header().Get("X-Auth-Type") != "public" ||
		mainResponse.Header().Get("X-Auth-ID") != "-" || mainResponse.Header().Get("X-Auth-User-ID") != "-" ||
		mainResponse.Header().Get("X-Auth-Reference") != "-" ||
		mainResponse.Header().Get(browserapi.MainAvatarStateHeader) != "none" ||
		mainResponse.Header().Get(browserapi.MainAvatarHeader) != "-" {
		t.Fatalf("Main response = %d headers=%v body=%q", mainResponse.Code, mainResponse.Header(), mainResponse.Body.String())
	}

	if err := graph.Close(); err != nil {
		t.Fatal(err)
	}
	if err := graph.Ping(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("closed graph readiness = %v", err)
	}
	if err := graph.Close(); err != nil {
		t.Fatalf("second Close() = %v", err)
	}
	if err := client.Ping(context.Background()).Err(); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("owned Redis client remained open: %v", err)
	}
}

func TestNewFormGraphRejectsIncompleteDependenciesAndClosesOnFailure(t *testing.T) {
	config := writeMaterialFixture(t)
	pool := newUnconnectedPool(t)
	validDependencies := FormGraphDependencies{
		PostgreSQL:           pool,
		MainRoutePublicRules: []forwardapp.PublicRule{},
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for name, test := range map[string]struct {
		ctx          context.Context
		dependencies FormGraphDependencies
		opener       redisOpener
		want         error
	}{
		"nil context":      {ctx: nil, dependencies: validDependencies, opener: unusedRedisOpener, want: ErrInvalidGraph},
		"canceled context": {ctx: canceled, dependencies: validDependencies, opener: unusedRedisOpener, want: context.Canceled},
		"nil PostgreSQL": {
			ctx:          context.Background(),
			dependencies: FormGraphDependencies{MainRoutePublicRules: []forwardapp.PublicRule{}},
			opener:       unusedRedisOpener,
			want:         ErrInvalidGraph,
		},
		"implicit route rules": {
			ctx: context.Background(), dependencies: FormGraphDependencies{PostgreSQL: pool}, opener: unusedRedisOpener, want: ErrInvalidGraph,
		},
		"nil opener": {ctx: context.Background(), dependencies: validDependencies, want: ErrInvalidGraph},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := newFormGraph(test.ctx, config, test.dependencies, test.opener)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}

	var opened *redis.Client
	invalidRules := validDependencies
	invalidRules.MainRoutePublicRules = []forwardapp.PublicRule{{
		Name:       "config.forward_auth",
		Conditions: []forwardapp.RuleCondition{{Field: forwardapp.SourceURI, Pattern: `/duplicate`}},
	}}
	_, err := newFormGraph(
		context.Background(),
		config,
		invalidRules,
		func(context.Context, Config, *materializedFiles) (*redis.Client, error) {
			opened = redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
			return opened, nil
		},
	)
	if !errors.Is(err, ErrInvalidGraph) {
		t.Fatalf("invalid Main rules error = %v", err)
	}
	if err := opened.Ping(context.Background()).Err(); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Redis client was not closed after composition failure: %v", err)
	}

	_, err = newFormGraph(
		context.Background(),
		config,
		validDependencies,
		func(context.Context, Config, *materializedFiles) (*redis.Client, error) { return nil, nil },
	)
	if !errors.Is(err, ErrInvalidGraph) {
		t.Fatalf("nil Redis client error = %v", err)
	}

	wantOpenError := errors.New("Redis unavailable")
	var temporaryPAT []byte
	var failedClient *redis.Client
	_, err = newFormGraph(
		context.Background(),
		config,
		validDependencies,
		func(_ context.Context, _ Config, material *materializedFiles) (*redis.Client, error) {
			temporaryPAT = material.patSigningKey
			failedClient = redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
			return failedClient, wantOpenError
		},
	)
	if !errors.Is(err, ErrInvalidGraph) || !errors.Is(err, wantOpenError) || !allZero(temporaryPAT) {
		t.Fatalf("Redis open failure = %v, temporary PAT cleared=%v", err, allZero(temporaryPAT))
	}
	if err := failedClient.Ping(context.Background()).Err(); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Redis opener error leaked ownership: %v", err)
	}
}

func TestDerivedPoliciesPreserveTypedBaselineAndSecurityCorrections(t *testing.T) {
	config := parsedValidConfig(t)
	policy := provisioningPolicy(config.Identity)
	if len(policy.InitialGlobalAdmins) != 1 || policy.InitialGlobalAdmins[0] != "admin" ||
		policy.ProjectEnrollment.ProjectID != 1 || policy.ProjectEnrollment.AllowedDomains != "centry.user" ||
		len(policy.ProjectEnrollment.AdditionalGlobalAdminRoles) != 7 {
		t.Fatalf("unexpected provisioning policy: %+v", policy)
	}
	config.Identity.InitialGlobalAdmins[0] = "mutated"
	config.Identity.ProjectEnrollment.AdditionalProjectRolesForGlobalAdmins[0] = "mutated"
	if policy.InitialGlobalAdmins[0] != "admin" || policy.ProjectEnrollment.AdditionalGlobalAdminRoles[0] != "system" {
		t.Fatalf("provisioning policy aliases config: %+v", policy)
	}
	if disabled := provisioningPolicy(IdentityConfig{InitialGlobalAdmins: []string{}}); disabled.ProjectEnrollment.ProjectID != 0 {
		t.Fatalf("disabled enrollment became active: %+v", disabled)
	}

	key := bytes.Repeat([]byte("k"), minAttemptKeyBytes)
	attempts := compiledAttemptConfig("centry:auth:v1:attempt:", key)
	clear(key)
	if allZero(attempts.KeySecret) || attempts.Global.MaxAttempts != 1000 || attempts.Global.Window != time.Minute ||
		attempts.FormBegin.MaxAttempts != 20 || attempts.FormCredentialClient.MaxAttempts != 5 ||
		attempts.FormCredentialLogin.MaxAttempts != 25 || attempts.OIDCBegin.MaxAttempts != 20 ||
		attempts.OIDCCallback.MaxAttempts != 30 {
		t.Fatalf("unexpected compiled attempt policy: %+v", attempts)
	}

	headers := credentialHeaders([]CredentialHeaderConfig{{Name: "X-Token", Type: "bearer"}})
	if len(headers) != 1 || headers[0].Name != "X-Token" || headers[0].Type != "bearer" {
		t.Fatalf("credential headers = %+v", headers)
	}
}

func TestCookiePolicyIsHostOnlySecureAndSeparateFromMainSession(t *testing.T) {
	config := parsedValidConfig(t)
	policy, err := cookiePolicy(config.Cookie)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	setResponse := httptest.NewRecorder()
	if err := policy.Set(setResponse, sessionID); err != nil {
		t.Fatal(err)
	}
	setCookies := setResponse.Result().Cookies()
	if len(setCookies) != 1 {
		t.Fatalf("Set-Cookie count = %d", len(setCookies))
	}
	set := setCookies[0]
	if set.Name != "centry_auth_session" || set.Name == "centry_main_session" || set.Domain != "" ||
		set.Path != "/" || !set.Secure || !set.HttpOnly || set.SameSite != http.SameSiteLaxMode ||
		set.MaxAge != int(config.Cookie.LifetimeSeconds) {
		t.Fatalf("unexpected auth cookie: %+v", set)
	}

	clearResponse := httptest.NewRecorder()
	if err := policy.Clear(clearResponse); err != nil {
		t.Fatal(err)
	}
	cleared := clearResponse.Result().Cookies()[0]
	if cleared.Name != set.Name || cleared.Domain != set.Domain || cleared.Path != set.Path ||
		cleared.Secure != set.Secure || cleared.HttpOnly != set.HttpOnly ||
		cleared.SameSite != set.SameSite || cleared.MaxAge != -1 {
		t.Fatalf("clear cookie attributes diverged: set=%+v clear=%+v", set, cleared)
	}
}

func TestNilFormGraphMethodsFailSafely(t *testing.T) {
	var graph *FormGraph
	if graph.Routes() != nil || graph.BrowserRoutes() != nil || graph.MainForwardAuth() != nil || graph.Close() != nil {
		t.Fatal("nil graph did not fail safely")
	}
	if err := graph.Ping(context.Background()); !errors.Is(err, ErrInvalidGraph) {
		t.Fatalf("nil graph Ping error = %v", err)
	}
	if _, err := graph.AuthorizeMain(context.Background(), publicMainRequest("/health")); !errors.Is(err, ErrInvalidGraph) {
		t.Fatalf("AuthorizeMain error = %v", err)
	}
	if _, err := graph.ValidateToken(context.Background(), "token"); !errors.Is(err, ErrInvalidGraph) {
		t.Fatalf("ValidateToken error = %v", err)
	}
	if _, err := graph.IssueToken(context.Background(), 7); !errors.Is(err, ErrInvalidGraph) {
		t.Fatalf("IssueToken error = %v", err)
	}
}

func newUnconnectedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	config, err := pgxpool.ParseConfig("postgres://elitea:password@127.0.0.1:1/elitea?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	config.MinConns = 0
	config.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func publicMainRequest(uri string) forwardapp.Request {
	return forwardapp.Request{Source: forwardapp.Source{
		Method: http.MethodGet,
		Proto:  "https",
		Host:   "elitea.example",
		URI:    uri,
		IP:     "203.0.113.7",
	}}
}

func unusedRedisOpener(context.Context, Config, *materializedFiles) (*redis.Client, error) {
	panic("Redis opener called for invalid dependencies")
}
