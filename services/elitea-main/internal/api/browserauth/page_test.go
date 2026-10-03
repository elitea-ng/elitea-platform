package browserauth

// The shared presentation of the two sign-in pages (page.go): both colour
// schemes, the hash-pinned theme script, and a page that loads nothing from
// anywhere.

import (
	"regexp"
	"strings"
	"testing"

	v2branding "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/branding"
)

// assertScriptPinned checks that body carries exactly one inline script, that
// it is the embedded theme script byte for byte, and that csp admits it by
// its hash and admits nothing else.
func assertScriptPinned(t *testing.T, body, csp string) {
	t.Helper()
	if n := strings.Count(body, "<script"); n != 1 {
		t.Fatalf("page carries %d scripts, want 1", n)
	}
	start := strings.Index(body, "<script>")
	end := strings.Index(body, "</script>")
	if start < 0 || end <= start {
		t.Fatal("the theme script is not a plain inline <script>")
	}
	script := body[start+len("<script>") : end]
	if script != authScriptSource {
		t.Fatalf("rendered script differs from the embedded source:\n%s", script)
	}
	want := "script-src " + cspHashSource(script) + ";"
	if !strings.Contains(csp, want) {
		t.Fatalf("CSP %q lacks %q", csp, want)
	}
	for _, forbidden := range []string{"'unsafe-inline'", "'unsafe-eval'", "'strict-dynamic'"} {
		if strings.Contains(csp, forbidden) {
			t.Errorf("CSP admits %s: %q", forbidden, csp)
		}
	}
}

func TestTheSSOPagePinsTheThemeScriptByHash(t *testing.T) {
	recorder := getLogin(newTestChooser(t, brandSourceStub{pack: brandedPack()}, gitHubProvider(), entraProvider()), "")
	assertScriptPinned(t, recorder.Body.String(), recorder.Header().Get("Content-Security-Policy"))
	// The re-rendered page after an email error carries the same policy.
	posted := postEmail(newTestChooser(t, nil, gitHubProvider(), entraProvider()), "bob@fabrikam.com", "/app")
	assertScriptPinned(t, posted.Body.String(), posted.Header().Get("Content-Security-Policy"))
}

func TestTheLoginPreviewCarriesTheThemeScript(t *testing.T) {
	body, err := RenderLoginPreview(brandedPack())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "<script>"+authScriptSource+"</script>") {
		t.Fatal("the preview lacks the theme script")
	}
}

// The script reads the web app's key and its three values; anything else,
// including no value, leaves the scheme to the stylesheet's OS media query.
func TestTheThemeScriptReadsTheAppsModeKey(t *testing.T) {
	for _, want := range []string{
		`var key = "el-mode";`,
		`value === "light" || value === "dark" || value === "system"`,
		`root.setAttribute("data-el-scheme", mode);`,
		`root.removeAttribute("data-el-scheme");`,
		`return "system";`,
		`window.addEventListener("storage"`,
		`window.localStorage.setItem(key, next);`,
	} {
		if !strings.Contains(authScriptSource, want) {
			t.Errorf("theme script lacks %q", want)
		}
	}
	// The script must not write markup or reach the network.
	for _, forbidden := range []string{"innerHTML", "document.write", "fetch(", "XMLHttpRequest", "eval(", "location"} {
		if strings.Contains(authScriptSource, forbidden) {
			t.Errorf("theme script uses %q", forbidden)
		}
	}
}

// darkBlock returns the declarations of the first rule for selector.
func darkBlock(t *testing.T, css, selector string) string {
	t.Helper()
	start := strings.Index(css, selector+" {")
	if start < 0 {
		t.Fatalf("stylesheet has no %q rule", selector)
	}
	rest := css[start+len(selector)+2:]
	end := strings.Index(rest, "}")
	lines := strings.Fields(rest[:end])
	return strings.Join(lines, " ")
}

func TestTheStylesheetCarriesBothSchemes(t *testing.T) {
	css := authStyleSource
	for _, want := range []string{
		"@media (prefers-color-scheme: dark)",
		`:root:not([data-el-scheme="light"])`,
		`:root[data-el-scheme="dark"]`,
		"--accent: var(--brand-light, #c428dd);",
		"--accent: var(--brand-dark, #6ae8fa);",
		"@media (prefers-reduced-motion: reduce)",
		"animation: none;",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("stylesheet lacks %q", want)
		}
	}
	// The OS-driven and the stored dark scheme are the same tokens.
	media := darkBlock(t, css, `  :root:not([data-el-scheme="light"])`)
	stored := darkBlock(t, css, `:root[data-el-scheme="dark"]`)
	if media != stored {
		t.Errorf("dark schemes differ:\nmedia:  %s\nstored: %s", media, stored)
	}
	// The light scheme is the default: no script, no OS preference.
	if !strings.HasPrefix(strings.TrimSpace(css[strings.Index(css, ":root {"):]), ":root {\n  color-scheme: light;") {
		t.Error("the default :root rule is not the light scheme")
	}
}

var externalReference = regexp.MustCompile(`(?i)(?:https?:|\bsrc=["']?//|href=["']?//|url\(\s*["']?//|@import|data:)`)

// Neither page, with or without a pack, refers to anything off this origin:
// no external stylesheet, font, image, script or data URI.
func TestTheSignInPagesLoadNothingExternal(t *testing.T) {
	pages := map[string]string{}
	for name, brand := range map[string]BrandSource{"default": nil, "branded": brandSourceStub{pack: brandedPack()}} {
		recorder := getLogin(newTestChooser(t, brand, gitHubProvider(), entraProvider()), "error=sso_failed")
		pages["sso/"+name] = recorder.Body.String()
	}
	for name, pack := range map[string]*v2branding.Pack{"default": nil, "branded": brandedPack()} {
		body, err := RenderLoginPreview(pack)
		if err != nil {
			t.Fatal(err)
		}
		pages["form/"+name] = string(body)
	}
	for name, body := range pages {
		if match := externalReference.FindString(body); match != "" {
			t.Errorf("%s: page refers to %q", name, match)
		}
		if strings.Contains(body, "<link") && !strings.Contains(body, `<link rel="icon" href="/api/v2/branding/assets/favicon/`) {
			t.Errorf("%s: page links something other than the pack's same-origin favicon", name)
		}
	}
}

func TestProviderButtonsShowARecognisableLogo(t *testing.T) {
	for name, want := range map[string]string{
		"GitHub": "github", "github.com": "github",
		"Microsoft": "microsoft", "Microsoft Entra ID": "microsoft", "Azure AD": "microsoft",
		"Google Workspace": "google", "Okta": "sso", "": "sso",
	} {
		if got := providerIcon(name); got != want {
			t.Errorf("providerIcon(%q) = %q, want %q", name, got, want)
		}
	}
	body := getLogin(newTestChooser(t, nil, gitHubProvider(), entraProvider(),
		SSOProvider{ID: "okta", DisplayName: "Okta", LoginPath: "/auth/okta/login"}), "").Body.String()
	for _, want := range []string{
		`<svg class="provider-icon" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true" focusable="false">`,
		`<rect width="10" height="10" fill="#f25022"/>`,
		`<svg class="provider-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks provider logo %q", want)
		}
	}
	// The logo is decoration: the accessible name is still the label alone.
	for _, label := range []string{"Continue with GitHub", "Continue with Microsoft", "Continue with Okta"} {
		if !strings.Contains(body, `<span class="provider-label">`+label+`</span>`) {
			t.Errorf("label %q missing", label)
		}
	}
}

func TestThePackSchemeTokensReachBothSchemes(t *testing.T) {
	pack := v2branding.DefaultPack()
	pack.Brand.Hue = "#6AE8FA"
	pack.Schemes.Light = map[string]string{
		"primary.main":            "rgba(196, 40, 221, 1)",
		"text.button.primary":     "rgba(248, 252, 255, 1)",
		"background.default":      "rgba(248, 252, 255, 1)",
		"background.card.default": "#FFFFFF",
		"text.secondary":          "#0E131D",
	}
	pack.Schemes.Dark = map[string]string{"background.default": "#0E131D"}
	css := string(loginBrandFromPack(pack).Style)
	want := ":root{--brand-light:rgba(196, 40, 221, 1);--on-brand-light:rgba(248, 252, 255, 1);" +
		"--page-light:rgba(248, 252, 255, 1);--card-light:#ffffff;--text-light:#0e131d;" +
		// No dark accent stated: the hue, with the ink that reads on it.
		"--brand-dark:#6ae8fa;--on-brand-dark:#0e131d;--page-dark:#0e131d}"
	if !strings.Contains(css, want) {
		t.Fatalf("brand css = %s\nwant it to contain %s", css, want)
	}
	for background, ink := range map[string]string{"#6ae8fa": "#0e131d", "#c428dd": "#ffffff", "#ff6600": "#0e131d", "#0b5ed7": "#ffffff"} {
		if got := readableInk(background); got != ink {
			t.Errorf("readableInk(%s) = %s, want %s", background, got, ink)
		}
	}
}

// A pack may ask for square corners: a stated 0 is a radius, not "unset".
func TestAZeroRadiusIsKept(t *testing.T) {
	pack := v2branding.DefaultPack()
	pack.Shape = v2branding.Shape{RadiusMd: 0, RadiusLg: 0, RadiusPill: 0}
	if css := string(loginBrandFromPack(pack).Style); !strings.Contains(css, ":root{--radius-md:0px;--radius-lg:0px;--radius-pill:0px}") {
		t.Fatalf("brand css = %s", css)
	}
}

// The built-in logo is the product's own: a renamed product without a logo
// shows its name, never Elitea's mark.
func TestTheBuiltInLogoIsOnlyForTheDefaultProduct(t *testing.T) {
	renamed := v2branding.DefaultPack()
	renamed.Product.Name = "Acme"
	renamed.Assets.LogoFull = "./brand/logo-full.svg"
	if brand := loginBrandFromPack(renamed); brand.DefaultLogo || brand.LogoURL != "" {
		t.Fatalf("renamed brand = %+v", brand)
	}
	plain := v2branding.DefaultPack()
	plain.Assets.LogoFull = "./brand/logo-full.svg"
	if !loginBrandFromPack(plain).DefaultLogo {
		t.Fatal("the default product without an uploaded logo lost its built-in logo")
	}
}
