package admin_test

// ADR-0024 WP1 acceptance for the Branding surface, against a real
// `centry.platform_config`. Every write is asserted by re-reading through the
// product's own GET AND by reading the row with SQL, and the pack the
// bootstrap route would serve is asserted through the very resolver the
// router wires — so a handler that echoed its request, or a resolver that
// never re-read, could not pass.
//
// Reuses newConfigPool from config_values_postgres_integration_test.go (same
// package): set ELITEA_TEST_DATABASE_URL to run.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/admin"
	v2branding "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/branding"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

type brandingBody struct {
	Values    map[string]any    `json:"values"`
	Layers    v2branding.Layers `json:"layers"`
	Effective *v2branding.Pack  `json:"effective"`
	ETag      string            `json:"etag"`
	Saved     bool              `json:"saved"`
	Error     string            `json:"error"`
}

func decodeBranding(t *testing.T, raw []byte) brandingBody {
	t.Helper()
	var body brandingBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return body
}

func TestBranding_SaveIsServedWithoutARestart(t *testing.T) {
	pool := newConfigPool(t)
	// A long TTL: the test proves the SAVE invalidates the cache, not that
	// the clock ran out.
	resolver := v2branding.NewResolver(v2branding.ResolverConfig{Pool: pool, TTL: time.Hour})
	// The generic Configuration-page path checks the section's own
	// `required_permission` inside the handler, so the caller must hold it;
	// the typed routes are gated by route middleware in router.go instead.
	handler := admin.NewHandler(pool,
		admin.WithBranding(resolver),
		admin.WithPermissionResolver(grantingResolver("configuration.branding")),
	)
	principal := auth.User{ID: "7", UserID: "7", Email: "operator@example.com"}

	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), principal)))
		})
	})
	router.Get("/admin/branding/administration", handler.BrandingRead)
	router.Put("/admin/branding/administration", handler.BrandingSave)
	router.Put("/admin/plugin_config_values/administration/{plugin}", handler.AdministrationPluginConfigValuesSave)
	bootstrap := v2branding.NewHandler(v2branding.Config{Resolver: resolver})
	router.Get("/api/v2/branding/bootstrap.js", bootstrap.Bootstrap)

	// Fresh install: no layers, nothing served, every declared key at its
	// inherit sentinel.
	rec := configDo(t, router, http.MethodGet, "/admin/branding/administration", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d (body %s)", rec.Code, rec.Body.String())
	}
	fresh := decodeBranding(t, rec.Body.Bytes())
	if fresh.Layers != (v2branding.Layers{}) || fresh.Effective != nil {
		t.Fatalf("fresh install: layers %+v effective %v, want none and null", fresh.Layers, fresh.Effective)
	}
	if got, ok := fresh.Values[platformconfig.KeyBrandingHue]; !ok || got != "" {
		t.Fatalf("fresh install brand_hue = %v, want the empty inherit sentinel", got)
	}
	before := configDo(t, router, http.MethodGet, "/api/v2/branding/bootstrap.js", nil)
	if !strings.Contains(before.Body.String(), "no deployment brand pack configured") {
		t.Fatalf("fresh install bootstrap should be inert, got %q", before.Body.String())
	}

	// Save through the typed route.
	rec = configDo(t, router, http.MethodPut, "/admin/branding/administration", map[string]any{
		"values": map[string]any{
			platformconfig.KeyBrandingProductName: "Acme AI",
			platformconfig.KeyBrandingHue:         "#FF6600",
			platformconfig.KeyBrandingBaseSize:    16,
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d (body %s)", rec.Code, rec.Body.String())
	}
	saved := decodeBranding(t, rec.Body.Bytes())
	if !saved.Saved || saved.Layers != (v2branding.Layers{Database: true}) {
		t.Fatalf("after save: saved %v layers %+v", saved.Saved, saved.Layers)
	}
	if saved.Effective == nil || saved.Effective.Product.Name != "Acme AI" || saved.Effective.Brand.Hue != "#FF6600" {
		t.Fatalf("effective pack after save = %+v", saved.Effective)
	}
	if saved.Effective.Typography.BaseSize != 16 {
		t.Errorf("base size not applied: %v", saved.Effective.Typography.BaseSize)
	}
	// The base under a database-only overlay is the PRODUCT default (its
	// Manrope stack), and a NEW hue drops its stated tokens so the UI
	// derives every id from #FF6600 rather than keeping the default palette
	// (branding.Resolver's applyOverlay).
	if !strings.Contains(saved.Effective.Typography.FontFamily, "Manrope") {
		t.Errorf("base is not the product default: font %q", saved.Effective.Typography.FontFamily)
	}
	if len(saved.Effective.Schemes.Dark) != 0 || len(saved.Effective.Schemes.Light) != 0 {
		t.Errorf("stated tokens survived a hue change; the hue would be inert in the UI")
	}

	// The row exists with those bytes.
	if raw, ok := storedValueSQL(t, pool, platformconfig.SectionBranding, platformconfig.KeyBrandingHue); !ok || raw != `"#FF6600"` {
		t.Fatalf("stored brand_hue = %q (present %v)", raw, ok)
	}

	// The bootstrap route serves the new brand NOW, under a long TTL — the
	// save invalidated the resolver.
	after := configDo(t, router, http.MethodGet, "/api/v2/branding/bootstrap.js", nil)
	if !strings.Contains(after.Body.String(), `"name":"Acme AI"`) {
		t.Fatalf("bootstrap after save does not carry the brand: %q", after.Body.String())
	}
	if after.Header().Get("ETag") == before.Header().Get("ETag") {
		t.Errorf("ETag did not change across a rebrand")
	}
	if saved.ETag != strings.Trim(after.Header().Get("ETag"), `"`) {
		t.Errorf("admin etag %q != bootstrap ETag %q", saved.ETag, after.Header().Get("ETag"))
	}

	// The generic Configuration page reaches the same rows, applies the same
	// rules, and invalidates the same resolver.
	rec = configDo(t, router, http.MethodPut, "/admin/plugin_config_values/administration/branding",
		map[string]any{"values": map[string]any{platformconfig.KeyBrandingHue: "not-a-colour"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("generic save of a bad hue: status %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	rec = configDo(t, router, http.MethodPut, "/admin/plugin_config_values/administration/branding",
		map[string]any{"values": map[string]any{platformconfig.KeyBrandingProductName: "Acme Two"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("generic save: status %d (body %s)", rec.Code, rec.Body.String())
	}
	again := configDo(t, router, http.MethodGet, "/api/v2/branding/bootstrap.js", nil)
	if !strings.Contains(again.Body.String(), `"name":"Acme Two"`) || !strings.Contains(again.Body.String(), `"hue":"#FF6600"`) {
		t.Fatalf("generic save not reflected, or it clobbered the hue: %q", again.Body.String())
	}

	// Clearing every value returns the deployment to "nothing served".
	rec = configDo(t, router, http.MethodPut, "/admin/branding/administration", map[string]any{
		"values": map[string]any{
			platformconfig.KeyBrandingProductName: "",
			platformconfig.KeyBrandingHue:         "",
			platformconfig.KeyBrandingBaseSize:    0,
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("clearing PUT status = %d (body %s)", rec.Code, rec.Body.String())
	}
	cleared := decodeBranding(t, rec.Body.Bytes())
	if cleared.Effective != nil || cleared.Layers.Database {
		t.Fatalf("after clearing: effective %v layers %+v, want null and no database layer", cleared.Effective, cleared.Layers)
	}
}

// TestBranding_GenericPathRequiresTheBrandingPermission pins that the
// Configuration page's door is gated on `configuration.branding` too — a
// holder of `runtime.plugins` alone cannot rebrand through it.
func TestBranding_GenericPathRequiresTheBrandingPermission(t *testing.T) {
	pool := newConfigPool(t)
	handler := admin.NewHandler(pool, admin.WithPermissionResolver(grantingResolver("runtime.plugins")))
	principal := auth.User{ID: "7", UserID: "7", Email: "operator@example.com"}
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), principal)))
		})
	})
	router.Get("/admin/plugin_config_values/administration/{plugin}", handler.AdministrationPluginConfigValues)
	router.Put("/admin/plugin_config_values/administration/{plugin}", handler.AdministrationPluginConfigValuesSave)

	for _, method := range []string{http.MethodGet, http.MethodPut} {
		var body any
		if method == http.MethodPut {
			body = map[string]any{"values": map[string]any{platformconfig.KeyBrandingHue: "#FF6600"}}
		}
		rec := configDo(t, router, method, "/admin/plugin_config_values/administration/branding", body)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s without configuration.branding: status %d, want 403 (body %s)", method, rec.Code, rec.Body.String())
		}
	}
}

func TestBranding_TypedSaveRefusesWhatTheSchemaWouldNot(t *testing.T) {
	pool := newConfigPool(t)
	handler := admin.NewHandler(pool)
	principal := auth.User{ID: "7", UserID: "7", Email: "operator@example.com"}
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), principal)))
		})
	})
	router.Put("/admin/branding/administration", handler.BrandingSave)

	for name, values := range map[string]map[string]any{
		"unknown key":      {"custom_css": "body{}"},
		"external logo":    {platformconfig.KeyBrandingLogoFull: "https://cdn.example/l.svg"},
		"javascript logo":  {platformconfig.KeyBrandingFavicon: "javascript:alert(1)"},
		"string base size": {platformconfig.KeyBrandingBaseSize: "big"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := configDo(t, router, http.MethodPut, "/admin/branding/administration", map[string]any{"values": values})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
			// Nothing was stored.
			var count int
			if err := pool.QueryRow(context.Background(),
				`SELECT count(*) FROM centry.platform_config WHERE section = $1`, platformconfig.SectionBranding,
			).Scan(&count); err != nil {
				t.Fatalf("count rows: %v", err)
			}
			if count != 0 {
				t.Fatalf("a refused save stored %d row(s)", count)
			}
		})
	}
}
