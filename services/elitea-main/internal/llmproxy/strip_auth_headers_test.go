package llmproxy

// Tests for the stripIdentityHeaders extension: verify that client-supplied
// authentication material (Cookie, Authorization, X-Api-Key, X-Auth-*) is
// stripped from the outbound gateway request and that only the edge-signed
// X-Elitea-* identity is present.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
)

// TestProxy_StripsClientAuthHeaders asserts that a request carrying spoofed
// X-Elitea-* identity headers, X-Auth-* Traefik headers, Authorization, Cookie,
// and X-Api-Key reaches the upstream with all of those stripped, and only the
// edge-signed X-Elitea-* headers present.
func TestProxy_StripsClientAuthHeaders(t *testing.T) {
	var gotHeaders http.Header
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		_, _ = io.WriteString(w, `{}`)
	}))
	defer backend.Close()

	p := proxyTo(t, backend.URL, "sekret")

	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", nil)

	// Spoof identity and auth headers that must be stripped.
	req.Header.Set(HeaderProjectID, "spoofed-project-999")
	req.Header.Set(HeaderUserID, "spoofed-user")
	req.Header.Set(HeaderTenantID, "spoofed-tenant")
	req.Header.Set(HeaderSignature, "sha256=deadbeef")
	req.Header.Set("X-Auth-Type", "jwt")
	req.Header.Set("X-Auth-Id", "attacker-id")
	req.Header.Set("X-Auth-Reference", "attacker@evil.com")
	req.Header.Set("Authorization", "Bearer evil-token")
	req.Header.Set("X-Api-Key", "evil-api-key")
	req.Header.Set("Cookie", "elitea_session=evilcookie")

	// Set a legitimate edge-resolved project on the context so the proxy injects
	// the correct signed identity.
	ctx := middleware.ContextWithProject(req.Context(), middleware.ProjectContext{ProjectID: 7})
	req = req.WithContext(ctx)

	p.ServeHTTP(httptest.NewRecorder(), req)

	// Client-supplied authentication material must be absent.
	mustBeAbsent := []string{
		"Authorization",
		"X-Api-Key",
		"Cookie",
		"X-Auth-Type",
		"X-Auth-Id",
		"X-Auth-Reference",
	}
	for _, h := range mustBeAbsent {
		if v := gotHeaders.Get(h); v != "" {
			t.Errorf("gateway received %q = %q; must be stripped", h, v)
		}
	}

	// Edge-resolved project must overwrite the spoofed value.
	if got := gotHeaders.Get(HeaderProjectID); got != "7" {
		t.Errorf("gateway %s = %q, want 7 (spoofed value not overwritten)", HeaderProjectID, got)
	}

	// The injected identity must carry a valid HMAC signature.
	if !verifyIdentitySignature(gotHeaders, []byte("sekret")) {
		t.Errorf("gateway identity signature did not verify; headers=%v", gotHeaders)
	}
}

// TestStripIdentityHeaders_StripsBroadAuthMaterial verifies the extended
// stripIdentityHeaders function directly, without going through the full proxy.
func TestStripIdentityHeaders_StripsBroadAuthMaterial(t *testing.T) {
	h := http.Header{}
	h.Set(HeaderProjectID, "p")
	h.Set(HeaderUserID, "u")
	h.Set(HeaderTenantID, "t")
	h.Set(HeaderSignature, "sig")
	h.Set("X-Auth-Type", "jwt")
	h.Set("X-Auth-Id", "aid")
	h.Set("X-Auth-Reference", "ref")
	h.Set("Authorization", "Bearer tok")
	h.Set("X-Api-Key", "key")
	h.Set("Cookie", "session=abc")
	h.Set("Content-Type", "application/json") // must survive

	stripIdentityHeaders(h)

	stripped := []string{
		HeaderProjectID, HeaderUserID, HeaderTenantID, HeaderSignature,
		"X-Auth-Type", "X-Auth-Id", "X-Auth-Reference",
		"Authorization", "X-Api-Key", "Cookie",
	}
	for _, k := range stripped {
		if v := h.Get(k); v != "" {
			t.Errorf("header %q not stripped: %q", k, v)
		}
	}

	// Non-auth headers must be untouched.
	if v := h.Get("Content-Type"); v != "application/json" {
		t.Errorf("Content-Type unexpectedly stripped or modified: %q", v)
	}
}

// TestStripIdentityHeaders_StripsEveryEdgeProjectionHeader covers the whole
// X-Auth-* family the edge projects, including the projection signature, so no
// provider or gateway hop receives inbound edge authentication context.
func TestStripIdentityHeaders_StripsEveryEdgeProjectionHeader(t *testing.T) {
	h := http.Header{}
	for _, name := range []string{
		"X-Auth-Type", "X-Auth-Id", "X-Auth-User-Id", "X-Auth-Reference",
		"X-Auth-Signature", "X-Auth-Avatar", "X-Auth-Avatar-State",
	} {
		h.Set(name, "value")
	}
	h.Set("X-Authz-Hint", "kept") // a different prefix survives

	stripIdentityHeaders(h)

	for name := range h {
		if strings.HasPrefix(strings.ToLower(name), "x-auth-") {
			t.Errorf("header %q not stripped", name)
		}
	}
	if h.Get("X-Authz-Hint") != "kept" {
		t.Error("X-Authz-Hint unexpectedly stripped")
	}
}
