package authcomposition

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	browserapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/browserauth"
	browserapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/browserauth"
	forwardapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/edgeauth"
)

func TestNewFormGraphComposesSeparateDirectAndMainPolicies(t *testing.T) {
	config := writeMaterialFixture(t)
	pool := newUnconnectedPool(t)
	var temporaryPAT []byte
	graph, err := newFormGraph(
		context.Background(),
		config,
		FormGraphDependencies{
			PostgreSQL:        pool,
			FormSignInEnabled: true,
			MainRoutePublicRules: []forwardapp.PublicRule{{
				Name: "route.health",
				Conditions: []forwardapp.RuleCondition{{
					Field: forwardapp.SourceURI, Pattern: `/health`,
				}},
			}},
		},
		func(material *materializedFiles) { temporaryPAT = material.patSigningKey },
	)
	if err != nil {
		t.Fatal(err)
	}
	if graph.Routes() == nil || graph.BrowserRoutes() == nil || graph.MainEdgeAuth() == nil ||
		graph.ForwardedIdentityVerifier() == nil || !allZero(temporaryPAT) {
		t.Fatalf("graph=%+v temporary PAT cleared=%v", graph, allZero(temporaryPAT))
	}
	if _, err := graph.ValidateToken(context.Background(), "not-a-token"); err == nil || errors.Is(err, ErrInvalidGraph) {
		t.Fatalf("runtime PAT bridge did not reuse the composed validator: %v", err)
	}
	if _, err := graph.IssueToken(context.Background(), 7); err == nil || errors.Is(err, ErrInvalidGraph) {
		t.Fatalf("runtime PAT bridge did not reuse the composed issuer: %v", err)
	}

	for _, uri := range []string{"/auth/login", "/health"} {
		decision, err := graph.AuthorizeMain(context.Background(), publicMainRequest(uri))
		if err != nil {
			t.Fatal(err)
		}
		if decision.Kind != forwardapp.DecisionAllow ||
			decision.Authentication.Type != forwardapp.AuthenticationPublic {
			t.Fatalf("Main decision for %s = %+v", uri, decision)
		}
	}

	request := httptest.NewRequest(http.MethodGet, "http://auth-internal/check", nil)
	request.RemoteAddr = "10.1.2.3:1234"
	request.Header.Set("X-Forwarded-For", "203.0.113.7")
	request.Header.Set("X-Forwarded-Method", http.MethodGet)
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("X-Forwarded-Host", "elitea.example")
	request.Header.Set("X-Forwarded-Uri", "/auth/login")
	response := httptest.NewRecorder()
	graph.Routes().ServeHTTP(response, request)
	if response.Code != http.StatusFound ||
		response.Header().Get("Location") != "/auth/login?target_to=%2Fauth%2Flogin" {
		t.Fatalf("Direct response = %d location=%q body=%q", response.Code, response.Header().Get("Location"), response.Body.String())
	}

	mainRequest := httptest.NewRequest(http.MethodGet, "http://auth-internal/internal/auth/main", nil)
	mainRequest.RemoteAddr = "10.1.2.3:1234"
	mainRequest.Header.Set("X-Forwarded-For", "203.0.113.7")
	mainRequest.Header.Set("X-Forwarded-Method", http.MethodGet)
	mainRequest.Header.Set("X-Forwarded-Proto", "https")
	mainRequest.Header.Set("X-Forwarded-Host", "elitea.example")
	mainRequest.Header.Set("X-Forwarded-Uri", "/health")
	mainResponse := httptest.NewRecorder()
	graph.MainEdgeAuth().ServeHTTP(mainResponse, mainRequest)
	if mainResponse.Code != http.StatusOK || mainResponse.Header().Get("X-Auth-Type") != "public" ||
		mainResponse.Header().Get("X-Auth-ID") != "-" || mainResponse.Header().Get("X-Auth-User-ID") != "-" ||
		mainResponse.Header().Get("X-Auth-Reference") != "-" ||
		mainResponse.Header().Get(browserapi.MainAvatarStateHeader) != "none" ||
		mainResponse.Header().Get(browserapi.MainAvatarHeader) != "-" {
		t.Fatalf("Main response = %d headers=%v body=%q", mainResponse.Code, mainResponse.Header(), mainResponse.Body.String())
	}
}

func TestNewFormGraphRejectsIncompleteDependenciesAndWipesMaterialOnFailure(t *testing.T) {
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
		want         error
	}{
		"nil context":      {ctx: nil, dependencies: validDependencies, want: ErrInvalidGraph},
		"canceled context": {ctx: canceled, dependencies: validDependencies, want: context.Canceled},
		"nil PostgreSQL": {
			ctx:          context.Background(),
			dependencies: FormGraphDependencies{MainRoutePublicRules: []forwardapp.PublicRule{}},
			want:         ErrInvalidGraph,
		},
		"implicit route rules": {
			ctx: context.Background(), dependencies: FormGraphDependencies{PostgreSQL: pool}, want: ErrInvalidGraph,
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := newFormGraph(test.ctx, config, test.dependencies, unusedObserver)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}

	var temporaryPAT []byte
	invalidRules := validDependencies
	invalidRules.MainRoutePublicRules = []forwardapp.PublicRule{{
		Name:       "config.edge_auth",
		Conditions: []forwardapp.RuleCondition{{Field: forwardapp.SourceURI, Pattern: `/duplicate`}},
	}}
	_, err := newFormGraph(
		context.Background(),
		config,
		invalidRules,
		func(material *materializedFiles) { temporaryPAT = material.patSigningKey },
	)
	if !errors.Is(err, ErrInvalidGraph) {
		t.Fatalf("invalid Main rules error = %v", err)
	}
	if len(temporaryPAT) == 0 || !allZero(temporaryPAT) {
		t.Fatalf("a failed composition left the PAT key in memory: cleared=%v", allZero(temporaryPAT))
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
	attempts := compiledAttemptConfig(key)
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
	if graph.Routes() != nil || graph.BrowserRoutes() != nil || graph.MainEdgeAuth() != nil {
		t.Fatal("nil graph did not fail safely")
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

func unusedObserver(*materializedFiles) {
	panic("material read for invalid dependencies")
}

// The boot log names the Form users whose sign-in will be refused because
// their configuration has no usable address. The graph carries the report.
func TestNewFormGraphReportsFormUsersWithoutEmail(t *testing.T) {
	config := writeMaterialFixture(t)
	if err := os.WriteFile(config.Provider.Form.UsersJSONFile, []byte(`{"users":[
		{"login":"no-address","password":"correct horse battery staple one"},
		{"login":"has-address","password":"correct horse battery staple two","email":"has@example.test"}
	]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	graph, err := newFormGraph(
		context.Background(),
		config,
		FormGraphDependencies{PostgreSQL: newUnconnectedPool(t), MainRoutePublicRules: []forwardapp.PublicRule{}},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	report := graph.FormUsers()
	if report.Configured != 2 || !reflect.DeepEqual(report.MisconfiguredLogins, []string{"no-address"}) {
		t.Fatalf("Form user report = %+v", report)
	}
}

// Form sign-in is OFF unless the composition root says otherwise: the zero
// value of FormGraphDependencies is the safe one. With it off the graph still composes everything the edge and
// the runtime need, exposes NO browser routes, and its Form handler holds an
// empty user list, so even a caller that mounted Routes() could not sign in
// with a configured password.
func TestNewFormGraphWithFormSignInDisabledAcceptsNoPassword(t *testing.T) {
	config := writeMaterialFixture(t)
	graph, err := newFormGraph(
		context.Background(),
		config,
		FormGraphDependencies{
			PostgreSQL:           newUnconnectedPool(t),
			MainRoutePublicRules: []forwardapp.PublicRule{},
		},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if graph.BrowserRoutes() != nil {
		t.Fatal("Form sign-in is disabled, yet the graph exposes browser routes")
	}
	if graph.MainEdgeAuth() == nil || graph.ForwardedIdentityVerifier() == nil || graph.Routes() == nil {
		t.Fatal("disabling Form sign-in removed the edge composition the runtime depends on")
	}
	if graph.FormSignInEnabled() {
		t.Fatal("the graph does not report that Form sign-in is disabled")
	}
	if report := graph.FormUsers(); report.Configured != 1 {
		t.Fatalf("the report must still count the ignored users: %+v", report)
	}
	// That the composed Form handler refuses the configured password is proven
	// end to end in TestTheComposedFormHandlerRefusesConfiguredPasswordsWhenDisabled.
}

type sessionAuthorizerStub struct {
	authorization browserapp.Authorization
	err           error
}

func (s sessionAuthorizerStub) Authorize(context.Context, string) (browserapp.Authorization, error) {
	return s.authorization, s.err
}

// A Form session minted before Form sign-in was switched off must not keep
// authorizing for the rest of its cookie lifetime.
func TestFormSessionsRefusedWhileFormSignInIsDisabled(t *testing.T) {
	refused := formSessionsRefused{next: sessionAuthorizerStub{authorization: browserapp.Authorization{
		Provider: browserapp.FormProviderName,
	}}}
	if _, err := refused.Authorize(context.Background(), "session"); !errors.Is(err, browserapp.ErrUnauthenticated) {
		t.Fatalf("a Form session was authorized while Form sign-in is disabled: %v", err)
	}

	other := formSessionsRefused{next: sessionAuthorizerStub{authorization: browserapp.Authorization{Provider: "oidc"}}}
	if authorization, err := other.Authorize(context.Background(), "session"); err != nil || authorization.Provider != "oidc" {
		t.Fatalf("a non-Form session must pass through: %+v, %v", authorization, err)
	}

	failing := formSessionsRefused{next: sessionAuthorizerStub{err: browserapp.ErrDependencyUnavailable}}
	if _, err := failing.Authorize(context.Background(), "session"); !errors.Is(err, browserapp.ErrDependencyUnavailable) {
		t.Fatalf("a dependency failure must not be rewritten: %v", err)
	}
}
