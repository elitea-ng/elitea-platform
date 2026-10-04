package eliteacore

// The egress guard of the two MCP authorization proxies.
//
// `mcp_oauth_proxy` and `mcp_dcr_proxy` POST to a URL the caller sends in the
// body (`token_endpoint`, `registration_endpoint`). Since #6885 a project
// VIEWER may call them. validateMCPProxyURL checks only the URL's shape: it
// accepts any https host and http to loopback. Without a guard, a viewer could
// make this service POST to 127.0.0.1, to 169.254.169.254, to a cluster
// service or to any RFC1918 address, and read part of the answer back through
// the error relay.
//
// The guard closes that in two places:
//
//   - before the request, Validate resolves the host and refuses a private,
//     loopback, link-local, multicast or unspecified address with 400;
//   - at dial time, DialContext resolves the host AGAIN and dials only an
//     address it has just checked, so DNS rebinding between the two steps
//     cannot reach a refused address. A same-origin redirect goes through the
//     same dialer.
//
// The guard is webhook.DestinationGuard, which the outbound webhooks already
// use. Its operator allowlist (ELITEA_MCP_OAUTH_EGRESS_ALLOWLIST in the
// composition root) can admit private addresses for a private identity
// provider. Link-local and multicast stay refused whatever it says.
//
// The MCP tool SYNC keeps the unguarded client. It is gated on
// `models.applications.tool.patch` (editors and admins), and it must reach
// MCP servers on private networks.

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/url"
)

// MCPAuthorizationEgressGuard is the SSRF guard of the MCP authorization
// proxies. webhook.DestinationGuard implements it.
type MCPAuthorizationEgressGuard interface {
	// Validate refuses a URL whose host resolves to a forbidden address.
	Validate(ctx context.Context, rawURL string) error
	// DialContext resolves, checks and dials one connection.
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// WithMCPAuthorizationEgressGuard puts the two MCP authorization proxies
// behind an egress guard. The production router always passes one. A handler
// built without it keeps the old, unguarded behaviour, which only tests use.
func WithMCPAuthorizationEgressGuard(guard MCPAuthorizationEgressGuard) Option {
	return func(handler *Handler) {
		handler.mcpAuthorizationGuard = guard
	}
}

// mcpAuthorizationDestinationAllowed answers 400 with `code` and reports false
// when the guard refuses the endpoint.
func (h *Handler) mcpAuthorizationDestinationAllowed(
	w http.ResponseWriter, r *http.Request, endpoint *url.URL, code string,
) bool {
	if h.mcpAuthorizationGuard == nil {
		return true
	}
	if err := h.mcpAuthorizationGuard.Validate(r.Context(), endpoint.String()); err != nil {
		slog.WarnContext(r.Context(), "MCP authorization proxy: destination refused",
			"host", endpoint.Hostname(), "reason", err.Error())
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": code})
		return false
	}
	return true
}

// doMCPAuthorizationRequest is doMCPProxyRequest through the guarded dialer.
func (h *Handler) doMCPAuthorizationRequest(req *http.Request) (*http.Response, error) {
	if h.mcpAuthorizationGuard == nil {
		return h.doMCPProxyRequest(req)
	}
	return h.doMCPProxyRequestWith(h.guardedMCPAuthorizationClient(), req)
}

// guardedMCPAuthorizationClient copies the configured client and replaces its
// dialer with the guard's. A custom trust bundle on the configured transport
// is kept: only DialContext changes.
func (h *Handler) guardedMCPAuthorizationClient() *http.Client {
	h.guardedClientOnce.Do(func() {
		base := h.httpClient
		if base == nil {
			base = http.DefaultClient
		}
		var transport *http.Transport
		switch configured := base.Transport.(type) {
		case *http.Transport:
			transport = configured.Clone()
		default:
			// A nil transport is http.DefaultTransport. Any other
			// RoundTripper cannot take a dialer, so the default replaces it:
			// a guard that the transport could bypass guards nothing.
			transport = http.DefaultTransport.(*http.Transport).Clone()
		}
		transport.DialContext = h.mcpAuthorizationGuard.DialContext
		// A TLS dial must use the guarded TCP dial too.
		transport.DialTLSContext = nil
		client := *base
		client.Transport = transport
		h.guardedClient = &client
	})
	return h.guardedClient
}
