package admin_test

// ADR-0024 WP9 acceptance: a branding package exported from a rebranded
// deployment imports into a fresh one and the served pack matches, a
// restore brings the previous brand back, and every step is asserted
// through the product's own routes and the rows.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/admin"
	v2branding "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/branding"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/brandpackage"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

const cleanLogoSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><rect width="10" height="10" fill="currentColor"/></svg>`

func packageEnvironment(t *testing.T) (chi.Router, *v2branding.Resolver, *v2branding.AssetStore) {
	t.Helper()
	pool := newConfigPool(t)
	assets := v2branding.NewAssetStore(&memoryObjectStore{})
	resolver := v2branding.NewResolver(v2branding.ResolverConfig{Pool: pool, TTL: time.Hour})
	packages := brandpackage.New(assets, brandpackage.Previews{}, "it")
	handler := admin.NewHandler(pool,
		admin.WithBranding(resolver),
		admin.WithBrandingAssets(assets),
		admin.WithBrandingPackages(packages),
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
	router.Get("/admin/branding/package/administration", handler.BrandingPackageExport)
	router.Post("/admin/branding/package/administration", handler.BrandingPackageImport)
	router.Get("/admin/branding/package/administration/versions", handler.BrandingPackageVersions)
	router.Post("/admin/branding/package/administration/versions/{digest}/restore", handler.BrandingPackageRestore)
	return router, resolver, assets
}

func TestBrandingPackage_ExportImportRestore(t *testing.T) {
	ctx := context.Background()

	// Deployment A: rebranded with an uploaded logo.
	routerA, _, assetsA := packageEnvironment(t)
	logo, err := assetsA.Put(ctx, v2branding.KindLogoFull, "logo.svg", []byte(cleanLogoSVG))
	if err != nil {
		t.Fatal(err)
	}
	rec := configDo(t, routerA, http.MethodPut, "/admin/branding/administration", map[string]any{"values": map[string]any{
		platformconfig.KeyBrandingProductName: "Acme AI",
		platformconfig.KeyBrandingHue:         "#FF6600",
		platformconfig.KeyBrandingLogoFull:    logo.Path,
	}})
	if rec.Code != http.StatusOK {
		t.Fatalf("brand A: %d %s", rec.Code, rec.Body.String())
	}
	rec = configDo(t, routerA, http.MethodGet, "/admin/branding/package/administration", nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("export: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	packageBytes := rec.Body.Bytes()

	// Deployment B: fresh. Dry run first, then apply.
	routerB, resolverB, _ := packageEnvironment(t)
	rec = httptest.NewRecorder()
	routerB.ServeHTTP(rec, multipartZip(t, "/admin/branding/package/administration?dry_run=true", packageBytes))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"dry_run":true`) || strings.Contains(rec.Body.String(), `"applied":true`) {
		t.Fatalf("dry run: %d %s", rec.Code, rec.Body.String())
	}
	if resolverB.Current(ctx).Pack != nil {
		t.Fatal("a dry run changed the served pack")
	}
	rec = httptest.NewRecorder()
	routerB.ServeHTTP(rec, multipartZip(t, "/admin/branding/package/administration", packageBytes))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"applied":true`) {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body.String())
	}
	served := resolverB.Current(ctx).Pack
	if served == nil || served.Product.Name != "Acme AI" || served.Brand.Hue != "#FF6600" {
		t.Fatalf("served pack after import = %+v", served)
	}
	if kind, _, _, ok := v2branding.ParseAssetPath(served.Assets.LogoFull); !ok || kind != v2branding.KindLogoFull {
		t.Fatalf("logo not re-homed as an asset on B: %q", served.Assets.LogoFull)
	}
	if len(served.Schemes.Light) != 0 {
		t.Fatalf("an untouched export pinned %d light tokens on import", len(served.Schemes.Light))
	}

	// A second brand on B, then restore the first from the kept versions.
	rec = configDo(t, routerB, http.MethodPut, "/admin/branding/administration", map[string]any{"values": map[string]any{
		platformconfig.KeyBrandingProductName: "Second Brand", platformconfig.KeyBrandingHue: "#00AA00",
	}})
	if rec.Code != http.StatusOK || resolverB.Current(ctx).Pack.Product.Name != "Second Brand" {
		t.Fatalf("second brand: %d", rec.Code)
	}
	rec = configDo(t, routerB, http.MethodGet, "/admin/branding/package/administration/versions", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"product":"Acme AI"`) {
		t.Fatalf("versions: %d %s", rec.Code, rec.Body.String())
	}
	digest := rec.Body.String()
	digest = digest[strings.Index(digest, `"digest":"`)+len(`"digest":"`):]
	digest = digest[:64]
	rec = configDo(t, routerB, http.MethodPost, "/admin/branding/package/administration/versions/"+digest+"/restore", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"applied":true`) {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body.String())
	}
	restored := resolverB.Current(ctx).Pack
	if restored == nil || restored.Product.Name != "Acme AI" || restored.Brand.Hue != "#FF6600" {
		t.Fatalf("restored pack = %+v", restored)
	}
}
