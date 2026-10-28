package adminui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"
)

// The default brand pack names its typefaces `./brand/fonts/*.woff2`; the
// console resolves them against its base, so the handler must serve
// <BasePath>/brand/* from <StaticDir>/brand (brand guide v3).
func TestRoutes_ServeBrandFonts(t *testing.T) {
	staticDir := t.TempDir()
	fonts := filepath.Join(staticDir, "brand", "fonts")
	if err := os.MkdirAll(fonts, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fonts, "manrope.woff2"), []byte("wOF2"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{StaticDir: staticDir, BasePath: "/admin/app", SecretKey: cspTestSecret})
	r := chi.NewRouter()
	r.Mount("/admin/app", h.Routes())

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/app/brand/fonts/manrope.woff2", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "wOF2" {
		t.Fatalf("GET brand font: status %d body %q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/app/brand/fonts/missing.woff2", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a missing font must 404, not fall through to the SPA: status %d", rec.Code)
	}
}
