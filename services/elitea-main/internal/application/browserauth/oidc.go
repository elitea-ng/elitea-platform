package browserauth

import (
	"context"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browserflow"
)

const (
	OIDCPKCEChallengeS256 = "S256"

	OIDCAuthorizationGET  OIDCAuthorizationTransport = "get"
	OIDCAuthorizationPOST OIDCAuthorizationTransport = "post"
)

// OIDCAuthorizationTransport is an explicit wire decision. The current
// legacy auth default is POST; GET remains a configured compatibility mode.
type OIDCAuthorizationTransport string

// OIDCAuthorization is generated before server-side state is allocated. Nonce
// and PKCE verifier are then persisted only in the one-time transaction.
type OIDCAuthorization struct {
	Correlation         browserflow.ProtocolCorrelation
	ProviderState       browserflow.ProviderState
	PKCEChallengeMethod string
}

// OIDCAuthorizationRequest is the closed browser-to-provider initiation
// contract. Fixed fields prevent duplicate or silently injected parameters at
// the HTTP boundary while retaining the current five business fields and the
// target nonce/PKCE security fields.
type OIDCAuthorizationRequest struct {
	Transport           OIDCAuthorizationTransport
	Endpoint            string
	ResponseType        string
	ClientID            string
	RedirectURI         string
	Scope               string
	State               string
	Nonce               string
	CodeChallenge       string
	CodeChallengeMethod string
}

// OIDCProtocol is the source-only composition seam for OpenID Connect. A
// production implementation generates CSPRNG values, uses S256 PKCE, exchanges
// only an authorization code, and verifies the ID token's signature, issuer,
// audience, expiry, not-before value, and nonce. Provider dependency failures
// wrap ErrAssertionVerifierUnavailable; invalid codes, tokens, or claims do
// not.
type OIDCProtocol interface {
	NewAuthorization(context.Context) (OIDCAuthorization, error)
	AuthorizationRequest(string, OIDCAuthorization) (OIDCAuthorizationRequest, error)
	NewVerifier(code string) AssertionVerifier
}
