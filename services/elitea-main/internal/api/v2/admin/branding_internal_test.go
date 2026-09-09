package admin

import (
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

var fontAssetPath = "/api/v2/branding/assets/font/" + strings.Repeat("cd", 32) + ".woff2"

func TestValidateBrandingValues(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]any
		want   string // "" = accepted; otherwise a substring of the refusal
	}{
		{name: "empty body", values: map[string]any{}},
		{name: "every field empty inherits", values: map[string]any{
			platformconfig.KeyBrandingProductName: "", platformconfig.KeyBrandingHue: "",
			platformconfig.KeyBrandingBaseSize: float64(0), platformconfig.KeyBrandingLogoFull: "",
			platformconfig.KeyBrandingDensity: "",
		}},
		{name: "a complete valid overlay", values: map[string]any{
			platformconfig.KeyBrandingProductName: "Acme AI",
			platformconfig.KeyBrandingHue:         "#1a73e8",
			platformconfig.KeyBrandingDocsURL:     "https://docs.acme.example/elitea",
			platformconfig.KeyBrandingFontFamily:  `"Inter", Arial, sans-serif`,
			platformconfig.KeyBrandingBaseSize:    float64(15),
			platformconfig.KeyBrandingScale:       1.25,
			platformconfig.KeyBrandingRadiusPill:  float64(9999),
			platformconfig.KeyBrandingDensity:     "compact",
			platformconfig.KeyBrandingLogoFull:    "/branding/assets/logo-full/abc123.svg",
		}},

		{name: "hue without hash", values: map[string]any{platformconfig.KeyBrandingHue: "1a73e8"}, want: "six-digit hex"},
		{name: "hue short form", values: map[string]any{platformconfig.KeyBrandingHue: "#fff"}, want: "six-digit hex"},
		{name: "on-brand colour is checked too", values: map[string]any{platformconfig.KeyBrandingOnBrand: "white"}, want: "six-digit hex"},

		// [W1b] docs_url uniquely also accepts a root-relative path: the
		// embedded docs SPA is same-origin, and ProductDefault() states it
		// this way, so a branding package export/import round trip must not
		// reject its own default value. supportUrl keeps the absolute-only
		// rule below.
		{name: "docs url relative is accepted (the embedded docs SPA)", values: map[string]any{platformconfig.KeyBrandingDocsURL: "/docs"}},
		{name: "docs url protocol-relative is still refused", values: map[string]any{platformconfig.KeyBrandingDocsURL: "//evil.example/docs"}, want: "absolute http"},
		{name: "support url javascript", values: map[string]any{platformconfig.KeyBrandingSupportURL: "javascript:alert(1)"}, want: "absolute http"},

		{name: "logo with a scheme", values: map[string]any{platformconfig.KeyBrandingLogoFull: "https://cdn.example/logo.svg"}, want: "path on this origin"},
		{name: "logo as data uri", values: map[string]any{platformconfig.KeyBrandingLogoMark: "data:image/svg+xml;base64,AAAA"}, want: "path on this origin"},
		{name: "logo javascript", values: map[string]any{platformconfig.KeyBrandingFavicon: "javascript:alert(1)"}, want: "path on this origin"},
		{name: "logo protocol-relative", values: map[string]any{platformconfig.KeyBrandingLoginArt: "//evil.example/x.png"}, want: "path on this origin"},
		{name: "logo with whitespace", values: map[string]any{platformconfig.KeyBrandingLogoFull: "/brand/my logo.svg"}, want: "whitespace"},
		{name: "logo with a newline", values: map[string]any{platformconfig.KeyBrandingLogoFull: "/brand/a.svg\n<script>"}, want: "whitespace"},

		{name: "name too long", values: map[string]any{platformconfig.KeyBrandingProductName: strings.Repeat("x", 81)}, want: "at most 80"},
		{name: "base size out of range", values: map[string]any{platformconfig.KeyBrandingBaseSize: float64(24)}, want: "between 12 and 18"},
		{name: "scale out of range", values: map[string]any{platformconfig.KeyBrandingScale: float64(2)}, want: "between 1.05 and 1.5"},
		{name: "negative radius", values: map[string]any{platformconfig.KeyBrandingRadiusSm: float64(-1)}, want: "between 0 and 9999"},
		{name: "density typo", values: map[string]any{platformconfig.KeyBrandingDensity: "cosy"}, want: "comfortable"},

		{name: "font faces valid", values: map[string]any{platformconfig.KeyBrandingFontFaces: []any{
			map[string]any{"family": "Inter", "url": fontAssetPath, "weight": "100 900", "style": "normal"},
			map[string]any{"family": "Inter", "url": fontAssetPath, "style": "italic"},
		}}},
		{name: "font faces empty array", values: map[string]any{platformconfig.KeyBrandingFontFaces: []any{}}},
		{name: "font faces too many", values: map[string]any{platformconfig.KeyBrandingFontFaces: []any{
			map[string]any{"family": "A", "url": fontAssetPath},
			map[string]any{"family": "B", "url": fontAssetPath},
			map[string]any{"family": "C", "url": fontAssetPath},
		}}, want: "at most 2"},
		{name: "font face external url", values: map[string]any{platformconfig.KeyBrandingFontFaces: []any{
			map[string]any{"family": "Inter", "url": "https://fonts.gstatic.com/inter.woff2"},
		}}, want: "uploaded font asset"},
		{name: "font face image asset as font", values: map[string]any{platformconfig.KeyBrandingFontFaces: []any{
			map[string]any{"family": "Inter", "url": "/api/v2/branding/assets/logo-full/" + strings.Repeat("ab", 32) + ".svg"},
		}}, want: "uploaded font asset"},
		{name: "font face no family", values: map[string]any{platformconfig.KeyBrandingFontFaces: []any{
			map[string]any{"url": fontAssetPath},
		}}, want: "family"},
		{name: "font face bad style", values: map[string]any{platformconfig.KeyBrandingFontFaces: []any{
			map[string]any{"family": "Inter", "url": fontAssetPath, "style": "oblique"},
		}}, want: "style"},
		{name: "font face unknown key", values: map[string]any{platformconfig.KeyBrandingFontFaces: []any{
			map[string]any{"family": "Inter", "url": fontAssetPath, "src": "x"},
		}}, want: "unknown key"},
		{name: "font faces not an array", values: map[string]any{platformconfig.KeyBrandingFontFaces: "Inter"}, want: "array"},

		{name: "support email valid", values: map[string]any{platformconfig.KeyBrandingSupportEmail: "support@acme.example"}},
		{name: "support email with display name", values: map[string]any{platformconfig.KeyBrandingSupportEmail: "Support <support@acme.example>"}, want: "plain e-mail"},
		{name: "support email no dot", values: map[string]any{platformconfig.KeyBrandingSupportEmail: "support@localhost"}, want: "plain e-mail"},
		{name: "e-mail logo external", values: map[string]any{platformconfig.KeyBrandingLogoEmail: "https://cdn.example/l.png"}, want: "path on this origin"},
		{name: "sender name too long", values: map[string]any{platformconfig.KeyBrandingSenderName: strings.Repeat("x", 81)}, want: "at most 80"},

		{name: "scheme tokens valid", values: map[string]any{platformconfig.KeyBrandingSchemeTokens: map[string]any{
			"light": map[string]any{"primary.main": "#1A73E8"}, "dark": map[string]any{}}}},
		{name: "scheme tokens empty object inherits", values: map[string]any{platformconfig.KeyBrandingSchemeTokens: map[string]any{}}},
		{name: "scheme tokens unknown scheme", values: map[string]any{platformconfig.KeyBrandingSchemeTokens: map[string]any{"sepia": map[string]any{}}}, want: "unknown scheme"},
		{name: "scheme tokens css expression", values: map[string]any{platformconfig.KeyBrandingSchemeTokens: map[string]any{
			"light": map[string]any{"primary.main": "var(--x)"}}}, want: "six-digit hex"},
		{name: "scheme tokens empty colour", values: map[string]any{platformconfig.KeyBrandingSchemeTokens: map[string]any{
			"light": map[string]any{"primary.main": ""}}}, want: "six-digit hex"},
		{name: "scheme tokens not an object", values: map[string]any{platformconfig.KeyBrandingSchemeTokens: []any{}}, want: "object of schemes"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := validateBrandingValues(tc.values)
			if tc.want == "" && got != "" {
				t.Fatalf("refused: %s", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("got %q, want a refusal containing %q", got, tc.want)
			}
		})
	}
}

// TestBrandingSection_DeclaresEveryOverlayKey pins the schema to the reader:
// a key the resolver reads but the form does not declare could never be set,
// and a key the form declares but nothing reads is the defect the section
// registry's "live only once read" rule exists to stop.
func TestBrandingSection_DeclaresEveryOverlayKey(t *testing.T) {
	section, ok := findConfigSection(platformconfig.SectionBranding)
	if !ok {
		t.Fatal("branding section is not registered")
	}
	if perm, _ := section.raw["required_permission"].(string); perm != "configuration.branding" {
		t.Fatalf("required_permission = %q, want configuration.branding", perm)
	}
	if reason, _ := section.raw["unavailable_reason"].(string); reason != "" {
		t.Fatalf("branding section must be live, has unavailable_reason %q", reason)
	}

	declared := map[string]bool{}
	for _, field := range section.fields {
		key, _ := field["key"].(string)
		declared[key] = true
		if format, _ := field["format"].(string); format == "password" {
			t.Errorf("field %q is a credential; the section must not carry one", key)
		}
	}
	for _, key := range []string{
		platformconfig.KeyBrandingProductName, platformconfig.KeyBrandingProductShortName,
		platformconfig.KeyBrandingProductTagline, platformconfig.KeyBrandingDocsURL,
		platformconfig.KeyBrandingSupportURL, platformconfig.KeyBrandingHue,
		platformconfig.KeyBrandingOnBrand, platformconfig.KeyBrandingFontFamily,
		platformconfig.KeyBrandingFontFamilyMono, platformconfig.KeyBrandingBaseSize,
		platformconfig.KeyBrandingScale, platformconfig.KeyBrandingRadiusSm,
		platformconfig.KeyBrandingRadiusMd, platformconfig.KeyBrandingRadiusLg,
		platformconfig.KeyBrandingRadiusPill, platformconfig.KeyBrandingDensity,
		platformconfig.KeyBrandingLogoFull, platformconfig.KeyBrandingLogoMark,
		platformconfig.KeyBrandingFavicon, platformconfig.KeyBrandingLoginArt,
		platformconfig.KeyBrandingFontFaces, platformconfig.KeyBrandingSenderName,
		platformconfig.KeyBrandingSupportEmail, platformconfig.KeyBrandingLogoEmail,
		platformconfig.KeyBrandingSchemeTokens,
	} {
		if !declared[key] {
			t.Errorf("overlay key %q is read by platformconfig but not declared by the section", key)
		}
		delete(declared, key)
	}
	for key := range declared {
		t.Errorf("section declares %q, which no reader consumes", key)
	}
}
