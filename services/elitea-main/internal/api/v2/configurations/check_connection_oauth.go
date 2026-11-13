package configurations

// check_connection_oauth.go answers a check of a DELEGATED (per-user OAuth)
// credential with the 401 the browser opens its sign-in window on (#1998).
//
// A SharePoint credential in delegated mode carries no secret: it names an
// identity provider (`oauth_discovery_endpoint`, "https://login.microsoftonline.com/{tenant}"),
// the scopes to ask for, and the signed-in user supplies the token. There is
// nothing for the server to test with, so a check can only say "this
// credential needs the user to authorise it" -- 401, `requires_authorization`,
// and `auth_metadata` holding the authorization-server metadata the browser's
// OAuth modal needs (shared/api/reauth-policy.ts resourceAuthorizationBody and
// features/toolkits/sharepoint/lib/hooks/useSharepointAuthModal.hooks.ts read
// exactly this shape). Without it the "Log in" button had nothing to open, and
// the type answered "not supported yet".
//
// The discovery document is fetched through the MCP authorization egress guard
// (ELITEA_MCP_OAUTH_EGRESS_ALLOWLIST) and internal/mcpoauth's walk, the same
// ones the MCP OAuth proxies use, because the endpoint is authored by the
// caller: an unguarded fetch would let a project member make this service read
// from an internal address.

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/egress"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/mcpoauth"
)

const (
	oauthDiscoveryEndpointField = "oauth_discovery_endpoint"
	oauthDiscoveryTimeout       = 5 * time.Second
)

// WithOAuthDiscoveryGuard supplies the egress guard the delegated-credential
// check reads its discovery document through. Without it the check refuses to
// fetch: it never falls back to an unguarded client.
func WithOAuthDiscoveryGuard(guard *egress.Guard) Option {
	return func(handler *Handler) {
		if guard == nil {
			return
		}
		transport := guard.Transport()
		client := &http.Client{
			Transport: transport,
			// Metadata documents never redirect; following one would read from
			// a host the guard was never asked about.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
		handler.oauthDiscovery = &mcpoauth.Fetcher{
			Do: client.Do, Validate: guard.Validate, PerFetch: oauthDiscoveryTimeout,
			IssuerMatch: microsoftTemplatedIssuer,
		}
	}
}

// delegatedOAuthEndpoint is the discovery endpoint a credential names, or ""
// when it names none (an app-only credential).
func delegatedOAuthEndpoint(data map[string]any) string {
	endpoint, _ := data[oauthDiscoveryEndpointField].(string)
	return strings.TrimSpace(endpoint)
}

// delegatedAuthorization answers one check of a delegated credential. It
// writes the response and reports true when the credential is delegated.
func (h *Handler) delegatedAuthorization(w http.ResponseWriter, r *http.Request, projectID string, data map[string]any) bool {
	endpoint := delegatedOAuthEndpoint(data)
	if endpoint == "" {
		return false
	}
	release := h.acquireDiscoveryProbe(r.Context(), projectID)
	if release == nil {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"success": false, "reason": "rate_limited",
			"message": "Rate limited: too many sign-in checks. Wait a minute, then try again.",
		})
		return true
	}
	defer release()
	metadata, err := h.discoverDelegatedIssuer(r.Context(), endpoint)
	if err != nil {
		slog.WarnContext(r.Context(), "check_connection: OAuth discovery failed", "err", err)
		writeDiscoveryFailed(w)
		return true
	}
	issuer, _ := metadata["issuer"].(string)
	if issuer == "" {
		slog.WarnContext(r.Context(), "check_connection: OAuth discovery document has no issuer")
		writeDiscoveryFailed(w)
		return true
	}
	resourceMetadata := map[string]any{
		"resource":                   endpoint,
		"authorization_servers":      []string{issuer},
		"oauth_authorization_server": metadata,
	}
	if scopes, ok := mcpoauth.BoundedMetadataList(data["scopes"]); ok {
		resourceMetadata["scopes_supported"] = scopes
	}
	writeJSON(w, http.StatusUnauthorized, map[string]any{
		"success":                false,
		"requires_authorization": true,
		"message":                "This credential signs in per user. Log in to authorise it.",
		"auth_metadata": map[string]any{
			"server_url":        endpoint,
			"resource_metadata": resourceMetadata,
		},
	})
	return true
}

// discoverDelegatedIssuer reads the authorization-server metadata of the
// identity provider a credential names. Microsoft's tenant URL
// ("…/{tenant}") publishes its v2.0 document under "…/{tenant}/v2.0" with that
// issuer, so the v2.0 form is tried first and the URL as written second.
func (h *Handler) discoverDelegatedIssuer(ctx context.Context, endpoint string) (map[string]any, error) {
	if h.oauthDiscovery == nil {
		return nil, mcpoauth.ErrDiscoveryMetadata
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, mcpoauth.ErrDiscoveryMetadata
	}
	trimmed := strings.TrimRight(endpoint, "/")
	candidates := []string{endpoint}
	if !strings.HasSuffix(parsed.Path, "/v2.0") && !strings.HasSuffix(parsed.Path, "/v2.0/") {
		candidates = []string{trimmed + "/v2.0", endpoint}
	}
	var lastErr error
	for _, candidate := range candidates {
		metadata, err := h.oauthDiscovery.AuthorizationServer(ctx, candidate)
		if err == nil {
			return metadata, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func writeDiscoveryFailed(w http.ResponseWriter) {
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"success": false,
		"message": "The OAuth discovery endpoint could not be read. It must be an https address that " +
			"publishes OAuth metadata and that the platform may reach " +
			"(an administrator can allow a private host with ELITEA_MCP_OAUTH_EGRESS_ALLOWLIST).",
		"reason": "oauth_discovery_failed",
	})
}

// microsoftTenantAliases are the multi-tenant authority names whose metadata
// publishes a templated issuer instead of the URL it was read from.
var microsoftTenantAliases = map[string]bool{"common": true, "organizations": true, "consumers": true}

var microsoftIdentityHosts = map[string]bool{
	"login.microsoftonline.com":        true,
	"login.microsoftonline.us":         true,
	"login.partner.microsoftonline.cn": true,
}

// microsoftTemplatedIssuer accepts the one non-identical issuer the Microsoft
// identity platform legitimately publishes: for the alias tenants (common,
// organizations, consumers) on a Microsoft login host the document carries
// "https://<same host>/{tenantid}/v2.0". Every other provider and every other
// path must match exactly.
func microsoftTemplatedIssuer(published, requested string) bool {
	req, err := url.Parse(requested)
	if err != nil || req.Scheme != "https" || !microsoftIdentityHosts[strings.ToLower(req.Host)] {
		return false
	}
	segments := strings.Split(strings.Trim(req.Path, "/"), "/")
	if len(segments) == 0 || !microsoftTenantAliases[strings.ToLower(segments[0])] {
		return false
	}
	if len(segments) > 2 || (len(segments) == 2 && segments[1] != "v2.0") {
		return false
	}
	return published == "https://"+req.Host+"/{tenantid}/v2.0"
}

// acquireDiscoveryProbe bounds the discovery fetches (up to two reads of 5 s
// each) per project and user with the model-test limiter, under its own key so
// the two kinds of test do not spend each other's tokens.
func (h *Handler) acquireDiscoveryProbe(ctx context.Context, projectID string) func() {
	if h.modelProbes == nil {
		return func() {}
	}
	user := "-"
	if caller, ok := currentConfigurationMutationAuthorID(ctx); ok {
		user = strconv.FormatInt(int64(caller), 10)
	}
	return h.modelProbes.acquire("oauth-discovery/" + projectID + "/" + user)
}
