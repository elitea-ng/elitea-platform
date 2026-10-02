package api_test

// Router-level wiring tests for the branding bootstrap endpoint (UI spec
// §4.3 channel C, §9.3 unit W3): the route must be reachable WITHOUT any
// session or token — it renders on the login/loading path before auth
// exists — while the neighbouring /api/v2 surface stays behind Auth.
//
// Reuses newTestAuthClient/buildMinimalRouterConfig from router_llm_test.go
// (same package) so the Auth middleware is fully armed for the contrast case.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api"
)

// minimalValidBrandPack is a schema-valid pack (spec §4.2) with a marker id,
// used to prove RouterConfig.BrandPackPath is wired through NewRouter.
const minimalValidBrandPack = `{
  "$schema": "https://elitea.ai/schemas/brand-pack/1.json",
  "id": "router-wired",
  "version": "1.0.0",
  "product": {"name": "Router Wired", "shortName": "RW"},
  "assets": {"logoFull": "/app/brand/l.svg", "logoMark": "/app/brand/m.svg", "favicon": "/app/brand/f.svg"},
  "typography": {"fontFamily": "sans-serif", "fontFamilyMono": "monospace"},
  "shape": {"radiusSm": 2, "radiusMd": 4, "radiusLg": 8, "radiusPill": 9999, "density": "comfortable"},
  "locale": {},
  "brand": {"hue": "#123456"},
  "schemes": {"light": {"surface": "#FFFFFF"}, "dark": {"surface": "#000000"}}
}`

func TestBrandingBootstrap_UnauthenticatedReachable(t *testing.T) {
	packPath := filepath.Join(t.TempDir(), "pack.json")
	if err := os.WriteFile(packPath, []byte(minimalValidBrandPack), 0o600); err != nil {
		t.Fatalf("writing pack: %v", err)
	}
	cfg := buildMinimalRouterConfig(t, nil, nil, nil)
	cfg.BrandPackPath = packPath
	r := api.NewRouter(cfg)

	// No Authorization header, no session cookie, no edge-auth headers.
	req := httptest.NewRequest(http.MethodGet, "/api/v2/branding/bootstrap.js", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unauthenticated GET /api/v2/branding/bootstrap.js: got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "window.elitea_brand = ") || !strings.HasSuffix(body, ";") {
		t.Errorf("body is not a window.elitea_brand assignment:\n%s", body)
	}
	if !strings.Contains(body, `"id":"router-wired"`) {
		t.Errorf("BrandPackPath not wired through NewRouter; body:\n%s", body)
	}
	// zod-parseable on the UI side implies at least: valid JSON payload.
	payload := strings.TrimSuffix(strings.TrimPrefix(body, "window.elitea_brand = "), ";")
	var decoded map[string]any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Errorf("payload is not valid JSON: %v", err)
	}
	if rec.Header().Get("ETag") == "" {
		t.Errorf("missing strong ETag on bootstrap response")
	}
}

// TestBrandingBootstrap_HEADUnauthenticated pins finding 3: without an
// explicit HEAD registration the request falls through to the auth-wrapped
// /api/v2 mount and 401s. CDN and monitoring probes expect HEAD to behave
// like GET minus the body (RFC 9110 §9.3.2).
func TestBrandingBootstrap_HEADUnauthenticated(t *testing.T) {

	r := api.NewRouter(buildMinimalRouterConfig(t, nil, nil, nil))

	req := httptest.NewRequest(http.MethodHead, "/api/v2/branding/bootstrap.js", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unauthenticated HEAD: got %d, want 200", rec.Code)
	}
	if rec.Header().Get("ETag") == "" {
		t.Errorf("HEAD response missing ETag")
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control: got %q, want no-cache", cc)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD must have an empty body, got %d bytes", rec.Body.Len())
	}
}

// TestBrandingBootstrap_AuthContrast pins that the unauthenticated
// reachability above is a deliberate exception, not a hole: a sibling
// /api/v2 route (registered unconditionally inside the Auth group) still
// rejects the same credential-less request.
func TestBrandingBootstrap_AuthContrast(t *testing.T) {

	r := api.NewRouter(buildMinimalRouterConfig(t, nil, nil, nil))

	req := httptest.NewRequest(http.MethodGet, "/api/v2/elitea_core/agent_categories/prompt_lib/1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET inside the auth group: got %d, want 401 — if this changed, the branding route's placement rationale needs re-review", rec.Code)
	}
}
