package eliteacore

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/mcpoauth"
)

// mcpAuthorizationChallengeTimeout bounds the whole discovery: one protected
// resource document and the first authorization server, each also under the
// guarded source's own per-fetch timeout.
const mcpAuthorizationChallengeTimeout = 10 * time.Second

// MCPAuthorizationChallenge answers the public consent metadata of one saved
// MCP toolkit (FU-250). A fan-out sign-in card names only its toolkit; the
// endpoint comes from the stored row the actor can see, never from the
// request, and the request body is not read. Discovery runs through the
// guarded authorization source, and the answer carries no credential: only
// server_url, resource_metadata_url and the projected resource_metadata.
func (h *Handler) MCPAuthorizationChallenge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	toolkitID64, err := strconv.ParseInt(chi.URLParam(r, "toolkitID"), 10, 32)
	if err != nil || toolkitID64 <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_toolkit_id"})
		return
	}
	project, actor, err := delegatedAuthIdentity(r.Context(), chi.URLParam(r, "projectID"))
	if err != nil {
		var refused *mcpOAuthResolutionError
		if errors.As(err, &refused) {
			writeJSON(w, refused.status, map[string]any{"error": refused.code})
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "authentication_required"})
		return
	}
	if h.delegatedAuth == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "authorization_challenge_unavailable"})
		return
	}
	resolved, found, err := h.delegatedAuth.ResolveDelegatedAuthToolkitSettings(r.Context(), project, actor, int32(toolkitID64))
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "authorization_challenge_unavailable"})
		return
	}
	// Another project's toolkit and one that does not exist answer the same.
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "toolkit_not_found"})
		return
	}
	if resolved.ToolkitType != "mcp" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported_toolkit_type"})
		return
	}
	resource, err := mcpoauth.ToolkitResource(resolved.ToolkitType, resolved.Settings)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_toolkit_resource"})
		return
	}
	endpoint, err := validateMCPProxyURL(resource)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_toolkit_resource"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), mcpAuthorizationChallengeTimeout)
	defer cancel()
	discovered, err := h.authorizationMetadataSource().discover(ctx, endpoint, "", mcpSyncMaxServers, false, nil)
	if err != nil {
		code, status := "authorization_discovery_failed", http.StatusBadGateway
		if discovered.timedOut || errors.Is(err, mcpoauth.ErrDiscoveryTimeout) {
			code, status = "authorization_discovery_timeout", http.StatusGatewayTimeout
		}
		slog.WarnContext(r.Context(), "MCP authorization challenge: discovery failed",
			"project_id", project, "toolkit_id", toolkitID64, "kind", code)
		writeJSON(w, status, map[string]any{"error": code})
		return
	}
	named, _ := discovered.resource["resource"].(string)
	if canonical, err := mcpoauth.CanonicalResource(resolved.ToolkitType, named); err != nil || canonical != resource {
		slog.WarnContext(r.Context(), "MCP authorization challenge: metadata names another resource",
			"project_id", project, "toolkit_id", toolkitID64)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "authorization_resource_mismatch"})
		return
	}
	projected := map[string]any{"resource": resource, "authorization_servers": discovered.servers, "oauth_authorization_server": discovered.issuers[0]}
	if scopes, ok := boundedMCPMetadataList(discovered.resource["scopes_supported"]); ok {
		projected["scopes_supported"] = scopes
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"server_url": resource, "resource_metadata_url": discovered.metadataURL, "resource_metadata": projected,
	})
}
