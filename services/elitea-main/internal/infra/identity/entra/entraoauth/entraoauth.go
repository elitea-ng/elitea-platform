// Package entraoauth holds the Microsoft identity platform token endpoint
// rules the Entra packages share: the OAuth error classification, the
// response checks and the unverified JWT payload decode. The obo (#1117)
// and agentid (#1152) packages map a Kind to their own typed errors.
//
// Classification reads the `error`, `suberror`, `error_codes` and `claims`
// fields only, never description text.
package entraoauth

import (
	"encoding/base64"
	"encoding/json"
	"mime"
	"net/http"
	"slices"
	"strings"
)

// MaxClaimsBytes bounds a claims challenge.
const MaxClaimsBytes = 8 << 10

// AADSTS codes the classification reads from `error_codes`.
const (
	AADSTSAudienceMismatch    = 50013
	AADSTSMFARequired         = 50076
	AADSTSMFAEnrollment       = 50079
	AADSTSConsentRequired     = 65001
	AADSTSStrongAuthRequired  = 50072
	AADSTSExternalChallenge   = 50158
	AADSTSAgentConsent        = 82014
	AADSTSApplicationDisabled = 7000112
)

var interactionCodes = []int{AADSTSMFARequired, AADSTSMFAEnrollment, AADSTSConsentRequired, AADSTSStrongAuthRequired, AADSTSExternalChallenge}

// Kind is the class of a token endpoint refusal.
type Kind int

const (
	// KindRejected is any other refusal: a deny.
	KindRejected Kind = iota
	// KindUnavailable is an unusable body, server_error or
	// temporarily_unavailable: the only retryable kind.
	KindUnavailable
	// KindApplicationDisabled is AADSTS7000112: the client is disabled.
	KindApplicationDisabled
	// KindAgentConsent is AADSTS82014: an agent identity cannot consent
	// interactively.
	KindAgentConsent
	// KindClaimsChallenge carries Classification.Claims.
	KindClaimsChallenge
	// KindAudienceMismatch is AADSTS50013.
	KindAudienceMismatch
	// KindInteractionRequired: the user must interact.
	KindInteractionRequired
)

// Classification is a classified refusal. Claims is set for
// KindClaimsChallenge only.
type Classification struct {
	Kind   Kind
	Claims string
}

// Classify classifies a 400/401/403 token endpoint body.
func Classify(body []byte) Classification {
	var parsed struct {
		Error      string `json:"error"`
		SubError   string `json:"suberror"`
		ErrorCodes []int  `json:"error_codes"`
		Claims     string `json:"claims"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Error == "" {
		return Classification{Kind: KindUnavailable}
	}
	codes := parsed.ErrorCodes
	switch {
	case slices.Contains(codes, AADSTSApplicationDisabled):
		return Classification{Kind: KindApplicationDisabled}
	case slices.Contains(codes, AADSTSAgentConsent):
		return Classification{Kind: KindAgentConsent}
	case parsed.Claims != "" && (parsed.Error == "interaction_required" || parsed.Error == "invalid_grant" ||
		parsed.Error == "insufficient_claims"):
		if len(parsed.Claims) > MaxClaimsBytes {
			return Classification{Kind: KindRejected}
		}
		return Classification{Kind: KindClaimsChallenge, Claims: parsed.Claims}
	case slices.Contains(codes, AADSTSAudienceMismatch):
		return Classification{Kind: KindAudienceMismatch}
	case parsed.Error == "interaction_required", parsed.Error == "consent_required", parsed.Error == "login_required":
		return Classification{Kind: KindInteractionRequired}
	case slices.ContainsFunc(codes, func(code int) bool { return slices.Contains(interactionCodes, code) }):
		return Classification{Kind: KindInteractionRequired}
	case parsed.Error == "invalid_grant":
		switch parsed.SubError {
		case "basic_action", "additional_action", "message_only", "consent_required", "token_expired", "user_password_expired":
			return Classification{Kind: KindInteractionRequired}
		}
		return Classification{Kind: KindRejected}
	case parsed.Error == "temporarily_unavailable", parsed.Error == "server_error":
		return Classification{Kind: KindUnavailable}
	}
	return Classification{Kind: KindRejected}
}

// JSONContentType reports whether the response is application/json.
func JSONContentType(header http.Header) bool {
	mediaType, _, err := mime.ParseMediaType(header.Get("Content-Type"))
	return err == nil && mediaType == "application/json"
}

// ExpiresInSeconds accepts the number or the numeric string Entra sends.
func ExpiresInSeconds(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var number int64
	if err := json.Unmarshal(raw, &number); err == nil {
		return number, number > 0
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return 0, false
	}
	if err := json.Unmarshal([]byte(text), &number); err != nil {
		return 0, false
	}
	return number, number > 0
}

// UnverifiedPayload decodes a JWT payload into out without checking the
// signature. Callers use it for defence-in-depth claim checks only.
func UnverifiedPayload(token string, out any) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	return json.Unmarshal(raw, out) == nil
}
