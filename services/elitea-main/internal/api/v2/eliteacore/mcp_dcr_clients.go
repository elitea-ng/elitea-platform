package eliteacore

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpoauth"
	"github.com/go-chi/chi/v5"
)

// MCPDCRClients owns registered client secrets, not browser authorization grants.
type MCPDCRClients interface {
	Save(context.Context, mcpoauth.Binding, string, time.Time) (string, error)
	Load(context.Context, string, mcpoauth.Binding) (string, error)
}

func WithMCPDCRClients(clients MCPDCRClients) Option {
	return func(h *Handler) { h.dcrClients = clients }
}

func (h *Handler) writeMCPDCRClient(w http.ResponseWriter, r *http.Request, body mcpDCRProxyRequest, provider map[string]any) {
	// Return only registration metadata. Registration access tokens and vendor
	// extensions can contain secrets and do not belong in the browser.
	result := make(map[string]any)
	for _, field := range []string{"client_id", "client_id_issued_at", "client_secret_expires_at", "token_endpoint_auth_method", "redirect_uris", "grant_types", "response_types", "scope"} {
		if value, exists := provider[field]; exists {
			result[field] = value
		}
	}
	secret, _ := provider["client_secret"].(string)
	if secret == "" {
		writeJSON(w, http.StatusOK, result)
		return
	}
	project, actor, err := delegatedAuthIdentity(r.Context(), chi.URLParam(r, "projectID"))
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "authentication_required"})
		return
	}
	if body.TokenEndpoint == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "dcr_token_endpoint_required"})
		return
	}
	if h.dcrClients == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "dcr_client_storage_unavailable"})
		return
	}
	expires, err := dcrClientSecretExpiry(provider["client_secret_expires_at"])
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "invalid_dcr_response"})
		return
	}
	clientID, _ := provider["client_id"].(string)
	reference, err := h.dcrClients.Save(r.Context(), mcpoauth.Binding{
		ProjectID: project, ActorID: actor, ClientID: clientID,
		TokenEndpoint: body.TokenEndpoint, Resource: body.Resource,
	}, secret, expires)
	if err != nil {
		slog.ErrorContext(r.Context(), "MCP DCR client persistence failed")
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "dcr_client_storage_unavailable"})
		return
	}
	result["client_reference"] = reference
	writeJSON(w, http.StatusOK, result)
}

func dcrClientSecretExpiry(value any) (time.Time, error) {
	if value == nil {
		return time.Time{}, nil
	}
	number, ok := value.(json.Number)
	if !ok {
		return time.Time{}, errMCPProxyInvalidRequest
	}
	seconds, err := number.Int64()
	if err != nil || seconds < 0 {
		return time.Time{}, errMCPProxyInvalidRequest
	}
	if seconds == 0 {
		return time.Time{}, nil
	}
	expires := time.Unix(seconds, 0)
	if !expires.After(time.Now()) || expires.Year() > 9999 {
		return time.Time{}, errMCPProxyInvalidRequest
	}
	return expires, nil
}

func (h *Handler) loadMCPDCRCredentials(ctx context.Context, projectText string, endpoint *url.URL, body mcpOAuthProxyRequest) (mcpOAuthCredentials, error) {
	// A reference and a raw secret cannot select competing identities.
	if !body.UsedDCR || body.ClientSecret != "" || body.ClientID == "" {
		return mcpOAuthCredentials{}, &mcpOAuthResolutionError{http.StatusBadRequest, "invalid_dcr_client_reference"}
	}
	project, actor, err := delegatedAuthIdentity(ctx, projectText)
	if err != nil {
		return mcpOAuthCredentials{}, err
	}
	if h.dcrClients == nil {
		return mcpOAuthCredentials{}, &mcpOAuthResolutionError{http.StatusServiceUnavailable, "dcr_client_storage_unavailable"}
	}
	secret, err := h.dcrClients.Load(ctx, body.ClientReference, mcpoauth.Binding{
		ProjectID: project, ActorID: actor, ClientID: body.ClientID,
		TokenEndpoint: endpoint.String(), Resource: body.Resource,
	})
	if errors.Is(err, mcpoauth.ErrClientUnavailable) {
		return mcpOAuthCredentials{}, &mcpOAuthResolutionError{http.StatusBadRequest, "invalid_client"}
	}
	if err != nil {
		slog.ErrorContext(ctx, "MCP DCR client lookup failed")
		return mcpOAuthCredentials{}, &mcpOAuthResolutionError{http.StatusServiceUnavailable, "dcr_client_storage_unavailable"}
	}
	return mcpOAuthCredentials{clientID: body.ClientID, clientSecret: secret, scope: body.Scope}, nil
}
