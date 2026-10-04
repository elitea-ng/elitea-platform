package api_test

// Router-level wiring for the ADR-0025 WP1 anonymous documents: the discovery
// document at /.well-known/elitea-client and the brand pack as JSON. Both must
// answer a request that carries no credential, keep their own revalidating
// Cache-Control (not the /api/v2 group's no-store), and agree with
// bootstrap.js on the pack version.

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

func TestDiscoveryAndPackJSON_UnauthenticatedReachable(t *testing.T) {
	packPath := filepath.Join(t.TempDir(), "pack.json")
	if err := os.WriteFile(packPath, []byte(minimalValidBrandPack), 0o600); err != nil {
		t.Fatalf("writing pack: %v", err)
	}
	cfg := buildMinimalRouterConfig(t, nil, nil, nil)
	cfg.BrandPackPath = packPath
	cfg.DeploymentKind = "saas"
	cfg.PublicOrigin = "https://elitea.example.com"
	r := api.NewRouter(cfg)

	serve := func(method, target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	boot := serve(http.MethodGet, "/api/v2/branding/bootstrap.js")
	bootETag := strings.Trim(boot.Header().Get("ETag"), `"`)

	disc := serve(http.MethodGet, "/.well-known/elitea-client")
	if disc.Code != http.StatusOK {
		t.Fatalf("unauthenticated GET discovery: got %d, want 200 (body: %s)", disc.Code, disc.Body.String())
	}
	if got := disc.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("discovery Cache-Control = %q, want no-cache", got)
	}
	var doc map[string]any
	if err := json.Unmarshal(disc.Body.Bytes(), &doc); err != nil {
		t.Fatalf("discovery body: %v", err)
	}
	if doc["deployment_kind"] != "saas" || doc["display_name"] != "Router Wired" {
		t.Errorf("discovery document not wired from RouterConfig/brand: %v", doc)
	}
	if v, present := doc["native_auth"]; !present || v != nil {
		t.Errorf("native_auth = %v (present %v), want null", v, present)
	}
	wantURL := "https://elitea.example.com/api/v2/branding/pack.json?v=" + bootETag
	if doc["brand_pack_url"] != wantURL {
		t.Errorf("brand_pack_url = %v, want %q (one resolver, one ETag)", doc["brand_pack_url"], wantURL)
	}

	// Follow brand_pack_url, as a client would.
	pack := serve(http.MethodGet, strings.TrimPrefix(wantURL, "https://elitea.example.com"))
	if pack.Code != http.StatusOK {
		t.Fatalf("unauthenticated GET pack.json: got %d, want 200 (body: %s)", pack.Code, pack.Body.String())
	}
	if !strings.Contains(pack.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("versioned pack.json Cache-Control = %q, want immutable", pack.Header().Get("Cache-Control"))
	}
	if got := pack.Header().Get("ETag"); got != boot.Header().Get("ETag") {
		t.Errorf("pack.json ETag %q != bootstrap.js ETag %q", got, boot.Header().Get("ETag"))
	}
	if !strings.Contains(pack.Body.String(), `"logoFull":"https://elitea.example.com/app/brand/l.svg"`) {
		t.Errorf("pack.json assets not absolute:\n%s", pack.Body.String())
	}

	bare := serve(http.MethodGet, "/api/v2/branding/pack.json")
	if got := bare.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("bare pack.json Cache-Control = %q, want the handler's own no-cache, not the API group's no-store", got)
	}

	for _, target := range []string{"/.well-known/elitea-client", "/api/v2/branding/pack.json"} {
		head := serve(http.MethodHead, target)
		if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") == "" {
			t.Errorf("HEAD %s: code %d, body %d bytes, Content-Length %q", target, head.Code, head.Body.Len(), head.Header().Get("Content-Length"))
		}
	}
}
