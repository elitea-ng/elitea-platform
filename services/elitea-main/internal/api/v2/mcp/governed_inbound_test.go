package mcp

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coreoidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-chi/chi/v5"
	jose "github.com/go-jose/go-jose/v4"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

const (
	tOrigin   = "https://elitea.example.com"
	tIssuer   = "https://idp.example.com"
	tResource = tOrigin + "/app/7/mcp"
)

type inboundHarness struct {
	key     *ecdsa.PrivateKey
	guard   *GovernedInbound
	logs    *bytes.Buffer
	users   *fakeInboundUsers
	reached []*http.Request
}

type fakeInboundUsers struct{ claims []InboundToken }

func (f *fakeInboundUsers) ResolveInboundUser(_ *http.Request, token InboundToken) (auth.User, error) {
	f.claims = append(f.claims, token)
	if token.UserClaim != "alice@example.com" {
		return auth.User{}, auth.ErrPrincipalInactive
	}
	return auth.User{ID: "11", UserID: "11", Email: token.UserClaim}, nil
}

func newInboundHarness(t *testing.T, oauthOnly bool) *inboundHarness {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewOAuthVerifier(OAuthVerifierConfig{
		Issuers:        map[string]coreoidc.KeySet{tIssuer: &coreoidc.StaticKeySet{PublicKeys: []crypto.PublicKey{key.Public()}}},
		RequiredScopes: []string{"mcp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &inboundHarness{key: key, logs: &bytes.Buffer{}, users: &fakeInboundUsers{}}
	h.guard, err = NewGovernedInbound(GovernedInboundConfig{
		PublicOrigin: tOrigin, Verifier: verifier, Users: h.users, OAuthOnly: oauthOnly,
		Logger: slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *inboundHarness) token(t *testing.T, claims map[string]any) string {
	t.Helper()
	base := map[string]any{
		"iss": tIssuer, "sub": "subject-1", "aud": tResource, "exp": time.Now().Add(time.Hour).Unix(),
		"scope": "mcp profile", "client_id": "https://client.example/metadata.json", "email": "alice@example.com",
	}
	for k, v := range claims {
		if v == nil {
			delete(base, k)
			continue
		}
		base[k] = v
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: h.key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(base)
	signed, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	compact, err := signed.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return compact
}

// platformAuth stands in for apimw.Auth: it admits "Bearer pat-ok" and
// answers every other request 401, the way the platform does.
func platformAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer pat-ok" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.ContextWithAuthenticatedUser(r.Context(), auth.User{ID: "11", UserID: "11"}, auth.AuthenticationSourceToken)))
	})
}

// router is the MCP endpoint behind the guard (governed) or behind the
// platform authentication alone (flag off), with the real handler.
func (h *inboundHarness) router(governed bool) chi.Router {
	handler := NewHandler(nil, nil, nil, nil)
	handler.source = staticSource(Tool{Name: "echo", Description: "echo", InputSchema: map[string]any{"type": "object"}})
	authenticate := platformAuth
	if governed {
		authenticate = h.guard.Wrap(platformAuth)
	}
	record := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.reached = append(h.reached, r)
			next.ServeHTTP(w, r)
		})
	}
	router := chi.NewRouter()
	router.Group(func(r chi.Router) {
		r.Use(authenticate, record)
		r.Post("/app/{projectID}/mcp", handler.Endpoint)
		r.Post("/app/{projectID}/mcp/*", handler.Endpoint)
	})
	router.Get(ProtectedResourceMetadataPrefix+"/app/{projectID}/mcp", h.guard.ServeMetadata)
	router.Get(ProtectedResourceMetadataPrefix+"/app/{projectID}/mcp/*", h.guard.ServeMetadata)
	return router
}

func call(router http.Handler, path, bearer string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	request.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// Acceptance (#1123): the metadata endpoint answers a valid RFC 9728
// document: every member RFC 9728 §2 defines has its defined type, the
// required `resource` is this endpoint's URI, and nothing else is in it.
func TestProtectedResourceMetadataIsAValidRFC9728Document(t *testing.T) {
	h := newInboundHarness(t, false)
	router := h.router(true)
	for path, resource := range map[string]string{
		ProtectedResourceMetadataPrefix + "/app/7/mcp":          tResource,
		ProtectedResourceMetadataPrefix + "/app/7/mcp/toolkits": tResource + "/toolkits",
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("%s = %d %s", path, recorder.Code, recorder.Header().Get("Content-Type"))
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(recorder.Body.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		// RFC 9728 §2: member name -> JSON type.
		schema := map[string]string{
			"resource": "string", "authorization_servers": "[]string", "jwks_uri": "string", "scopes_supported": "[]string",
			"bearer_methods_supported": "[]string", "resource_signing_alg_values_supported": "[]string", "resource_name": "string",
			"resource_documentation": "string", "resource_policy_uri": "string", "resource_tos_uri": "string",
			"tls_client_certificate_bound_access_tokens": "bool", "authorization_details_types_supported": "[]string",
			"dpop_signing_alg_values_supported": "[]string", "dpop_bound_access_tokens_required": "bool",
		}
		for member, raw := range doc {
			kind, known := schema[member]
			if !known {
				t.Errorf("%s: member %q is not an RFC 9728 member", path, member)
				continue
			}
			var err error
			switch kind {
			case "string":
				var v string
				err = json.Unmarshal(raw, &v)
			case "[]string":
				var v []string
				err = json.Unmarshal(raw, &v)
			case "bool":
				var v bool
				err = json.Unmarshal(raw, &v)
			}
			if err != nil {
				t.Errorf("%s: %q is not a %s: %v", path, member, kind, err)
			}
		}
		var parsed ProtectedResourceMetadata
		_ = json.Unmarshal(recorder.Body.Bytes(), &parsed)
		if parsed.Resource != resource || len(parsed.AuthorizationServers) != 1 || parsed.AuthorizationServers[0] != tIssuer ||
			len(parsed.BearerMethodsSupported) != 1 || parsed.BearerMethodsSupported[0] != "header" || parsed.ScopesSupported[0] != "mcp" {
			t.Fatalf("%s: %+v", path, parsed)
		}
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, ProtectedResourceMetadataPrefix+"/app/x/mcp", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("a non-numeric project = %d", recorder.Code)
	}
}

// Acceptance (#1123), governed: a 401 from the MCP endpoint names the
// metadata URL in WWW-Authenticate, for no credential, a bad platform
// credential and a refused OAuth token alike.
func TestGovernedMCP401CarriesResourceMetadata(t *testing.T) {
	h := newInboundHarness(t, false)
	router := h.router(true)
	want := `resource_metadata="` + tOrigin + ProtectedResourceMetadataPrefix + "/app/7/mcp" + `"`
	for name, bearer := range map[string]string{"none": "", "bad PAT": "pat-bad", "foreign audience": h.token(t, map[string]any{"aud": "https://other.example.com/mcp"})} {
		recorder := call(router, "/app/7/mcp", bearer)
		if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Header().Get("WWW-Authenticate"), want) {
			t.Errorf("%s: %d %q", name, recorder.Code, recorder.Header().Get("WWW-Authenticate"))
		}
	}
	sub := call(router, "/app/7/mcp/toolkits", "")
	if !strings.Contains(sub.Header().Get("WWW-Authenticate"), ProtectedResourceMetadataPrefix+"/app/7/mcp/toolkits") {
		t.Fatalf("subpath challenge %q", sub.Header().Get("WWW-Authenticate"))
	}
	if ok := call(router, "/app/7/mcp", "pat-ok"); ok.Code != http.StatusOK {
		t.Fatalf("a platform PAT is refused in governed mode: %d", ok.Code)
	}
}

// Acceptance (#1123), flag off: the same 401 carries no resource_metadata
// challenge and no metadata route exists.
func TestUngovernedMCP401HasNoResourceMetadata(t *testing.T) {
	h := newInboundHarness(t, false)
	recorder := call(h.router(false), "/app/7/mcp", "")
	if recorder.Code != http.StatusUnauthorized || recorder.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("flag off: %d %q", recorder.Code, recorder.Header().Get("WWW-Authenticate"))
	}
}

// Acceptance (#1123), negative: a token whose audience is not this MCP
// resource is refused with 401, as is every other defect; none reaches the
// handler.
func TestInboundTokenWithAnotherAudienceIs401(t *testing.T) {
	h := newInboundHarness(t, false)
	router := h.router(true)
	for name, claims := range map[string]map[string]any{
		"another resource":      {"aud": "https://other.example.com/mcp"},
		"the platform audience": {"aud": tOrigin},
		"another project":       {"aud": tOrigin + "/app/8/mcp"},
		"two audiences":         {"aud": []string{tResource, "https://other.example.com/mcp"}},
		"no audience":           {"aud": nil},
		"expired":               {"exp": time.Now().Add(-time.Hour).Unix()},
		"missing scope":         {"scope": "profile"},
		"unknown user":          {"email": "mallory@example.com"},
		"no subject":            {"sub": nil},
		"not yet valid":         {"nbf": time.Now().Add(time.Hour).Unix()},
	} {
		h.reached = nil
		recorder := call(router, "/app/7/mcp", h.token(t, claims))
		if recorder.Code != http.StatusUnauthorized || len(h.reached) != 0 {
			t.Errorf("%s: %d, reached %d", name, recorder.Code, len(h.reached))
		}
	}
	forged := h.token(t, nil)
	forged = forged[:len(forged)-4] + "AAAA"
	if recorder := call(router, "/app/7/mcp", forged); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("a forged signature: %d", recorder.Code)
	}
	if recorder := call(router, "/app/7/mcp", h.token(t, nil)); recorder.Code != http.StatusOK {
		t.Fatalf("a valid token: %d %s", recorder.Code, recorder.Body.String())
	}
}

// Acceptance (#1123), negative: the inbound token value never reaches the
// handler's request, the principal, the response or the logs. The run's
// caller is the token's client, recorded as a calling application.
func TestInboundTokenIsNeverForwarded(t *testing.T) {
	h := newInboundHarness(t, false)
	router := h.router(true)
	token := h.token(t, nil)
	recorder := call(router, "/app/7/mcp", token)
	if recorder.Code != http.StatusOK || len(h.reached) != 1 {
		t.Fatalf("%d, reached %d", recorder.Code, len(h.reached))
	}
	r := h.reached[0]
	user, _ := auth.UserFromContext(r.Context())
	source, _ := auth.AuthenticationSourceFromContext(r.Context())
	application, _ := auth.CallingApplicationFromContext(r.Context())
	if user.UserID != "11" || source != auth.AuthenticationSourceToken || application != CallingApplicationPrefix+"https://client.example/metadata.json" {
		t.Fatalf("user %+v source %v application %q", user, source, application)
	}
	principal, _ := json.Marshal(user)
	headers, _ := json.Marshal(r.Header)
	signature := token[strings.LastIndex(token, ".")+1:]
	for name, surface := range map[string]string{
		"request headers": string(headers), "principal": string(principal), "response": recorder.Body.String(),
		"logs": h.logs.String(), "url": r.URL.String(),
	} {
		if strings.Contains(surface, token) || strings.Contains(surface, signature) {
			t.Errorf("the inbound token reached the %s", name)
		}
	}
	if r.Header.Get("Authorization") != "" {
		t.Fatal("the Authorization header was forwarded")
	}
}

// Gate D8: a deployment may refuse platform credentials on the MCP endpoint.
func TestOAuthOnlyRefusesPlatformCredentials(t *testing.T) {
	h := newInboundHarness(t, true)
	router := h.router(true)
	if recorder := call(router, "/app/7/mcp", "pat-ok"); recorder.Code != http.StatusUnauthorized ||
		!strings.Contains(recorder.Header().Get("WWW-Authenticate"), "resource_metadata=") {
		t.Fatalf("PAT under OAuth only: %d", recorder.Code)
	}
	if recorder := call(router, "/app/7/mcp", h.token(t, nil)); recorder.Code != http.StatusOK {
		t.Fatalf("OAuth under OAuth only: %d", recorder.Code)
	}
}

func TestVerifierRefusesAnUnlistedIssuer(t *testing.T) {
	h := newInboundHarness(t, false)
	token := h.token(t, map[string]any{"iss": "https://evil.example.com"})
	if h.guard.verifier.Recognizes(token) {
		t.Fatal("an unlisted issuer is recognized")
	}
	if _, err := h.guard.verifier.Verify(t.Context(), token, tResource); !errors.Is(err, ErrInboundToken) {
		t.Fatalf("verify: %v", err)
	}
	if _, err := NewOAuthVerifier(OAuthVerifierConfig{Issuers: map[string]coreoidc.KeySet{"http://idp": &coreoidc.StaticKeySet{}}}); err == nil {
		t.Fatal("an http issuer was accepted")
	}
}
