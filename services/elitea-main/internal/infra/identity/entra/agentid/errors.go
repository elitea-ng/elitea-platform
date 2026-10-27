package agentid

import (
	"encoding/json"
	"errors"
	"slices"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity"
)

// Typed errors. None carries a token, an assertion or response text.
var (
	// ErrInteractionRequired: the user must interact (consent, MFA). It also
	// unwraps to *agentidentity.InteractionRequired, so the broker denies
	// with interaction_required and never falls back to app-only.
	ErrInteractionRequired = errors.New("entra agent id: user interaction required")
	// ErrClaimsChallenge: a CAE or Conditional Access claims challenge.
	ErrClaimsChallenge = errors.New("entra agent id: claims challenge")
	// ErrAudienceMismatch: the user evidence or assertion was not issued to
	// the blueprint (a middle-tier delegation, for example).
	ErrAudienceMismatch = errors.New("entra agent id: assertion audience is not the blueprint")
	// ErrIdentityDisabled: the agent identity has accountEnabled=false.
	ErrIdentityDisabled = errors.New("entra agent id: the agent identity is disabled")
	// ErrUnavailable: a transport failure, a throttle, a 5xx or an unusable
	// answer. The only retryable error.
	ErrUnavailable = errors.New("entra agent id: Entra is unavailable")
	// ErrRejected: any other refusal by Entra or Graph.
	ErrRejected = errors.New("entra agent id: Entra rejected the request")
	// ErrConsentNotGranted is AADSTS82014: an agent identity cannot consent
	// interactively. An administrator must pre-assign the grants. It is a
	// configuration error, not an interaction requirement.
	ErrConsentNotGranted = errors.New("entra agent id: grants must be pre-assigned to the agent identity by an administrator")
	// ErrScopeNotAllowed refuses a scope outside allowed_resource_scopes or
	// outside the session's audience, before any Entra call.
	ErrScopeNotAllowed = errors.New("entra agent id: scope not allowed")
	// ErrAssertedUserRefused: Entra Agent ID OBO takes verified user
	// evidence only (Capabilities().AcceptsAssertedUser is false).
	ErrAssertedUserRefused = errors.New("entra agent id: asserted user evidence is refused")
	// ErrIdentityLimit refuses Provision once the blueprint holds
	// identity_limit agent identities. Shard blueprints or create the
	// identity by delegated creation (see the operator documentation).
	ErrIdentityLimit = errors.New("entra agent id: the blueprint reached its agent identity limit")
	// ErrNoSponsor refuses Provision for a project with no sponsor group.
	ErrNoSponsor = errors.New("entra agent id: no sponsor group is configured for the project")
	// ErrTokenValidation: an issued token does not name the agent, the
	// blueprint, the tenant or the expected token type.
	ErrTokenValidation = errors.New("entra agent id: the issued token failed validation")
	// ErrInvalidRequest refuses a call before it leaves the process.
	ErrInvalidRequest = errors.New("entra agent id: invalid request")
	// ErrDelegationUnavailable: no delegation custody is configured, or the
	// session names no usable delegation.
	ErrDelegationUnavailable = errors.New("entra agent id: no delegated user assertion is available")
)

// AADSTS codes read from `error_codes`.
const (
	aadstsAudienceMismatch    = 50013
	aadstsAgentConsent        = 82014
	aadstsApplicationDisabled = 7000112
)

var interactionCodes = []int{50076, 50079, 65001, 50072, 50158}

func interaction(reason string, typed error) error {
	return errors.Join(typed, &agentidentity.InteractionRequired{Reason: reason})
}

// mapOAuthError maps by `error`, `suberror` and `error_codes`, never by the
// description.
func mapOAuthError(body []byte) error {
	var parsed struct {
		Error      string `json:"error"`
		SubError   string `json:"suberror"`
		ErrorCodes []int  `json:"error_codes"`
		Claims     string `json:"claims"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Error == "" {
		return ErrUnavailable
	}
	codes := parsed.ErrorCodes
	switch {
	case slices.Contains(codes, aadstsApplicationDisabled):
		return ErrIdentityDisabled
	case slices.Contains(codes, aadstsAgentConsent):
		return ErrConsentNotGranted
	case parsed.Claims != "":
		return interaction("claims_challenge", ErrClaimsChallenge)
	case slices.Contains(codes, aadstsAudienceMismatch):
		return ErrAudienceMismatch
	case parsed.Error == "interaction_required", parsed.Error == "consent_required", parsed.Error == "login_required",
		slices.ContainsFunc(codes, func(c int) bool { return slices.Contains(interactionCodes, c) }):
		return interaction("interaction_required", ErrInteractionRequired)
	case parsed.Error == "temporarily_unavailable", parsed.Error == "server_error":
		return ErrUnavailable
	}
	return ErrRejected
}
