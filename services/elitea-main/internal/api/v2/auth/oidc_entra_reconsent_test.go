package auth

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/oauth2"

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

// A base64 claims challenge (the WWW-Authenticate form) is read like raw
// JSON, padded or not, standard or URL alphabet.
func TestMergeEntraClaimsReadsBase64(t *testing.T) {
	challenge := `{"access_token":{"nbf":{"essential":true,"value":"1700000000"}}}`
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		merged, ok := mergeEntraClaims(identityoidc.EntraClientCapabilities, encoding.EncodeToString([]byte(challenge)))
		if !ok {
			t.Fatalf("refused a base64 challenge")
		}
		var got map[string]map[string]any
		if err := json.Unmarshal([]byte(merged), &got); err != nil || got["access_token"]["nbf"] == nil || got["access_token"]["xms_cc"] == nil {
			t.Fatalf("merged %s", merged)
		}
	}
	if _, ok := mergeEntraClaims(identityoidc.EntraClientCapabilities, base64.StdEncoding.EncodeToString([]byte(`["x"]`))); ok {
		t.Fatal("accepted base64 that is not an object of objects")
	}
}

// loginURL renders the options the way Login does.
func loginURL(t *testing.T, query url.Values, entra *entraRuntime) url.Values {
	t.Helper()
	cfg := oauth2.Config{ClientID: "c", Endpoint: oauth2.Endpoint{AuthURL: "https://idp.example/authorize"}}
	raw := cfg.AuthCodeURL("s", reconsentOptions(httptest.NewRequest("GET", "/auth/oidc/login?"+query.Encode(), nil), entra)...)
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Query()
}

// Only prompt=login or prompt=consent passes. Claims pass only in Entra
// mode. Claims that cannot be used fall back to prompt=login, so a
// re-consent never ends in a silent single sign-on.
func TestReconsentOptions(t *testing.T) {
	challenge := `{"access_token":{"acrs":{"essential":true}}}`
	for _, tc := range []struct {
		name         string
		query        url.Values
		entra        *entraRuntime
		prompt       string
		claimsIsSent bool
	}{
		{"no re-consent", url.Values{}, &entraRuntime{}, "", false},
		{"prompt none is dropped", url.Values{"prompt": {"none"}}, &entraRuntime{}, "", false},
		{"prompt login", url.Values{"prompt": {"login"}}, nil, "login", false},
		{"raw claims", url.Values{"claims": {challenge}}, &entraRuntime{}, "", true},
		{"base64 claims", url.Values{"claims": {base64.StdEncoding.EncodeToString([]byte(challenge))}}, &entraRuntime{}, "", true},
		{"claims and prompt", url.Values{"prompt": {"login"}, "claims": {challenge}}, &entraRuntime{}, "login", true},
		{"claims without Entra", url.Values{"claims": {challenge}}, nil, "login", false},
		{"claims that do not decode", url.Values{"claims": {"%%not-claims"}}, &entraRuntime{}, "login", false},
		{"claims too large", url.Values{"claims": {`{"a":{"b":"` + strings.Repeat("x", maxReconsentClaimsBytes) + `"}}`}}, &entraRuntime{}, "login", false},
		{"bad claims keep consent", url.Values{"prompt": {"consent"}, "claims": {"[]"}}, &entraRuntime{}, "consent", false},
		{"bad claims and prompt none", url.Values{"prompt": {"none"}, "claims": {"[]"}}, &entraRuntime{}, "login", false},
	} {
		got := loginURL(t, tc.query, tc.entra)
		if got.Get("prompt") != tc.prompt || (got.Get("claims") != "") != tc.claimsIsSent {
			t.Errorf("%s: prompt %q, claims %q", tc.name, got.Get("prompt"), got.Get("claims"))
		}
	}
}
