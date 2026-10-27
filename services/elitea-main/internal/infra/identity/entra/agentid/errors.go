package agentid

import (
	"errors"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/entraoauth"
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
	// ErrModeNotSupported refuses Provision for any mode but dedicated_agent.
	ErrModeNotSupported = errors.New("entra agent id: only the dedicated_agent identity mode is provisioned")
)

func interaction(reason string, typed error) error {
	return errors.Join(typed, &agentidentity.InteractionRequired{Reason: reason})
}

// mapOAuthError maps the shared classification to this package's errors.
func mapOAuthError(body []byte) error {
	switch entraoauth.Classify(body).Kind {
	case entraoauth.KindApplicationDisabled:
		return ErrIdentityDisabled
	case entraoauth.KindAgentConsent:
		return ErrConsentNotGranted
	case entraoauth.KindClaimsChallenge:
		return interaction("claims_challenge", ErrClaimsChallenge)
	case entraoauth.KindAudienceMismatch:
		return ErrAudienceMismatch
	case entraoauth.KindInteractionRequired:
		return interaction("interaction_required", ErrInteractionRequired)
	case entraoauth.KindUnavailable:
		return ErrUnavailable
	}
	return ErrRejected
}
