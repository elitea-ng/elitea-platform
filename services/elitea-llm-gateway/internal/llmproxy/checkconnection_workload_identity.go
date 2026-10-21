package llmproxy

import (
	"context"
	"net/http"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/workloadidentity"
)

// checkconnection_workload_identity.go — keyless PLATFORM credentials on the
// probe routes (POST /llm/v1/check_connection and /llm/v1/list_provider_models).
//
// The request path admits auth_mode=workload_identity only for a shared row of
// the public project, with GATEWAY_WORKLOAD_IDENTITY_ENABLED=true
// (internal/account/workload_identity.go). A probe has no row: it tests a
// payload. So the probe applies the same two conditions to what it CAN verify:
//
//  1. the operator flag, as the account reads it;
//  2. the SIGNED project of the request is the public project. elitea-main
//     signs the stored check of a platform provider as the public project, and
//     it refuses auth_mode=workload_identity on every tenant surface before a
//     probe is sent. The signed header is the one fact here that a caller
//     cannot choose; a body field saying "this is a platform credential" would
//     be one it can.
//
// Without (2), any project could probe with a keyless payload and an api_base
// of its choosing, and the gateway would mint a Google-signed ID token for that
// host — the platform's identity, handed to the tenant's server.
//
// The tokens come from ADC (workloadidentity.ADC). The ADC clients do not use
// the probe client: the probe client refuses link-local addresses, and on GKE
// the metadata server IS a link-local address. That refusal stays for every
// tenant-named destination; only the gateway's own identity fetch reaches the
// metadata server.
//
// The token is minted AFTER every destination passed the egress allowlist, so
// a refused destination never causes a token to exist.

// Reasons on the checkConnectionResponse contract.
const (
	checkConnectionReasonWorkloadIdentityNotAllowed = "workload_identity_not_allowed"
	checkConnectionReasonWorkloadIdentityToken      = "workload_identity_token_unavailable"
	checkConnectionReasonInvalidAuthMode            = "invalid_auth_mode"
)

// probeWorkloadIdentity is the Handler's view of the mode. The zero value
// refuses every keyless probe.
type probeWorkloadIdentity struct {
	enabled         bool
	publicProjectID string
	tokens          workloadidentity.Tokens
}

// admitProbeAuthMode decides the payload's auth mode BEFORE any target is
// resolved. It sets req.keyless for an admitted keyless payload and returns a
// field error for every refusal. A key-mode payload passes untouched.
func (h *Handler) admitProbeAuthMode(r *http.Request, req *checkConnectionRequest) *checkConnectionFieldError {
	mode, known := workloadidentity.ParseAuthMode(req.AuthMode)
	if !known {
		return &checkConnectionFieldError{
			reason: checkConnectionReasonInvalidAuthMode,
			detail: "auth_mode must be key or workload_identity",
		}
	}
	if mode == workloadidentity.AuthModeKey {
		return nil
	}
	switch req.Type {
	case "vertex_ai":
		if strings.TrimSpace(string(req.VertexCredentials)) != "" {
			return &checkConnectionFieldError{
				reason: checkConnectionReasonInvalidAuthMode,
				detail: "a workload_identity credential must not carry vertex_credentials",
			}
		}
	case "ai_dial":
		if strings.TrimSpace(req.APIKey) != "" {
			return &checkConnectionFieldError{
				reason: checkConnectionReasonInvalidAuthMode,
				detail: "a workload_identity credential must not carry api_key",
			}
		}
	default:
		return &checkConnectionFieldError{
			reason: checkConnectionReasonWorkloadIdentityNotAllowed,
			detail: "workload identity is supported for vertex_ai and ai_dial only",
		}
	}
	wi := h.workloadIdentity
	if !wi.enabled || wi.tokens == nil {
		return &checkConnectionFieldError{
			reason: checkConnectionReasonWorkloadIdentityNotAllowed,
			detail: "workload identity is not enabled on this gateway",
		}
	}
	if wi.publicProjectID == "" || identityFromHeaders(r.Header).projectID != wi.publicProjectID {
		return &checkConnectionFieldError{
			reason: checkConnectionReasonWorkloadIdentityNotAllowed,
			detail: "only a platform credential may use workload identity",
		}
	}
	req.keyless = true
	return nil
}

// mintProbeToken mints the token an admitted keyless payload authenticates
// with, and puts it on req.bearer. It is a no-op for a key-mode payload.
//
// The ADC error is not returned to the caller: it can name the metadata
// server's answer. The caller logs nothing more than the reason.
func (h *Handler) mintProbeToken(ctx context.Context, req *checkConnectionRequest) *checkConnectionFieldError {
	if !req.keyless {
		return nil
	}
	var (
		token string
		err   error
	)
	switch req.Type {
	case "vertex_ai":
		token, err = h.workloadIdentity.tokens.AccessToken(ctx)
	case "ai_dial":
		audience, aerr := workloadidentity.Audience(req.APIBase, req.TokenAudience)
		if aerr != nil {
			return &checkConnectionFieldError{reason: checkConnectionReasonMissingBase}
		}
		token, err = h.workloadIdentity.tokens.IDToken(ctx, audience)
	}
	if err != nil || strings.TrimSpace(token) == "" {
		h.logger.WarnContext(ctx, "check_connection: could not mint the workload-identity token",
			"type", req.Type, "reason", checkConnectionReasonWorkloadIdentityToken, "err", err)
		return &checkConnectionFieldError{
			reason: checkConnectionReasonWorkloadIdentityToken,
			detail: "the gateway could not obtain a token from its workload identity",
		}
	}
	req.bearer = token
	return nil
}

// azureAuthHeaders is the auth header of an Azure-shaped probe: the minted
// bearer for an admitted keyless DIAL payload, the api-key header otherwise.
// It mirrors bifrost's getAzureAuthHeaders, where a context token takes
// precedence over the key.
func azureAuthHeaders(req checkConnectionRequest) map[string]string {
	if req.bearer != "" {
		return map[string]string{"Authorization": "Bearer " + req.bearer}
	}
	return apiKeyHeader(req.APIKey)
}
