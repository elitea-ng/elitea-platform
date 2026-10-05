//go:build conformance

package nativeclient_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/conformance/nativeclient/browser"
	"github.com/EliteaAI/elitea-platform/conformance/nativeclient/client"
)

// config is everything the suite reads from its environment. The defaults
// match deploy/scripts/native-conformance.sh and the E2E seed.
type config struct {
	// Origin is the deployment origin a user types into the app.
	Origin string
	// Leg selects the sign-in planes the deployment composes: `sso` (OIDC,
	// plus SAML authored at run time; what CI runs) or `form`. A Form leg
	// needs a deployment whose browser edge runs the Form plane's ForwardAuth
	// hop: the native continue route never reads the Form cookie directly.
	// The standalone stack has no such edge (deploy/scripts/
	// native-conformance.sh says why), so CI has no form leg.
	// That deployment must also set ELITEA_FORM_LOGIN_ENABLED=true: Form
	// sign-in is off by default, and with it off no Form password is accepted.
	Leg         string
	ClientID    string
	RedirectURI string
	// User is the persona the app signs in as (the E2E chat driver: its
	// personal project holds the chat grants). Admin is the persona whose
	// console session registers the client and edits the policy.
	User  string
	Admin string
	// FormUsersFile is the Form provider's users document (leg form): the
	// personas' passwords are read from it, never typed into this file.
	FormUsersFile string
	// Model is the chat model the stack seeded (seed-llm's offline mock).
	Model string
	// SAMLUser signs in through the SAML provider the suite authors. It is a
	// persona the OIDC scenarios never use: an account already linked to one
	// federated subject is not adopted by another (identityrepo adoption).
	SAMLUser string
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func loadConfig(t *testing.T) config {
	t.Helper()
	cfg := config{
		Origin:        strings.TrimRight(env("ELITEA_CONFORMANCE_ORIGIN", ""), "/"),
		Leg:           env("ELITEA_CONFORMANCE_LEG", "sso"),
		ClientID:      env("ELITEA_CONFORMANCE_CLIENT_ID", "dev.elitea.conformance"),
		RedirectURI:   env("ELITEA_CONFORMANCE_REDIRECT_URI", "dev.elitea.conformance:/oauth/callback"),
		User:          env("ELITEA_CONFORMANCE_USER", "e2e-chat@autotest.local"),
		Admin:         env("ELITEA_CONFORMANCE_ADMIN", "e2e-admin@autotest.local"),
		FormUsersFile: env("ELITEA_CONFORMANCE_FORM_USERS_FILE", ""),
		Model:         env("ELITEA_CONFORMANCE_MODEL", "vllm/E2E-MOCK-MODEL"),
		SAMLUser:      env("ELITEA_CONFORMANCE_SAML_USER", "e2e-member@autotest.local"),
	}
	// A missing origin is a FAILURE, not a skip: this file only compiles under
	// the `conformance` tag, so whoever built it meant to run it.
	if cfg.Origin == "" {
		t.Fatal("ELITEA_CONFORMANCE_ORIGIN is not set (e.g. http://localhost:8084)")
	}
	switch cfg.Leg {
	case "sso":
	case "form":
		if cfg.FormUsersFile == "" {
			t.Fatal("leg form needs ELITEA_CONFORMANCE_FORM_USERS_FILE (the Form provider's users document)")
		}
	default:
		t.Fatalf("ELITEA_CONFORMANCE_LEG=%q: want sso or form", cfg.Leg)
	}
	return cfg
}

// formPassword reads a persona's password from the Form users document.
func (cfg config) formPassword(t *testing.T, email string) (login, password string) {
	t.Helper()
	raw, err := os.ReadFile(cfg.FormUsersFile)
	if err != nil {
		t.Fatalf("read the Form users document: %v", err)
	}
	var document struct {
		Users []struct {
			Login    string `json:"login"`
			Password string `json:"password"`
			Email    string `json:"email"`
		} `json:"users"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("parse the Form users document: %v", err)
	}
	for _, user := range document.Users {
		if strings.EqualFold(user.Email, email) || strings.EqualFold(user.Login, email) {
			return user.Login, user.Password
		}
	}
	t.Fatalf("the Form users document has no user for %s; run native-conformance.sh form-users before up", email)
	return "", ""
}

// signInStrategies answers the deployment's own sign-in pages for one persona.
func (cfg config) signInStrategies(t *testing.T, persona, provider string) []browser.Strategy {
	t.Helper()
	switch {
	case cfg.Leg == "form":
		login, password := cfg.formPassword(t, persona)
		return []browser.Strategy{browser.FormLogin(login, password)}
	case provider == "saml":
		return []browser.Strategy{browser.Chooser("saml")}
	default:
		return []browser.Strategy{browser.Chooser("oidc"), browser.OIDCMock(persona)}
	}
}

func testContext(t *testing.T, timeout time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	return ctx
}

// consoleSession signs a persona in to the web console (no native client
// involved) and returns an API client that rides its session cookie: the
// administrator's tool for registering the client and editing the policy.
func (cfg config) consoleSession(t *testing.T, persona string) *client.Client {
	t.Helper()
	ctx := testContext(t, 2*time.Minute)
	b := browser.New()
	// Sign in the way the web app does, returning to the app shell. The
	// drive ends at the SPA (or an edge page), which no strategy answers, so
	// its verdict is not the proof; the authenticated API call below is.
	start := cfg.Origin + "/auth/login?target_to=" + url.QueryEscape("/app/")
	if callback, _, _ := b.Drive(ctx, start, "deny", cfg.signInStrategies(t, persona, "oidc")...); callback != nil {
		t.Fatalf("console sign-in for %s ended at an app callback %s", persona, callback)
	}
	api := client.New(cfg.Origin)
	api.HTTP = b.HTTPClient()
	projects, err := api.Get(ctx, client.ProjectsPath)
	if err != nil {
		t.Fatalf("console sign-in for %s: %v", persona, err)
	}
	if projects.Status != http.StatusOK {
		t.Fatalf("console sign-in for %s did not authenticate: %s (trail: %s)", persona, projects,
			strings.Join(b.Trail, " → "))
	}
	return api
}

// session is one native device session.
type session struct {
	cfg       config
	discovery client.Discovery
	tokens    client.Tokens
	api       *client.Client
	device    string
}

// authorization is a completed browser leg: the code at the redirect URI and
// the verifier only this app knows.
type authorization struct {
	code     string
	verifier string
}

// authorize runs the browser half of RFC 8252 for one device: the system
// browser to the authorization endpoint, the deployment's own sign-in, the
// consent page, then the private-use redirect, whose state and iss it checks.
func (cfg config) authorize(t *testing.T, discovery client.Discovery, persona, provider, device string) authorization {
	t.Helper()
	ctx := testContext(t, 2*time.Minute)
	if discovery.NativeAuth == nil {
		t.Fatal("discovery has no native_auth: no native client is registered")
	}
	verifier, state := client.NewVerifier(), client.NewState()
	authorizeURL := client.AuthorizeURL(discovery.NativeAuth.AuthorizationEndpoint, client.AuthorizationRequest{
		ClientID: cfg.ClientID, RedirectURI: cfg.RedirectURI, State: state,
		Challenge: client.S256Challenge(verifier), DeviceName: device, Platform: "linux",
		ClientVersion: "1.0.0",
	})
	b := browser.New()
	callbackURL, consents, err := b.Drive(ctx, authorizeURL, "allow", cfg.signInStrategies(t, persona, provider)...)
	if err != nil {
		t.Fatalf("native sign-in (%s, %s): %v", cfg.Leg, provider, err)
	}
	if consents != 1 {
		t.Fatalf("native sign-in answered %d consent pages, want exactly 1 (trail: %s)", consents,
			strings.Join(b.Trail, " → "))
	}
	if !strings.HasPrefix(callbackURL.String(), cfg.RedirectURI+"?") {
		t.Fatalf("callback %q is not the registered redirect URI %q", callbackURL, cfg.RedirectURI)
	}
	callback := client.ParseCallback(callbackURL)
	if err := callback.Verify(state, discovery.NativeAuth.Issuer); err != nil {
		t.Fatalf("authorization response: %v", err)
	}
	return authorization{code: callback.Code, verifier: verifier}
}

// signIn is authorize plus the code exchange.
func (cfg config) signIn(t *testing.T, discovery client.Discovery, persona, provider, device string) session {
	t.Helper()
	ctx := testContext(t, time.Minute)
	granted := cfg.authorize(t, discovery, persona, provider, device)
	api := client.New(cfg.Origin).WithVersion("1.0.0")
	tokens, err := api.ExchangeCode(ctx, discovery.NativeAuth.TokenEndpoint, cfg.ClientID, cfg.RedirectURI,
		granted.code, granted.verifier)
	if err != nil {
		t.Fatalf("code exchange: %v", err)
	}
	return session{cfg: cfg, discovery: discovery, tokens: tokens, api: api.WithToken(tokens.AccessToken), device: device}
}

// refresh rotates the session's refresh token.
func (s *session) refresh(ctx context.Context, t *testing.T) client.Tokens {
	t.Helper()
	tokens, err := s.api.Refresh(ctx, s.discovery.NativeAuth.TokenEndpoint, s.cfg.ClientID, s.tokens.RefreshToken)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	s.tokens = tokens
	s.api = s.api.WithToken(tokens.AccessToken)
	return tokens
}

func mustJSON(t *testing.T, response *client.Response, want int) map[string]any {
	t.Helper()
	if response.Status != want {
		t.Fatalf("want HTTP %d, got %s", want, response)
	}
	body, err := response.Map()
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// need fails the test on a transport error: need(t)(api.Get(ctx, path)).
func need(t *testing.T) func(*client.Response, error) *client.Response {
	t.Helper()
	return func(response *client.Response, err error) *client.Response {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
}

func asInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		return int64(typed), true
	case string:
		var out int64
		_, err := fmt.Sscan(typed, &out)
		return out, err == nil
	}
	return 0, false
}
