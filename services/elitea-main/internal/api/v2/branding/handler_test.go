package branding_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/branding"
)

const bootstrapPath = "/api/v2/branding/bootstrap.js"

// writePack writes data to a fresh temp file and returns its path.
func writePack(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "brand-pack.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing pack file: %v", err)
	}
	return path
}

func get(t *testing.T, h *branding.Handler, target string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	h.Bootstrap(rec, req)
	return rec
}

// etagValue strips the quotes off the handler's strong ETag — the ?v= token.
func etagValue(t *testing.T, h *branding.Handler) string {
	t.Helper()
	et := h.ETag()
	if !strings.HasPrefix(et, `"`) || !strings.HasSuffix(et, `"`) || strings.HasPrefix(et, "W/") {
		t.Fatalf("ETag %q is not a strong quoted entity tag", et)
	}
	return strings.Trim(et, `"`)
}

// --- pack resolution / degradation --------------------------------------------

func TestNewHandler_PackResolution(t *testing.T) {
	valid := packJSON(t, func(m map[string]any) {
		m["id"] = "acme"
		section(m, "product")["name"] = "Acme AI"
	})

	tests := []struct {
		name         string
		packPath     func(t *testing.T) string
		wantContains []string
		wantAbsent   []string
	}{
		{
			// No deployment pack -> publish NOTHING, so the UI keeps its own
			// compiled-in default pack (channel A). Serving the built-in
			// DefaultPack here instead would WIN over channel A and repaint
			// the whole app from its placeholder hue — see `noPackBody`.
			name:         "empty path publishes no pack",
			packPath:     func(t *testing.T) string { return "" },
			wantContains: []string{"no deployment brand pack configured"},
			wantAbsent:   []string{"window.elitea_brand", `"id":"default"`},
		},
		{
			name: "missing file degrades to no pack",
			packPath: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "does-not-exist.json")
			},
			wantContains: []string{"no deployment brand pack configured"},
			wantAbsent:   []string{"window.elitea_brand"},
		},
		{
			name: "invalid JSON degrades to no pack",
			packPath: func(t *testing.T) string {
				return writePack(t, []byte(`{"$schema": `))
			},
			wantContains: []string{"no deployment brand pack configured"},
			wantAbsent:   []string{"window.elitea_brand"},
		},
		{
			name: "schema-violating pack degrades whole, never partially merges",
			packPath: func(t *testing.T) string {
				return writePack(t, packJSON(t, func(m map[string]any) {
					section(m, "product")["name"] = "Acme AI"
					section(m, "typography")["baseSize"] = 99 // out of [12,18]
				}))
			},
			wantContains: []string{"no deployment brand pack configured"},
			// no field of the invalid pack leaks — and nothing else does either
			wantAbsent: []string{"Acme AI", "window.elitea_brand"},
		},
		{
			name: "unknown top-level key degrades to no pack",
			packPath: func(t *testing.T) string {
				return writePack(t, packJSON(t, func(m map[string]any) {
					m["sneaky"] = true
				}))
			},
			wantContains: []string{"no deployment brand pack configured"},
			wantAbsent:   []string{"acme", "window.elitea_brand"},
		},
		{
			name:         "valid pack is served",
			packPath:     func(t *testing.T) string { return writePack(t, valid) },
			wantContains: []string{`"id":"acme"`, `"name":"Acme AI"`},
			wantAbsent:   []string{`"id":"default"`},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := branding.NewHandler(branding.Config{PackPath: tc.packPath(t)})
			rec := get(t, h, bootstrapPath, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status: got %d, want 200", rec.Code)
			}
			body := rec.Body.String()
			for _, want := range tc.wantContains {
				if !strings.Contains(body, want) {
					t.Errorf("body does not contain %q:\n%s", want, body)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(body, absent) {
					t.Errorf("body must not contain %q:\n%s", absent, body)
				}
			}
		})
	}
}

func TestNewHandler_NestedUnknownKeysAreStripped(t *testing.T) {
	path := writePack(t, packJSON(t, func(m map[string]any) {
		section(m, "product")["internalNote"] = "must-not-be-served"
	}))
	h := branding.NewHandler(branding.Config{PackPath: path})
	rec := get(t, h, bootstrapPath, nil)
	if strings.Contains(rec.Body.String(), "must-not-be-served") {
		t.Errorf("nested unknown key leaked into the served pack (zod strip mirror broken)")
	}
	if !strings.Contains(rec.Body.String(), `"id":"acme"`) {
		t.Errorf("pack with nested unknown key should still be served, got:\n%s", rec.Body.String())
	}
}

// --- body shape ----------------------------------------------------------------

func TestBootstrap_BodyIsExecutableAssignment(t *testing.T) {
	h := branding.NewHandler(branding.Config{PackPath: writePack(t, packJSON(t, nil))})
	rec := get(t, h, bootstrapPath, nil)

	body := rec.Body.String()
	const prefix = "window.elitea_brand = "
	if !strings.HasPrefix(body, prefix) {
		t.Fatalf("body does not start with %q:\n%s", prefix, body)
	}
	if !strings.HasSuffix(body, ";") {
		t.Fatalf("body does not end with ';':\n%s", body)
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(body, prefix), ";")

	// The payload must be valid JSON that round-trips through the Pack type
	// byte-for-byte (i.e. it IS a schema-shaped pack, not JSON-ish text).
	var pack branding.Pack
	if err := json.Unmarshal([]byte(payload), &pack); err != nil {
		t.Fatalf("payload is not valid JSON: %v\n%s", err, payload)
	}
	remarshaled, err := json.Marshal(&pack)
	if err != nil {
		t.Fatalf("re-marshaling payload: %v", err)
	}
	if string(remarshaled) != payload {
		t.Errorf("payload does not round-trip:\n  served: %s\n  round:  %s", payload, remarshaled)
	}

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/javascript") {
		t.Errorf("Content-Type: got %q, want application/javascript", ct)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("X-Content-Type-Options missing")
	}
}

// --- ETag behaviour --------------------------------------------------------------

func TestETag_StableAndContentAddressed(t *testing.T) {
	data := packJSON(t, nil)
	h1 := branding.NewHandler(branding.Config{PackPath: writePack(t, data)})
	h2 := branding.NewHandler(branding.Config{PackPath: writePack(t, data)})
	if h1.ETag() != h2.ETag() {
		t.Errorf("identical content produced different ETags: %q vs %q", h1.ETag(), h2.ETag())
	}

	changed := packJSON(t, func(m map[string]any) { m["version"] = "9.9.9" })
	h3 := branding.NewHandler(branding.Config{PackPath: writePack(t, changed)})
	if h3.ETag() == h1.ETag() {
		t.Errorf("different content produced the same ETag %q", h1.ETag())
	}

	// The header actually carries it.
	rec := get(t, h1, bootstrapPath, nil)
	if got := rec.Header().Get("ETag"); got != h1.ETag() {
		t.Errorf("ETag header: got %q, want %q", got, h1.ETag())
	}
}

// --- header matrix ----------------------------------------------------------------

func TestBootstrap_HeaderMatrix(t *testing.T) {
	h := branding.NewHandler(branding.Config{PackPath: writePack(t, packJSON(t, nil))})
	current := etagValue(t, h)

	t.Run("bare URL revalidates", func(t *testing.T) {
		rec := get(t, h, bootstrapPath, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status: got %d, want 200", rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("Cache-Control: got %q, want no-cache", cc)
		}
		if rec.Header().Get("ETag") == "" {
			t.Errorf("bare URL response is missing ETag")
		}
	})

	t.Run("matching v is immutable", func(t *testing.T) {
		rec := get(t, h, bootstrapPath+"?v="+current, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status: got %d, want 200", rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
			t.Errorf("Cache-Control: got %q, want immutable policy", cc)
		}
	})

	t.Run("immutable headers ONLY on matching v", func(t *testing.T) {
		for _, target := range []string{bootstrapPath, bootstrapPath + "?v="} {
			rec := get(t, h, target, nil)
			if cc := rec.Header().Get("Cache-Control"); strings.Contains(cc, "immutable") {
				t.Errorf("%s: immutable Cache-Control %q leaked onto a non-versioned URL", target, cc)
			}
		}
	})

	t.Run("mismatched v redirects to current version", func(t *testing.T) {
		rec := get(t, h, bootstrapPath+"?v=deadbeef", nil)
		if rec.Code != http.StatusFound {
			t.Fatalf("status: got %d, want 302", rec.Code)
		}
		wantLoc := bootstrapPath + "?v=" + current
		if loc := rec.Header().Get("Location"); loc != wantLoc {
			t.Errorf("Location: got %q, want %q", loc, wantLoc)
		}
		if cc := rec.Header().Get("Cache-Control"); strings.Contains(cc, "immutable") {
			t.Errorf("redirect must not be immutable, got Cache-Control %q", cc)
		}
	})

	t.Run("If-None-Match returns 304", func(t *testing.T) {
		rec := get(t, h, bootstrapPath, http.Header{"If-None-Match": {h.ETag()}})
		if rec.Code != http.StatusNotModified {
			t.Fatalf("status: got %d, want 304", rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("304 must have an empty body, got %d bytes", rec.Body.Len())
		}
		if rec.Header().Get("ETag") != h.ETag() {
			t.Errorf("304 must repeat the ETag")
		}
	})

	t.Run("If-None-Match variants", func(t *testing.T) {
		for _, inm := range []string{
			"W/" + h.ETag(),             // weak comparison still matches for GET
			`"other"` + ", " + h.ETag(), // list form
			"*",                         // wildcard
		} {
			rec := get(t, h, bootstrapPath, http.Header{"If-None-Match": {inm}})
			if rec.Code != http.StatusNotModified {
				t.Errorf("If-None-Match %q: got %d, want 304", inm, rec.Code)
			}
		}
		rec := get(t, h, bootstrapPath, http.Header{"If-None-Match": {`"stale"`}})
		if rec.Code != http.StatusOK {
			t.Errorf("non-matching If-None-Match: got %d, want 200", rec.Code)
		}
	})

	t.Run("304 on versioned URL keeps immutable policy", func(t *testing.T) {
		rec := get(t, h, bootstrapPath+"?v="+current, http.Header{"If-None-Match": {h.ETag()}})
		if rec.Code != http.StatusNotModified {
			t.Fatalf("status: got %d, want 304", rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
			t.Errorf("Cache-Control on versioned 304: got %q", cc)
		}
	})

	t.Run("mismatched v wins over If-None-Match", func(t *testing.T) {
		rec := get(t, h, bootstrapPath+"?v=deadbeef", http.Header{"If-None-Match": {h.ETag()}})
		if rec.Code != http.StatusFound {
			t.Errorf("stale version with matching ETag: got %d, want 302", rec.Code)
		}
	})

	t.Run("quoted entity-tag containing comma and star is ONE non-matching tag", func(t *testing.T) {
		// RFC 9110 §8.8.3/§13.1.2: `"abc,*,def"` is a single entity-tag; a
		// naive comma split would surface a bare `*` member and wrongly 304.
		rec := get(t, h, bootstrapPath, http.Header{"If-None-Match": {`"abc,*,def"`}})
		if rec.Code != http.StatusOK {
			t.Errorf(`If-None-Match: "abc,*,def": got %d, want 200 (it is one non-matching tag, not a wildcard)`, rec.Code)
		}
	})

	t.Run("star inside a list is not a wildcard", func(t *testing.T) {
		rec := get(t, h, bootstrapPath, http.Header{"If-None-Match": {`"abc", *`}})
		if rec.Code != http.StatusOK {
			t.Errorf("got %d, want 200 (* is only valid as the entire field value)", rec.Code)
		}
	})

	t.Run("list containing the current tag still matches after tokenizer fix", func(t *testing.T) {
		rec := get(t, h, bootstrapPath, http.Header{"If-None-Match": {`"a,b", ` + h.ETag()}})
		if rec.Code != http.StatusNotModified {
			t.Errorf("got %d, want 304 (current tag after a comma-bearing tag)", rec.Code)
		}
	})
}

// --- HEAD ------------------------------------------------------------------------

func TestBootstrap_HEAD(t *testing.T) {
	h := branding.NewHandler(branding.Config{PackPath: writePack(t, packJSON(t, nil))})
	current := etagValue(t, h)

	head := func(t *testing.T, target string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodHead, target, nil)
		rec := httptest.NewRecorder()
		h.Bootstrap(rec, req)
		return rec
	}

	t.Run("bare HEAD: 200, ETag, no-cache, empty body", func(t *testing.T) {
		rec := head(t, bootstrapPath)
		if rec.Code != http.StatusOK {
			t.Fatalf("status: got %d, want 200", rec.Code)
		}
		if rec.Header().Get("ETag") != h.ETag() {
			t.Errorf("ETag: got %q, want %q", rec.Header().Get("ETag"), h.ETag())
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("Cache-Control: got %q, want no-cache", cc)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("HEAD must have an empty body, got %d bytes", rec.Body.Len())
		}
		if rec.Header().Get("Content-Length") == "" || rec.Header().Get("Content-Length") == "0" {
			t.Errorf("HEAD should carry the GET body's Content-Length, got %q", rec.Header().Get("Content-Length"))
		}
	})

	t.Run("HEAD with matching v: immutable headers, empty body", func(t *testing.T) {
		rec := head(t, bootstrapPath+"?v="+current)
		if rec.Code != http.StatusOK {
			t.Fatalf("status: got %d, want 200", rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
			t.Errorf("Cache-Control: got %q, want immutable policy", cc)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("HEAD must have an empty body, got %d bytes", rec.Body.Len())
		}
	})
}

// TestBootstrap_CaseVariantKeyNotServed pins finding 1 end to end: a valid
// pack carrying `"RADIUSSM": 999` must serve the pack's REAL radiusSm value;
// the case-variant unknown key is stripped, never bound.
func TestBootstrap_CaseVariantKeyNotServed(t *testing.T) {
	// Injected as raw JSON AFTER the real key — document order is what makes
	// a raw struct unmarshal clobber (see TestParsePack_CaseVariantKeysNeverClobber).
	doc := string(packJSON(t, nil))
	const realKey = `"radiusSm":2`
	if !strings.Contains(doc, realKey) {
		t.Fatalf("fixture drift: %q not found in\n%s", realKey, doc)
	}
	path := writePack(t, []byte(strings.Replace(doc, realKey, realKey+`,"RADIUSSM":999`, 1)))
	h := branding.NewHandler(branding.Config{PackPath: path})
	rec := get(t, h, bootstrapPath, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (case-variant nested key must not reject the pack)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"radiusSm":2`) {
		t.Errorf("served radiusSm lost the pack's real value:\n%s", body)
	}
	if strings.Contains(body, "999") || strings.Contains(body, "RADIUSSM") {
		t.Errorf("case-variant unknown key leaked into the served pack:\n%s", body)
	}
}
