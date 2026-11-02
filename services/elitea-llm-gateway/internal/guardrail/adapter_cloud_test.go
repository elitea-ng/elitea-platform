package guardrail

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// cloudServer is a TLS mock of a vendor API. The cloud adapters accept https
// endpoints only.
func cloudServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// cloudEnv allows only the mock host and resolves NAME to "value-NAME".
func cloudEnv(srv *httptest.Server) Env {
	return Env{
		HTTPClient:   srv.Client(),
		EgressAllows: func(u string) bool { return strings.HasPrefix(u, srv.URL) },
		ResolveSecret: func(_ context.Context, ref string) (string, error) {
			return "value-" + strings.TrimSuffix(strings.TrimPrefix(ref, "{{secret."), "}}"), nil
		},
	}
}

func cloudPipeline(t *testing.T, stage string, env Env, detector map[string]any) (*Pipeline, *Collector) {
	t.Helper()
	col := NewCollector()
	row := mustRow(t, map[string]any{"guardrail": map[string]any{
		"stage": stage, "timeout_ms": 2000.0, "detectors": []any{detector},
	}})
	return &Pipeline{Stage: stage, Rows: []*Row{row}, Env: env, Collector: col}, col
}

func TestCodePointChunks(t *testing.T) {
	content := strings.Repeat("é", 5) + "ab"
	parts := codePointChunks(content, 3)
	if len(parts) != 3 {
		t.Fatalf("parts = %v", parts)
	}
	got := []string{content[parts[0].start:parts[0].end], content[parts[1].start:parts[1].end], content[parts[2].start:parts[2].end]}
	if got[0] != "ééé" || got[1] != "ééa" || got[2] != "b" {
		t.Fatalf("parts = %q", got)
	}
	if p := codePointChunks("", 3); len(p) != 1 || p[0] != (bound{}) {
		t.Fatalf("empty content parts = %v", p)
	}
}

func TestCodePointAndByteSpans(t *testing.T) {
	content := "héllo Alice"
	if b, ok := codePointSpan(content, 6, 11); !ok || content[b.start:b.end] != "Alice" {
		t.Fatalf("codePointSpan = %v %v", b, ok)
	}
	if b, ok := codePointSpan(content, 0, 2); !ok || content[b.start:b.end] != "hé" {
		t.Fatalf("codePointSpan = %v %v", b, ok)
	}
	if _, ok := codePointSpan(content, 3, 12); ok {
		t.Fatal("a range past the end was accepted")
	}
	if b, ok := byteSpan(content, 7, 12); !ok || content[b.start:b.end] != "Alice" {
		t.Fatalf("byteSpan = %v %v", b, ok)
	}
	// Byte 2 is inside "é".
	if _, ok := byteSpan(content, 2, 4); ok {
		t.Fatal("a range inside a character was accepted")
	}
	if _, ok := byteSpan(content, 0, 99); ok {
		t.Fatal("a range past the end was accepted")
	}
}

func TestVendorLabel(t *testing.T) {
	cases := map[[2]string]string{
		{"pii:", "EMAIL"}:                     "pii:email",
		{"pii:", "US_SOCIAL_SECURITY_NUMBER"}: "pii:us_social_security_number",
		{"regex:", "Employee Id"}:             "regex:employee_id",
		{"pii:", ""}:                          "fallback",
		{"", "HATE"}:                          "hate",
	}
	for in, want := range cases {
		if got := vendorLabel(in[0], in[1], "fallback"); got != want {
			t.Errorf("vendorLabel(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
	if got := vendorLabel("pii:", strings.Repeat("x", 100), "pii"); len(got) != 64 || !ValidLabel(got) {
		t.Errorf("a long name gave %q", got)
	}
}

func TestLocateAll(t *testing.T) {
	spans := locateAll("Mail a@b.io or A@B.IO", "a@b.io", true)
	if len(spans) != 2 {
		t.Fatalf("spans = %v", spans)
	}
	if len(locateAll("abc", "", false)) != 0 || len(locateAll("abc", "x", false)) != 0 {
		t.Fatal("an empty or absent match gave spans")
	}
}

func TestCloudEndpointRefusesBadURLs(t *testing.T) {
	for _, bad := range []string{"", "http://x.example.com", "https://u:p@x.example.com", "https://x.example.com/?a=1", "ftp://x"} {
		if _, err := cloudEndpoint("endpoint", bad); err == nil {
			t.Errorf("cloudEndpoint(%q) was accepted", bad)
		}
	}
	if got, err := cloudEndpoint("endpoint", "https://x.example.com/"); err != nil || got != "https://x.example.com" {
		t.Errorf("cloudEndpoint = %q, %v", got, err)
	}
}
