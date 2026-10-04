package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/branding"
)

func brandResolver(t *testing.T) *branding.Resolver {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pack.json")
	data := `{
  "$schema": "https://elitea.ai/schemas/brand-pack/1.json",
  "id": "acme", "version": "1.0.0",
  "product": {"name": "Acme AI", "shortName": "Acme"},
  "assets": {"logoFull": "/l.svg", "logoMark": "/m.svg", "favicon": "/f.svg"},
  "typography": {"fontFamily": "sans-serif", "fontFamilyMono": "monospace"},
  "shape": {"radiusSm": 2, "radiusMd": 4, "radiusLg": 8, "radiusPill": 9999, "density": "comfortable"},
  "locale": {}, "brand": {"hue": "#123456"}, "schemes": {"light": {}, "dark": {}}
}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return branding.NewResolver(branding.ResolverConfig{PackPath: path})
}

func serve(h http.Handler, method, target string, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return m
}

func TestHandler_DefaultsWithNoSources(t *testing.T) {
	resolver := brandResolver(t)
	h := NewHandler(Config{
		ServerVersion:  "v1.62.3",
		DeploymentKind: DeploymentKindSaaS,
		PublicOrigin:   "https://elitea.example.com",
		Brand:          resolver,
	})
	rec := serve(h, http.MethodGet, Path, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d", rec.Code)
	}
	if rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers %v", rec.Header())
	}
	if rec.Header().Get("Cache-Control") != "no-cache" || rec.Header().Get("Vary") != "" {
		t.Errorf("Cache-Control %q Vary %q", rec.Header().Get("Cache-Control"), rec.Header().Get("Vary"))
	}
	doc := decode(t, rec)
	etag := resolver.Current(context.Background()).ETagValue
	want := map[string]any{
		"server_version":  "1.62",
		"client_contract": ClientContract,
		"deployment_kind": "saas",
		"display_name":    "Acme AI",
		"brand_pack_url":  "https://elitea.example.com/api/v2/branding/pack.json?v=" + etag,
	}
	for k, v := range want {
		if doc[k] != v {
			t.Errorf("%s = %v, want %v", k, doc[k], v)
		}
	}
	if v, ok := doc["native_auth"]; !ok || v != nil {
		t.Errorf("native_auth = %v (present %v), want null", v, ok)
	}
	policy, _ := doc["client_policy"].(map[string]any)
	if policy["require_device_lock"] != false || policy["offline_enabled"] != true || policy["min_client_version"] != "" {
		t.Errorf("client_policy = %v", policy)
	}
	if m, ok := doc["min_client_version"].(map[string]any); !ok || len(m) != 0 {
		t.Errorf("min_client_version = %v, want {}", doc["min_client_version"])
	}
}

func TestHandler_ETagAndHEAD(t *testing.T) {
	h := NewHandler(Config{ServerVersion: "1.2.3", PublicOrigin: "https://e.example", Brand: brandResolver(t)})
	first := serve(h, http.MethodGet, Path, nil)
	second := serve(h, http.MethodGet, Path, nil)
	etag := first.Header().Get("ETag")
	if etag == "" || etag != second.Header().Get("ETag") || first.Body.String() != second.Body.String() {
		t.Fatalf("ETag not stable: %q vs %q", etag, second.Header().Get("ETag"))
	}
	if nm := serve(h, http.MethodGet, Path, http.Header{"If-None-Match": {etag}}); nm.Code != http.StatusNotModified || nm.Body.Len() != 0 {
		t.Errorf("If-None-Match: code %d", nm.Code)
	}
	if stale := serve(h, http.MethodGet, Path, http.Header{"If-None-Match": {`"stale"`}}); stale.Code != http.StatusOK {
		t.Errorf("stale If-None-Match: code %d", stale.Code)
	}
	head := serve(h, http.MethodHead, Path, nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") != first.Header().Get("Content-Length") {
		t.Errorf("HEAD: code %d body %d CL %q", head.Code, head.Body.Len(), head.Header().Get("Content-Length"))
	}
	if doc := decode(t, first); doc["deployment_kind"] != DeploymentKindSelfHosted {
		t.Errorf("empty kind = %v, want self_hosted", doc["deployment_kind"])
	}
}

func TestHandler_RequestDerivedOrigin(t *testing.T) {
	h := NewHandler(Config{Brand: brandResolver(t)})
	rec := serve(h, http.MethodGet, "http://elitea.local:8080"+Path, http.Header{"X-Forwarded-Host": {"evil.example"}})
	if rec.Header().Get("Vary") == "" {
		t.Error("request-derived body without Vary")
	}
	doc := decode(t, rec)
	if url, _ := doc["brand_pack_url"].(string); len(url) < 24 || url[:24] != "http://elitea.local:8080" {
		t.Errorf("brand_pack_url = %q", url)
	}
}

type fakeNativeAuth struct {
	na  *NativeAuth
	err error
}

func (f fakeNativeAuth) NativeAuth(_ context.Context, origin string) (*NativeAuth, error) {
	if f.na != nil {
		na := *f.na
		na.Issuer = origin
		return &na, f.err
	}
	return nil, f.err
}

type fakePolicy struct {
	p    PublicPolicy
	mins map[string]string
	err  error
}

func (f fakePolicy) PublicPolicy(context.Context) (PublicPolicy, map[string]string, error) {
	return f.p, f.mins, f.err
}

func TestHandler_Sources(t *testing.T) {
	h := NewHandler(Config{
		PublicOrigin: "https://e.example",
		Brand:        brandResolver(t),
		NativeAuth:   fakeNativeAuth{na: &NativeAuth{TokenEndpoint: "https://e.example/api/v2/auth/native/token"}},
		ClientPolicy: fakePolicy{
			p:    PublicPolicy{RequireDeviceLock: true, MinClientVersion: "1.0.0"},
			mins: map[string]string{"ai.elitea.app": "1.2.0"},
		},
	})
	doc := decode(t, serve(h, http.MethodGet, Path, nil))
	na, _ := doc["native_auth"].(map[string]any)
	if na["issuer"] != "https://e.example" || na["token_endpoint"] != "https://e.example/api/v2/auth/native/token" {
		t.Errorf("native_auth = %v", doc["native_auth"])
	}
	if p, _ := doc["client_policy"].(map[string]any); p["require_device_lock"] != true || p["offline_enabled"] != false {
		t.Errorf("client_policy = %v", p)
	}
	if m, _ := doc["min_client_version"].(map[string]any); m["ai.elitea.app"] != "1.2.0" {
		t.Errorf("min_client_version = %v", m)
	}

	// A registry that says "no client registered" renders null.
	none := NewHandler(Config{PublicOrigin: "https://e.example", Brand: brandResolver(t), NativeAuth: fakeNativeAuth{}})
	if v := decode(t, serve(none, http.MethodGet, Path, nil))["native_auth"]; v != nil {
		t.Errorf("native_auth = %v, want null", v)
	}
}

// A failing source answers 503 no-store: a cached answer built without it
// would tell clients this deployment has no native sign-in.
func TestHandler_SourceFailureIs503(t *testing.T) {
	for name, cfg := range map[string]Config{
		"native auth": {NativeAuth: fakeNativeAuth{err: errors.New("db down")}},
		"policy":      {ClientPolicy: fakePolicy{err: errors.New("db down")}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg.Brand = brandResolver(t)
			cfg.PublicOrigin = "https://e.example"
			rec := serve(NewHandler(cfg), http.MethodGet, Path, nil)
			if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("ETag") != "" {
				t.Errorf("code %d Cache-Control %q ETag %q", rec.Code, rec.Header().Get("Cache-Control"), rec.Header().Get("ETag"))
			}
		})
	}
}
