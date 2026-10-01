package admin_test

// The branding package routes, driven without a database: the export's
// headers and bytes, the import's dry run and its refusals, the version list
// and the restore's guards, over a fake package service.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/admin"
	v2branding "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/branding"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/brandpackage"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

type fakePackages struct {
	exported  []byte
	problems  []brandpackage.Problem
	pack      *v2branding.Pack
	versions  []brandpackage.Version
	stored    [][]byte
	loadErr   error
	applyErr  error
	applyKeys map[string]any
}

func (f *fakePackages) Export(context.Context, *v2branding.Pack) ([]byte, string, error) {
	return f.exported, "acme-branding.zip", nil
}
func (f *fakePackages) Parse([]byte) (*brandpackage.Imported, []brandpackage.Problem) {
	if len(f.problems) > 0 {
		return &brandpackage.Imported{Warnings: []string{"ignored entry notes.txt"}}, f.problems
	}
	return &brandpackage.Imported{Pack: f.pack, Manifest: &brandpackage.Manifest{Format: 1, Product: f.pack.Product.Name}}, nil
}
func (f *fakePackages) Apply(context.Context, *brandpackage.Imported) (map[string]any, error) {
	return f.applyKeys, f.applyErr
}
func (f *fakePackages) Store(_ context.Context, data []byte) (brandpackage.Version, error) {
	f.stored = append(f.stored, data)
	return brandpackage.Version{Digest: strings.Repeat("ab", 32)}, nil
}
func (f *fakePackages) Versions(context.Context) ([]brandpackage.Version, error) {
	return f.versions, nil
}
func (f *fakePackages) Load(context.Context, string) ([]byte, error) {
	return f.exported, f.loadErr
}

func packageRouter(handler *admin.Handler) chi.Router {
	r := chi.NewRouter()
	r.Get("/admin/branding/package/administration", handler.BrandingPackageExport)
	r.Post("/admin/branding/package/administration", handler.BrandingPackageImport)
	r.Get("/admin/branding/package/administration/versions", handler.BrandingPackageVersions)
	r.Post("/admin/branding/package/administration/versions/{digest}/restore", handler.BrandingPackageRestore)
	return r
}

func multipartZip(t *testing.T, target string, data []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, _ := w.CreateFormFile("file", "brand.zip")
	_, _ = part.Write(data)
	_ = w.Close()
	req := httptest.NewRequest(http.MethodPost, target, &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	return req
}

func TestNativeBrandingPackageRoundTrip(t *testing.T) {
	preview := []byte("<html><head></head><body>" + strings.Repeat("x", 1_004_110) + "</body></html>")
	packages := brandpackage.New(nil, brandpackage.Previews{App: preview}, "test")
	router := packageRouter(admin.NewHandler(nil, admin.WithBrandingPackages(packages)))
	exported := httptest.NewRecorder()
	router.ServeHTTP(exported, httptest.NewRequest(http.MethodGet, "/admin/branding/package/administration", nil))
	if exported.Code != http.StatusOK {
		t.Fatalf("export status %d: %s", exported.Code, exported.Body.String())
	}
	checked := httptest.NewRecorder()
	router.ServeHTTP(checked, multipartZip(t, "/admin/branding/package/administration?dry_run=true", exported.Body.Bytes()))
	if checked.Code != http.StatusOK {
		t.Fatalf("dry-run status %d: %s", checked.Code, checked.Body.String())
	}
	var report struct {
		OK       bool                   `json:"ok"`
		DryRun   bool                   `json:"dry_run"`
		Applied  bool                   `json:"applied"`
		Problems []brandpackage.Problem `json:"problems"`
		Manifest *brandpackage.Manifest `json:"manifest"`
	}
	if err := json.Unmarshal(checked.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if !report.OK || !report.DryRun || report.Applied || len(report.Problems) != 0 ||
		report.Manifest == nil || report.Manifest.Product != v2branding.ProductDefault().Product.Name {
		t.Fatalf("dry-run report = %+v", report)
	}
}

func TestBrandingPackageRoutes(t *testing.T) {
	pack := v2branding.ProductDefault()
	pack.Product.Name = "Acme AI"
	pack.Brand.Hue = "#FF6600"
	fake := &fakePackages{exported: []byte("PK\x03\x04zip-bytes"), pack: pack}
	handler := admin.NewHandler(nil, admin.WithBrandingPackages(fake))
	router := packageRouter(handler)
	serve := func(req *http.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	t.Run("export streams a zip download", func(t *testing.T) {
		rec := serve(httptest.NewRequest(http.MethodGet, "/admin/branding/package/administration", nil))
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/zip" ||
			!strings.Contains(rec.Header().Get("Content-Disposition"), `filename="acme-branding.zip"`) ||
			!bytes.Equal(rec.Body.Bytes(), fake.exported) {
			t.Fatalf("status %d headers %v", rec.Code, rec.Header())
		}
	})

	t.Run("dry run reports the diff and stores nothing", func(t *testing.T) {
		rec := serve(multipartZip(t, "/admin/branding/package/administration?dry_run=true", fake.exported))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var report struct {
			OK       bool                   `json:"ok"`
			DryRun   bool                   `json:"dry_run"`
			Applied  bool                   `json:"applied"`
			Diff     []brandpackage.Change  `json:"diff"`
			Manifest *brandpackage.Manifest `json:"manifest"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if !report.OK || !report.DryRun || report.Applied || report.Manifest == nil || report.Manifest.Product != "Acme AI" {
			t.Fatalf("report = %+v", report)
		}
		keys := map[string]bool{}
		for _, change := range report.Diff {
			keys[change.Key] = true
		}
		if !keys[platformconfig.KeyBrandingProductName] || !keys[platformconfig.KeyBrandingHue] {
			t.Fatalf("diff lacks the changed keys: %+v", report.Diff)
		}
		if len(fake.stored) != 0 {
			t.Fatal("a dry run stored a package")
		}
	})

	t.Run("a refused package answers 400 with every problem", func(t *testing.T) {
		refusing := &fakePackages{problems: []brandpackage.Problem{{Entry: "assets/logo-full.svg", Reason: "the SVG contains a <script> element"}}}
		r := packageRouter(admin.NewHandler(nil, admin.WithBrandingPackages(refusing)))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, multipartZip(t, "/admin/branding/package/administration", []byte("PK\x03\x04x")))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "assets/logo-full.svg") || !strings.Contains(rec.Body.String(), "ignored entry") {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("apply without a database answers 503 after the checks", func(t *testing.T) {
		rec := serve(multipartZip(t, "/admin/branding/package/administration", fake.exported))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("missing file part is 400", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/admin/branding/package/administration", strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		if rec := serve(req); rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status %d", rec.Code)
		}
	})

	t.Run("versions list and restore guards", func(t *testing.T) {
		fake.versions = []brandpackage.Version{{Digest: strings.Repeat("cd", 32), Product: "Old"}}
		rec := serve(httptest.NewRequest(http.MethodGet, "/admin/branding/package/administration/versions", nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"product":"Old"`) {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		rec = serve(httptest.NewRequest(http.MethodPost, "/admin/branding/package/administration/versions/not-a-digest/restore", nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("bad digest: status %d", rec.Code)
		}
		fake.loadErr = errors.New("gone")
		rec = serve(httptest.NewRequest(http.MethodPost, "/admin/branding/package/administration/versions/"+strings.Repeat("cd", 32)+"/restore", nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("unknown version: status %d", rec.Code)
		}
	})

	t.Run("503 without a package service", func(t *testing.T) {
		bare := packageRouter(admin.NewHandler(nil))
		for _, req := range []*http.Request{
			httptest.NewRequest(http.MethodGet, "/admin/branding/package/administration", nil),
			multipartZip(t, "/admin/branding/package/administration", []byte("x")),
			httptest.NewRequest(http.MethodGet, "/admin/branding/package/administration/versions", nil),
		} {
			rec := httptest.NewRecorder()
			bare.ServeHTTP(rec, req)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("%s %s: status %d", req.Method, req.URL.Path, rec.Code)
			}
		}
	})
}
