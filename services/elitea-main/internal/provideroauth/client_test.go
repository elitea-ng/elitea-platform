package provideroauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func atlassianConfig(tokenURL string) ProviderConfig {
	return ProviderConfig{
		Provider: "jira", Flavor: FlavorAtlassian, ClientID: "client-1", ClientSecret: "client-secret-value",
		AuthorizeURL: "https://auth.example.com/authorize", TokenURL: tokenURL,
		RedirectURI: "https://elitea.example.com/api/v2/credential_consent/jira:callback",
		Scopes:      []string{"read:jira-work", "offline_access"},
	}
}

func TestAuthorizationURLCarriesStatePKCEAndTheExactRedirectURI(t *testing.T) {
	c, err := NewClient(atlassianConfig("https://auth.example.com/oauth/token"), nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(c.AuthorizationURL("state-1", "challenge-1"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	want := map[string]string{
		"response_type": "code", "client_id": "client-1", "state": "state-1",
		"redirect_uri":   "https://elitea.example.com/api/v2/credential_consent/jira:callback",
		"code_challenge": "challenge-1", "code_challenge_method": "S256",
		"audience": "api.atlassian.com", "prompt": "consent", "scope": "read:jira-work offline_access",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	if strings.Contains(u.String(), "client-secret-value") {
		t.Fatal("the client secret is in the authorization URL")
	}
}

func TestProviderConfigValidation(t *testing.T) {
	cases := map[string]func(*ProviderConfig){
		"redirect off the callback route": func(p *ProviderConfig) { p.RedirectURI = "https://elitea.example.com/callback" },
		"redirect for another provider": func(p *ProviderConfig) {
			p.RedirectURI = "https://e.example.com/api/v2/credential_consent/aha:callback"
		},
		"redirect with a query":    func(p *ProviderConfig) { p.RedirectURI += "?x=1" },
		"redirect with a fragment": func(p *ProviderConfig) { p.RedirectURI += "#x" },
		"redirect over plain http": func(p *ProviderConfig) {
			p.RedirectURI = "http://elitea.example.com/api/v2/credential_consent/jira:callback"
		},
		"token URL over plain http":        func(p *ProviderConfig) { p.TokenURL = "http://auth.example.com/oauth/token" },
		"token URL with user info":         func(p *ProviderConfig) { p.TokenURL = "https://u:p@auth.example.com/oauth/token" },
		"no client secret":                 func(p *ProviderConfig) { p.ClientSecret = "" },
		"unknown flavor":                   func(p *ProviderConfig) { p.Flavor = "saml" },
		"Atlassian without scopes":         func(p *ProviderConfig) { p.Scopes = nil },
		"a scope with a space or a quote":  func(p *ProviderConfig) { p.Scopes = []string{`a"b`} },
		"a repeated scope":                 func(p *ProviderConfig) { p.Scopes = []string{"a", "a"} },
		"a provider type that is not safe": func(p *ProviderConfig) { p.Provider = "Jira!" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := atlassianConfig("https://auth.example.com/oauth/token")
			mutate(&cfg)
			if _, err := NewClient(cfg, nil); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	ok := atlassianConfig("http://127.0.0.1:1/oauth/token")
	ok.RedirectURI = "http://localhost:8080/elitea/api/v2/credential_consent/jira:callback"
	if _, err := NewClient(ok, nil); err != nil {
		t.Fatalf("loopback http and a base path were refused: %v", err)
	}
	if s := fmt.Sprintf("%v %+v %#v", ok, ok, ok); strings.Contains(s, ok.ClientSecret) {
		t.Fatal("the client secret is printable")
	}
}

type tokenCall struct {
	contentType string
	fields      map[string]string
}

func tokenServer(t *testing.T, status int, body string, calls *[]tokenCall) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		call := tokenCall{contentType: r.Header.Get("Content-Type"), fields: map[string]string{}}
		if strings.HasPrefix(call.contentType, "application/json") {
			_ = json.Unmarshal(raw, &call.fields)
		} else {
			form, _ := url.ParseQuery(string(raw))
			for k := range form {
				call.fields[k] = form.Get(k)
			}
		}
		*calls = append(*calls, call)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestExchangeSendsTheVerifierAndTheExactRedirectURI(t *testing.T) {
	var calls []tokenCall
	srv := tokenServer(t, 200, `{"access_token":"at","refresh_token":"rt","expires_in":3600,"scope":"read:jira-work offline_access","token_type":"Bearer"}`, &calls)
	c, err := NewClient(atlassianConfig(srv.URL+"/oauth/token"), nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	tokens, err := c.Exchange(context.Background(), "code-1", "verifier-1")
	if err != nil {
		t.Fatal(err)
	}
	if tokens.AccessToken != "at" || tokens.RefreshToken != "rt" || !tokens.ExpiresAt.Equal(now.Add(time.Hour)) ||
		len(tokens.Scopes) != 2 {
		t.Fatalf("tokens: expires=%s scopes=%v", tokens.ExpiresAt, tokens.Scopes)
	}
	got := calls[0]
	if got.contentType != "application/json" || got.fields["grant_type"] != "authorization_code" ||
		got.fields["code_verifier"] != "verifier-1" || got.fields["code"] != "code-1" ||
		got.fields["redirect_uri"] != "https://elitea.example.com/api/v2/credential_consent/jira:callback" ||
		got.fields["client_secret"] != "client-secret-value" {
		t.Fatalf("token request: %+v", got)
	}
	if fmt.Sprintf("%v %+v %#v", tokens, tokens, tokens) != fmt.Sprintf("%v %v %v", tokens.String(), tokens.String(), tokens.String()) {
		t.Fatal("token response formats past its redaction")
	}
}

func TestAhaUsesAFormBodyAndKeepsTheRefreshToken(t *testing.T) {
	var calls []tokenCall
	srv := tokenServer(t, 200, `{"access_token":"at2","token_type":"bearer"}`, &calls)
	cfg := ProviderConfig{
		Provider: "aha", Flavor: FlavorAha, ClientID: "c", ClientSecret: "s",
		AuthorizeURL: srv.URL + "/oauth/authorize", TokenURL: srv.URL + "/oauth/token",
		RedirectURI: "https://elitea.example.com/api/v2/credential_consent/aha:callback",
	}
	c, err := NewClient(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if q, _ := url.Parse(c.AuthorizationURL("s", "c")); q.Query().Has("scope") || q.Query().Has("audience") {
		t.Fatal("Aha! authorization carries Atlassian parameters")
	}
	tokens, err := c.Refresh(context.Background(), "rt-old")
	if err != nil {
		t.Fatal(err)
	}
	if tokens.RefreshToken != "" || !tokens.ExpiresAt.IsZero() {
		t.Fatal("an answer without a refresh token or a lifetime grew one")
	}
	if calls[0].contentType != "application/x-www-form-urlencoded" || calls[0].fields["refresh_token"] != "rt-old" ||
		calls[0].fields["grant_type"] != "refresh_token" {
		t.Fatalf("refresh request: %+v", calls[0])
	}
}

func TestTokenEndpointErrorsAreTypedAndCarryNoBody(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   error
	}{
		{400, `{"error":"invalid_grant","error_description":"refresh token rt-secret is used"}`, ErrInvalidGrant},
		{401, `{"error":"invalid_client","error_description":"rt-secret"}`, ErrProviderRejected},
		{400, `not json rt-secret`, ErrProviderRejected},
		{500, `rt-secret`, ErrProviderUnavailable},
		{429, `{}`, ErrProviderUnavailable},
		{200, `{"token_type":"bearer"}`, ErrProviderRejected},
		{200, `{"access_token":"x","token_type":"mac"}`, ErrProviderRejected},
		{302, ``, ErrProviderRejected},
	}
	for _, tc := range cases {
		var calls []tokenCall
		srv := tokenServer(t, tc.status, tc.body, &calls)
		c, err := NewClient(atlassianConfig(srv.URL+"/oauth/token"), nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Refresh(context.Background(), "rt-secret")
		if !errors.Is(err, tc.want) {
			t.Errorf("%d %q: err = %v, want %v", tc.status, tc.body, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "rt-secret") {
			t.Errorf("%d: the error carries the token: %v", tc.status, err)
		}
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var followed atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		followed.Add(1)
		_, _ = io.WriteString(w, `{"access_token":"stolen"}`)
	}))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	c, err := NewClient(atlassianConfig(srv.URL+"/oauth/token"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exchange(context.Background(), "code", "verifier"); !errors.Is(err, ErrProviderRejected) || followed.Load() != 0 {
		t.Fatalf("err = %v, followed %d", err, followed.Load())
	}
}

// The PKCE verifier is required: an exchange without one never leaves the
// process.
func TestExchangeWithoutAVerifierIsRefusedLocally(t *testing.T) {
	var calls []tokenCall
	srv := tokenServer(t, 200, `{"access_token":"at"}`, &calls)
	c, err := NewClient(atlassianConfig(srv.URL+"/oauth/token"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exchange(context.Background(), "code", ""); !errors.Is(err, ErrProviderRejected) || len(calls) != 0 {
		t.Fatalf("err = %v, calls %d", err, len(calls))
	}
}

func TestTimeoutIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(500 * time.Millisecond)
	}))
	defer srv.Close()
	cfg := atlassianConfig(srv.URL + "/oauth/token")
	cfg.Timeout = 50 * time.Millisecond
	c, err := NewClient(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Refresh(context.Background(), "rt"); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestConfigFromEnv(t *testing.T) {
	env := func(values map[string]string) func(string) (string, bool) {
		return func(name string) (string, bool) { v, ok := values[name]; return v, ok }
	}
	none, err := ConfigFromEnv(env(nil))
	if err != nil || len(none) != 0 {
		t.Fatalf("empty environment: %v %v", none, err)
	}
	cfgs, err := ConfigFromEnv(env(map[string]string{
		jiraClientIDEnv: "c", jiraClientSecretEnv: "s",
		jiraRedirectURIEnv: "https://e.example.com/api/v2/credential_consent/jira:callback",
		ahaClientIDEnv:     "c2", ahaClientSecretEnv: "s2",
		ahaRedirectURIEnv:   "https://e.example.com/api/v2/credential_consent/aha:callback",
		ahaAuthorizeURLEnv:  "https://acme.aha.io/oauth/authorize",
		ahaTokenURLEnv:      "https://acme.aha.io/oauth/token",
		ahaTimeoutEnv:       "5s",
		confluenceScopesEnv: "",
	}))
	if err != nil || len(cfgs) != 2 {
		t.Fatalf("configs %d err %v", len(cfgs), err)
	}
	if cfgs[0].TokenURL != AtlassianTokenURL || cfgs[0].AuthorizeURL != AtlassianAuthorizeURL || len(cfgs[0].Scopes) == 0 {
		t.Fatalf("Atlassian defaults not applied: %v", cfgs[0])
	}
	if cfgs[1].Timeout != 5*time.Second {
		t.Fatalf("Aha! timeout %s", cfgs[1].Timeout)
	}
	if _, err := ConfigFromEnv(env(map[string]string{jiraClientSecretEnv: "s"})); err == nil {
		t.Fatal("a secret without a client id was accepted")
	}
	if _, err := ConfigFromEnv(env(map[string]string{
		jiraClientIDEnv: "c", jiraClientSecretEnv: "s",
		jiraRedirectURIEnv: "https://e.example.com/api/v2/credential_consent/jira:callback", jiraTimeoutEnv: "2h",
	})); err == nil {
		t.Fatal("a timeout above one minute was accepted")
	}
}

func TestSealBindsTheRowAndThePurpose(t *testing.T) {
	s, err := newSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	aad := GrantAAD{TenantProjectID: 1, GrantID: "g", Provider: "jira", TargetBindingID: "t", SubjectUserID: 7, AgentBindingID: "b"}
	sealed := s.seal("refresh-token", aad.with(purposeRefresh))
	if got, err := s.open(sealed, aad.with(purposeRefresh)); err != nil || got != "refresh-token" {
		t.Fatalf("open = %q %v", got, err)
	}
	moved := []GrantAAD{aad.with(purposeAccess)}
	for _, mutate := range []func(*GrantAAD){
		func(a *GrantAAD) { a.TenantProjectID = 2 }, func(a *GrantAAD) { a.GrantID = "h" },
		func(a *GrantAAD) { a.Provider = "aha" }, func(a *GrantAAD) { a.TargetBindingID = "u" },
		func(a *GrantAAD) { a.SubjectUserID = 8 }, func(a *GrantAAD) { a.AgentBindingID = "c" },
	} {
		other := aad.with(purposeRefresh)
		mutate(&other)
		moved = append(moved, other)
	}
	for i, other := range moved {
		if _, err := s.open(sealed, other); !errors.Is(err, ErrCustodyIntegrity) {
			t.Errorf("case %d: a moved ciphertext opened", i)
		}
	}
	if _, err := newSealer(make([]byte, 16)); err == nil {
		t.Fatal("a short master key was accepted")
	}
}
