package profile

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// A document without cache parameters keeps its canonical bytes (and so its
// digest): the field is optional and omitted when absent (#1121).
func TestCachePolicyIsOptionalAndKeepsCanonicalBytes(t *testing.T) {
	doc, canonical, err := ParsePolicyDocument([]byte(validPolicy))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Cache != nil || bytes.Contains(canonical, []byte("cache")) {
		t.Fatalf("canonical bytes changed: %s", canonical)
	}
	settings := doc.CacheSettings()
	if settings != (CacheSettings{MaxEntries: DefaultCacheMaxEntries, MaxBytes: DefaultCacheMaxBytes, Skew: DefaultCacheSkew}) {
		t.Fatalf("defaults %+v", settings)
	}
	if settings.CacheStaticSecrets {
		t.Fatal("static secrets are cached by default")
	}
}

func TestCachePolicyParsesAndBoundsItsParameters(t *testing.T) {
	with := func(cache string) string {
		return strings.TrimSuffix(validPolicy, "}") + `,"cache":` + cache + `}`
	}
	doc, _, err := ParsePolicyDocument([]byte(with(`{"max_entries":10,"max_bytes":2048,"skew_seconds":5,"cache_static_secrets":true}`)))
	if err != nil {
		t.Fatal(err)
	}
	if got := doc.CacheSettings(); got != (CacheSettings{MaxEntries: 10, MaxBytes: 2048, Skew: 5 * time.Second, CacheStaticSecrets: true}) {
		t.Fatalf("settings %+v", got)
	}
	for _, bad := range []string{
		`{"max_entries":-1}`, `{"max_bytes":2147483648}`, `{"skew_seconds":3601}`, `{"ttl":1}`,
	} {
		if _, _, err := ParsePolicyDocument([]byte(with(bad))); !errors.Is(err, ErrInvalidPolicy) {
			t.Fatalf("%s: err = %v", bad, err)
		}
	}
}
