package eliteacore

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/mcpoauth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/mcpregistry"
)

type mcpDiscoveryToken struct {
	AccessToken string `json:"access_token"`
	SessionID   string `json:"session_id"`
}

// A discovery preview uses the exact resource or its admitted prebuilt alias.
// Refresh tokens and other browser metadata never enter an MCP request.
// A configured Authorization header (a PAT) wins over a browser OAuth token,
// matching the agent runtime, so Load Tools and a run act as the same
// identity against the server (#6691, #6418).
func mcpDiscoveryHeaders(headers map[string]string, tokens map[string]mcpDiscoveryToken, endpoint, toolkitType string) map[string]string {
	result := make(map[string]string, len(headers)+2)
	configuredAuthorization := false
	for key, value := range headers {
		result[key] = value
		if strings.EqualFold(key, "Authorization") && strings.TrimSpace(value) != "" {
			configuredAuthorization = true
		}
	}
	if configuredAuthorization {
		return result
	}
	token, ok := tokens[endpoint]
	if !ok && mcpregistry.IsPrebuiltToolkitType(toolkitType) {
		token = tokens[toolkitType]
	}
	if token.AccessToken == "" {
		return result
	}
	for key := range result {
		if strings.EqualFold(key, "Authorization") || strings.EqualFold(key, "Mcp-Session-Id") {
			delete(result, key)
		}
	}
	result["Authorization"] = "Bearer " + token.AccessToken
	if token.SessionID != "" {
		result["Mcp-Session-Id"] = token.SessionID
	}
	return result
}

// mcpMetadataSource fetches PUBLIC authorization metadata through the one
// walk in internal/mcpoauth (discovery.go), which governed MCP shares. The
// tool sync reads through the handler's default client; the OAuth proxy
// reads through the authorization egress guard, with a per-fetch timeout,
// because its answer decides where a stored client secret may be sent.
type mcpMetadataSource struct {
	h        *Handler
	guarded  bool
	perFetch time.Duration
}

const (
	mcpDiscoveryFetchTimeout = 5 * time.Second
	mcpSyncMaxServers        = 16
	mcpBindingMaxServers     = 4
)

func (h *Handler) syncMetadataSource() mcpMetadataSource { return mcpMetadataSource{h: h} }

func (h *Handler) authorizationMetadataSource() mcpMetadataSource {
	return mcpMetadataSource{h: h, guarded: true, perFetch: mcpDiscoveryFetchTimeout}
}

func (s mcpMetadataSource) fetcher() mcpoauth.Fetcher {
	f := mcpoauth.Fetcher{Do: s.h.doMCPProxyRequest, PerFetch: s.perFetch}
	if s.guarded {
		f.Do = s.h.doMCPAuthorizationRequest
		if s.h.mcpAuthorizationGuard != nil {
			f.Validate = s.h.mcpAuthorizationGuard.Validate
		}
	}
	return f
}

// mcpDiscovered is mcpoauth.Discovered.
type mcpDiscovered struct {
	resource    map[string]any
	metadataURL string
	servers     []string
	issuers     []map[string]any
	timedOut    bool
}

// discover is mcpoauth.Fetcher.Discover through this source's client.
func (s mcpMetadataSource) discover(
	ctx context.Context, endpoint *url.URL, advertised string, maxServers int, all bool,
	stop func(issuer map[string]any) bool,
) (mcpDiscovered, error) {
	found, err := s.fetcher().Discover(ctx, endpoint, advertised, maxServers, all, stop)
	return mcpDiscovered{
		resource: found.Resource, metadataURL: found.MetadataURL, servers: found.Servers,
		issuers: found.Issuers, timedOut: found.TimedOut,
	}, err
}

func (s mcpMetadataSource) authorizationServer(ctx context.Context, rawIssuer string) (map[string]any, error) {
	return s.fetcher().AuthorizationServer(ctx, rawIssuer)
}

// Resolve public metadata without any invocation headers or credentials.
// Only fields used by the existing consent contract cross back to the browser.
func (h *Handler) mcpDiscoveryAuthorization(ctx context.Context, endpoint *url.URL, cause error) (map[string]any, error) {
	var required *mcpregistry.AuthorizationRequired
	if !errors.As(cause, &required) {
		return nil, cause
	}
	found, err := h.syncMetadataSource().discover(ctx, endpoint, required.ResourceMetadataURL(), mcpSyncMaxServers, false, nil)
	if err != nil {
		return nil, err
	}
	projected := map[string]any{"resource": endpoint.String(), "authorization_servers": found.servers, "oauth_authorization_server": found.issuers[0]}
	if scopes, ok := boundedMCPMetadataList(found.resource["scopes_supported"]); ok {
		projected["scopes_supported"] = scopes
	}
	return map[string]any{"server_url": endpoint.String(), "resource_metadata_url": found.metadataURL, "resource_metadata": projected}, nil
}

// mcpCatalogueOriginMatches reports whether a caller-chosen URL has the origin
// of a pre-built catalogue URL template. A catalogue authority never holds a
// placeholder (validatePrebuiltURLTemplate), so the origin of the template is
// the origin of every URL it materializes. An empty or invalid template
// matches nothing.
func mcpCatalogueOriginMatches(catalogueTemplate, callerURL string) bool {
	if strings.TrimSpace(catalogueTemplate) == "" {
		return false
	}
	probe := strings.NewReplacer("{", "", "}", "").Replace(catalogueTemplate)
	catalogue, err := url.Parse(probe)
	if err != nil || catalogue.Host == "" {
		return false
	}
	caller, err := validateMCPProxyURL(callerURL)
	if err != nil {
		return false
	}
	return sameMCPProxyOrigin(catalogue, caller)
}

func equivalentMCPMetadataURL(first, second string) bool {
	return mcpoauth.EquivalentMetadataURL(first, second)
}

func validateMCPMetadataURL(address string) (*url.URL, error) {
	return mcpoauth.ValidateMetadataURL(address)
}

func boundedMCPMetadataList(value any) ([]string, bool) {
	return mcpoauth.BoundedMetadataList(value)
}
