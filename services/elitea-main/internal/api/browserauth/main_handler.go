package browserauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"

	forwardapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/edgeauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browserflow"
)

// MainEdgeAuthPath is an internal gateway address, not a browser-facing
// compatibility alias. The gateway calls it before forwarding every request
// to the current Main during the incremental cutover.
const MainEdgeAuthPath = "/internal/auth/main"

const (
	mainDecisionTimeout = 15 * time.Second
	maxMainAvatarBytes  = 2 << 10

	MainAvatarHeader      = "X-Auth-Avatar"
	MainAvatarStateHeader = "X-Auth-Avatar-State"
	mainAvatarNone        = "none"
	mainAvatarUnavailable = "unavailable"
	mainAvatarValue       = "value"
)

type MainAuthorizer interface {
	Authorize(context.Context, forwardapp.Request) (forwardapp.Decision, error)
}

type MainConfig struct {
	CredentialHeaders  []CredentialHeader
	AccessDeniedTarget string
	// PublicOrigin is the origin a BROWSER reaches this deployment on, and it
	// is what makes this handler's redirects usable.
	//
	// The redirects below were relative. An EdgeAuth response is consumed by
	// the EDGE, not by the browser, and an edge resolves a relative Location
	// against the address it called — which is this service's INTERNAL
	// address. Traefik handed browsers
	//   http://elitea-main.elitea.svc.cluster.local:8080/auth/login?...
	// and the browser answered ERR_NAME_NOT_RESOLVED, because that name exists
	// only inside the cluster. Every deployment that puts this endpoint behind
	// a proxy hits this, which is every deployment that uses it at all.
	//
	// Empty keeps the previous relative form, for a caller that reaches this
	// handler directly.
	PublicOrigin string
}

// MainHandler translates one trusted gateway request into the typed in-process
// Main authorization call. It intentionally does not accept target/scope query
// selection: the only supported output is the canonical RPC identity header
// projection consumed by the current Main traefik mode.
type MainHandler struct {
	authorizer         MainAuthorizer
	sources            *TrustedProxyResolver
	cookies            *CookiePolicy
	credentialHeaders  []CredentialHeader
	accessDeniedTarget string
	publicOrigin       string
}

func NewMainHandler(
	authorizer MainAuthorizer,
	sources *TrustedProxyResolver,
	cookies *CookiePolicy,
	config MainConfig,
) (*MainHandler, error) {
	if authorizer == nil || sources == nil || cookies == nil ||
		len(config.CredentialHeaders) >= forwardapp.MaxCredentials {
		return nil, ErrInvalidHandlerConfiguration
	}
	if config.AccessDeniedTarget == "" {
		config.AccessDeniedTarget = "/app/access_denied"
	}
	if browserflow.ValidateReturnTarget(config.AccessDeniedTarget) != nil {
		return nil, ErrInvalidHandlerConfiguration
	}

	seen := make(map[string]struct{}, len(config.CredentialHeaders))
	headers := append([]CredentialHeader(nil), config.CredentialHeaders...)
	for _, header := range headers {
		name := http.CanonicalHeaderKey(header.Name)
		if !httpguts.ValidHeaderFieldName(header.Name) ||
			strings.EqualFold(name, "Authorization") || header.Type != "bearer" && header.Type != "basic" {
			return nil, ErrInvalidHandlerConfiguration
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, ErrInvalidHandlerConfiguration
		}
		seen[name] = struct{}{}
	}

	return &MainHandler{
		authorizer:         authorizer,
		sources:            sources,
		cookies:            cookies,
		credentialHeaders:  headers,
		accessDeniedTarget: config.AccessDeniedTarget,
		publicOrigin:       strings.TrimSuffix(config.PublicOrigin, "/"),
	}, nil
}

// absoluteTarget makes a same-origin path absolute against the configured
// public origin, so the Location survives an edge that resolves a relative URL
// against the address it called.
func (h *MainHandler) absoluteTarget(path string) string {
	if h.publicOrigin == "" {
		return path
	}
	return h.publicOrigin + path
}

func (h *MainHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	securityHeaders(writer)
	if request.URL.RawQuery != "" {
		writeProblem(writer, http.StatusBadRequest)
		return
	}
	forwarded, err := h.sources.Resolve(request)
	if err != nil {
		writeProblem(writer, http.StatusForbidden)
		return
	}

	// Main always consumes the fixed RPC projection. Caller-selected mappers
	// would turn an internal gateway edge into a second public Auth Core API.
	forwarded.Target = "rpc"
	forwarded.TargetPresent = true
	forwarded.Scope = ""
	forwarded.ScopePresent = false

	browserSession := forwardapp.BrowserSessionInput{}
	sessionID, readErr := h.cookies.Read(request)
	switch {
	case readErr == nil:
		browserSession = forwardapp.BrowserSessionInput{Present: true, ID: sessionID, Reference: "-"}
	case errors.Is(readErr, ErrSessionCookieMissing):
	case errors.Is(readErr, ErrSessionCookieInvalid):
		if clearErr := h.cookies.Clear(writer); clearErr != nil {
			writeProblem(writer, http.StatusServiceUnavailable)
			return
		}
	default:
		writeProblem(writer, http.StatusServiceUnavailable)
		return
	}

	decisionContext, cancelDecision := context.WithTimeout(request.Context(), mainDecisionTimeout)
	defer cancelDecision()
	decision, err := h.authorizer.Authorize(decisionContext, forwardapp.Request{
		Source:         applicationSource(forwarded),
		Credentials:    credentials(request.Header, h.credentialHeaders),
		BrowserSession: browserSession,
		Traversal:      forwardapp.MainTraversal,
	})
	if err != nil {
		writeProblem(writer, http.StatusServiceUnavailable)
		return
	}

	if browserSession.Present && (decision.Kind == forwardapp.DecisionLogin ||
		decision.Authentication.Type == forwardapp.AuthenticationPublic) {
		if clearErr := h.cookies.Clear(writer); clearErr != nil {
			writeProblem(writer, http.StatusServiceUnavailable)
			return
		}
	}

	switch decision.Kind {
	case forwardapp.DecisionAllow:
		if !writeMainIdentity(writer, h.sources, decision) {
			writeProblem(writer, http.StatusServiceUnavailable)
		}
	case forwardapp.DecisionDeny:
		if bearerCredential(request.Header) {
			// A bearer caller is a program, not a browser: a 302 to the
			// access-denied PAGE tells a native client nothing (ADR-0025
			// WP3). A revoked native device session gets the ADR's
			// device_revoked answer, any other refused bearer a plain 401.
			if decision.Reason == forwardapp.ReasonCredentialRevoked {
				apimw.WriteDeviceRevoked(writer)
				return
			}
			writeBearerRejected(writer)
			return
		}
		http.Redirect(writer, request, h.absoluteTarget(h.accessDeniedTarget), http.StatusFound)
	case forwardapp.DecisionLogin:
		query := url.Values{"target_to": {forwarded.URI}}
		http.Redirect(writer, request, h.absoluteTarget(BasePath+LoginPath+"?"+query.Encode()), http.StatusFound)
	case forwardapp.DecisionDependencyFailure:
		writeProblem(writer, http.StatusServiceUnavailable)
	default:
		writeProblem(writer, http.StatusServiceUnavailable)
	}
}

func writeMainIdentity(writer http.ResponseWriter, signer *TrustedProxyResolver, decision forwardapp.Decision) bool {
	avatarState, avatar, ok := mainAvatarProjection(decision)
	if !ok {
		return false
	}
	// Both headers are emitted for every allow decision. This makes the
	// EdgeAuth response authoritative over caller-supplied profile headers
	// and distinguishes an explicit null projection from a missing/mixed-version
	// contract.
	writer.Header().Set(MainAvatarStateHeader, avatarState)
	writer.Header().Set(MainAvatarHeader, avatar)

	switch decision.Authentication.Type {
	case forwardapp.AuthenticationToken:
		writer.Header().Set("X-Auth-Type", "token")
		writer.Header().Set("X-Auth-ID", decision.Authentication.Principal.TokenID)
		writer.Header().Set("X-Auth-User-ID", decision.Authentication.Principal.UserID)
	case forwardapp.AuthenticationUser:
		writer.Header().Set("X-Auth-Type", "user")
		writer.Header().Set("X-Auth-ID", decision.Authentication.Principal.UserID)
		writer.Header().Set("X-Auth-User-ID", decision.Authentication.Principal.UserID)
	case forwardapp.AuthenticationPublic:
		writer.Header().Set("X-Auth-Type", "public")
		writer.Header().Set("X-Auth-ID", "-")
		writer.Header().Set("X-Auth-User-ID", "-")
	default:
		return false
	}
	if !signProjection(writer.Header(), signer, decision) {
		return false
	}
	writer.Header().Set("X-Auth-Reference", "-")
	writeEdgeAuthOK(writer)
	return true
}

// mainAvatarProjection exposes only the bounded scalar still consumed by the
// current Main profile APIs. Raw provider attributes and the reusable browser
// session reference never cross the gateway boundary.
func mainAvatarProjection(decision forwardapp.Decision) (string, string, bool) {
	if decision.Authentication.Type != forwardapp.AuthenticationUser {
		return mainAvatarNone, "-", true
	}
	authorization, ok := decision.AuthorizedBrowser()
	if !ok {
		return "", "", false
	}

	var provider struct {
		Attributes json.RawMessage `json:"attributes"`
	}
	if err := json.Unmarshal(authorization.ProviderAttributes, &provider); err != nil {
		return mainAvatarUnavailable, "-", true
	}
	if nullJSON(provider.Attributes) {
		return mainAvatarNone, "-", true
	}

	var attributes struct {
		Picture json.RawMessage `json:"picture"`
	}
	if err := json.Unmarshal(provider.Attributes, &attributes); err != nil {
		return mainAvatarUnavailable, "-", true
	}
	if nullJSON(attributes.Picture) {
		return mainAvatarNone, "-", true
	}

	var avatar string
	if err := json.Unmarshal(attributes.Picture, &avatar); err != nil {
		return mainAvatarUnavailable, "-", true
	}
	if avatar == "" {
		return mainAvatarNone, "-", true
	}
	if !validMainAvatarValue(avatar) {
		return mainAvatarUnavailable, "-", true
	}
	return mainAvatarValue, avatar, true
}

func validMainAvatarValue(value string) bool {
	if value == "" || value == "-" || len(value) > maxMainAvatarBytes ||
		!httpguts.ValidHeaderFieldValue(value) {
		return false
	}
	// Keep the raw header contract byte-stable across HTTP/2, reverse proxies,
	// WSGI's Latin-1 decoding, and Python. Unicode values must be encoded by a
	// future versioned contract instead of relying on intermediary behavior.
	for index := range len(value) {
		if value[index] < '!' || value[index] > '~' {
			return false
		}
	}
	return true
}

func nullJSON(value json.RawMessage) bool {
	value = bytes.TrimSpace(value)
	return len(value) == 0 || bytes.Equal(value, []byte("null"))
}

// bearerCredential reports an `Authorization: Bearer` credential on the
// request: the caller is an API client, and a deny must be a 401 it can read.
func bearerCredential(headers http.Header) bool {
	value := headers.Get("Authorization")
	scheme, _, ok := strings.Cut(value, " ")
	return ok && strings.EqualFold(scheme, "Bearer")
}

// writeBearerRejected is the 401 the API's own Auth middleware writes for a
// refused bearer token, so a client sees the same refusal through the edge.
func writeBearerRejected(writer http.ResponseWriter) {
	writer.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusUnauthorized)
	_, _ = writer.Write([]byte(`{"error":{"message":"token validation failed","type":"authentication_error","code":"token_rejected"}}` + "\n"))
}
