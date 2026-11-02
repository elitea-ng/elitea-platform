package auth

// Credential re-consent sign-in (ADR-0039, issue 1477). A governed run that
// paused with AUTHORIZATION_REQUIRED resumes after the user signs in again.
// POST /credential_consent/entra:start answers a login URL that can ask the
// identity provider to prompt again and, for an Entra claims challenge, to
// issue the claims the challenge names. Login reads both here.

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	"golang.org/x/oauth2"

	identityoidc "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/oidc"
)

// maxReconsentClaimsBytes bounds the claims challenge a login URL carries.
const maxReconsentClaimsBytes = 8 << 10

// reconsentOptions are the extra authorization parameters of a re-consent
// login. Only `prompt=login` or `prompt=consent` is passed. A `claims`
// value is passed only in Entra mode: it must be a JSON object of objects,
// as raw JSON or base64 (the WWW-Authenticate form), and it is merged with
// the cp1 client capability, so the new token stays CAE-capable.
//
// A re-consent must never end in a silent single sign-on, because the
// assertion that failed would come back. So claims that cannot be used
// (not Entra mode, or a value that does not decode) fall back to
// `prompt=login`.
func reconsentOptions(r *http.Request, entra *entraRuntime) []oauth2.AuthCodeOption {
	var options []oauth2.AuthCodeOption
	query := r.URL.Query()
	prompt := query.Get("prompt")
	if prompt != "login" && prompt != "consent" {
		prompt = ""
	}
	if challenge := query.Get("claims"); challenge != "" {
		merged, ok := "", false
		if entra != nil {
			merged, ok = mergeEntraClaims(identityoidc.EntraClientCapabilities, challenge)
		}
		if ok {
			options = append(options, oauth2.SetAuthURLParam("claims", merged))
		} else if prompt == "" {
			prompt = "login"
		}
	}
	if prompt != "" {
		options = append(options, oauth2.SetAuthURLParam("prompt", prompt))
	}
	return options
}

// decodeEntraClaims reads a claims challenge as a JSON object of objects.
// Entra sends it as raw JSON in a token error, and base64 encoded in a
// WWW-Authenticate header; both forms are accepted.
func decodeEntraClaims(challenge string) (map[string]map[string]json.RawMessage, bool) {
	var claims map[string]map[string]json.RawMessage
	if json.Unmarshal([]byte(challenge), &claims) == nil {
		return claims, true
	}
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		raw, err := encoding.DecodeString(strings.TrimSpace(challenge))
		if err != nil {
			continue
		}
		claims = nil
		if json.Unmarshal(raw, &claims) == nil {
			return claims, true
		}
	}
	return nil, false
}

// mergeEntraClaims merges challenge into base, one level deep: each token
// type keeps the claims of both, and a claim the challenge names wins.
func mergeEntraClaims(base, challenge string) (string, bool) {
	if len(challenge) > maxReconsentClaimsBytes {
		return "", false
	}
	var merged map[string]map[string]json.RawMessage
	if json.Unmarshal([]byte(base), &merged) != nil {
		return "", false
	}
	extra, ok := decodeEntraClaims(challenge)
	if !ok || len(extra) == 0 {
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
