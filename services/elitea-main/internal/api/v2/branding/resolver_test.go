package branding

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// filePack writes a schema-valid pack with a marker product name and returns
// its path. (pack_test.go's helpers live in the external test package; this
// file is internal so it can drive the clock and the overlay loader.)
func filePack(t *testing.T, name string) string {
	t.Helper()
	data := []byte(`{
  "$schema": "https://elitea.ai/schemas/brand-pack/1.json",
  "id": "file-layer",
  "version": "1.0.0",
  "product": {"name": ` + strconv.Quote(name) + `, "shortName": "FC"},
  "assets": {"logoFull": "/app/brand/l.svg", "logoMark": "/app/brand/m.svg", "favicon": "/app/brand/f.svg"},
  "typography": {"fontFamily": "sans-serif", "fontFamilyMono": "monospace"},
  "shape": {"radiusSm": 2, "radiusMd": 4, "radiusLg": 8, "radiusPill": 9999, "density": "comfortable"},
  "locale": {},
  "brand": {"hue": "#123456"},
  "schemes": {"light": {}, "dark": {}}
}`)
	path := filepath.Join(t.TempDir(), "brand-pack.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing pack: %v", err)
	}
	return path
}

type fakeClock struct{ at time.Time }

func (c *fakeClock) now() time.Time { return c.at }

func overlayLoader(o platformconfig.BrandingOverlay, err error) func(context.Context) (platformconfig.BrandingOverlay, error) {
	return func(context.Context) (platformconfig.BrandingOverlay, error) { return o, err }
}

func body(s Snapshot) string { return string(s.Body) }

func TestResolver_Layering(t *testing.T) {
	ctx := context.Background()

	t.Run("no file and an empty database layer publishes nothing", func(t *testing.T) {
		r := NewResolver(ResolverConfig{loadOverlay: overlayLoader(platformconfig.BrandingOverlay{}, nil)})
		snap := r.Current(ctx)
		if snap.Pack != nil || !strings.Contains(body(snap), "no deployment brand pack configured") {
			t.Fatalf("expected the inert body, got %q", body(snap))
		}
		if snap.Layers != (Layers{}) {
			t.Errorf("layers = %+v, want none", snap.Layers)
		}
	})

	t.Run("file only serves the file pack", func(t *testing.T) {
		r := NewResolver(ResolverConfig{PackPath: filePack(t, "File Co")})
		snap := r.Current(ctx)
		if snap.Pack == nil || snap.Pack.Product.Name != "File Co" {
			t.Fatalf("pack = %+v, want the file pack", snap.Pack)
		}
		if snap.Layers != (Layers{File: true}) {
			t.Errorf("layers = %+v", snap.Layers)
		}
	})

	t.Run("database only lays over the PRODUCT default, not DefaultPack", func(t *testing.T) {
		r := NewResolver(ResolverConfig{loadOverlay: overlayLoader(
			platformconfig.BrandingOverlay{Hue: "#FF6600", ProductName: "Acme AI"}, nil)})
		snap := r.Current(ctx)
		if snap.Pack == nil {
			t.Fatalf("expected a pack, got the inert body %q", body(snap))
		}
		if snap.Pack.Product.Name != "Acme AI" || snap.Pack.Brand.Hue != "#FF6600" {
			t.Errorf("overlay not applied: %+v", snap.Pack.Product)
		}
		// The base is the UI's compiled default: Manrope, 408 tokens per
		// scheme. DefaultPack() has empty schemes.
		if !strings.Contains(snap.Pack.Typography.FontFamily, "Manrope") {
			t.Errorf("base should be the product default, got font %q", snap.Pack.Typography.FontFamily)
		}
		// The overlay changed the hue, so the base's stated tokens are dropped
		// and every id derives from #FF6600 in the UI (see the hue subtest).
		if len(snap.Pack.Schemes.Light) != 0 {
			t.Errorf("stated tokens must not survive a hue change")
		}
		if snap.Layers != (Layers{Database: true}) {
			t.Errorf("layers = %+v", snap.Layers)
		}
	})

	t.Run("a new hue drops the stated tokens so the UI derives everything", func(t *testing.T) {
		r := NewResolver(ResolverConfig{loadOverlay: overlayLoader(platformconfig.BrandingOverlay{Hue: "#FF6600"}, nil)})
		snap := r.Current(ctx)
		if snap.Pack == nil || snap.Pack.Brand.Hue != "#FF6600" {
			t.Fatalf("pack = %+v", snap.Pack)
		}
		if len(snap.Pack.Schemes.Light) != 0 || len(snap.Pack.Schemes.Dark) != 0 {
			t.Fatalf("stated tokens survived a hue change: %d light, %d dark — the hue would be inert in the UI",
				len(snap.Pack.Schemes.Light), len(snap.Pack.Schemes.Dark))
		}
		// The SAME hue keeps them: nothing to re-derive.
		same := NewResolver(ResolverConfig{loadOverlay: overlayLoader(platformconfig.BrandingOverlay{Hue: "#4a4de8", ProductName: "X"}, nil)})
		if got := same.Current(ctx); len(got.Pack.Schemes.Light) == 0 {
			t.Fatal("the product default's own hue dropped its tokens")
		}
	})

	t.Run("database lays over the file pack field by field", func(t *testing.T) {
		r := NewResolver(ResolverConfig{
			PackPath:    filePack(t, "File Co"),
			loadOverlay: overlayLoader(platformconfig.BrandingOverlay{Hue: "#00FF00", BaseSize: 16}, nil),
		})
		snap := r.Current(ctx)
		if snap.Pack.Product.Name != "File Co" {
			t.Errorf("unset overlay field clobbered the file value: %q", snap.Pack.Product.Name)
		}
		if snap.Pack.Brand.Hue != "#00FF00" || snap.Pack.Typography.BaseSize != 16 {
			t.Errorf("overlay fields not applied: hue %q size %v", snap.Pack.Brand.Hue, snap.Pack.Typography.BaseSize)
		}
		if snap.Layers != (Layers{File: true, Database: true}) {
			t.Errorf("layers = %+v", snap.Layers)
		}
	})

	t.Run("an overlay the schema rejects falls back to the file layer", func(t *testing.T) {
		r := NewResolver(ResolverConfig{
			PackPath:    filePack(t, "File Co"),
			loadOverlay: overlayLoader(platformconfig.BrandingOverlay{BaseSize: 99}, nil), // outside [12,18]
		})
		snap := r.Current(ctx)
		if snap.Pack == nil || snap.Pack.Typography.BaseSize == 99 {
			t.Fatalf("invalid overlay was served or nothing was: %+v", snap.Pack)
		}
		if snap.Layers.Database {
			t.Errorf("a rejected database layer must not report as contributing")
		}
	})

	t.Run("an overlay the schema rejects with no file publishes nothing", func(t *testing.T) {
		r := NewResolver(ResolverConfig{loadOverlay: overlayLoader(platformconfig.BrandingOverlay{Density: "cosy"}, nil)})
		snap := r.Current(ctx)
		if snap.Pack != nil {
			t.Fatalf("expected the inert body, got a pack: %+v", snap.Pack)
		}
	})
}

func TestResolver_CacheAndFailure(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{at: time.Unix(1_700_000_000, 0)}

	overlay := platformconfig.BrandingOverlay{ProductName: "First"}
	var loadErr error
	calls := 0
	r := NewResolver(ResolverConfig{
		TTL: 15 * time.Second,
		now: clock.now,
		loadOverlay: func(context.Context) (platformconfig.BrandingOverlay, error) {
			calls++
			return overlay, loadErr
		},
	})

	first := r.Current(ctx)
	if first.Pack.Product.Name != "First" || calls != 1 {
		t.Fatalf("first resolve: name %q calls %d", first.Pack.Product.Name, calls)
	}

	// Within the TTL the store is not consulted, even after a change.
	overlay.ProductName = "Second"
	clock.at = clock.at.Add(10 * time.Second)
	if got := r.Current(ctx); got.Pack.Product.Name != "First" || calls != 1 {
		t.Fatalf("within TTL: name %q calls %d, want cached First and 1 call", got.Pack.Product.Name, calls)
	}

	// Invalidate makes the next read hit the store immediately.
	r.Invalidate()
	if got := r.Current(ctx); got.Pack.Product.Name != "Second" || calls != 2 {
		t.Fatalf("after Invalidate: name %q calls %d", got.Pack.Product.Name, calls)
	}
	if got := r.Current(ctx); got.ETag == first.ETag {
		t.Errorf("a changed pack kept the same ETag %q", got.ETag)
	}

	// Past the TTL the store is re-read.
	overlay.ProductName = "Third"
	clock.at = clock.at.Add(16 * time.Second)
	if got := r.Current(ctx); got.Pack.Product.Name != "Third" || calls != 3 {
		t.Fatalf("after TTL: name %q calls %d", got.Pack.Product.Name, calls)
	}

	// A failing store keeps the last good answer rather than blanking the app.
	loadErr = errors.New("connection refused")
	clock.at = clock.at.Add(16 * time.Second)
	if got := r.Current(ctx); got.Pack == nil || got.Pack.Product.Name != "Third" {
		t.Fatalf("store failure should keep the last good pack, got %+v", got.Pack)
	}

	// A failing store with NO last good answer serves the lower layers.
	fresh := NewResolver(ResolverConfig{
		PackPath:    filePack(t, "File Co"),
		loadOverlay: overlayLoader(platformconfig.BrandingOverlay{}, errors.New("down")),
	})
	if got := fresh.Current(ctx); got.Pack == nil || got.Pack.Product.Name != "File Co" {
		t.Fatalf("store failure with no cache should serve the file layer, got %+v", got.Pack)
	}
}

// TestProductDefault_PinnedToTheWebApp keeps the embedded base identical to
// the pack the web app compiles in, so "database layer over the product
// default" means exactly what the UI would render with channel C absent.
func TestProductDefault_PinnedToTheWebApp(t *testing.T) {
	if _, err := ParsePack(productDefaultJSON); err != nil {
		t.Fatalf("embedded product default does not parse: %v", err)
	}
	uiPath := filepath.Join("..", "..", "..", "..", "..", "..", "apps", "elitea-web", "src", "shared", "brand", "tokens", "default.pack.json")
	ui, err := os.ReadFile(uiPath)
	if err != nil {
		t.Skipf("web app default pack not present at %s: %v", uiPath, err)
	}
	if string(ui) != string(productDefaultJSON) {
		t.Fatalf("product_default.pack.json differs from %s; copy the web app's file over it", uiPath)
	}
}

func TestResolver_FontFacesOverlay(t *testing.T) {
	r := NewResolver(ResolverConfig{loadOverlay: overlayLoader(platformconfig.BrandingOverlay{
		FontFamily: `"Inter", sans-serif`,
		FontFaces: []platformconfig.FontFaceOverlay{
			{Family: "Inter", URL: "/api/v2/branding/assets/font/" + strings.Repeat("ab", 32) + ".woff2", Weight: "400"},
		},
	}, nil)})
	snap := r.Current(context.Background())
	if snap.Pack == nil || len(snap.Pack.Typography.FontFaces) != 1 {
		t.Fatalf("pack = %+v", snap.Pack)
	}
	face := snap.Pack.Typography.FontFaces[0]
	if face.Family != "Inter" || face.Weight == nil || *face.Weight != "400" || face.Style != nil {
		t.Fatalf("face = %+v", face)
	}
	if !strings.Contains(string(snap.Body), `"fontFaces":[{"family":"Inter"`) {
		t.Fatalf("body does not carry the face: %s", snap.Body)
	}
}

func TestResolver_SchemeTokensWinOverDerivation(t *testing.T) {
	r := NewResolver(ResolverConfig{loadOverlay: overlayLoader(platformconfig.BrandingOverlay{
		Hue:          "#FF6600",
		SchemeTokens: map[string]map[string]string{"light": {"primary.main": "#123456"}},
	}, nil)})
	snap := r.Current(context.Background())
	if snap.Pack == nil {
		t.Fatal("no pack")
	}
	if snap.Pack.Schemes.Light["primary.main"] != "#123456" || len(snap.Pack.Schemes.Light) != 1 {
		t.Fatalf("light tokens = %v", snap.Pack.Schemes.Light)
	}
	if len(snap.Pack.Schemes.Dark) != 0 {
		t.Fatalf("dark should derive from the hue, got %d stated tokens", len(snap.Pack.Schemes.Dark))
	}
}
