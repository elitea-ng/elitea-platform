package browserauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v2branding "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/branding"
)

type brandSourceStub struct{ pack *v2branding.Pack }

func (s brandSourceStub) Current(context.Context) v2branding.Snapshot {
	return v2branding.Snapshot{Pack: s.pack}
}

func ptr(s string) *string { return &s }

func brandedPack() *v2branding.Pack {
	pack := v2branding.DefaultPack()
	pack.Product.Name = "Acme <AI>"
	pack.Product.Tagline = ptr("Ship faster & safer")
	pack.Brand.Hue = "#FF6600"
	pack.Brand.OnBrand = ptr("#101010")
	pack.Assets.LogoFull = "/api/v2/branding/assets/logo-full/" + strings.Repeat("ab", 32) + ".svg"
	pack.Assets.Favicon = "/api/v2/branding/assets/favicon/" + strings.Repeat("cd", 32) + ".png"
	pack.Assets.LoginArt = ptr("/api/v2/branding/assets/login-art/" + strings.Repeat("ef", 32) + ".webp")
	pack.Typography.FontFamily = `"Inter", Arial, sans-serif`
	pack.Typography.FontFaces = []v2branding.FontFace{{
		Family: "Inter", URL: "/api/v2/branding/assets/font/" + strings.Repeat("12", 32) + ".woff2", Weight: ptr("100 900"),
	}}
	pack.Shape.RadiusMd = 14
	return pack
}

func TestLoginBrandFromPack_AllowlistsEveryValue(t *testing.T) {
	brand := loginBrandFromPack(brandedPack())
	if brand.ProductName != "Acme <AI>" || brand.Tagline != "Ship faster & safer" {
		t.Fatalf("brand = %+v", brand)
	}
	if brand.LogoURL == "" || brand.FaviconURL == "" {
		t.Fatalf("assets dropped: %+v", brand)
	}
	css := string(brand.Style)
	for _, want := range []string{
		// The hue paints both schemes; onBrand is the text on it.
		":root{--brand-light:#ff6600;--on-brand-light:#101010;--brand-dark:#ff6600;--on-brand-dark:#101010}",
		`@font-face{font-family:"Inter";src:url("/api/v2/branding/assets/font/`,
		";font-weight:100 900}",
		`:root{font-family:"Inter", Arial, sans-serif}`,
		":root{--radius-md:14px;--radius-lg:12px;--radius-pill:9999px}",
		`body{background-image:url("/api/v2/branding/assets/login-art/`,
		// The operator's artwork replaces the generated rings.
		".auth-backdrop{display:none}",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("brand css lacks %q:\n%s", want, css)
		}
	}
	digest := sha256.Sum256([]byte(css))
	if want := "'sha256-" + base64.StdEncoding.EncodeToString(digest[:]) + "'"; brand.StyleSource != want {
		t.Errorf("StyleSource = %q, want %q", brand.StyleSource, want)
	}

	// Hostile values degrade to nothing, field by field.
	hostile := v2branding.DefaultPack()
	hostile.Brand.Hue = "red;}body{display:none"
	hostile.Assets.LogoFull = `/x.svg" onerror="alert(1)`
	hostile.Assets.Favicon = "//evil.example/f.ico"
	hostile.Assets.LoginArt = ptr(`/art.png")}body{background:url(https://evil.example/x`)
	hostile.Typography.FontFamily = `Inter}; body{display:none}`
	hostile.Typography.FontFaces = []v2branding.FontFace{{Family: "x", URL: "https://fonts.example/x.woff2"}}
	hostile.Shape = v2branding.Shape{RadiusMd: -5, RadiusLg: 1e9, RadiusPill: -1}
	hostile.Schemes.Light = map[string]string{
		"primary.main":            "red;}body{display:none",
		"background.default":      "url(https://evil.example/x)",
		"text.secondary":          "rgb(1,2,3);}*{color:red",
		"background.card.default": "linear-gradient(red, blue)",
	}
	got := loginBrandFromPack(hostile)
	if got.LogoURL != "" || got.FaviconURL != "" {
		t.Errorf("hostile asset paths admitted: %+v", got)
	}
	if got.Style != "" || got.StyleSource != "" {
		t.Errorf("hostile values produced css: %q", got.Style)
	}

	// The compiled default's relative placeholders are not root paths and
	// render no logo — the pre-branding presentation.
	plain := v2branding.DefaultPack()
	plain.Assets.LogoFull = "./brand/logo-full.svg"
	if loginBrandFromPack(plain).LogoURL != "" {
		t.Error("a document-relative placeholder was rendered as a logo")
	}
}

func TestFormPageRendersTheBrandUnderTheHashedCSP(t *testing.T) {
	handler, dependencies := newTestHandler(t)
	handler.brand = brandSourceStub{pack: brandedPack()}
	request := httptest.NewRequest(
		http.MethodGet,
		BasePath+FormLoginPath+"?target_to="+dependencies.flow.beginResult.TransactionID,
		nil,
	)
	request.AddCookie(sessionCookie(dependencies.flow.beginResult.SessionID))
	recorder := httptest.NewRecorder()
	mount(handler).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, marker := range []string{
		"<title>Acme &lt;AI&gt; login</title>",
		`<img class="brand-logo" src="/api/v2/branding/assets/logo-full/`,
		`alt="Acme &lt;AI&gt;"`,
		`<p class="brand-tagline">Ship faster &amp; safer</p>`,
		`<link rel="icon" href="/api/v2/branding/assets/favicon/`,
		"--brand-light:#ff6600;",
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("branded login page is missing %q", marker)
		}
	}
	for _, forbidden := range []string{"http://", "https://", "<AI>", `class="brand-mark"`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("branded login page contains %q", forbidden)
		}
	}

	// Both style blocks are hash-pinned; images and fonts are same-origin only.
	csp := recorder.Header().Get("Content-Security-Policy")
	if strings.Contains(csp, "'unsafe-inline'") || !strings.Contains(csp, "img-src 'self'") || !strings.Contains(csp, "font-src 'self'") {
		t.Fatalf("CSP = %q", csp)
	}
	rest := body
	for i := 0; i < 2; i++ {
		start := strings.Index(rest, "<style>")
		end := strings.Index(rest, "</style>")
		if start < 0 || end <= start {
			t.Fatalf("expected two style blocks, found %d", i)
		}
		digest := sha256.Sum256([]byte(rest[start+len("<style>") : end]))
		source := "'sha256-" + base64.StdEncoding.EncodeToString(digest[:]) + "'"
		if !strings.Contains(csp, source) {
			t.Errorf("style block %d hash %s absent from CSP %q", i, source, csp)
		}
		rest = rest[end+len("</style>"):]
	}

	// No pack served: the default presentation, one style block, no logo.
	handler.brand = brandSourceStub{pack: nil}
	recorder = httptest.NewRecorder()
	mount(handler).ServeHTTP(recorder, request)
	plain := recorder.Body.String()
	if !strings.Contains(plain, "<title>Elitea login</title>") || strings.Contains(plain, `class="brand-logo"`) ||
		!strings.Contains(plain, `class="brand-mark"`) || strings.Count(plain, "<style>") != 1 {
		t.Fatalf("unbranded page: %s", plain)
	}
}
