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

	"github.com/elitea-ng/elitea-platform/libs/go/faults"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/egress"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/mcpoauth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftapps"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftauth"
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
		// The identity read of a token goes through the same guard, one host at
		// a time, and never follows a redirect.
		handler.microsoftProbe = func(request *http.Request) (*http.Response, error) {
			if err := guard.Validate(request.Context(), request.URL.String()); err != nil {
				return nil, err
			}
			return client.Do(request)
		}
		handler.oauthDiscovery = &mcpoauth.Fetcher{
			Do: client.Do, Validate: guard.Validate, PerFetch: oauthDiscoveryTimeout,
			IssuerMatch: microsoftTemplatedIssuer,
		}
	}
}

// accessTokenField is the signed-in user's access token, when the caller has
// one. With it a check reads an identity instead of asking for a sign-in.
const accessTokenField = "access_token"

// delegatedAuthorization answers one check of a delegated credential. It
// writes the response and reports true when the credential is delegated.
//
// What is delegated is decided by the Microsoft descriptors
// (internal/microsoftauth, ADR-0057): SharePoint in delegated mode, and the
// Outlook, Teams, Microsoft Graph and Dataverse types. Per descriptor the check
//
//   - refuses an authority that is not Entra (the new types), a bad Dataverse
//     host or tenant, and a discovery document that points the browser or the
//     token exchange at another host;
//   - with no token, answers the 401 `requires_authorization` the browser opens
//     its sign-in window on;
//   - with a token, reads an identity (Graph GET /me, Dataverse WhoAmI): 200 is
//     success, 401 asks for a sign-in again, anything else is reported.
func (h *Handler) delegatedAuthorization(w http.ResponseWriter, r *http.Request, projectID, configType string, data map[string]any) bool {
	descriptor, known := microsoftauth.For(configType)
	if !known {
		// Any other type that names a discovery endpoint (the OpenAPI
		// credential's OAuth2 mode) keeps the answer it always had.
		return h.genericDelegatedAuthorization(w, r, projectID, data)
	}
	if !descriptor.IsDelegated(data) {
		return false
	}
	var defaultApp *microsoftapps.Resolved
	if appID, _ := data[microsoftauth.FieldAppID].(string); descriptor.DefaultAppSupported() && strings.TrimSpace(appID) != "" {
		// A Default app (ADR-0057 decision 5): the client comes from the admin
		// registry, never from the credential, and an app that cannot be used is
		// refused with its stable code instead of falling back to another client.
		resolved, ok := h.resolveDefaultApp(w, r, projectID, configType, appID, data)
		if !ok {
			return true
		}
		defaultApp = &resolved
	}
	authority, err := descriptor.Authority(data)
	if defaultApp != nil {
		authority, err = defaultApp.Authority, nil
	}
	if err != nil {
		writeDelegatedRefused(w, err.Error())
		return true
	}
	audience, err := descriptor.AudienceURL(data)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "reason": ToolkitCheckReasonUnreachable, "message": "Dataverse: " + err.Error() + ".",
		})
		return true
	}
	signInScopes, dropped, err := descriptor.SignInScopes(data)
	storedScopes := data["scopes"]
	if defaultApp != nil {
		var effective []string
		signInScopes, effective, dropped, err = defaultAppScopes(descriptor, *defaultApp, configType, data)
		storedScopes = anyStrings(effective)
	}
	if err != nil {
		writeDelegatedRefused(w, "Scopes: "+err.Error()+".")
		return true
	}
	if len(dropped) > 0 {
		slog.WarnContext(r.Context(), "check_connection: scopes this credential type does not offer were ignored",
			"type", configType, "ignored", dropped)
	}
	if token, _ := data[accessTokenField].(string); strings.TrimSpace(token) != "" && descriptor.Probe() != microsoftauth.ProbeNone {
		if h.probeDelegatedIdentity(w, r, descriptor, data, strings.TrimSpace(token)) {
			return true
		}
	}
	return h.answerSignInRequest(w, r, projectID, signInRequest{
		authority: authority, descriptor: &descriptor, audience: audience, scopes: signInScopes,
		storedScopes: storedScopes,
	})
}

// anyStrings is a string list in the shape a stored credential value has.
func anyStrings(values []string) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

// genericDelegatedAuthorization is the check of a credential that names an
// OAuth discovery endpoint and is not a Microsoft type: 401, the metadata of
// that endpoint, and the scopes it lists, as before the descriptors.
func (h *Handler) genericDelegatedAuthorization(w http.ResponseWriter, r *http.Request, projectID string, data map[string]any) bool {
	endpoint, _ := data[oauthDiscoveryEndpointField].(string)
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return false
	}
	return h.answerSignInRequest(w, r, projectID, signInRequest{authority: endpoint, storedScopes: data["scopes"]})
}

// signInRequest is what the 401 answer is built from.
type signInRequest struct {
	authority string
	// descriptor is set for a Microsoft type: its audience, resource name and
	// sign-in scopes go in the metadata, and the authority must stay on Entra.
	descriptor *microsoftauth.Descriptor
	audience   string
	scopes     []string
	// storedScopes is the credential's own list, for a type without a descriptor
	// and for SharePoint, which has always passed it through.
	storedScopes any
}

// answerSignInRequest fetches the authority's discovery document through the
// egress guard and answers the 401 `requires_authorization` the browser opens
// its sign-in window on.
func (h *Handler) answerSignInRequest(w http.ResponseWriter, r *http.Request, projectID string, request signInRequest) bool {
	authority, descriptor := request.authority, request.descriptor
	release := h.acquireDiscoveryProbe(r.Context(), projectID)
	if release == nil {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"success": false, "reason": "rate_limited",
			"message": "Rate limited: too many sign-in checks. Wait a minute, then try again.",
		})
		return true
	}
	defer release()
	metadata, err := h.discoverDelegatedIssuer(r.Context(), authority)
	if err == nil {
		err = refuseForeignEndpoints(descriptor, authority, metadata)
	}
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
		"resource":                   authority,
		"authorization_servers":      []string{issuer},
		"oauth_authorization_server": metadata,
	}
	if descriptor == nil || descriptor.ScopeModel() == microsoftauth.ScopesAsStored {
		if scopes, ok := mcpoauth.BoundedMetadataList(request.storedScopes); ok {
			resourceMetadata["scopes_supported"] = scopes
		}
	} else {
		resourceMetadata["resource"] = request.audience
		resourceMetadata["resource_name"] = descriptor.ResourceName()
		resourceMetadata["scopes_supported"] = request.scopes
	}
	writeJSON(w, http.StatusUnauthorized, map[string]any{
		"success":                false,
		"requires_authorization": true,
		"message":                "This credential signs in per user. Log in to authorise it.",
		"auth_metadata": map[string]any{
			"server_url":        authority,
			"resource_metadata": resourceMetadata,
		},
	})
	return true
}

// probeDelegatedIdentity reads the identity of a token. It writes the answer
// and reports true, except for a rejected token (401), where it reports false
// so the caller answers with the sign-in request instead. The token goes to
// the descriptor's own host only: Graph, or the validated Dataverse org.
func (h *Handler) probeDelegatedIdentity(w http.ResponseWriter, r *http.Request, descriptor microsoftauth.Descriptor, data map[string]any, token string) bool {
	unreachable := func() bool {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "reason": ToolkitCheckReasonUnreachable, "message": toolkitCheckUnreachableMessage,
		})
		return true
	}
	request, err := descriptor.ProbeRequest(r.Context(), data, token)
	if err != nil || request == nil {
		return unreachable()
	}
	if h.microsoftProbe == nil {
		slog.WarnContext(r.Context(), "check_connection: no guarded client composed for the identity read", "type", descriptor.Type())
		return unreachable()
	}
	ctx, cancel := context.WithTimeout(r.Context(), oauthDiscoveryTimeout)
	defer cancel()
	response, err := h.microsoftProbe(request.WithContext(ctx))
	if err != nil {
		// Never log err: a transport error can carry the request URL.
		slog.WarnContext(r.Context(), "check_connection: identity read failed", "type", descriptor.Type())
		return unreachable()
	}
	defer func() { _ = response.Body.Close() }()
	switch microsoftauth.ClassifyProbe(response.StatusCode) {
	case microsoftauth.ProbeOK:
		writeJSON(w, http.StatusOK, map[string]any{
			"success": true, "reason": ToolkitCheckReasonOK, "message": toolkitCheckOKMessage,
		})
		return true
	case microsoftauth.ProbeUnauthorized:
		return false
	case microsoftauth.ProbeForbidden:
		// 403, not 401: the token is good and lacks the permission this type
		// needs. A failure that names it.
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "reason": ToolkitCheckReasonAuthFailed, "message": descriptor.ForbiddenMessage(data),
		})
		return true
	default:
		message := "Microsoft did not answer the check (HTTP " + strconv.Itoa(response.StatusCode) + "). Try again shortly."
		if response.StatusCode == http.StatusNotFound {
			if specific := descriptor.NotFoundMessage(data); specific != "" {
				message = specific
			}
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "reason": ToolkitCheckReasonUnreachable, "message": message,
		})
		return true
	}
}

// refuseForeignEndpoints refuses, for the types that accept an Entra authority
// only, a discovery document whose authorization endpoint leaves Entra. The
// browser sends the person to it, so a document a user could influence must not
// choose it. The token endpoint served back is always
// {authority}/oauth2/v2.0/token, never the document's own: the platform posts a
// client secret there (ADR-0057 decision 5).
func refuseForeignEndpoints(descriptor *microsoftauth.Descriptor, authority string, metadata map[string]any) error {
	if descriptor == nil || !descriptor.EntraOnly() {
		return nil
	}
	if err := microsoftauth.RequireEntraEndpoint(metadata["authorization_endpoint"]); err != nil {
		return err
	}
	metadata["token_endpoint"] = microsoftauth.TokenEndpoint(authority)
	return nil
}

func writeDelegatedRefused(w http.ResponseWriter, message string) {
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"success": false, "reason": string(faults.ToolkitOauthDiscoveryFailed), "message": message,
	})
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
		"reason": string(faults.ToolkitOauthDiscoveryFailed),
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
