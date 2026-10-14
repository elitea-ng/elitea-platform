package eliteacore

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpregistry"
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

var (
	errMCPDiscoveryMetadata = errors.New("MCP authorization metadata is unavailable")
	errMCPDiscoveryTimeout  = errors.New("MCP authorization metadata timed out")
)

// mcpMetadataSource fetches PUBLIC authorization metadata. The tool sync reads
// through the handler's default client; the OAuth proxy reads through the
// authorization egress guard, with a per-fetch timeout, because its answer
// decides where a stored client secret may be sent.
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

// mcpDiscovered is a fail-closed walk of protected-resource then
// authorization-server metadata.
type mcpDiscovered struct {
	resource    map[string]any
	metadataURL string
	servers     []string
	// issuers holds the validated metadata of the servers that answered, in
	// order. With all=false it holds only the first server's.
	issuers []map[string]any
	// timedOut reports that at least one fetch ran out of time. A server that
	// did not answer is skipped, but a refusal that followed a timeout is not
	// the same thing as one that followed a mismatch.
	timedOut bool
}

// discover is the one implementation of the walk, shared by the tool sync and
// the OAuth proxy's token-endpoint binding. Every document must validate: a
// server list that is empty, over maxServers or holds an issuer with a query is
// refused outright, and so is a first issuer that does not answer. With
// all=true an issuer that does not answer is skipped, and at least one must.
func (s mcpMetadataSource) discover(
	ctx context.Context, endpoint *url.URL, advertised string, maxServers int, all bool,
	stop func(issuer map[string]any) bool,
) (mcpDiscovered, error) {
	resource, metadataURL, err := s.protected(ctx, endpoint, advertised)
	if err != nil {
		return mcpDiscovered{timedOut: errors.Is(err, errMCPDiscoveryTimeout)}, err
	}
	servers, ok := boundedMCPMetadataList(resource["authorization_servers"])
	if !ok || len(servers) == 0 || len(servers) > maxServers {
		return mcpDiscovered{}, errMCPDiscoveryMetadata
	}
	for _, server := range servers {
		issuer, err := validateMCPMetadataURL(server)
		if err != nil || issuer.RawQuery != "" {
			return mcpDiscovered{}, errMCPDiscoveryMetadata
		}
	}
	found := mcpDiscovered{resource: resource, metadataURL: metadataURL, servers: servers}
	for _, server := range servers {
		metadata, err := s.authorizationServer(ctx, server)
		if err == nil {
			found.issuers = append(found.issuers, metadata)
			// The caller has what it came for: later servers, which may be
			// slow or dead, must not spend the budget the exchange needs.
			if stop != nil && stop(metadata) {
				break
			}
		} else if !all {
			return mcpDiscovered{timedOut: errors.Is(err, errMCPDiscoveryTimeout)}, err
		} else if errors.Is(err, errMCPDiscoveryTimeout) {
			found.timedOut = true
			if ctx.Err() != nil {
				break
			}
		}
		if !all {
			break
		}
	}
	if len(found.issuers) == 0 {
		err := errMCPDiscoveryMetadata
		if found.timedOut {
			err = errMCPDiscoveryTimeout
		}
		return found, err
	}
	return found, nil
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

func (s mcpMetadataSource) protected(ctx context.Context, endpoint *url.URL, advertised string) (map[string]any, string, error) {
	candidates := []string{advertised}
	if advertised == "" {
		base := endpoint.Scheme + "://" + endpoint.Host
		candidates = []string{base + "/.well-known/oauth-protected-resource" + endpoint.EscapedPath(), base + "/.well-known/oauth-protected-resource"}
	}
	for _, candidate := range candidates {
		metadata, err := s.read(ctx, candidate)
		if errors.Is(err, errMCPDiscoveryTimeout) {
			return nil, "", err
		}
		if err != nil {
			continue
		}
		resource, ok := metadata["resource"].(string)
		if !ok || !equivalentMCPMetadataURL(resource, endpoint.String()) {
			return nil, "", errMCPDiscoveryMetadata
		}
		return metadata, candidate, nil
	}
	return nil, "", errMCPDiscoveryMetadata
}

func (s mcpMetadataSource) authorizationServer(ctx context.Context, rawIssuer string) (map[string]any, error) {
	issuer, err := validateMCPMetadataURL(rawIssuer)
	if err != nil {
		return nil, err
	}
	// RFC 8414 inserts the well-known segment between host and path; a trailing
	// slash on the issuer must not leak into the built URL ("…/server/" or "//").
	base := issuer.Scheme + "://" + issuer.Host
	path := strings.TrimRight(issuer.EscapedPath(), "/")
	candidates := []string{base + "/.well-known/oauth-authorization-server" + path, base + path + "/.well-known/openid-configuration"}
	for _, candidate := range candidates {
		metadata, err := s.read(ctx, candidate)
		if errors.Is(err, errMCPDiscoveryTimeout) {
			return nil, err
		}
		if err != nil {
			continue
		}
		// RFC 8414 §3.3 asks for an identical issuer, but providers differ by
		// a trailing slash (Box advertises "https://api.box.com/" and publishes
		// "https://api.box.com"). That is the same URL, so it is accepted. A
		// real mismatch moves on to the next candidate document.
		published, ok := metadata["issuer"].(string)
		if !ok || !equivalentMCPMetadataURL(published, rawIssuer) {
			continue
		}
		projected := map[string]any{"issuer": rawIssuer}
		for _, key := range []string{"authorization_endpoint", "token_endpoint", "registration_endpoint", "revocation_endpoint"} {
			value, exists := metadata[key]
			if !exists && key != "authorization_endpoint" && key != "token_endpoint" {
				continue
			}
			address, ok := value.(string)
			if !ok {
				return nil, errMCPDiscoveryMetadata
			}
			if _, err := validateMCPMetadataURL(address); err != nil {
				return nil, err
			}
			projected[key] = address
		}
		for _, key := range []string{"scopes_supported", "grant_types_supported", "response_types_supported", "code_challenge_methods_supported", "token_endpoint_auth_methods_supported"} {
			if values, ok := boundedMCPMetadataList(metadata[key]); ok {
				projected[key] = values
			}
		}
		return projected, nil
	}
	return nil, errMCPDiscoveryMetadata
}

func (s mcpMetadataSource) read(ctx context.Context, address string) (map[string]any, error) {
	endpoint, err := validateMCPMetadataURL(address)
	if err != nil {
		return nil, err
	}
	if s.perFetch > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.perFetch)
		defer cancel()
	}
	if s.guarded && s.h.mcpAuthorizationGuard != nil {
		if err := s.h.mcpAuthorizationGuard.Validate(ctx, endpoint.String()); err != nil {
			return nil, errMCPDiscoveryMetadata
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, errMCPDiscoveryMetadata
	}
	request.Header.Set("Accept", "application/json")
	var response *http.Response
	if s.guarded {
		response, err = s.h.doMCPAuthorizationRequest(request)
	} else {
		response, err = s.h.doMCPProxyRequest(request)
	}
	if err != nil {
		var netErr net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
			return nil, errMCPDiscoveryTimeout
		}
		return nil, errMCPDiscoveryMetadata
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, errMCPDiscoveryMetadata
	}
	const limit = 64 << 10
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || len(body) > limit {
		return nil, errMCPDiscoveryMetadata
	}
	var metadata map[string]any
	if err := json.Unmarshal(body, &metadata); err != nil || metadata == nil {
		return nil, errMCPDiscoveryMetadata
	}
	return metadata, nil
}

// equivalentMCPMetadataURL compares two metadata URLs (an issuer or a
// protected resource) as the same resource identifier: scheme, host and port
// must match exactly (case-insensitive, default ports made explicit), the
// query must match, and the paths must match once trailing slashes are
// removed, so an empty path equals "/".
func equivalentMCPMetadataURL(first, second string) bool {
	if first == second {
		return true
	}
	a, err := url.Parse(first)
	if err != nil {
		return false
	}
	b, err := url.Parse(second)
	if err != nil {
		return false
	}
	if a.Host == "" || b.Host == "" || a.User != nil || b.User != nil || a.Opaque != "" || b.Opaque != "" {
		return false
	}
	return sameMCPProxyOrigin(a, b) &&
		strings.TrimRight(a.EscapedPath(), "/") == strings.TrimRight(b.EscapedPath(), "/") &&
		a.RawQuery == b.RawQuery
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

func validateMCPMetadataURL(address string) (*url.URL, error) {
	if len(address) > 4096 || strings.Contains(address, "#") {
		return nil, errMCPDiscoveryMetadata
	}
	endpoint, err := validateMCPProxyURL(address)
	if err != nil {
		return nil, errMCPDiscoveryMetadata
	}
	return endpoint, nil
}

func boundedMCPMetadataList(value any) ([]string, bool) {
	items, ok := value.([]any)
	if !ok || len(items) > 64 {
		return nil, false
	}
	values := make([]string, 0, len(items))
	total := 0
	for _, item := range items {
		text, ok := item.(string)
		total += len(text)
		if !ok || text == "" || total > 4096 || strings.ContainsAny(text, "\r\n\x00") {
			return nil, false
		}
		values = append(values, text)
	}
	return values, true
}
