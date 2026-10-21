package admin_test

// #1057, against a real `centry.platform_config`: the server-side Reset
// deletes every row of the section (the ones no editor renders included),
// and the asset collection a save runs spares an upload younger than the
// grace period while still collecting an old unreferenced one.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/admin"
	v2branding "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/branding"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

func brandingResetEnvironment(t *testing.T) (chi.Router, *memoryObjectStore, *v2branding.AssetStore, func() int) {
	t.Helper()
	pool := newConfigPool(t)
	objects := &memoryObjectStore{}
	assets := v2branding.NewAssetStore(objects)
	resolver := v2branding.NewResolver(v2branding.ResolverConfig{Pool: pool, TTL: time.Hour})
	handler := admin.NewHandler(pool,
		admin.WithBranding(resolver),
		admin.WithBrandingAssets(assets),
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
	router.Delete("/admin/branding/administration", handler.BrandingReset)
	rows := func() int {
		var count int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM centry.platform_config WHERE section = $1`,
			platformconfig.SectionBranding).Scan(&count); err != nil {
			t.Fatalf("count branding rows: %v", err)
		}
		return count
	}
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM centry.platform_config WHERE section = $1`, platformconfig.SectionBranding); err != nil {
		t.Fatalf("clear branding rows: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM centry.platform_config WHERE section = $1`, platformconfig.SectionBranding)
	})
	// The rows the client's old reset emulation missed: a package import's
	// scheme tokens and the e-mail sender name are written outside the page.
	if _, err := pool.Exec(context.Background(), `
INSERT INTO centry.platform_config (section, key, value, updated_at, updated_by) VALUES
  ($1, 'scheme_tokens', '{"light":{"text.primary":"#101010"},"dark":{}}'::jsonb, (now() AT TIME ZONE 'UTC'), 'it'),
  ($1, 'sender_name', '"Acme Mail"'::jsonb, (now() AT TIME ZONE 'UTC'), 'it')`, platformconfig.SectionBranding); err != nil {
		t.Fatalf("seed branding rows: %v", err)
	}
	return router, objects, assets, rows
}

func TestBranding_ResetDeletesEveryRowOfTheSection(t *testing.T) {
	router, _, _, rows := brandingResetEnvironment(t)
	rec := configDo(t, router, http.MethodPut, "/admin/branding/administration", map[string]any{
		"values": map[string]any{
			platformconfig.KeyBrandingProductName: "Acme AI",
			platformconfig.KeyBrandingHue:         "#FF6600",
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d (body %s)", rec.Code, rec.Body.String())
	}
	if got := rows(); got != 4 {
		t.Fatalf("rows before reset = %d, want 4", got)
	}

	rec = configDo(t, router, http.MethodDelete, "/admin/branding/administration", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d (body %s)", rec.Code, rec.Body.String())
	}
	reset := decodeBranding(t, rec.Body.Bytes())
	if !reset.Saved || reset.Layers.Database || reset.Effective != nil {
		t.Fatalf("after reset: saved %v layers %+v effective %v", reset.Saved, reset.Layers, reset.Effective)
	}
	if got := reset.Values[platformconfig.KeyBrandingProductName]; got != "" {
		t.Fatalf("product_name after reset = %v, want the inherit sentinel", got)
	}
	// No rows of empty values: the scheme tokens and the sender name are gone
	// with the keys the page edits.
	if got := rows(); got != 0 {
		t.Fatalf("rows after reset = %d, want 0", got)
	}
}

func TestBranding_SaveSparesAFreshUnreferencedUploadAndCollectsAnOldOne(t *testing.T) {
	router, objects, assets, _ := brandingResetEnvironment(t)
	ctx := context.Background()
	old, err := assets.Put(ctx, v2branding.KindLogoFull, "old.svg", []byte(cleanLogoSVG))
	if err != nil {
		t.Fatal(err)
	}
	objects.age(old.Digest, time.Hour)
	// Another administrator's upload, not saved yet: referenced by nothing.
	fresh, err := assets.Put(ctx, v2branding.KindLogoMark, "fresh.svg",
		[]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 4 4"><rect width="4" height="4"/></svg>`))
	if err != nil {
		t.Fatal(err)
	}

	// This administrator saves something unrelated.
	rec := configDo(t, router, http.MethodPut, "/admin/branding/administration", map[string]any{
		"values": map[string]any{platformconfig.KeyBrandingProductName: "Acme AI"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d (body %s)", rec.Code, rec.Body.String())
	}

	if _, _, err := assets.Get(ctx, fresh.Path); err != nil {
		t.Fatalf("the fresh upload was collected by another save: %v", err)
	}
	if _, _, err := assets.Get(ctx, old.Path); err == nil {
		t.Fatal("the old unreferenced asset survived the save")
	}

	// The uploader's save then references a file that still exists.
	rec = configDo(t, router, http.MethodPut, "/admin/branding/administration", map[string]any{
		"values": map[string]any{platformconfig.KeyBrandingLogoMark: fresh.Path},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("uploader's PUT status = %d (body %s)", rec.Code, rec.Body.String())
	}
	if _, _, err := assets.Get(ctx, fresh.Path); err != nil {
		t.Fatalf("the referenced upload is missing: %v", err)
	}
}
