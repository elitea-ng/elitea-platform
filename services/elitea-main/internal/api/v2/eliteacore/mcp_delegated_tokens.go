package eliteacore

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpoauth"
	"github.com/go-chi/chi/v5"
)

// MCPDelegatedTokens saves verified grants. It exposes no browser redemption method.
type MCPDelegatedTokens interface {
	Save(context.Context, mcpoauth.TokenBinding, mcpoauth.AccessToken, time.Time) (mcpoauth.TokenReference, error)
}

func WithMCPDelegatedTokens(store MCPDelegatedTokens) Option {
	return func(h *Handler) { h.delegatedTokens = store }
}

func (h *Handler) writeMCPTokenResponse(w http.ResponseWriter, r *http.Request, body mcpOAuthProxyRequest, provider map[string]any) {
	result := oauthTokenResponse(provider)
	toolkitID, present, err := decodeOptionalPositiveInt32(body.ToolkitID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_toolkit_id"})
		return
	}
	if body.AuthorizationReferenceOnly && !present {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "toolkit_required"})
		return
	}
	if body.AuthorizationReferenceOnly && h.delegatedTokens == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "oauth_token_storage_unavailable"})
		return
	}
	// Unsaved toolkit exchanges retain their existing browser-only behavior.
	if !present || h.delegatedTokens == nil {
		writeJSON(w, http.StatusOK, result)
		return
	}
	project, actor, err := delegatedAuthIdentity(r.Context(), chi.URLParam(r, "projectID"))
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "authentication_required"})
		return
	}
	if h.delegatedAuth == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "oauth_credentials_unavailable"})
		return
	}
	resolved, found, err := h.delegatedAuth.ResolveDelegatedAuthToolkitSettings(r.Context(), project, actor, toolkitID)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "oauth_credentials_unavailable"})
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "toolkit_not_found"})
		return
	}
	if resolved.ToolkitType != "mcp" && resolved.ToolkitType != "openapi" {
		if body.AuthorizationReferenceOnly {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported_toolkit_type"})
			return
		}
		writeJSON(w, http.StatusOK, result)
		return
	}
	settings, err := h.resolvePrebuiltSettings(r.Context(), resolved.Settings, resolved.ToolkitType)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "oauth_credentials_unavailable"})
		return
	}
	resource, err := mcpoauth.ToolkitResource(resolved.ToolkitType, settings)
	submittedResource, submittedErr := mcpoauth.CanonicalResource(resolved.ToolkitType, body.Resource)
	// OpenAPI providers need not support OAuth resource indicators. Bind their
	// saved reference to the authorized toolkit without adding a provider parameter.
	if resolved.ToolkitType == "openapi" && body.Resource == "" {
		submittedResource, submittedErr = resource, nil
	}
	if err != nil || submittedErr != nil || resource != submittedResource {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "oauth_resource_mismatch"})
		return
	}
	expires, err := delegatedTokenExpiry(provider["expires_in"], time.Now())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "invalid_token_response"})
		return
	}
	access, _ := provider["access_token"].(string)
	session, _ := provider["session_id"].(string)
	tokenType, _ := provider["token_type"].(string)
	if tokenType == "" {
		tokenType = "Bearer"
	}
	ref, err := h.delegatedTokens.Save(r.Context(), mcpoauth.TokenBinding{ProjectID: project, ActorID: actor, ToolkitID: int64(toolkitID), Resource: resource}, mcpoauth.AccessToken{AccessToken: access, TokenType: tokenType, SessionID: session}, expires)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "oauth_token_storage_unavailable"})
		return
	}
	if body.AuthorizationReferenceOnly {
		result = make(map[string]any)
		for _, key := range []string{"scope", "token_type"} {
			if value, exists := provider[key]; exists {
				result[key] = value
			}
		}
	}
	result["authorization_resource"] = resource
	result["authorization_reference"] = ref.Reference
	result["authorization_revision"] = ref.Revision
	result["authorization_expires_at"] = ref.ExpiresAt.UTC().Format(time.RFC3339Nano)
	writeJSON(w, http.StatusOK, result)
}

// Missing expiry gets a five-minute local ceiling. Provider expiry is capped at one day.
func delegatedTokenExpiry(value any, now time.Time) (time.Time, error) {
	if value == nil {
		return now.Add(5 * time.Minute), nil
	}
	var text string
	switch value := value.(type) {
	case json.Number:
		text = value.String()
	case string:
		text = value
	default:
		return time.Time{}, errMCPProxyInvalidRequest
	}
	seconds, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	if err != nil || seconds <= 0 {
		return time.Time{}, errMCPProxyInvalidRequest
	}
	if seconds > 86400 {
		seconds = 86400
	}
	return now.Add(time.Duration(seconds) * time.Second), nil
}
