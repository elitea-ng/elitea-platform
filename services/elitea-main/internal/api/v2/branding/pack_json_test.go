package branding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/httpcache"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

func servePackJSON(t *testing.T, h *Handler, method, target string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	h.PackJSON(rec, req)
	return rec
}

func decodePack(t *testing.T, rec *httptest.ResponseRecorder) *Pack {
	t.Helper()
	pack, err := ParsePack(rec.Body.Bytes())
	if err != nil {
		t.Fatalf("pack.json body does not parse as a brand pack: %v\n%s", err, rec.Body.String())
	}
	return pack
}

func TestPackJSON_HeaderMatrix(t *testing.T) {
	h := NewHandler(Config{
		Resolver:     NewResolver(ResolverConfig{PackPath: filePack(t, "Acme AI")}),
		PublicOrigin: "https://elitea.example.com",
	})
	snap := h.Resolver().Current(context.Background())
	version := snap.PackJSONVersion("https://elitea.example.com")

	t.Run("bare URL revalidates against the body's own ETag", func(t *testing.T) {
		rec := servePackJSON(t, h, http.MethodGet, PackJSONPath, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("code %d", rec.Code)
		}
		if got := rec.Header().Get("ETag"); got != `"`+version+`"` {
			t.Errorf("ETag = %q, want the pack.json version %q", got, version)
		}
		if got := rec.Header().Get("Cache-Control"); got != cacheRevalidate {
			t.Errorf("Cache-Control = %q", got)
		}
		if got := rec.Header().Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Error("missing nosniff")
		}
		if rec.Header().Get("Vary") != "" {
			t.Errorf("Vary = %q with a configured origin, want none", rec.Header().Get("Vary"))
		}
		if got := rec.Header().Get(LayersHeader); got != "file" {
			t.Errorf("%s = %q, want file", LayersHeader, got)
		}
		pack := decodePack(t, rec)
		if pack.Product.Name != "Acme AI" || pack.Assets.LogoFull != "https://elitea.example.com/app/brand/l.svg" {
			t.Errorf("pack = %+v", pack)
		}
	})

	t.Run("matching ?v= is immutable", func(t *testing.T) {
		rec := servePackJSON(t, h, http.MethodGet, PackJSONPath+"?v="+version, nil)
		if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != cacheImmutable {
			t.Errorf("code %d, Cache-Control %q", rec.Code, rec.Header().Get("Cache-Control"))
		}
	})

	t.Run("stale ?v= redirects to the current one", func(t *testing.T) {
		rec := servePackJSON(t, h, http.MethodGet, PackJSONPath+"?v=deadbeef", nil)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != PackJSONPath+"?v="+version {
			t.Errorf("code %d, Location %q", rec.Code, rec.Header().Get("Location"))
		}
	})

	t.Run("If-None-Match answers 304", func(t *testing.T) {
		rec := servePackJSON(t, h, http.MethodGet, PackJSONPath, http.Header{"If-None-Match": {`W/"` + version + `"`}})
		if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
			t.Errorf("code %d, body %q", rec.Code, rec.Body.String())
		}
	})

	t.Run("HEAD carries the GET headers and no body", func(t *testing.T) {
		get := servePackJSON(t, h, http.MethodGet, PackJSONPath, nil)
		head := servePackJSON(t, h, http.MethodHead, PackJSONPath, nil)
		if head.Code != http.StatusOK || head.Body.Len() != 0 ||
			head.Header().Get("Content-Length") != get.Header().Get("Content-Length") {
			t.Errorf("HEAD code %d, body %d, Content-Length %q vs %q", head.Code, head.Body.Len(),
				head.Header().Get("Content-Length"), get.Header().Get("Content-Length"))
		}
	})
}

// An unbranded deployment serves the PRODUCT default (never DefaultPack(),
// whose placeholder hue repainted the app), with 200 and a layer header that
// says so, while bootstrap.js keeps its inert body.
func TestPackJSON_UnbrandedServesTheProductDefault(t *testing.T) {
	h := NewHandler(Config{
		Resolver:     NewResolver(ResolverConfig{loadOverlay: overlayLoader(platformconfig.BrandingOverlay{}, nil)}),
		PublicOrigin: "https://elitea.example.com",
	})
	if !strings.Contains(string(h.Resolver().Current(context.Background()).Body), "no deployment brand pack configured") {
		t.Fatal("bootstrap.js should stay inert on an unbranded deployment")
	}
	rec := servePackJSON(t, h, http.MethodGet, PackJSONPath, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d", rec.Code)
	}
	if got := rec.Header().Get(LayersHeader); got != "default" {
		t.Errorf("%s = %q, want default", LayersHeader, got)
	}
	pack := decodePack(t, rec)
	def := ProductDefault()
	if pack.ID != def.ID || pack.Brand.Hue != def.Brand.Hue || pack.Brand.Hue == DefaultPack().Brand.Hue {
		t.Errorf("served id %q hue %q, want the product default's %q %q", pack.ID, pack.Brand.Hue, def.ID, def.Brand.Hue)
	}
	// The product default's "./brand/…" placeholders are document-relative:
	// they resolve under the web app's /app/, where its image serves them
	// (apps/elitea-web/nginx/spa.conf). Nothing serves /brand/ at the root.
	if pack.Assets.LogoFull != "https://elitea.example.com/app/brand/logo-full.svg" {
		t.Errorf("logoFull = %q", pack.Assets.LogoFull)
	}
	if pack.Product.DocsURL == nil || *pack.Product.DocsURL != "https://elitea.example.com/docs/" {
		t.Errorf("docsUrl = %v", pack.Product.DocsURL)
	}
}

func TestPackJSON_DatabaseLayerOverProductDefault(t *testing.T) {
	h := NewHandler(Config{
		Resolver: NewResolver(ResolverConfig{loadOverlay: overlayLoader(platformconfig.BrandingOverlay{
			ProductName: "Overlay Co",
			LogoMark:    "/api/v2/branding/assets/logo-mark/" + strings.Repeat("ab", 32) + ".svg",
			Favicon:     "data:image/svg+xml;base64,PHN2Zy8+",
			FontFaces:   []platformconfig.FontFaceOverlay{{Family: "Brand", URL: "/api/v2/branding/assets/font/" + strings.Repeat("cd", 32) + ".woff2"}},
		}, nil)}),
		PublicOrigin: "https://elitea.example.com",
	})
	rec := servePackJSON(t, h, http.MethodGet, PackJSONPath, nil)
	if got := rec.Header().Get(LayersHeader); got != "default, db" {
		t.Errorf("%s = %q, want \"default, db\"", LayersHeader, got)
	}
	pack := decodePack(t, rec)
	if pack.Product.Name != "Overlay Co" {
		t.Errorf("name = %q", pack.Product.Name)
	}
	if want := "https://elitea.example.com/api/v2/branding/assets/logo-mark/" + strings.Repeat("ab", 32) + ".svg"; pack.Assets.LogoMark != want {
		t.Errorf("logoMark = %q, want %q", pack.Assets.LogoMark, want)
	}
	if pack.Assets.Favicon != "data:image/svg+xml;base64,PHN2Zy8+" {
		t.Errorf("a data: URI must pass through, got %q", pack.Assets.Favicon)
	}
	if len(pack.Typography.FontFaces) != 1 || !strings.HasPrefix(pack.Typography.FontFaces[0].URL, "https://elitea.example.com/api/v2/branding/assets/font/") {
		t.Errorf("fontFaces = %+v", pack.Typography.FontFaces)
	}
	// The resolver's own pack is not modified by absolutisation.
	if snap := h.Resolver().Current(context.Background()); strings.HasPrefix(snap.Pack.Assets.LogoMark, "https://") {
		t.Errorf("absolutize mutated the shared pack: %q", snap.Pack.Assets.LogoMark)
	}
}

// With no configured origin the body is built from the request Host: two
// hosts get two bodies, two entity tags, Vary, and never immutable.
func TestPackJSON_RequestDerivedOrigin(t *testing.T) {
	h := NewHandler(Config{Resolver: NewResolver(ResolverConfig{PackPath: filePack(t, "Acme AI")})})
	snap := h.Resolver().Current(context.Background())

	a := servePackJSON(t, h, http.MethodGet, "http://a.example"+PackJSONPath+"?v="+snap.PackJSONVersion("http://a.example"), nil)
	b := servePackJSON(t, h, http.MethodGet, "http://b.example"+PackJSONPath+"?v="+snap.PackJSONVersion("https://b.example"),
		http.Header{"X-Forwarded-Proto": {"https"}, "X-Forwarded-Host": {"evil.example"}})
	if a.Code != http.StatusOK || b.Code != http.StatusOK {
		t.Fatalf("codes %d %d", a.Code, b.Code)
	}
	if a.Header().Get("Vary") == "" || a.Header().Get("Cache-Control") != cacheRevalidate {
		t.Errorf("Vary %q, Cache-Control %q", a.Header().Get("Vary"), a.Header().Get("Cache-Control"))
	}
	if a.Header().Get("ETag") == b.Header().Get("ETag") {
		t.Error("two origins share one entity tag")
	}
	if got := decodePack(t, a).Assets.LogoFull; got != "http://a.example/app/brand/l.svg" {
		t.Errorf("a logoFull = %q", got)
	}
	if got := decodePack(t, b).Assets.LogoFull; got != "https://b.example/app/brand/l.svg" {
		t.Errorf("b logoFull = %q (X-Forwarded-Host must be ignored)", got)
	}
	// 304 still works against the request-derived tag.
	again := servePackJSON(t, h, http.MethodGet, "http://a.example"+PackJSONPath, http.Header{"If-None-Match": {a.Header().Get("ETag")}})
	if again.Code != http.StatusNotModified {
		t.Errorf("revalidation code %d", again.Code)
	}
}

func TestSnapshotHelpers(t *testing.T) {
	for _, tc := range []struct {
		layers Layers
		want   string
	}{
		{Layers{}, "default"},
		{Layers{Database: true}, "default,db"},
		{Layers{File: true}, "file"},
		{Layers{File: true, Database: true}, "file,db"},
	} {
		if got := strings.Join(Snapshot{Layers: tc.layers}.LayerNames(), ","); got != tc.want {
			t.Errorf("%+v → %q, want %q", tc.layers, got, tc.want)
		}
	}
	if got := (Snapshot{}).DisplayName(); got != ProductDefault().Product.Name {
		t.Errorf("nil-pack display name = %q", got)
	}
	// The JSON body is the pack, not a script.
	var probe map[string]any
	if err := json.Unmarshal(packJSONBody(Snapshot{}, "https://x.example"), &probe); err != nil {
		t.Fatal(err)
	}
}

// pack.json's entity tag and ?v= token cover ITS body, not bootstrap.js's:
// the body carries absolute URLs (the origin) and, unbranded, the embedded
// product default — neither of which is in the bootstrap ETag. A versioned
// URL is cached as immutable, so a token that does not move when the body
// does pins clients to a stale pack (old origin, old default) for a year.
func TestPackJSON_EntityTagCoversTheBody(t *testing.T) {
	for name, cfg := range map[string]ResolverConfig{
		"branded":   {PackPath: filePack(t, "Acme AI")},
		"unbranded": {loadOverlay: overlayLoader(platformconfig.BrandingOverlay{}, nil)},
	} {
		t.Run(name, func(t *testing.T) {
			resolver := NewResolver(cfg)
			oldOrigin := NewHandler(Config{Resolver: resolver, PublicOrigin: "https://old.example.com"})
			newOrigin := NewHandler(Config{Resolver: resolver, PublicOrigin: "https://new.example.com"})
			a := servePackJSON(t, oldOrigin, http.MethodGet, PackJSONPath, nil)
			b := servePackJSON(t, newOrigin, http.MethodGet, PackJSONPath, nil)
			for _, rec := range []*httptest.ResponseRecorder{a, b} {
				want, _ := httpcache.StrongETag(rec.Body.Bytes())
				if got := rec.Header().Get("ETag"); got != want {
					t.Fatalf("ETag = %q, want the strong tag of the body %q", got, want)
				}
			}
			if a.Header().Get("ETag") == b.Header().Get("ETag") {
				t.Fatal("two origins (two bodies) share one entity tag")
			}
			snap := resolver.Current(context.Background())
			version := snap.PackJSONVersion("https://new.example.com")
			if `"`+version+`"` != b.Header().Get("ETag") {
				t.Fatalf("PackJSONVersion = %q, want the served ETag %s", version, b.Header().Get("ETag"))
			}
			// The old origin's versioned URL is stale on the new origin.
			oldVersion := strings.Trim(a.Header().Get("ETag"), `"`)
			stale := servePackJSON(t, newOrigin, http.MethodGet, PackJSONPath+"?v="+oldVersion, nil)
			if stale.Code != http.StatusFound || stale.Header().Get("Location") != PackJSONPath+"?v="+version {
				t.Fatalf("stale ?v= = %d %q, want 302 to ?v=%s", stale.Code, stale.Header().Get("Location"), version)
			}
			current := servePackJSON(t, newOrigin, http.MethodGet, PackJSONPath+"?v="+version, nil)
			if current.Code != http.StatusOK || current.Header().Get("Cache-Control") != cacheImmutable {
				t.Fatalf("current ?v= = %d %q, want 200 immutable", current.Code, current.Header().Get("Cache-Control"))
			}
		})
	}
}
