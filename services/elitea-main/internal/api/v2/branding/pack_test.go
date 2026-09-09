package branding_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/branding"
)

// validPackMap returns a fully-populated, schema-valid brand pack as a
// mutable map. Tests copy-and-mutate it to produce each violation.
func validPackMap() map[string]any {
	return map[string]any{
		"$schema": branding.SchemaURL,
		"id":      "acme",
		"version": "2.1.0",
		"product": map[string]any{
			"name":      "Acme AI",
			"shortName": "Acme",
			"docsUrl":   "https://docs.acme.example/start",
		},
		"assets": map[string]any{
			"logoFull": "/app/brand/acme-full.svg",
			"logoMark": "/app/brand/acme-mark.svg",
			"favicon":  "/app/brand/acme-favicon.svg",
		},
		"typography": map[string]any{
			"fontFamily":     "'Inter', sans-serif",
			"fontFamilyMono": "'Fira Code', monospace",
			"baseSize":       16,
			"scale":          1.25,
		},
		"shape": map[string]any{
			"radiusSm": 2, "radiusMd": 6, "radiusLg": 10, "radiusPill": 500, // 500, not 9999: TestBootstrap_CaseVariantKeyNotServed asserts the served body contains no "999" sentinel, and "9999" contains it.
			"density": "compact",
		},
		"locale": map[string]any{"default": "en-US", "dateLocale": "en-US"},
		"brand":  map[string]any{"hue": "#3366FF", "onBrand": "#FFFFFF"},
		"schemes": map[string]any{
			"light": map[string]any{"surface": "#FAFAFA"},
			"dark":  map[string]any{"surface": "#0B0D10"},
		},
	}
}

func packJSON(t *testing.T, mutate func(m map[string]any)) []byte {
	t.Helper()
	m := validPackMap()
	if mutate != nil {
		mutate(m)
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshaling test pack: %v", err)
	}
	return data
}

func section(m map[string]any, key string) map[string]any {
	return m[key].(map[string]any)
}

func TestParsePack_Table(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(m map[string]any)
		wantErr string // "" = must parse
	}{
		{name: "valid full pack", mutate: nil},

		// --- top-level .strict() and required keys ---
		{name: "unknown top-level key", wantErr: "unknown top-level key",
			mutate: func(m map[string]any) { m["sneaky"] = true }},
		{name: "missing version", wantErr: `missing required key "version"`,
			mutate: func(m map[string]any) { delete(m, "version") }},
		{name: "missing brand", wantErr: `missing required key "brand"`,
			mutate: func(m map[string]any) { delete(m, "brand") }},
		{name: "missing schemes", wantErr: `missing required key "schemes"`,
			mutate: func(m map[string]any) { delete(m, "schemes") }},
		{name: "null top-level value", wantErr: `"product" must not be null`,
			mutate: func(m map[string]any) { m["product"] = nil }},

		// --- $schema literal / id ---
		{name: "wrong $schema literal", wantErr: "$schema must be exactly",
			mutate: func(m map[string]any) { m["$schema"] = "https://elitea.ai/schemas/brand-pack/2.json" }},
		{name: "empty id", wantErr: "id must be a non-empty string",
			mutate: func(m map[string]any) { m["id"] = "" }},
		{name: "id wrong type", wantErr: "wrong type",
			mutate: func(m map[string]any) { m["id"] = 7 }},

		// --- nested required keys / null handling ---
		{name: "product missing name", wantErr: `missing required key "name" in product`,
			mutate: func(m map[string]any) { delete(section(m, "product"), "name") }},
		{name: "product name null", wantErr: `"name" in product must not be null`,
			mutate: func(m map[string]any) { section(m, "product")["name"] = nil }},
		{name: "assets missing favicon", wantErr: `missing required key "favicon" in assets`,
			mutate: func(m map[string]any) { delete(section(m, "assets"), "favicon") }},
		{name: "shape missing radiusLg", wantErr: `missing required key "radiusLg" in shape`,
			mutate: func(m map[string]any) { delete(section(m, "shape"), "radiusLg") }},
		{name: "brand missing hue", wantErr: `missing required key "hue" in brand`,
			mutate: func(m map[string]any) { delete(section(m, "brand"), "hue") }},
		{name: "nested unknown key is allowed (zod strip)",
			mutate: func(m map[string]any) { section(m, "product")["internalNote"] = "kept out of output" }},
		{name: "locale wrong shape", wantErr: "locale is not a JSON object",
			mutate: func(m map[string]any) { m["locale"] = []any{"en-GB"} }},

		// --- url format ---
		{name: "docsUrl not a url", wantErr: "product.docsUrl must be a valid absolute URL",
			mutate: func(m map[string]any) { section(m, "product")["docsUrl"] = "not a url" }},
		// [W1b] docsUrl uniquely also accepts a root-relative path — the
		// embedded docs SPA is same origin, so the compiled default pack
		// states it this way (apps/elitea-web's default.pack.json). A
		// protocol-relative "//host/..." is still rejected: it would
		// resolve against whatever scheme the browser is currently on.
		{name: "docsUrl root-relative path is valid",
			mutate: func(m map[string]any) { section(m, "product")["docsUrl"] = "/docs/start" }},
		{name: "docsUrl protocol-relative path rejected", wantErr: "product.docsUrl must be a valid absolute URL",
			mutate: func(m map[string]any) { section(m, "product")["docsUrl"] = "//evil.example/docs" }},
		{name: "supportUrl invalid", wantErr: "product.supportUrl must be a valid absolute URL",
			mutate: func(m map[string]any) { section(m, "product")["supportUrl"] = "elitea.ai/support" }},
		{name: "docsUrl absent is fine",
			mutate: func(m map[string]any) { delete(section(m, "product"), "docsUrl") }},

		// --- typography ranges and defaults ---
		{name: "baseSize below min", wantErr: "typography.baseSize must be in [12, 18]",
			mutate: func(m map[string]any) { section(m, "typography")["baseSize"] = 11.9 }},
		{name: "baseSize above max", wantErr: "typography.baseSize must be in [12, 18]",
			mutate: func(m map[string]any) { section(m, "typography")["baseSize"] = 18.1 }},
		{name: "baseSize at lower bound",
			mutate: func(m map[string]any) { section(m, "typography")["baseSize"] = 12 }},
		{name: "baseSize at upper bound",
			mutate: func(m map[string]any) { section(m, "typography")["baseSize"] = 18 }},
		{name: "baseSize wrong type", wantErr: "wrong type",
			mutate: func(m map[string]any) { section(m, "typography")["baseSize"] = "14" }},
		{name: "scale below min", wantErr: "typography.scale must be in [1.05, 1.5]",
			mutate: func(m map[string]any) { section(m, "typography")["scale"] = 1.04 }},
		{name: "scale above max", wantErr: "typography.scale must be in [1.05, 1.5]",
			mutate: func(m map[string]any) { section(m, "typography")["scale"] = 1.51 }},
		{name: "scale at bounds ok",
			mutate: func(m map[string]any) { section(m, "typography")["scale"] = 1.5 }},
		{name: "typography missing fontFamily", wantErr: `missing required key "fontFamily" in typography`,
			mutate: func(m map[string]any) { delete(section(m, "typography"), "fontFamily") }},

		// --- density enum ---
		{name: "density invalid", wantErr: `shape.density must be "comfortable" or "compact"`,
			mutate: func(m map[string]any) { section(m, "shape")["density"] = "cozy" }},
		{name: "density comfortable",
			mutate: func(m map[string]any) { section(m, "shape")["density"] = "comfortable" }},

		// --- schemes ---
		{name: "schemes missing dark", wantErr: `missing required key "dark" in schemes`,
			mutate: func(m map[string]any) { delete(section(m, "schemes"), "dark") }},
		{name: "schemes light null", wantErr: `"light" in schemes must not be null`,
			mutate: func(m map[string]any) { section(m, "schemes")["light"] = nil }},
		{name: "scheme value not a string", wantErr: `value of "surface" in schemes.light must be a string`,
			mutate: func(m map[string]any) {
				section(m, "schemes")["light"] = map[string]any{"surface": 42}
			}},
		{name: "scheme value null", wantErr: `value of "surface" in schemes.dark must be a string, got null`,
			mutate: func(m map[string]any) {
				section(m, "schemes")["dark"] = map[string]any{"surface": nil}
			}},
		{name: "optional hc scheme accepted",
			mutate: func(m map[string]any) {
				section(m, "schemes")["hc"] = map[string]any{"surface": "#000000"}
			}},
		{name: "empty scheme records are valid",
			mutate: func(m map[string]any) {
				section(m, "schemes")["light"] = map[string]any{}
				section(m, "schemes")["dark"] = map[string]any{}
			}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pack, err := branding.ParsePack(packJSON(t, tc.mutate))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected valid pack, got error: %v", err)
				}
				if pack == nil {
					t.Fatal("expected non-nil pack")
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got valid pack", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestParsePack_MalformedInputs(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{"truncated JSON", `{"$schema":`},
		{"empty input", ``},
		{"top-level array", `[]`},
		{"top-level string", `"pack"`},
		{"top-level null", `null`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := branding.ParsePack([]byte(tc.data)); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

func TestParsePack_DefaultsMaterialised(t *testing.T) {
	data := packJSON(t, func(m map[string]any) {
		delete(section(m, "typography"), "baseSize")
		delete(section(m, "typography"), "scale")
		m["locale"] = map[string]any{} // object required, all keys default
	})
	pack, err := branding.ParsePack(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pack.Typography.BaseSize != 14 {
		t.Errorf("baseSize default: got %v, want 14", pack.Typography.BaseSize)
	}
	if pack.Typography.Scale != 1.2 {
		t.Errorf("scale default: got %v, want 1.2", pack.Typography.Scale)
	}
	if pack.Locale.Default != "en-GB" || pack.Locale.DateLocale != "en-GB" {
		t.Errorf("locale defaults: got %+v, want en-GB/en-GB", pack.Locale)
	}
}

func TestParsePack_ValuesRoundTrip(t *testing.T) {
	pack, err := branding.ParsePack(packJSON(t, nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pack.ID != "acme" || pack.Version != "2.1.0" {
		t.Errorf("id/version: got %q/%q", pack.ID, pack.Version)
	}
	if pack.Product.Name != "Acme AI" || pack.Product.ShortName != "Acme" {
		t.Errorf("product: got %+v", pack.Product)
	}
	if pack.Product.DocsURL == nil || *pack.Product.DocsURL != "https://docs.acme.example/start" {
		t.Errorf("docsUrl: got %v", pack.Product.DocsURL)
	}
	if pack.Shape.Density != "compact" {
		t.Errorf("density: got %q", pack.Shape.Density)
	}
	if pack.Schemes.Light["surface"] != "#FAFAFA" || pack.Schemes.Dark["surface"] != "#0B0D10" {
		t.Errorf("schemes: got %+v", pack.Schemes)
	}
}

// TestParsePack_CaseVariantKeysNeverClobber pins the zod-strip mirror
// against encoding/json's case-INsensitive struct field matching: a
// case-variant of a known key is an UNKNOWN key — zod strips it and keeps
// the exactly-spelled value. A raw `json.Unmarshal(data, &Pack{})` would let
// "RADIUSSM" bind to RadiusSm and clobber the real value; the exact-key
// decode must be immune for every field class (validated, defaulted,
// unvalidated).
func TestParsePack_CaseVariantKeysNeverClobber(t *testing.T) {
	// The poison key is injected as raw JSON text AFTER the real key.
	// Placement matters for reproducing the bug: encoding/json assigns keys
	// in document order, so a case-variant that FOLLOWS the real key is the
	// one that wins under a raw struct unmarshal. (Map-built fixtures
	// marshal with sorted keys, which would put "RADIUSSM" first and mask
	// the clobber.)
	inject := func(t *testing.T, doc, after, poison string) []byte {
		t.Helper()
		if !strings.Contains(doc, after) {
			t.Fatalf("fixture drift: %q not found in\n%s", after, doc)
		}
		return []byte(strings.Replace(doc, after, after+","+poison, 1))
	}

	tests := []struct {
		name   string
		mutate func(m map[string]any) // optional pre-marshal edit
		after  string                 // real key the poison is injected after
		poison string                 // raw JSON member, case-variant key
		check  func(t *testing.T, p *branding.Pack)
	}{
		{
			name:   "unvalidated number field: RADIUSSM stripped, radiusSm kept",
			after:  `"radiusSm":2`,
			poison: `"RADIUSSM":999`,
			check: func(t *testing.T, p *branding.Pack) {
				if p.Shape.RadiusSm != 2 {
					t.Errorf("radiusSm: got %v, want the pack's real value 2 (case-variant key clobbered it)", p.Shape.RadiusSm)
				}
			},
		},
		{
			name:   "unvalidated string field: HUE stripped, hue kept",
			after:  `"hue":"#3366FF"`,
			poison: `"HUE":"#BAD000"`,
			check: func(t *testing.T, p *branding.Pack) {
				if p.Brand.Hue != "#3366FF" {
					t.Errorf("brand.hue: got %q, want the pack's real value #3366FF", p.Brand.Hue)
				}
			},
		},
		{
			name:   "validated enum field: DENSITY junk is STRIPPED, not rejected",
			after:  `"density":"compact"`,
			poison: `"DENSITY":"junk"`,
			check: func(t *testing.T, p *branding.Pack) {
				if p.Shape.Density != "compact" {
					t.Errorf("density: got %q, want the pack's real value compact", p.Shape.Density)
				}
			},
		},
		{
			name:   "validated range field: BASESIZE out-of-range is stripped, not validated",
			after:  `"baseSize":16`,
			poison: `"BASESIZE":99`,
			check: func(t *testing.T, p *branding.Pack) {
				if p.Typography.BaseSize != 16 {
					t.Errorf("baseSize: got %v, want the pack's real value 16", p.Typography.BaseSize)
				}
			},
		},
		{
			name: "defaulted field: SCALE stripped while real scale absent, default applied",
			mutate: func(m map[string]any) {
				delete(section(m, "typography"), "scale")
			},
			after:  `"fontFamilyMono":"'Fira Code', monospace"`,
			poison: `"SCALE":99`,
			check: func(t *testing.T, p *branding.Pack) {
				if p.Typography.Scale != 1.2 {
					t.Errorf("scale: got %v, want the zod default 1.2 (case-variant key must not feed the field)", p.Typography.Scale)
				}
			},
		},
		{
			name: "optional pointer field: DOCSURL stripped, stays absent",
			mutate: func(m map[string]any) {
				delete(section(m, "product"), "docsUrl")
			},
			after:  `"name":"Acme AI"`,
			poison: `"DOCSURL":"not a url"`, // would fail .url() if it ever bound
			check: func(t *testing.T, p *branding.Pack) {
				if p.Product.DocsURL != nil {
					t.Errorf("docsUrl: got %q, want absent (case-variant key bound to the pointer field)", *p.Product.DocsURL)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := inject(t, string(packJSON(t, tc.mutate)), tc.after, tc.poison)
			pack, err := branding.ParsePack(doc)
			if err != nil {
				t.Fatalf("case-variant nested keys are unknown keys and must be stripped, not rejected; got error: %v", err)
			}
			tc.check(t, pack)
		})
	}

	// Top level is .strict(): a case-variant there is an unknown key and is
	// REJECTED (not stripped) — matching zod exactly.
	t.Run("top-level case variant is rejected by strict", func(t *testing.T) {
		_, err := branding.ParsePack(packJSON(t, func(m map[string]any) { m["ID"] = "spoof" }))
		if err == nil || !strings.Contains(err.Error(), "unknown top-level key") {
			t.Fatalf("expected strict rejection of top-level \"ID\", got err=%v", err)
		}
	})
}

// TestDefaultPack_IsSchemaValid pins the degradation floor: the built-in
// default must itself survive the validator it degrades from, and must carry
// the mandated identity (id "default", product name "Elitea").
func TestDefaultPack_IsSchemaValid(t *testing.T) {
	def := branding.DefaultPack()
	if def.ID != "default" {
		t.Errorf("default pack id: got %q, want \"default\"", def.ID)
	}
	if def.Product.Name != "Elitea" {
		t.Errorf("default pack product name: got %q, want \"Elitea\"", def.Product.Name)
	}

	data, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshaling default pack: %v", err)
	}
	reparsed, err := branding.ParsePack(data)
	if err != nil {
		t.Fatalf("built-in default pack does not validate against its own schema: %v", err)
	}
	if !reflect.DeepEqual(def, reparsed) {
		t.Errorf("default pack does not round-trip:\n  out: %+v\n  in:  %+v", def, reparsed)
	}
}

func TestParsePack_FontFaces(t *testing.T) {
	face := func(m map[string]any, faces ...any) { section(m, "typography")["fontFaces"] = faces }
	good := packJSON(t, func(m map[string]any) {
		face(m,
			map[string]any{"family": "Inter", "url": "/api/v2/branding/assets/font/a.woff2", "weight": "100 900", "style": "italic", "extra": 1},
			map[string]any{"family": "Inter", "url": "/api/v2/branding/assets/font/b.woff2"},
		)
	})
	pack, err := branding.ParsePack(good)
	if err != nil {
		t.Fatalf("ParsePack: %v", err)
	}
	if len(pack.Typography.FontFaces) != 2 || pack.Typography.FontFaces[0].Family != "Inter" ||
		pack.Typography.FontFaces[0].Weight == nil || *pack.Typography.FontFaces[0].Weight != "100 900" ||
		pack.Typography.FontFaces[1].Style != nil {
		t.Fatalf("fontFaces = %+v", pack.Typography.FontFaces)
	}
	served, _ := json.Marshal(pack)
	if strings.Contains(string(served), `"extra"`) {
		t.Fatalf("unknown face key leaked: %s", served)
	}
	// Absent means absent — no empty array is materialised.
	plain, _ := branding.ParsePack(packJSON(t, nil))
	if data, _ := json.Marshal(plain); strings.Contains(string(data), "fontFaces") {
		t.Fatalf("fontFaces materialised for a pack that declared none: %s", data)
	}

	for name, mutate := range map[string]func(m map[string]any){
		"not an array": func(m map[string]any) { section(m, "typography")["fontFaces"] = "Inter" },
		"null":         func(m map[string]any) { section(m, "typography")["fontFaces"] = nil },
		"missing url":  func(m map[string]any) { face(m, map[string]any{"family": "Inter"}) },
		"empty family": func(m map[string]any) { face(m, map[string]any{"family": "", "url": "/x.woff2"}) },
		"bad style":    func(m map[string]any) { face(m, map[string]any{"family": "I", "url": "/x", "style": "oblique"}) },
		"null field":   func(m map[string]any) { face(m, map[string]any{"family": "I", "url": nil}) },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := branding.ParsePack(packJSON(t, mutate)); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}
