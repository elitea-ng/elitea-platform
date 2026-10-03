package browserauth

// The BRAND on the login page (ADR-0024 WP5).
//
// The login page is Go-served HTML behind the strictest CSP on the platform
// (`default-src 'none'`, one hash-pinned stylesheet). It is also the first
// thing a customer's users see, so it is the one surface a rebrand must
// reach that the web app's BrandThemeProvider cannot. This file derives what
// the page shows from the same resolved pack the bootstrap route serves —
// the product name, tagline, logo, favicon, login artwork, brand colour and
// the per-scheme accent, page, card and text colours, radii and font — and
// renders it under the same CSP discipline:
//
//   - Every value passes through a narrow allowlist before it becomes HTML
//     or CSS. A hue is six hex digits or nothing; a scheme colour is a hex
//     colour or a numeric rgb()/rgba() or nothing; an asset is a root-relative
//     path with no quote, bracket, backslash, angle bracket or whitespace, or
//     nothing; a font family is letters, digits, spaces, commas, hyphens and
//     quotes, or nothing. The values were validated on the way in
//     (admin/branding.go) — the allowlist here is what makes that a
//     defence in depth rather than the only line.
//   - The brand rules go in a SECOND <style> element whose SHA-256 joins the
//     static stylesheet's hash in `style-src`. No 'unsafe-inline', ever.
//   - `img-src 'self'` and `font-src 'self'` admit the same-origin assets and
//     nothing else; a served pack cannot point the login page at another
//     origin because the allowlist above already dropped the value.
//
// The brand reaches the stylesheet as custom properties (`--brand-light`,
// `--brand-dark`, `--page-dark`, `--radius-lg`, ...) that templates/auth.css
// reads with the product default as the fallback, so a pack restyles both
// colour schemes and the ring art without the brand rules knowing selectors.
//
// Without a brand source, or with nothing served, the page renders the
// product default: the name "Elitea" and its built-in logo.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"html/template"
	"math"
	"regexp"
	"strings"

	v2branding "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/branding"
)

// BrandSource is the seam to the resolved brand pack. The bootstrap route's
// resolver satisfies it; nil means "no brand, render the default".
type BrandSource interface {
	Current(ctx context.Context) v2branding.Snapshot
}

// DefaultProductName is what the page says when no pack names a product.
const DefaultProductName = "Elitea"

// loginBrand is what the template renders.
type loginBrand struct {
	ProductName string
	Tagline     string
	LogoURL     string
	FaviconURL  string
	// DefaultLogo shows the product's built-in logo (templates/partials.html)
	// when the pack names no logo of its own and keeps the default name; a
	// renamed product without a logo shows its name instead.
	DefaultLogo bool
	// Style is the brand's own rules; empty when the pack states nothing the
	// page renders. StyleSource is its CSP hash source, "" when Style is empty.
	Style       template.CSS
	StyleSource string
}

func (h *Handler) loginBrand(ctx context.Context) loginBrand {
	if h.brand == nil {
		return defaultLoginBrand()
	}
	snapshot := h.brand.Current(ctx)
	if snapshot.Pack == nil {
		return defaultLoginBrand()
	}
	return loginBrandFromPack(snapshot.Pack)
}

// defaultLoginBrand is the page with no pack served.
func defaultLoginBrand() loginBrand {
	return loginBrand{ProductName: DefaultProductName, DefaultLogo: true}
}

var (
	hexColourPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	// schemeColourPattern is the subset of a scheme token the page admits:
	// #RGB, #RRGGBB, #RRGGBBAA, or rgb()/rgba() with plain numbers. Gradients
	// and every other function are left to the web app.
	schemeColourPattern = regexp.MustCompile(`^(?:#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})|rgba?\(\s*\d{1,3}\s*,\s*\d{1,3}\s*,\s*\d{1,3}\s*(?:,\s*(?:0|1|0?\.\d{1,4}|1\.0{1,4})\s*)?\))$`)
	fontFamilyPattern   = regexp.MustCompile(`^[A-Za-z0-9 ,'"_-]{1,200}$`)
	fontWeightPattern   = regexp.MustCompile(`^(?:normal|bold|[1-9][0-9]{0,2}(?: [1-9][0-9]{0,2})?)$`)
)

// loginBrandFromPack derives the page's brand from a resolved pack. Every
// field degrades to "nothing" on its own; a pack with a good name and a bad
// logo path shows the name and no logo.
func loginBrandFromPack(pack *v2branding.Pack) loginBrand {
	brand := loginBrand{ProductName: DefaultProductName}
	if name := strings.TrimSpace(pack.Product.Name); name != "" {
		brand.ProductName = name
	}
	if pack.Product.Tagline != nil {
		brand.Tagline = strings.TrimSpace(*pack.Product.Tagline)
	}
	brand.LogoURL = cssSafeAssetPath(pack.Assets.LogoFull)
	brand.FaviconURL = cssSafeAssetPath(pack.Assets.Favicon)
	brand.DefaultLogo = brand.LogoURL == "" && brand.ProductName == DefaultProductName

	var css strings.Builder
	if vars := brandSchemeVariables(pack); len(vars) > 0 {
		css.WriteString(":root{")
		for i, v := range vars {
			if i > 0 {
				css.WriteString(";")
			}
			fmt.Fprintf(&css, "%s:%s", v.name, v.value)
		}
		css.WriteString("}")
	}
	for _, face := range pack.Typography.FontFaces {
		family := cssFontFamily(face.Family)
		path := cssSafeAssetPath(face.URL)
		if family == "" || path == "" {
			continue
		}
		// One quoted family name: a face declares one family, and the quotes
		// make a name with a space or a digit unambiguous.
		fmt.Fprintf(&css, "@font-face{font-family:%q;src:url(%q) format(\"woff2\");font-display:swap",
			strings.Trim(family, `"'`), path)
		if face.Weight != nil && fontWeightPattern.MatchString(*face.Weight) {
			fmt.Fprintf(&css, ";font-weight:%s", *face.Weight)
		}
		if face.Style != nil && (*face.Style == "normal" || *face.Style == "italic") {
			fmt.Fprintf(&css, ";font-style:%s", *face.Style)
		}
		css.WriteString("}")
	}
	if family := cssFontFamily(pack.Typography.FontFamily); family != "" {
		fmt.Fprintf(&css, ":root{font-family:%s}", family)
	}
	var radii []string
	for _, radius := range []struct {
		name  string
		value float64
	}{
		{"--radius-md", pack.Shape.RadiusMd},
		{"--radius-lg", pack.Shape.RadiusLg},
		{"--radius-pill", pack.Shape.RadiusPill},
	} {
		if radius.value > 0 && radius.value <= 9999 {
			radii = append(radii, fmt.Sprintf("%s:%gpx", radius.name, radius.value))
		}
	}
	if len(radii) > 0 {
		fmt.Fprintf(&css, ":root{%s}", strings.Join(radii, ";"))
	}
	if pack.Assets.LoginArt != nil {
		if art := cssSafeAssetPath(*pack.Assets.LoginArt); art != "" {
			// The operator's artwork replaces the generated rings.
			fmt.Fprintf(&css, "body{background-image:url(%q);background-size:cover;background-position:center}"+
				".auth-backdrop{display:none}", art)
		}
	}
	if css.Len() > 0 {
		brand.Style = template.CSS(css.String()) //nolint:gosec // every value passed an allowlist above
		digest := sha256.Sum256([]byte(css.String()))
		brand.StyleSource = "'sha256-" + base64.StdEncoding.EncodeToString(digest[:]) + "'"
	}
	return brand
}

type cssVariable struct{ name, value string }

// schemeTokens maps the web app's scheme token ids (default.pack.json) to the
// page's per-scheme inputs. The muted text colour is deliberately not taken
// from the pack: the app's `text.primary` is below AA on the light card.
var schemeTokens = []struct{ id, name string }{
	{"primary.main", "brand"},
	{"text.button.primary", "on-brand"},
	{"background.default", "page"},
	{"background.card.default", "card"},
	{"text.secondary", "text"},
}

// brandSchemeVariables derives the page's colour inputs for both schemes. A
// token the pack states for a scheme wins; otherwise the accent is the brand
// hue, and the text on it is the pack's onBrand or, without one, whichever of
// white and the default ink reads better on it.
func brandSchemeVariables(pack *v2branding.Pack) []cssVariable {
	hue := cssHexColour(pack.Brand.Hue)
	onBrand := ""
	if pack.Brand.OnBrand != nil {
		onBrand = cssHexColour(*pack.Brand.OnBrand)
	}
	var vars []cssVariable
	for _, scheme := range []struct {
		suffix string
		tokens map[string]string
	}{
		{"light", pack.Schemes.Light},
		{"dark", pack.Schemes.Dark},
	} {
		values := map[string]string{}
		for _, token := range schemeTokens {
			if value := cssSchemeColour(scheme.tokens[token.id]); value != "" {
				values[token.name] = value
			}
		}
		if values["brand"] == "" && hue != "" {
			values["brand"] = hue
		}
		if values["brand"] != "" && values["on-brand"] == "" {
			if onBrand != "" {
				values["on-brand"] = onBrand
			} else if ink := readableInk(values["brand"]); ink != "" {
				values["on-brand"] = ink
			}
		}
		for _, token := range schemeTokens {
			if value := values[token.name]; value != "" {
				vars = append(vars, cssVariable{"--" + token.name + "-" + scheme.suffix, value})
			}
		}
	}
	return vars
}

// cssSchemeColour admits a scheme token the page can use as a colour.
func cssSchemeColour(value string) string {
	value = strings.TrimSpace(value)
	if !schemeColourPattern.MatchString(value) {
		return ""
	}
	return strings.ToLower(value)
}

// readableInk is #ffffff or #0e131d, whichever contrasts more with a #RRGGBB
// background; "" for any other form.
func readableInk(background string) string {
	if !hexColourPattern.MatchString(background) {
		return ""
	}
	var channels [3]float64
	for i := range channels {
		var v int
		_, _ = fmt.Sscanf(background[1+2*i:3+2*i], "%02x", &v)
		c := float64(v) / 255
		if c <= 0.04045 {
			channels[i] = c / 12.92
		} else {
			channels[i] = math.Pow((c+0.055)/1.055, 2.4)
		}
	}
	luminance := 0.2126*channels[0] + 0.7152*channels[1] + 0.0722*channels[2]
	// #0e131d has a relative luminance of about 0.0065.
	if (1.05)/(luminance+0.05) >= (luminance+0.05)/(0.0065+0.05) {
		return "#ffffff"
	}
	return "#0e131d"
}

// cssHexColour admits exactly #RRGGBB, lower-cased.
func cssHexColour(value string) string {
	value = strings.TrimSpace(value)
	if !hexColourPattern.MatchString(value) {
		return ""
	}
	return strings.ToLower(value)
}

// cssFontFamily admits a plain family list. Quotes are kept (a family with a
// space needs them); every character that could close a rule or a string
// context is outside the class.
func cssFontFamily(value string) string {
	value = strings.TrimSpace(value)
	if !fontFamilyPattern.MatchString(value) {
		return ""
	}
	return value
}

// cssSafeAssetPath admits a root-relative same-origin path — one leading
// slash, no `//`, no whitespace or control characters, none of the
// characters that end a CSS url() or an HTML attribute early.
func cssSafeAssetPath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 512 || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") {
		return ""
	}
	for _, r := range value {
		if r <= ' ' || r == 0x7f || strings.ContainsRune(`"'()\<>`, r) {
			return ""
		}
	}
	return value
}

// RenderLoginPreview renders the login page as it would look under pack, for
// a branding package's preview folder (ADR-0024 decision 9). It is the same
// template and the same allowlists as the live page, with a placeholder
// transaction target and no error state; the form posts nowhere useful from
// a file on disk, which is the point of a preview.
func RenderLoginPreview(pack *v2branding.Pack) ([]byte, error) {
	page, err := parseAuthPage("login.html", loginTemplateSource)
	if err != nil {
		return nil, err
	}
	brand := defaultLoginBrand()
	if pack != nil {
		brand = loginBrandFromPack(pack)
	}
	var body bytes.Buffer
	if err := page.Execute(&body, loginPage{
		Target: "preview",
		Style:  authPageStyle(),
		Script: authPageScript(),
		Brand:  brand,
	}); err != nil {
		return nil, err
	}
	return body.Bytes(), nil
}
