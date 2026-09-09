// Package branding serves the deployment-level brand pack as an executable
// bootstrap script: GET /api/v2/branding/bootstrap.js emits
// `window.elitea_brand = {...};` (UI reimplementation spec §4.3 delivery
// channel C, §9.3 unit W3).
//
// Pack resolution is deployment-level only in v1 (decision record 2026-07-26,
// "Defaults in force" Q5): one pack file per deployment, path taken from the
// BRAND_PACK_PATH environment variable. Tenant resolution is a later unit.
//
// The branding path must NEVER fail: a missing, unreadable, malformed or
// schema-violating pack file degrades to the built-in default pack (id
// "default", product name "Elitea") with the reason logged. There is no
// partial merge — a pack is served either exactly as validated or not at all.
package branding

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
)

// SchemaURL is the exact $schema literal the brand-pack contract requires
// (spec §4.2: z.literal('https://elitea.ai/schemas/brand-pack/1.json')).
const SchemaURL = "https://elitea.ai/schemas/brand-pack/1.json"

// Typography bounds and defaults (spec §4.2:
// baseSize z.number().min(12).max(18).default(14),
// scale    z.number().min(1.05).max(1.5).default(1.2)).
const (
	minBaseSize     = 12
	maxBaseSize     = 18
	defaultBaseSize = 14
	minScale        = 1.05
	maxScale        = 1.5
	defaultScale    = 1.2
	defaultLocale   = "en-GB"
)

// The Go structs below mirror shared/brand/schema.ts (spec §4.2) field for
// field, including JSON key spelling and declaration order, so that any pack
// this package accepts and re-serialises parses under the UI's zod schema.
//
// zod semantics mirrored:
//   - `.strict()` applies to the TOP-LEVEL object only: unknown top-level
//     keys reject the pack. Nested objects follow zod's default "strip"
//     mode — unknown nested keys are accepted and dropped (Go's struct
//     unmarshal drops them naturally, so the served JSON matches what
//     zod's parse would have produced).
//   - `.optional()` fields accept ABSENT but not JSON null (zod z.string()
//     rejects null); no field in the schema is nullable, so a null value
//     anywhere at object depth rejects the pack.
//   - `.default()` fields are materialised on parse (zod fills defaults into
//     its output), so the served JSON carries the defaults explicitly.

// Product mirrors BrandPack.product.
type Product struct {
	Name       string  `json:"name"`
	ShortName  string  `json:"shortName"`
	Tagline    *string `json:"tagline,omitempty"`
	DocsURL    *string `json:"docsUrl,omitempty"`
	SupportURL *string `json:"supportUrl,omitempty"`
	// SenderName is the display name outbound e-mail is sent as; the
	// product name when absent (ADR-0024 WP7).
	SenderName *string `json:"senderName,omitempty"`
	// SupportEmail is the address the app's "contact support" paths use
	// (ADR-0024 WP8) and the reply-to hint in e-mail footers.
	SupportEmail *string `json:"supportEmail,omitempty"`
}

// Assets mirrors BrandPack.assets. Values are data: URIs or same-origin paths.
type Assets struct {
	LogoFull string  `json:"logoFull"`
	LogoMark string  `json:"logoMark"`
	Favicon  string  `json:"favicon"`
	LoginArt *string `json:"loginArt,omitempty"`
	// LogoEmail is a RASTER logo for e-mail (mail clients render neither SVG
	// nor a relative path); absolutised against PUBLIC_BASE_URL at send time.
	LogoEmail *string `json:"logoEmail,omitempty"`
}

// Typography mirrors BrandPack.typography.
type Typography struct {
	FontFamily     string  `json:"fontFamily"`
	FontFamilyMono string  `json:"fontFamilyMono"`
	BaseSize       float64 `json:"baseSize"`
	Scale          float64 `json:"scale"`
	// FontFaces are the self-hosted faces the web app declares as @font-face
	// rules so that FontFamily resolves (ADR-0024 WP2/WP3; schema.ts
	// `typography.fontFaces`, optional). Absent means "no self-hosted face".
	FontFaces []FontFace `json:"fontFaces,omitempty"`
}

// FontFace mirrors one BrandPack.typography.fontFaces entry.
type FontFace struct {
	Family string `json:"family"`
	// URL is a same-origin path — the asset route hands out exactly this
	// shape — never an external origin (the theme gate forbids those).
	URL    string  `json:"url"`
	Weight *string `json:"weight,omitempty"`
	Style  *string `json:"style,omitempty"` // enum: normal | italic
}

// Shape mirrors BrandPack.shape.
//
// RadiusPill was added to the UI schema after this mirror was written
// (shared/brand/schema.ts's "[S1 Part B] Additive" note — the pill/circle
// radius MuiButton's icon-only and `maxi` variants need). It is REQUIRED
// there, not optional, so a pack served without it fails
// `BrandPack.parse()` outright: the whole point of this mirror is that
// "any pack this package accepts and re-serialises parses under the UI's
// zod schema", and omitting the field broke exactly that. Found by wiring
// channel C into the running app (issue #136 C): the UI silently fell back
// to its compiled default pack because the served pack would not parse.
type Shape struct {
	RadiusSm   float64 `json:"radiusSm"`
	RadiusMd   float64 `json:"radiusMd"`
	RadiusLg   float64 `json:"radiusLg"`
	RadiusPill float64 `json:"radiusPill"`
	Density    string  `json:"density"` // enum: comfortable | compact
}

// Locale mirrors BrandPack.locale. Both fields default to en-GB.
type Locale struct {
	Default    string `json:"default"`
	DateLocale string `json:"dateLocale"`
}

// Brand mirrors BrandPack.brand. The hue is scheme-independent by design
// (spec §4.2: one hue, two lightness ramps derived from it).
type Brand struct {
	Hue     string  `json:"hue"`
	OnBrand *string `json:"onBrand,omitempty"`
}

// Schemes mirrors BrandPack.schemes: token id -> colour records.
type Schemes struct {
	Light map[string]string `json:"light"`
	Dark  map[string]string `json:"dark"`
	HC    map[string]string `json:"hc,omitempty"`
}

// Pack mirrors the top-level BrandPack zod object.
type Pack struct {
	Schema     string     `json:"$schema"`
	ID         string     `json:"id"`
	Version    string     `json:"version"`
	Product    Product    `json:"product"`
	Assets     Assets     `json:"assets"`
	Typography Typography `json:"typography"`
	Shape      Shape      `json:"shape"`
	Locale     Locale     `json:"locale"`
	Brand      Brand      `json:"brand"`
	Schemes    Schemes    `json:"schemes"`
}

// topLevelKeys is the closed key set enforced by the top-level .strict().
// Every key is required (none of the top-level zod fields is .optional()).
var topLevelKeys = []string{
	"$schema", "id", "version", "product", "assets",
	"typography", "shape", "locale", "brand", "schemes",
}

// DefaultPack returns the built-in minimal default pack — the never-500
// degradation floor for the branding endpoint. It is NOT the canonical
// product default (unit T1 owns that, compiled into the UI as channel A);
// it only has to be schema-valid and recognisably "Elitea".
//
// Asset values are same-origin paths on purpose: no data: blobs, no binary
// payloads (scripts/no-binaries-check.sh must stay green).
func DefaultPack() *Pack {
	return &Pack{
		Schema:  SchemaURL,
		ID:      "default",
		Version: "1.0.0",
		Product: Product{Name: "Elitea", ShortName: "Elitea"},
		Assets: Assets{
			LogoFull: "/app/brand/logo-full.svg",
			LogoMark: "/app/brand/logo-mark.svg",
			Favicon:  "/app/brand/favicon.svg",
		},
		Typography: Typography{
			FontFamily:     "'Roboto', 'Helvetica Neue', Arial, sans-serif",
			FontFamilyMono: "'Roboto Mono', Consolas, monospace",
			BaseSize:       defaultBaseSize,
			Scale:          defaultScale,
		},
		// radiusPill is the pill/circle radius; 9999 is the value the UI's
		// own default pack (shared/brand/tokens/default.pack.json) uses.
		Shape:  Shape{RadiusSm: 4, RadiusMd: 8, RadiusLg: 12, RadiusPill: 9999, Density: "comfortable"},
		Locale: Locale{Default: defaultLocale, DateLocale: defaultLocale},
		// Placeholder hue pending unit T1's hue unification (spec §4.1
		// blocker 1); any valid value serves, since channel A's compiled-in
		// pack is the real default the UI renders with.
		Brand: Brand{Hue: "#C428DD"},
		// EMPTY, deliberately. The schema permits any token id (the records
		// are open), but the id VOCABULARY belongs to the UI's default pack,
		// and the UI expands dotted ids into nested groups
		// (`shared/brand/toMuiPalette.ts`'s `unflatten`). The previous values
		// here — `"surface"` and `"text"` — were not real ids: `text` is a
		// GROUP in that vocabulary (`text.primary`, `text.secondary`, …), so
		// stating it as a leaf made `unflatten` throw "token id text collides
		// with a group of the same name", which took down the entire provider
		// tree the moment channel C was actually wired to the app.
		//
		// Empty records are also the RIGHT floor rather than merely a safe
		// one: with no token stated, every id is derived from `brand.hue`
		// (`resolveScheme`), so this pack paints a complete, coherent surface
		// instead of two arbitrary colours plus 360 defaults.
		Schemes: Schemes{
			Light: map[string]string{},
			Dark:  map[string]string{},
		},
	}
}

// LoadPack reads and validates the brand pack at path. Any failure — read
// error, malformed JSON, schema violation — is returned as the degradation
// reason; callers fall back to DefaultPack and log it.
func LoadPack(path string) (*Pack, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading brand pack: %w", err)
	}
	return ParsePack(data)
}

// ParsePack validates data against the §4.2 BrandPack schema and returns the
// typed pack with defaults materialised. The returned pack is valid in full
// or the error explains the first violation — never a partial result.
func ParsePack(data []byte) (*Pack, error) {
	top, err := objectKeys(data, "brand pack")
	if err != nil {
		return nil, err
	}

	// .strict(): reject unknown TOP-LEVEL keys.
	allowed := make(map[string]bool, len(topLevelKeys))
	for _, k := range topLevelKeys {
		allowed[k] = true
	}
	// Deterministic error output: report the first unknown key in sorted order.
	unknown := make([]string, 0, 1)
	for k := range top {
		if !allowed[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("unknown top-level key %q (schema is strict)", unknown[0])
	}

	// Every top-level key is required and non-null.
	for _, k := range topLevelKeys {
		raw, ok := top[k]
		if !ok {
			return nil, fmt.Errorf("missing required key %q", k)
		}
		if isJSONNull(raw) {
			return nil, fmt.Errorf("key %q must not be null", k)
		}
	}

	// Presence + non-null checks per nested object (zod: required keys of
	// nested objects; no field is nullable). These run BEFORE the typed
	// decode so structural violations get precise messages.
	pkeys, err := sectionKeys(top["product"], "product", "name", "shortName")
	if err != nil {
		return nil, err
	}
	akeys, err := sectionKeys(top["assets"], "assets", "logoFull", "logoMark", "favicon")
	if err != nil {
		return nil, err
	}
	tkeys, err := sectionKeys(top["typography"], "typography", "fontFamily", "fontFamilyMono")
	if err != nil {
		return nil, err
	}
	shkeys, err := sectionKeys(top["shape"], "shape", "radiusSm", "radiusMd", "radiusLg", "radiusPill", "density")
	if err != nil {
		return nil, err
	}
	lkeys, err := sectionKeys(top["locale"], "locale") // object required; all keys default
	if err != nil {
		return nil, err
	}
	bkeys, err := sectionKeys(top["brand"], "brand", "hue")
	if err != nil {
		return nil, err
	}
	skeys, err := sectionKeys(top["schemes"], "schemes", "light", "dark")
	if err != nil {
		return nil, err
	}

	// schemes.* are records of string -> string; a struct decode of
	// {"a": null} into map[string]string would silently coerce to "", so the
	// records are decoded strictly, value by value.
	light, err := stringRecord(skeys["light"], "schemes.light")
	if err != nil {
		return nil, err
	}
	dark, err := stringRecord(skeys["dark"], "schemes.dark")
	if err != nil {
		return nil, err
	}
	var hc map[string]string
	if hcRaw, ok := skeys["hc"]; ok {
		if hc, err = stringRecord(hcRaw, "schemes.hc"); err != nil {
			return nil, err
		}
	}

	// Typed decode, field by field from the EXACT-key maps — never a raw
	// re-unmarshal of the whole document. encoding/json matches struct
	// fields case-INsensitively, so `json.Unmarshal(data, &p)` would let an
	// unknown key like "RADIUSSM" bind to RadiusSm and clobber the real
	// value, breaking the zod-strip mirror (zod key matching is exact:
	// a case-variant key is an unknown key and is stripped, period).
	// Unknown keys of any casing therefore never reach a struct field.
	var p Pack
	for _, f := range []struct {
		src  map[string]json.RawMessage
		key  string
		name string
		dst  any
	}{
		{top, "$schema", "$schema", &p.Schema},
		{top, "id", "id", &p.ID},
		{top, "version", "version", &p.Version},
		{pkeys, "name", "product.name", &p.Product.Name},
		{pkeys, "shortName", "product.shortName", &p.Product.ShortName},
		{pkeys, "tagline", "product.tagline", &p.Product.Tagline},
		{pkeys, "docsUrl", "product.docsUrl", &p.Product.DocsURL},
		{pkeys, "supportUrl", "product.supportUrl", &p.Product.SupportURL},
		{pkeys, "senderName", "product.senderName", &p.Product.SenderName},
		{pkeys, "supportEmail", "product.supportEmail", &p.Product.SupportEmail},
		{akeys, "logoFull", "assets.logoFull", &p.Assets.LogoFull},
		{akeys, "logoMark", "assets.logoMark", &p.Assets.LogoMark},
		{akeys, "favicon", "assets.favicon", &p.Assets.Favicon},
		{akeys, "loginArt", "assets.loginArt", &p.Assets.LoginArt},
		{akeys, "logoEmail", "assets.logoEmail", &p.Assets.LogoEmail},
		{tkeys, "fontFamily", "typography.fontFamily", &p.Typography.FontFamily},
		{tkeys, "fontFamilyMono", "typography.fontFamilyMono", &p.Typography.FontFamilyMono},
		{tkeys, "baseSize", "typography.baseSize", &p.Typography.BaseSize},
		{tkeys, "scale", "typography.scale", &p.Typography.Scale},
		{shkeys, "radiusSm", "shape.radiusSm", &p.Shape.RadiusSm},
		{shkeys, "radiusMd", "shape.radiusMd", &p.Shape.RadiusMd},
		{shkeys, "radiusLg", "shape.radiusLg", &p.Shape.RadiusLg},
		{shkeys, "radiusPill", "shape.radiusPill", &p.Shape.RadiusPill},
		{shkeys, "density", "shape.density", &p.Shape.Density},
		{lkeys, "default", "locale.default", &p.Locale.Default},
		{lkeys, "dateLocale", "locale.dateLocale", &p.Locale.DateLocale},
		{bkeys, "hue", "brand.hue", &p.Brand.Hue},
		{bkeys, "onBrand", "brand.onBrand", &p.Brand.OnBrand},
	} {
		if err := decodeExact(f.src, f.key, f.name, f.dst); err != nil {
			return nil, err
		}
	}
	p.Schemes = Schemes{Light: light, Dark: dark, HC: hc}

	if raw, ok := tkeys["fontFaces"]; ok {
		faces, err := fontFaces(raw)
		if err != nil {
			return nil, err
		}
		p.Typography.FontFaces = faces
	}

	// Materialise .default() values for ABSENT keys (zod fills defaults on
	// parse; a present-but-null key was already rejected by sectionKeys).
	if _, ok := tkeys["baseSize"]; !ok {
		p.Typography.BaseSize = defaultBaseSize
	}
	if _, ok := tkeys["scale"]; !ok {
		p.Typography.Scale = defaultScale
	}
	if _, ok := lkeys["default"]; !ok {
		p.Locale.Default = defaultLocale
	}
	if _, ok := lkeys["dateLocale"]; !ok {
		p.Locale.DateLocale = defaultLocale
	}

	// Value constraints.
	if p.Schema != SchemaURL {
		return nil, fmt.Errorf("$schema must be exactly %q, got %q", SchemaURL, p.Schema)
	}
	if p.ID == "" {
		return nil, fmt.Errorf("id must be a non-empty string")
	}
	if err := optionalRelativeOrAbsoluteURL(p.Product.DocsURL, "product.docsUrl"); err != nil {
		return nil, err
	}
	if err := optionalURL(p.Product.SupportURL, "product.supportUrl"); err != nil {
		return nil, err
	}
	if p.Typography.BaseSize < minBaseSize || p.Typography.BaseSize > maxBaseSize {
		return nil, fmt.Errorf("typography.baseSize must be in [%d, %d], got %v", minBaseSize, maxBaseSize, p.Typography.BaseSize)
	}
	if p.Typography.Scale < minScale || p.Typography.Scale > maxScale {
		return nil, fmt.Errorf("typography.scale must be in [%v, %v], got %v", minScale, maxScale, p.Typography.Scale)
	}
	if p.Shape.Density != "comfortable" && p.Shape.Density != "compact" {
		return nil, fmt.Errorf("shape.density must be \"comfortable\" or \"compact\", got %q", p.Shape.Density)
	}

	return &p, nil
}

// --- helpers -----------------------------------------------------------------

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// objectKeys decodes raw as a JSON object and returns its key -> value map.
func objectKeys(raw []byte, name string) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s is not a JSON object: %w", name, err)
	}
	if m == nil {
		return nil, fmt.Errorf("%s is not a JSON object", name)
	}
	return m, nil
}

// sectionKeys decodes one nested object, requires the listed keys to be
// present, and rejects a JSON null anywhere at its depth (no schema field is
// nullable — zod .optional() means absent, not null).
func sectionKeys(raw json.RawMessage, name string, required ...string) (map[string]json.RawMessage, error) {
	m, err := objectKeys(raw, name)
	if err != nil {
		return nil, err
	}
	for _, k := range required {
		if _, ok := m[k]; !ok {
			return nil, fmt.Errorf("missing required key %q in %s", k, name)
		}
	}
	for k, v := range m {
		if isJSONNull(v) {
			return nil, fmt.Errorf("key %q in %s must not be null", k, name)
		}
	}
	return m, nil
}

// decodeExact unmarshals the exact-key entry src[key] into dst when the key
// is present; an absent key leaves dst at its zero value (defaults, where
// the schema has them, are applied afterwards). Decoding single values from
// the exact-key map is what keeps the zod-strip mirror sound: a struct-level
// unmarshal of raw input would match field names case-insensitively and let
// an unknown key (any casing) clobber a real value.
func decodeExact(src map[string]json.RawMessage, key, name string, dst any) error {
	raw, ok := src[key]
	if !ok {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("wrong type: %s: %w", name, err)
	}
	return nil
}

// stringRecord decodes a z.record(z.string(), z.string()) — every value must
// be a JSON string (map[string]string alone would coerce null to "").
func stringRecord(raw json.RawMessage, name string) (map[string]string, error) {
	entries, err := objectKeys(raw, name)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(entries))
	for k, v := range entries {
		if isJSONNull(v) {
			return nil, fmt.Errorf("value of %q in %s must be a string, got null", k, name)
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, fmt.Errorf("value of %q in %s must be a string: %w", k, name, err)
		}
		out[k] = s
	}
	return out, nil
}

// fontFaces decodes typography.fontFaces: an array of objects with a
// non-empty family and url, an optional weight, and an optional style that
// must be "normal" or "italic". Unknown keys inside an entry are stripped
// (zod default mode), a null entry or field rejects the pack.
func fontFaces(raw json.RawMessage) ([]FontFace, error) {
	if isJSONNull(raw) {
		return nil, fmt.Errorf("typography.fontFaces must not be null")
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("typography.fontFaces must be an array: %w", err)
	}
	faces := make([]FontFace, 0, len(entries))
	for i, entry := range entries {
		name := fmt.Sprintf("typography.fontFaces[%d]", i)
		keys, err := sectionKeys(entry, name, "family", "url")
		if err != nil {
			return nil, err
		}
		var face FontFace
		for _, f := range []struct {
			key string
			dst any
		}{
			{"family", &face.Family}, {"url", &face.URL}, {"weight", &face.Weight}, {"style", &face.Style},
		} {
			if err := decodeExact(keys, f.key, name+"."+f.key, f.dst); err != nil {
				return nil, err
			}
		}
		if face.Family == "" {
			return nil, fmt.Errorf("%s.family must be a non-empty string", name)
		}
		if face.URL == "" {
			return nil, fmt.Errorf("%s.url must be a non-empty string", name)
		}
		if face.Style != nil && *face.Style != "normal" && *face.Style != "italic" {
			return nil, fmt.Errorf("%s.style must be \"normal\" or \"italic\", got %q", name, *face.Style)
		}
		faces = append(faces, face)
	}
	return faces, nil
}

// optionalURL validates an optional z.string().url() field: absent is fine,
// present must parse as an absolute URL (scheme plus host or opaque part —
// matching what `new URL(...)` accepts and a relative path does not satisfy).
func optionalURL(v *string, name string) error {
	if v == nil {
		return nil
	}
	u, err := url.Parse(*v)
	if err != nil || u.Scheme == "" || (u.Host == "" && u.Opaque == "") {
		return fmt.Errorf("%s must be a valid absolute URL, got %q", name, *v)
	}
	return nil
}

// optionalRelativeOrAbsoluteURL validates product.docsUrl (unit W1b, the
// embedded docs SPA): absent is fine; present must be either an absolute URL
// (see optionalURL — a tenant may still point docs at an externally hosted
// origin) or a root-relative path starting with exactly one "/", same
// origin as the app itself. A protocol-relative "//host/..." is rejected —
// it would resolve against whatever scheme the browser is currently on,
// which a same-origin path must not depend on. Mirrors schema.ts's
// `relativeOrAbsoluteUrl`.
func optionalRelativeOrAbsoluteURL(v *string, name string) error {
	if v == nil {
		return nil
	}
	if strings.HasPrefix(*v, "/") && !strings.HasPrefix(*v, "//") {
		return nil
	}
	return optionalURL(v, name)
}
