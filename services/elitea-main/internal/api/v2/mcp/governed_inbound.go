package mcp

// Governed inbound MCP (ADR-0039, issue #1123): with credential governance
// on and an inbound issuer configured, the MCP endpoint is an OAuth
// protected resource in the MCP authorization specification's sense.
//
//   - GET /.well-known/oauth-protected-resource/app/{projectID}/mcp[/...]
//     answers the RFC 9728 metadata of that endpoint.
//   - A 401 from the endpoint carries
//     `WWW-Authenticate: Bearer resource_metadata="<metadata URL>"`.
//   - A bearer JWT from an allow-listed issuer is verified by OAuthVerifier
//     against the endpoint's own resource URI and resolved to an Elitea user.
//     Platform credentials (session, PAT, API key) still authenticate unless
//     the deployment disables them for MCP (gate D8).
//   - The inbound token is consumed here: it is removed from the request
//     before any handler runs, so it never reaches an input bundle, a worker
//     request or a log. The run's security context records the caller
//     (authentication source "token", calling application the OAuth client);
//     the inbound token never becomes the agent's identity.
//
// The composition root builds GovernedInbound only in the governed profile;
// with it nil the routes, responses and authentication are unchanged.

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// ProtectedResourceMetadataPrefix is the RFC 9728 well-known prefix; the
// endpoint's path follows it.
const ProtectedResourceMetadataPrefix = "/.well-known/oauth-protected-resource"

// CallingApplicationPrefix prefixes the OAuth client id the security
// context records for an inbound MCP token.
const CallingApplicationPrefix = "mcp_client:"

var mcpEndpointPath = regexp.MustCompile(`^/app/[1-9][0-9]{0,18}/mcp(/[A-Za-z0-9._~%/-]*)?$`)

// InboundUserResolver resolves a verified inbound token to an active Elitea
// user. An unknown or suspended user is auth.ErrPrincipalInactive; any
// other error means the store did not answer.
type InboundUserResolver interface {
	ResolveInboundUser(r *http.Request, token InboundToken) (auth.User, error)
}

// GovernedInboundConfig configures the governed MCP resource server.
type GovernedInboundConfig struct {
	// PublicOrigin is the deployment origin the resource URIs are built
	// on (DEPLOYMENT_URL); required, since an audience cannot follow Host.
	PublicOrigin string
	Verifier     *OAuthVerifier
	Users        InboundUserResolver
	// OAuthOnly refuses platform credentials on the MCP endpoint (gate D8).
	OAuthOnly bool
	Logger    *slog.Logger
}

// GovernedInbound is the governed MCP resource server.
type GovernedInbound struct {
	origin    string
	verifier  *OAuthVerifier
	users     InboundUserResolver
	oauthOnly bool
	logger    *slog.Logger
}

// NewGovernedInbound validates cfg.
func NewGovernedInbound(cfg GovernedInboundConfig) (*GovernedInbound, error) {
	origin := strings.TrimSuffix(cfg.PublicOrigin, "/")
	if !strings.HasPrefix(origin, "https://") && !strings.HasPrefix(origin, "http://localhost") {
		return nil, errors.New("mcp: governed inbound MCP needs an https public origin (DEPLOYMENT_URL)")
	}
	if cfg.Verifier == nil || cfg.Users == nil {
		return nil, errors.New("mcp: governed inbound MCP needs a verifier and a user resolver")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &GovernedInbound{origin: origin, verifier: cfg.Verifier, users: cfg.Users, oauthOnly: cfg.OAuthOnly, logger: cfg.Logger}, nil
}

// resource is the resource URI of an MCP endpoint path.
func (g *GovernedInbound) resource(path string) (string, bool) {
	if !mcpEndpointPath.MatchString(path) || strings.Contains(path, "..") {
		return "", false
	}
	return g.origin + path, true
}

// MetadataURL is the RFC 9728 metadata URL of an MCP endpoint path.
func (g *GovernedInbound) MetadataURL(path string) string {
	return g.origin + ProtectedResourceMetadataPrefix + path
}

// ProtectedResourceMetadata is the RFC 9728 §2 document.
type ProtectedResourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	ScopesSupported        []string `json:"scopes_supported,omitempty"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
	ResourceName           string   `json:"resource_name,omitempty"`
}

// ServeMetadata serves GET /.well-known/oauth-protected-resource/app/{projectID}/mcp[/...].
// It is anonymous by design (a client reads it before it holds a token)
// and carries URLs only: no project data, not even whether the project
// exists.
func (g *GovernedInbound) ServeMetadata(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.EscapedPath(), ProtectedResourceMetadataPrefix)
	resource, ok := g.resource(path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_ = json.NewEncoder(w).Encode(ProtectedResourceMetadata{
		Resource: resource, AuthorizationServers: g.verifier.Issuers(), ScopesSupported: g.verifier.RequiredScopes(),
		BearerMethodsSupported: []string{"header"}, ResourceName: "Elitea MCP",
	})
}

// Wrap puts the resource server in front of the platform authentication of
// the MCP routes.
func (g *GovernedInbound) Wrap(authenticate func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		platform := authenticate(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path := r.URL.EscapedPath()
			resource, ok := g.resource(path)
			if !ok {
				platform.ServeHTTP(w, r)
				return
			}
			challenge := `Bearer resource_metadata="` + g.MetadataURL(path) + `"`
			if raw, bearer := bearerToken(r); bearer && g.verifier.Recognizes(raw) {
				g.serveOAuth(w, r, next, raw, resource, challenge)
				return
			}
			if g.oauthOnly {
				refuse(w, challenge)
				return
			}
			platform.ServeHTTP(&challengeWriter{ResponseWriter: w, challenge: challenge}, r)
		})
	}
}

func (g *GovernedInbound) serveOAuth(w http.ResponseWriter, r *http.Request, next http.Handler, raw, resource, challenge string) {
	token, err := g.verifier.Verify(r.Context(), raw, resource)
	if err != nil {
		refuse(w, challenge+`, error="invalid_token"`)
		return
	}
	user, err := g.users.ResolveInboundUser(r, token)
	switch {
	case errors.Is(err, auth.ErrPrincipalInactive):
		refuse(w, challenge+`, error="invalid_token"`)
		return
	case err != nil:
		g.logger.WarnContext(r.Context(), "governed MCP inbound user resolution unavailable")
		http.Error(w, `{"error":"authentication unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	ctx := auth.ContextWithAuthenticatedUser(r.Context(), user, auth.AuthenticationSourceToken)
	if client := token.ClientID; client != "" && len(client) <= 200 && printableRef(client) {
		ctx = auth.ContextWithCallingApplication(ctx, CallingApplicationPrefix+client)
	}
	forwarded := r.Clone(ctx)
	// The token stops here: no handler, bundle or worker request can carry it.
	forwarded.Header.Del("Authorization")
	next.ServeHTTP(w, forwarded)
}

func printableRef(value string) bool {
	for _, c := range value {
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}

func bearerToken(r *http.Request) (string, bool) {
	scheme, value, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	value = strings.TrimSpace(value)
	return value, value != ""
}

func refuse(w http.ResponseWriter, challenge string) {
	w.Header().Set("WWW-Authenticate", challenge)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
}

// challengeWriter adds the resource_metadata challenge to a 401 the
// platform authentication answers. Every other response passes untouched,
// streaming included.
type challengeWriter struct {
	http.ResponseWriter
	challenge string
}

func (c *challengeWriter) WriteHeader(status int) {
	if status == http.StatusUnauthorized && c.Header().Get("WWW-Authenticate") == "" {
		c.Header().Set("WWW-Authenticate", c.challenge)
	}
	c.ResponseWriter.WriteHeader(status)
}

// Flush keeps an SSE response streaming through the wrapper.
func (c *challengeWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (c *challengeWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }
