package governedmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/egress"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/provideroauth"
)

const (
	testRedirect = "https://elitea.example.com/api/v2/credential_consent/mcp:callback"
	testCIMD     = "https://elitea.example.com" + ClientMetadataPath
)

// mockServer is one remote MCP server with its own authorization server,
// both in process: RFC 9728 resource metadata, RFC 8414 metadata, RFC 7591
// registration and a token endpoint that records every request.
type mockServer struct {
	*httptest.Server
	cimd     bool
	resource string // overrides the metadata's resource when set

	// issuer overrides the authorization server the metadata names.
	issuer string

	mu         sync.Mutex
	tokenForms []url.Values
	registered int
	requests   int
}

func (m *mockServer) requestCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.requests
}

func newMockServer(t *testing.T, cimd bool) *mockServer {
	t.Helper()
	m := &mockServer{cimd: cimd}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) {
		resource := m.URL + "/mcp"
		if m.resource != "" {
			resource = m.resource
		}
		issuer := m.URL + "/as"
		if m.issuer != "" {
			issuer = m.issuer
		}
		writeJSONT(w, map[string]any{"resource": resource, "authorization_servers": []string{issuer}, "scopes_supported": []string{"tools.call"}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server/as", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONT(w, map[string]any{
			"issuer": m.URL + "/as", "authorization_endpoint": m.URL + "/as/authorize", "token_endpoint": m.URL + "/as/token",
			"registration_endpoint": m.URL + "/as/register", "code_challenge_methods_supported": []string{"S256"},
			"client_id_metadata_document_supported": m.cimd,
		})
	})
	mux.HandleFunc("/as/register", func(w http.ResponseWriter, _ *http.Request) {
		m.mu.Lock()
		m.registered++
		m.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		writeJSONT(w, map[string]any{"client_id": "dcr-client", "client_secret": "dcr-secret"})
	})
	mux.HandleFunc("/as/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		m.mu.Lock()
		m.tokenForms = append(m.tokenForms, r.PostForm)
		count := len(m.tokenForms)
		m.mu.Unlock()
		writeJSONT(w, map[string]any{
			"access_token": "access-" + string(rune('0'+count)), "refresh_token": "refresh-" + string(rune('0'+count)),
			"token_type": "Bearer", "expires_in": 600, "scope": "tools.call",
		})
	})
	m.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.requests++
		m.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(m.Close)
	return m
}

func (m *mockServer) target() credentialgrant.TargetBinding {
	return credentialgrant.TargetBinding{ID: "t", TenantProjectID: 1, ProviderType: ProviderType, TargetIdentity: m.URL + "/mcp"}
}

func (m *mockServer) lastForm(t *testing.T) url.Values {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.tokenForms) == 0 {
		t.Fatal("no token request")
	}
	return m.tokenForms[len(m.tokenForms)-1]
}

func writeJSONT(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func newService(t *testing.T, m *mockServer, dcr bool) *Service {
	t.Helper()
	return newGuardedService(t, m, dcr, allowOnly(t, m))
}

// allowOnly is the operator allowlist naming exactly m's loopback address
// and port, as ELITEA_MCP_OAUTH_EGRESS_ALLOWLIST would for a private
// identity provider.
func allowOnly(t *testing.T, m *mockServer) *egress.Guard {
	t.Helper()
	u, err := url.Parse(m.URL)
	if err != nil {
		t.Fatal(err)
	}
	allowlist, err := egress.ParseAllowlist([]string{u.Host})
	if err != nil {
		t.Fatal(err)
	}
	return egress.New(allowlist)
}

func newGuardedService(t *testing.T, m *mockServer, dcr bool, guard *egress.Guard) *Service {
	t.Helper()
	s, err := New(Config{RedirectURI: testRedirect, ClientMetadataURL: testCIMD, AllowDynamicRegistration: dcr,
		Guard: guard, HTTPClient: m.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Acceptance (#1123): the authorization request, the code exchange and the
// refresh all carry the RFC 8707 resource, and a CIMD-capable server gets
// the metadata document URL as the client id with no registration.
func TestAuthorizationAndTokenRequestsCarryTheResource(t *testing.T) {
	m := newMockServer(t, true)
	s := newService(t, m, true)
	resource := m.URL + "/mcp"

	authorizer, sealed, err := s.Begin(context.Background(), m.target())
	if err != nil {
		t.Fatal(err)
	}
	authURL, err := url.Parse(authorizer.AuthorizationURL("state-1", "challenge-1"))
	if err != nil {
		t.Fatal(err)
	}
	q := authURL.Query()
	if authURL.Path != "/as/authorize" || q.Get("resource") != resource || q.Get("client_id") != testCIMD ||
		q.Get("code_challenge_method") != "S256" || q.Get("redirect_uri") != testRedirect || q.Get("scope") != "tools.call" {
		t.Fatalf("authorization request %s", authURL)
	}
	if m.registered != 0 {
		t.Fatal("a CIMD-capable server was registered with dynamically")
	}

	resumed, err := s.Resume(context.Background(), m.target(), sealed)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := resumed.Exchange(context.Background(), "code-1", "verifier-1")
	if err != nil {
		t.Fatal(err)
	}
	form := m.lastForm(t)
	if form.Get("resource") != resource || form.Get("grant_type") != "authorization_code" || form.Get("code_verifier") != "verifier-1" ||
		form.Get("client_id") != testCIMD || form.Get("client_secret") != "" {
		t.Fatalf("code exchange form %v", form)
	}
	if tokens.AccessToken != "access-1" || tokens.ExpiresAt.IsZero() || !strings.Contains(tokens.RefreshToken, `"refresh_token":"refresh-1"`) ||
		!strings.Contains(tokens.RefreshToken, `"resource":"`+resource+`"`) {
		t.Fatalf("tokens %+v custody %q", tokens, tokens.RefreshToken)
	}

	refreshed, err := s.Refresh(context.Background(), tokens.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	form = m.lastForm(t)
	if form.Get("resource") != resource || form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "refresh-1" {
		t.Fatalf("refresh form %v", form)
	}
	if refreshed.AccessToken != "access-2" || !strings.Contains(refreshed.RefreshToken, `"refresh_token":"refresh-2"`) {
		t.Fatalf("refreshed %+v", refreshed)
	}
	if _, err := s.Refresh(context.Background(), "plain-refresh-token"); !errors.Is(err, provideroauth.ErrInvalidGrant) {
		t.Fatalf("an unsealed refresh token refreshed: %v", err)
	}
}

// Client registration preference: CIMD first; dynamic registration only
// when the server lacks CIMD and the operator allows it.
func TestDynamicRegistrationIsTheFallbackOnly(t *testing.T) {
	m := newMockServer(t, false)
	if _, _, err := newService(t, m, false).Begin(context.Background(), m.target()); !errors.Is(err, ErrRegistrationUnavailable) {
		t.Fatalf("no CIMD, registration not allowed: %v", err)
	}
	if m.registered != 0 {
		t.Fatal("registered without permission")
	}
	s := newService(t, m, true)
	authorizer, sealed, err := s.Begin(context.Background(), m.target())
	if err != nil {
		t.Fatal(err)
	}
	if m.registered != 1 || !strings.Contains(authorizer.AuthorizationURL("s", "c"), "client_id=dcr-client") {
		t.Fatalf("registered %d times", m.registered)
	}
	resumed, err := s.Resume(context.Background(), m.target(), sealed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Exchange(context.Background(), "code", "verifier"); err != nil {
		t.Fatal(err)
	}
	if form := m.lastForm(t); form.Get("client_id") != "dcr-client" || form.Get("client_secret") != "dcr-secret" || m.registered != 1 {
		t.Fatalf("exchange form %v, registrations %d", form, m.registered)
	}
}

// Target binding: a registration made for server A never completes a
// consent for server B, and metadata naming another resource is refused.
func TestARegistrationForServerANeverServesServerB(t *testing.T) {
	a, b := newMockServer(t, true), newMockServer(t, true)
	s := newService(t, a, true)
	_, sealed, err := s.Begin(context.Background(), a.target())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resume(context.Background(), b.target(), sealed); !errors.Is(err, provideroauth.ErrStateInvalid) {
		t.Fatalf("server A's registration resumed for server B: %v", err)
	}
	b.resource = a.URL + "/mcp"
	if _, _, err := newService(t, b, true).Begin(context.Background(), b.target()); !errors.Is(err, ErrMetadata) {
		t.Fatalf("metadata naming another resource: %v", err)
	}
	other := b.target()
	other.ProviderType = "jira"
	if _, _, err := s.Begin(context.Background(), other); !errors.Is(err, provideroauth.ErrTargetNotFound) {
		t.Fatalf("a jira target: %v", err)
	}
	if !EnvelopeServes(a.URL+"/mcp", a.URL+"/mcp") || EnvelopeServes(a.URL+"/mcp", b.URL+"/mcp") || EnvelopeServes("", a.URL+"/mcp") {
		t.Fatal("EnvelopeServes does not bind the token to its target")
	}
}

func TestClientMetadataDocument(t *testing.T) {
	m := newMockServer(t, true)
	recorder := httptest.NewRecorder()
	newService(t, m, false).ServeClientMetadata(recorder, httptest.NewRequest(http.MethodGet, ClientMetadataPath, nil))
	var doc map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["client_id"] != testCIMD || doc["token_endpoint_auth_method"] != "none" || !bytes.Contains(recorder.Body.Bytes(), []byte(testRedirect)) {
		t.Fatalf("document %v", doc)
	}
}

func TestNewRefusesAForeignRedirect(t *testing.T) {
	if _, err := New(Config{RedirectURI: "https://elitea.example.com/api/v2/credential_consent/jira:callback", ClientMetadataURL: testCIMD,
		Guard: egress.New(nil)}); err == nil {
		t.Fatal("a jira redirect URI was accepted")
	}
	if _, err := New(Config{RedirectURI: testRedirect, ClientMetadataURL: testCIMD}); err == nil {
		t.Fatal("a service without the egress guard was built")
	}
}

// SSRF (#1123 review): every request goes through the MCP authorization
// egress guard. A tenant's MCP server on a loopback address the operator
// did not allow is never fetched, and metadata naming a loopback
// authorization server the allowlist does not cover is refused before any
// connection is made to it.
func TestMetadataNamingAPrivateHostIsRefusedBeforeAnyConnection(t *testing.T) {
	m := newMockServer(t, true)
	if _, _, err := newGuardedService(t, m, true, egress.New(nil)).Begin(context.Background(), m.target()); !errors.Is(err, ErrMetadata) {
		t.Fatalf("a loopback MCP server with no allowlist: %v", err)
	}
	if got := m.requestCount(); got != 0 {
		t.Fatalf("the refused loopback server got %d requests", got)
	}

	internal := newMockServer(t, true)
	m.issuer = internal.URL + "/as"
	if _, _, err := newService(t, m, true).Begin(context.Background(), m.target()); !errors.Is(err, ErrMetadata) {
		t.Fatalf("metadata naming an internal authorization server: %v", err)
	}
	if got := internal.requestCount(); got != 0 {
		t.Fatalf("the internal authorization server got %d requests", got)
	}
	if got := m.requestCount(); got == 0 {
		t.Fatal("the allowed MCP server's metadata was never read")
	}
}

// Acceptance (#1123): the authorization-required reference is an opaque,
// printable token of the shape the worker carries (and #1061 answers),
// bound to its user and tenant project and to nothing else.
func TestAuthorizationReferenceIsBoundAndOpaque(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	refs, err := NewReferences(key, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	req := provideroauth.AuthorizationRequirement{
		Key: provideroauth.GrantKey{TenantProjectID: 42, Provider: ProviderType, TargetBindingID: "0192f0aa-0000-7000-8000-00000000000a",
			SubjectUserID: 7, AgentBindingID: "0192f0aa-0000-7000-8000-000000000001"},
		ExecutionID: "execution-1", Generation: 3,
		ConversationUUID: "5f5a1ad4-2b30-4a54-9b7f-2d05a0d3f6c1", ResponseMessageID: "6f5a1ad4-2b30-4a54-9b7f-2d05a0d3f6c2",
	}
	reference, err := refs.Mint(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(reference, ReferencePrefix) || len(reference) > 4096 || strings.Contains(reference, "0192f0aa") {
		t.Fatalf("reference %q", reference)
	}
	for _, c := range reference {
		if c < 0x21 || c > 0x7e {
			t.Fatalf("unprintable reference byte %q", c)
		}
	}
	opened, err := refs.Open(reference, 42, 7)
	if err != nil || opened.Key != req.Key || opened.ExecutionID != req.ExecutionID || opened.ConversationUUID != req.ConversationUUID {
		t.Fatalf("opened %+v, %v", opened, err)
	}
	other, _ := NewReferences(bytes.Repeat([]byte{8}, 32), time.Minute)
	for name, open := range map[string]func() error{
		"another user":    func() error { _, err := refs.Open(reference, 42, 8); return err },
		"another project": func() error { _, err := refs.Open(reference, 43, 7); return err },
		"another key":     func() error { _, err := other.Open(reference, 42, 7); return err },
		"tampered":        func() error { _, err := refs.Open(reference[:len(reference)-2]+"AA", 42, 7); return err },
		"an Entra ref": func() error {
			_, err := refs.Open("car_"+strings.TrimPrefix(reference, ReferencePrefix), 42, 7)
			return err
		},
	} {
		if err := open(); !errors.Is(err, ErrReferenceInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	refs.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if _, err := refs.Open(reference, 42, 7); !errors.Is(err, ErrReferenceInvalid) {
		t.Fatalf("an expired reference opened: %v", err)
	}
	req.Key.Provider = "jira"
	if _, err := refs.Mint(context.Background(), req); !errors.Is(err, provideroauth.ErrProviderNotConfigured) {
		t.Fatalf("a jira requirement minted: %v", err)
	}
}
