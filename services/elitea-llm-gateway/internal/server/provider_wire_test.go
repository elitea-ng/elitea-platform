package server

import (
	"testing"

	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
)

// TestServerLinksCompactProviderBodies proves the package that initialises
// bifrost core carries internal/providerwire: the encoder of every provider
// request body emits compact JSON. Removing the import fails this test.
func TestServerLinksCompactProviderBodies(t *testing.T) {
	got, err := providerUtils.MarshalSortedIndent(map[string]any{"role": "system", "content": "<x>"}, "", "  ")
	if err != nil {
		t.Fatalf("MarshalSortedIndent: %v", err)
	}
	if want := `{"content":"<x>","role":"system"}`; string(got) != want {
		t.Fatalf("provider body encoder = %s, want compact %s", got, want)
	}
}
