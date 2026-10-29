package server

import (
	"testing"

	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
)

// TestProviderBodiesAreCompact pins the encoder bifrost/core builds every
// converted provider request body with (CheckContextAndGetRequestBody →
// MarshalProviderRequest) to compact JSON. An indented body whose strings carry
// ordinary markup is refused by the edge in front of a provider (a WAF answers
// 403); bifrost v1.7.x indented them and the gateway patched the encoder
// (internal/providerwire, removed once v1.8.0+ sent compact bodies). A bifrost
// release that indents again fails here.
func TestProviderBodiesAreCompact(t *testing.T) {
	body := map[string]any{"role": "system", "content": "<active_instruction> a < b and c > d"}
	got, err := providerUtils.MarshalProviderRequest(body)
	if err != nil {
		t.Fatalf("MarshalProviderRequest: %v", err)
	}
	if want := `{"content":"<active_instruction> a < b and c > d","role":"system"}`; string(got) != want {
		t.Fatalf("provider body encoder = %s, want compact %s", got, want)
	}
}
