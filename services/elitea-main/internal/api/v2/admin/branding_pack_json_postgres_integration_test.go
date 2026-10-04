package admin_test

// ADR-0025 WP1 acceptance: a Branding save moves bootstrap.js, pack.json and
// the discovery document at once — one resolver — against a real
// `centry.platform_config`. pack.json has its OWN entity tag (its body carries
// the origin and, unbranded, the product default), and discovery publishes
// exactly that tag. Reuses newConfigPool and
// configDo from config_values_postgres_integration_test.go: set
// ELITEA_TEST_DATABASE_URL to run.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/admin"
	v2branding "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/branding"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/discovery"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

func TestBranding_SaveMovesPackJSONAndDiscoveryWithBootstrap(t *testing.T) {
	pool := newConfigPool(t)
	resolver := v2branding.NewResolver(v2branding.ResolverConfig{Pool: pool, TTL: time.Hour})
	handler := admin.NewHandler(pool, admin.WithBranding(resolver))
	principal := auth.User{ID: "7", UserID: "7", Email: "operator@example.com"}
	const origin = "https://elitea.example.com"

	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), principal)))
		})
	})
	router.Put("/admin/branding/administration", handler.BrandingSave)
	brand := v2branding.NewHandler(v2branding.Config{Resolver: resolver, PublicOrigin: origin})
	router.Get("/api/v2/branding/bootstrap.js", brand.Bootstrap)
	router.Get(v2branding.PackJSONPath, brand.PackJSON)
	router.Get(discovery.Path, discovery.NewHandler(discovery.Config{
		ServerVersion: "1.2.3", PublicOrigin: origin, Brand: resolver,
	}).ServeHTTP)

	type state struct{ bootETag, packETag, layers, name, packURL string }
	read := func(t *testing.T) state {
		t.Helper()
		boot := configDo(t, router, http.MethodGet, "/api/v2/branding/bootstrap.js", nil)
		pack := configDo(t, router, http.MethodGet, v2branding.PackJSONPath, nil)
		disc := configDo(t, router, http.MethodGet, discovery.Path, nil)
		if boot.Code != http.StatusOK || pack.Code != http.StatusOK || disc.Code != http.StatusOK {
			t.Fatalf("codes boot %d pack %d discovery %d", boot.Code, pack.Code, disc.Code)
		}
		var p v2branding.Pack
		if err := json.Unmarshal(pack.Body.Bytes(), &p); err != nil {
			t.Fatalf("pack.json: %v", err)
		}
		var d discovery.Document
		if err := json.Unmarshal(disc.Body.Bytes(), &d); err != nil {
			t.Fatalf("discovery: %v", err)
		}
		if d.DisplayName != p.Product.Name {
			t.Errorf("discovery display_name %q != pack.json product.name %q", d.DisplayName, p.Product.Name)
		}
		return state{
			bootETag: boot.Header().Get("ETag"),
			packETag: pack.Header().Get("ETag"),
			layers:   pack.Header().Get(v2branding.LayersHeader),
			name:     p.Product.Name,
			packURL:  d.BrandPackURL,
		}
	}

	before := read(t)
	if before.layers != "default" || before.name != v2branding.ProductDefault().Product.Name {
		t.Fatalf("fresh install: layers %q name %q, want the product default", before.layers, before.name)
	}
	if want := origin + v2branding.PackJSONPath + "?v=" + strings.Trim(before.packETag, `"`); before.packURL != want {
		t.Fatalf("fresh install: discovery brand_pack_url = %q, want %q", before.packURL, want)
	}

	rec := configDo(t, router, http.MethodPut, "/admin/branding/administration", map[string]any{
		"values": map[string]any{platformconfig.KeyBrandingProductName: "Acme AI"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d (body %s)", rec.Code, rec.Body.String())
	}

	after := read(t)
	if after.name != "Acme AI" || after.layers != "default, db" {
		t.Fatalf("after save: name %q layers %q", after.name, after.layers)
	}
	if after.bootETag == before.bootETag {
		t.Fatal("bootstrap ETag did not move across a save")
	}
	if after.packETag == before.packETag {
		t.Fatal("pack.json ETag did not move across a save")
	}
	if want := origin + v2branding.PackJSONPath + "?v=" + strings.Trim(after.packETag, `"`); after.packURL != want {
		t.Errorf("discovery brand_pack_url = %q, want %q", after.packURL, want)
	}
}
