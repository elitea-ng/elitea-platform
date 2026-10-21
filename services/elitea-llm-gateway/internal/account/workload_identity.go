// workload_identity.go — keyless PLATFORM credentials for vertex_ai and
// ai_dial (internal/workloadidentity has the token side).
//
// # The rule
//
// A credential whose `data.auth_mode` is "workload_identity" authenticates with
// the gateway pod's own Google identity. That is the exact thing the rest of
// this package refuses for a tenant: an empty key that lets bifrost fall back to
// ambient identity is how a tenant credential borrows the platform's cloud
// account (issue #454 for Bedrock; the Azure and Vertex branches of buildKey
// carry the same rule). So the mode is admitted only when BOTH hold:
//
//  1. the operator armed it: GATEWAY_WORKLOAD_IDENTITY_ENABLED=true;
//  2. the ROW is a platform credential: it was read from the public project's
//     schema AND its own `shared` column is true.
//
// (2) is decided from the row, not from the bifrost Key ID. The `shared:` prefix
// is written only for a cross-project read, so when the caller IS the public
// project its own rows carry no prefix, and a public row that is NOT shared can
// also arrive through the model-owner path of loadCredentials. Neither the
// prefix nor its absence says what the row is; ownerProjectID and shared do.
//
// Every other combination is refused with WORKLOAD_IDENTITY_NOT_ALLOWED before
// any secret is resolved. It is never a silent fallback to the key mode. A
// refused row is skipped on an unlinked request, so one platform row's state
// does not take the provider's other credentials down; a request whose model
// links that row fails with the reason.
//
// # How the token reaches the provider
//
//   - vertex_ai: buildKey gives bifrost a VertexKeyConfig with an EMPTY
//     AuthCredentials. bifrost v1.7.15 providers/vertex getAuthTokenSource then
//     calls google.FindDefaultCredentials with the cloud-platform scope.
//   - ai_dial: the gateway mints a Google-signed ID token for the credential's
//     audience and puts it on the request context under
//     azure.AzureAuthorizationTokenKey. bifrost's getAzureAuthHeaders sends that
//     value as `Authorization: Bearer`, ahead of api-key and ahead of
//     DefaultAzureCredential, on EVERY Azure route — the deployment route, the
//     OpenAI Responses route and the Anthropic Messages route all build their
//     headers through that one function.
//
// The context token is per REQUEST, not per key. bifrost would send it with
// whichever Azure key it selects (every key is built with Models ["*"], so
// bifrost has no per-model filter that could keep them apart), and a request
// that offered a tenant's own key beside a workload-identity key could send the
// platform's ID token to the tenant's endpoint. The one statement of which
// credential serves a model is the model row's link (credential_selector.go):
// a linked request reaches this file with exactly one credential.
// bindDialIdentityToken therefore serves a workload-identity DIAL key only
// when it is the ONLY key of the request, and refuses an unlinked request that
// would offer it beside another key (WORKLOAD_IDENTITY_CREDENTIAL_AMBIGUOUS):
// dropping either side would be a guess about which endpoint serves the model.
// It clears the context token on every Azure resolution that does not serve
// a workload-identity key.
//
// If the token cannot be minted the request FAILS with
// WORKLOAD_IDENTITY_TOKEN_UNAVAILABLE. Letting it through with no token would
// reach the empty-key branch of getAzureAuthHeaders, which is
// DefaultAzureCredential — the fallback this whole file exists to avoid.

package account

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/maximhq/bifrost/core/providers/azure"
	"github.com/maximhq/bifrost/core/schemas"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/workloadidentity"
)

// WorkloadIdentityNotAllowedReason is the rejection reason for a
// workload-identity credential that the two conditions above do not admit.
const WorkloadIdentityNotAllowedReason = "WORKLOAD_IDENTITY_NOT_ALLOWED"

// ErrWorkloadIdentityNotAllowed carries WorkloadIdentityNotAllowedReason.
var ErrWorkloadIdentityNotAllowed = errors.New(WorkloadIdentityNotAllowedReason)

// WorkloadIdentityTokenUnavailableReason is the rejection reason when the
// gateway could not mint the ID token a workload-identity DIAL credential needs.
const WorkloadIdentityTokenUnavailableReason = "WORKLOAD_IDENTITY_TOKEN_UNAVAILABLE"

// ErrWorkloadIdentityTokenUnavailable carries
// WorkloadIdentityTokenUnavailableReason.
var ErrWorkloadIdentityTokenUnavailable = errors.New(WorkloadIdentityTokenUnavailableReason)

// WorkloadIdentityAmbiguousReason is the rejection reason for an unlinked
// Azure request whose credential set holds a workload-identity DIAL key beside
// another key: nothing says which one serves the requested model.
const WorkloadIdentityAmbiguousReason = "WORKLOAD_IDENTITY_CREDENTIAL_AMBIGUOUS"

// ErrWorkloadIdentityAmbiguous carries WorkloadIdentityAmbiguousReason.
var ErrWorkloadIdentityAmbiguous = errors.New(WorkloadIdentityAmbiguousReason)

// InvalidAuthModeReason is the rejection reason for a credential whose
// auth_mode is unknown, or that is ambiguous: a workload-identity row that also
// stores the key it claims not to use.
const InvalidAuthModeReason = "INVALID_CREDENTIAL_AUTH_MODE"

// ErrInvalidAuthMode carries InvalidAuthModeReason.
var ErrInvalidAuthMode = errors.New(InvalidAuthModeReason)

// WorkloadIdentityEnabled reports whether the operator armed the mode. The
// governance status route publishes it so the admin screen can say why a saved
// keyless provider is refused.
func (a *EliteaAccount) WorkloadIdentityEnabled() bool {
	return a != nil && a.workloadIdentityEnabled
}

// isPlatformCredential reports whether a row is a platform credential: read
// from the public project's schema, with its own `shared` column true.
func (a *EliteaAccount) isPlatformCredential(c credential) bool {
	return a.publicProjectID != "" && c.ownerProjectID == a.publicProjectID && c.shared
}

// admitWorkloadIdentity decides a credential's auth mode. It returns true for
// an admitted workload-identity credential, false for a key-mode one, and an
// error for every refusal. It reads row fields only and resolves no secret.
func (a *EliteaAccount) admitWorkloadIdentity(provider schemas.ModelProvider, c credential) (bool, error) {
	mode, known := workloadidentity.ParseAuthMode(c.authMode)
	if !known {
		return false, fmt.Errorf("%w: unknown auth_mode", ErrInvalidAuthMode)
	}
	if mode == workloadidentity.AuthModeKey {
		return false, nil
	}
	switch {
	case provider == schemas.Vertex && c.configType == "vertex_ai":
		// Ambiguity is refused, not resolved. A row that holds a service-account
		// document AND asks for the pod identity has two answers to "who calls
		// Google", and picking either one silently is a guess about billing.
		if c.vertexCredentialsRef != "" {
			return false, fmt.Errorf("%w: a workload_identity credential must not carry vertex_credentials", ErrInvalidAuthMode)
		}
	case provider == schemas.Azure && c.configType == DialCredentialType:
		if c.apiKeyRef != "" {
			return false, fmt.Errorf("%w: a workload_identity credential must not carry api_key", ErrInvalidAuthMode)
		}
	default:
		return false, fmt.Errorf("%w: auth_mode workload_identity is supported for vertex_ai and ai_dial only",
			ErrWorkloadIdentityNotAllowed)
	}
	if !a.workloadIdentityEnabled {
		return false, fmt.Errorf("%w: GATEWAY_WORKLOAD_IDENTITY_ENABLED is not true", ErrWorkloadIdentityNotAllowed)
	}
	if !a.isPlatformCredential(c) {
		return false, fmt.Errorf("%w: only a shared credential of the public project may use workload identity",
			ErrWorkloadIdentityNotAllowed)
	}
	return true, nil
}

// bindDialIdentityToken puts the ID token for the one workload-identity DIAL
// key on the request context.
//
// audiences maps the Key ID of every workload-identity DIAL key in keys to its
// audience. It is empty for a request that serves none, and then any token a
// previous resolution of this request left on the context is cleared: a
// fallback to another Azure credential must not inherit it.
func (a *EliteaAccount) bindDialIdentityToken(
	ctx context.Context, projectID string, keys []schemas.Key, audiences map[string]string,
) ([]schemas.Key, error) {
	bctx, _ := ctx.(*schemas.BifrostContext)
	if len(audiences) == 0 {
		clearDialIdentityToken(bctx)
		return keys, nil
	}

	if len(keys) > 1 {
		// Only an UNLINKED request gets here: a linked one was narrowed to the
		// one credential its model names. bifrost would pick among these keys
		// by its own rotation, and the context token would ride whichever it
		// picked (see the file header). Nothing says which credential serves
		// the requested model, so the request is refused rather than guessed.
		clearDialIdentityToken(bctx)
		a.logger.WarnContext(ctx, "rejected request: a workload-identity DIAL credential is offered beside other "+
			"credentials; link the model to one credential",
			"reason", WorkloadIdentityAmbiguousReason, "project_id", projectID, "keys", len(keys))
		return nil, fmt.Errorf("account: %w: link the model to one credential", ErrWorkloadIdentityAmbiguous)
	}

	key := keys[0]
	audience := audiences[key.ID]
	if bctx == nil {
		// Nothing to carry the token on. Sending the key without it reaches
		// DefaultAzureCredential, so this refuses.
		return nil, fmt.Errorf("account: credential %s: %w: no request context carries the token",
			key.ID, ErrWorkloadIdentityTokenUnavailable)
	}
	if a.identityTokens == nil {
		return nil, fmt.Errorf("account: credential %s: %w: no token source is configured",
			key.ID, ErrWorkloadIdentityTokenUnavailable)
	}
	token, err := a.identityTokens.IDToken(ctx, audience)
	if err != nil || strings.TrimSpace(token) == "" {
		// The error can name the metadata server's answer; it is logged here
		// and never returned to the caller, whose error names the reason only.
		a.logger.WarnContext(ctx, "rejected provider credential: could not mint the workload-identity ID token",
			"reason", WorkloadIdentityTokenUnavailableReason,
			"project_id", projectID,
			"key_id", key.ID,
			"err", err,
		)
		return nil, fmt.Errorf("account: credential %s: %w", key.ID, ErrWorkloadIdentityTokenUnavailable)
	}
	bctx.SetValue(azure.AzureAuthorizationTokenKey, token)
	return keys, nil
}

// clearDialIdentityToken removes a context token this package set earlier in
// the same request. It writes only when a value is there, so an ordinary
// request is left exactly as it was.
func clearDialIdentityToken(bctx *schemas.BifrostContext) {
	if bctx == nil {
		return
	}
	if v, _ := bctx.Value(azure.AzureAuthorizationTokenKey).(string); v != "" {
		bctx.SetValue(azure.AzureAuthorizationTokenKey, "")
	}
}
