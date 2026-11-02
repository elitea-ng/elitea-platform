package auth

// Credential re-consent sign-in (ADR-0039, issue 1477). A governed run that
// paused with AUTHORIZATION_REQUIRED resumes after the user signs in again.
// POST /credential_consent/entra:start answers a login URL that can ask the
// identity provider to prompt again and, for an Entra claims challenge, to
// issue the claims the challenge names. Login reads both here.

import (
	"encoding/json"
	"net/http"

	"golang.org/x/oauth2"

	identityoidc "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/oidc"
)

// maxReconsentClaimsBytes bounds the claims challenge a login URL carries.
const maxReconsentClaimsBytes = 8 << 10

// reconsentOptions are the extra authorization parameters of a re-consent
// login. Only `prompt=login` or `prompt=consent` is passed. A `claims`
// value is passed only in Entra mode: it must be a JSON object of objects,
// and it is merged with the cp1 client capability, so the new token stays
// CAE-capable.
func reconsentOptions(r *http.Request, entra *entraRuntime) []oauth2.AuthCodeOption {
	var options []oauth2.AuthCodeOption
	query := r.URL.Query()
	if prompt := query.Get("prompt"); prompt == "login" || prompt == "consent" {
		options = append(options, oauth2.SetAuthURLParam("prompt", prompt))
	}
	if challenge := query.Get("claims"); challenge != "" && entra != nil {
		if merged, ok := mergeEntraClaims(identityoidc.EntraClientCapabilities, challenge); ok {
			options = append(options, oauth2.SetAuthURLParam("claims", merged))
		}
	}
	return options
}

// mergeEntraClaims merges challenge into base, one level deep: each token
// type keeps the claims of both, and a claim the challenge names wins.
func mergeEntraClaims(base, challenge string) (string, bool) {
	if len(challenge) > maxReconsentClaimsBytes {
		return "", false
	}
	var merged, extra map[string]map[string]json.RawMessage
	if json.Unmarshal([]byte(base), &merged) != nil || json.Unmarshal([]byte(challenge), &extra) != nil || len(extra) == 0 {
		return "", false
	}
	for token, claims := range extra {
		if claims == nil {
			return "", false
		}
		if merged[token] == nil {
			merged[token] = map[string]json.RawMessage{}
		}
		for name, value := range claims {
			merged[token][name] = value
		}
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		return "", false
	}
	return string(raw), true
}
