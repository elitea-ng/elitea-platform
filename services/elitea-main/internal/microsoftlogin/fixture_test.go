package microsoftlogin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftapps"
)

// The Entra fixture: authorize (what a browser would do at login.microsoftonline.com),
// the token endpoint with PKCE and client secret checks, and the identity reads of
// Graph. It is an http.RoundTripper-shaped Doer, so no socket is opened, and it
// records every request the service made so a test can prove where a secret went.

const (
	fixtureTenant = "11111111-1111-1111-1111-111111111111"
	fixtureOID    = "22222222-2222-2222-2222-222222222222"
	fixtureClient = "33333333-3333-3333-3333-333333333333"
	fixtureSecret = "client-secret-PLANTED-7f3a"
	refreshToken  = "refresh-token-PLANTED-9b1c"
)

type grant struct {
	clientID, challenge, redirectURI, scope string
	used                                    bool
}

type fixture struct {
	mu       sync.Mutex
	grants   map[string]*grant
	tokens   map[string]string // access token -> client id
	secrets  map[string]string // client id -> expected secret ("" = none)
	requests []string
	// secretHosts records the host and path of every request that carried a client secret.
	secretHosts []string
	counter     int

	tid, oid, name string
	forbidGraph    bool
	rejectGraph    bool
	failToken      bool
	expiresIn      int
}

func newFixture() *fixture {
	return &fixture{
		grants: map[string]*grant{}, tokens: map[string]string{},
		secrets: map[string]string{fixtureClient: fixtureSecret},
		tid:     fixtureTenant, oid: fixtureOID, name: "Alex Example", expiresIn: 3599,
	}
}

// authorize is what the browser does after Main sends the person to the URL: it
// checks the URL points at Entra's login host and returns the code Entra would put in
// the callback.
func (f *fixture) authorize(authorizationURL string) (state, code string, err error) {
	parsed, err := url.Parse(authorizationURL)
	if err != nil {
		return "", "", err
	}
	if parsed.Scheme != "https" || parsed.Host != "login.microsoftonline.com" || !strings.HasSuffix(parsed.Path, "/oauth2/v2.0/authorize") {
		return "", "", fmt.Errorf("the person was sent to %s%s", parsed.Host, parsed.Path)
	}
	q := parsed.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("response_type") != "code" || q.Get("state") == "" {
		return "", "", fmt.Errorf("the authorization request is not PKCE S256 with a state")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counter++
	code = fmt.Sprintf("auth-code-%d-PLANTED", f.counter)
	f.grants[code] = &grant{clientID: q.Get("client_id"), challenge: q.Get("code_challenge"), redirectURI: q.Get("redirect_uri"), scope: q.Get("scope")}
	return q.Get("state"), code, nil
}

func (f *fixture) do(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req.Method+" "+req.URL.Host+req.URL.Path)
	switch {
	case req.URL.Host == "login.microsoftonline.com" && strings.HasSuffix(req.URL.Path, "/oauth2/v2.0/token"):
		return f.token(req)
	case req.URL.Host == "graph.microsoft.com":
		return f.graph(req)
	}
	return reply(http.StatusBadGateway, map[string]any{"error": "unexpected host"}), nil
}

func reply(status int, body any) *http.Response {
	raw, _ := json.Marshal(body)
	return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(raw)), Header: http.Header{}}
}

func (f *fixture) token(req *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(req.Body)
	form, _ := url.ParseQuery(string(raw))
	if form.Get("client_secret") != "" {
		f.secretHosts = append(f.secretHosts, req.URL.Host+req.URL.Path)
	}
	if f.failToken {
		return reply(http.StatusBadRequest, map[string]any{"error": "invalid_grant", "correlation_id": "c0ffee00-0000-0000-0000-000000000001", "error_description": "AADSTS70008: the code " + form.Get("code") + " expired"}), nil
	}
	g := f.grants[form.Get("code")]
	switch {
	case form.Get("grant_type") != "authorization_code" || g == nil || g.used:
		return reply(http.StatusBadRequest, map[string]any{"error": "invalid_grant"}), nil
	case g.clientID != form.Get("client_id") || g.redirectURI != form.Get("redirect_uri"):
		return reply(http.StatusBadRequest, map[string]any{"error": "invalid_grant"}), nil
	}
	sum := sha256.Sum256([]byte(form.Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		return reply(http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "PKCE verification failed"}), nil
	}
	if want, ok := f.secrets[g.clientID]; ok && form.Get("client_secret") != want {
		return reply(http.StatusUnauthorized, map[string]any{"error": "invalid_client"}), nil
	}
	g.used = true
	f.counter++
	access := fmt.Sprintf("access-token-%d-PLANTED", f.counter)
	f.tokens[access] = g.clientID
	claims, _ := json.Marshal(map[string]any{"aud": g.clientID, "tid": f.tid, "oid": f.oid, "name": f.name, "preferred_username": "alex@example.com"})
	idToken := "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
	return reply(http.StatusOK, map[string]any{
		"token_type": "Bearer", "access_token": access, "refresh_token": refreshToken,
		"id_token": idToken, "expires_in": f.expiresIn, "scope": g.scope,
	}), nil
}

func (f *fixture) graph(req *http.Request) (*http.Response, error) {
	token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	if f.rejectGraph || f.tokens[token] == "" {
		return reply(http.StatusUnauthorized, map[string]any{}), nil
	}
	if f.forbidGraph {
		return reply(http.StatusForbidden, map[string]any{}), nil
	}
	return reply(http.StatusOK, map[string]any{"value": []any{}}), nil
}

func (f *fixture) sawHost(host string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		if strings.Contains(r, " "+host+"/") {
			return true
		}
	}
	return false
}

func (f *fixture) onlyEntraAndGraph() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		host := strings.SplitN(strings.TrimPrefix(strings.TrimPrefix(r, "GET "), "POST "), "/", 2)[0]
		if host != "login.microsoftonline.com" && host != "graph.microsoft.com" {
			return fmt.Errorf("a request left for %s", r)
		}
	}
	return nil
}

// fakeCredentials serves saved credentials by id.
type fakeCredentials struct {
	mu    sync.Mutex
	items map[string]Credential
}

func (c *fakeCredentials) Load(_ context.Context, project int64, id string) (Credential, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.items[fmt.Sprintf("%d/%s", project, id)]
	if !ok {
		return Credential{}, ErrCredentialNotFound
	}
	copyData := map[string]any{}
	for k, v := range item.Data {
		copyData[k] = v
	}
	item.Data = copyData
	return item, nil
}

func (c *fakeCredentials) put(project int64, item Credential) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = map[string]Credential{}
	}
	c.items[fmt.Sprintf("%d/%s", project, item.ID)] = item
}

// fakeApps is the registry.
type fakeApps struct {
	mu          sync.Mutex
	resolved    microsoftapps.Resolved
	err         error
	customAllow bool
	requests    []microsoftapps.Request
}

func (a *fakeApps) Resolve(_ context.Context, req microsoftapps.Request) (microsoftapps.Resolved, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = append(a.requests, req)
	return a.resolved, a.err
}

func (a *fakeApps) CustomAppsAllowed(context.Context) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.customAllow
}

func (a *fakeApps) set(f func(*fakeApps)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f(a)
}

// clock is a movable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}
