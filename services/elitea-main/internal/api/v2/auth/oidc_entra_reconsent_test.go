package auth

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	identityoidc "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/oidc"
)

// A claims challenge keeps the cp1 capability; a claim the challenge names
// wins; anything that is not an object of objects is dropped.
func TestMergeEntraClaims(t *testing.T) {
	merged, ok := mergeEntraClaims(identityoidc.EntraClientCapabilities, `{"access_token":{"acrs":{"essential":true,"value":"c1"}},"id_token":{"auth_time":{"essential":true}}}`)
	if !ok {
		t.Fatal("refused a valid challenge")
	}
	var got map[string]map[string]any
	if err := json.Unmarshal([]byte(merged), &got); err != nil {
		t.Fatal(err)
	}
	if got["access_token"]["xms_cc"] == nil || got["access_token"]["acrs"] == nil || got["id_token"]["auth_time"] == nil {
		t.Fatalf("merged %s", merged)
	}
	for _, bad := range []string{`[]`, `{"access_token":"x"}`, `{}`, `not json`, `{"access_token":null}`, `{"a":{"b":"` + strings.Repeat("x", maxReconsentClaimsBytes) + `"}}`} {
		if _, ok := mergeEntraClaims(identityoidc.EntraClientCapabilities, bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}

// Only prompt=login or prompt=consent passes; claims pass only in Entra mode.
func TestReconsentOptions(t *testing.T) {
	q := url.Values{"prompt": {"none"}, "claims": {`{"access_token":{"acrs":{"essential":true}}}`}}
	if got := reconsentOptions(httptest.NewRequest("GET", "/auth/oidc/login?"+q.Encode(), nil), nil); len(got) != 0 {
		t.Fatalf("options without Entra: %d", len(got))
	}
	q.Set("prompt", "login")
	if got := reconsentOptions(httptest.NewRequest("GET", "/auth/oidc/login?"+q.Encode(), nil), &entraRuntime{}); len(got) != 2 {
		t.Fatalf("options with Entra: %d", len(got))
	}
}
