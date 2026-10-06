package browserauth

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"golang.org/x/net/http/httpguts"

	forwardapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/edgeauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browserflow"
)

// AuthPath is the Form plane's core check, `/auth/check`. It is not `/auth`
// itself, which is the single sign-on plane's edge auth check (router.go).
const AuthPath = "/check"

// CredentialHeader preserves the configured other_auth_headers order from the
// current service. Only the two current credential handlers are accepted.
type CredentialHeader struct {
	Name string
	Type string
}

type CoreConfig struct {
	CredentialHeaders  []CredentialHeader
	AccessDeniedTarget string
	// Mappers contains only sanitized, already-authorized projections. Nil
	// preserves the built-in no-op and RPC targets and disables optional
	// targets. In particular it never enables the current raw /info transport.
	Mappers *SuccessMapper
}

// CoreHandler implements the unversioned Auth Core /auth slice. Provider
// login/logout routes are separate. The tracked header mapper is supported;
// the tracked JSON mapping remains evidence-only until a workload-authenticated
// consumer transport replaces the current raw session-reference /info contract.
// This slice stays unmounted until production PostgreSQL, proxy, and
// rate-limit composition is verified.
type CoreHandler struct {
	kernel             *forwardapp.Kernel
	sources            *TrustedProxyResolver
	cookies            *CookiePolicy
	credentialHeaders  []CredentialHeader
	accessDeniedTarget string
	mappers            *SuccessMapper
}

func NewCoreHandler(
	kernel *forwardapp.Kernel,
	sources *TrustedProxyResolver,
	cookies *CookiePolicy,
	config CoreConfig,
) (*CoreHandler, error) {
	if kernel == nil || sources == nil || cookies == nil ||
		len(config.CredentialHeaders) >= forwardapp.MaxCredentials {
		return nil, ErrInvalidHandlerConfiguration
	}
	if config.AccessDeniedTarget == "" {
		config.AccessDeniedTarget = "/access_denied"
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

	return &CoreHandler{
		kernel:             kernel,
		sources:            sources,
		cookies:            cookies,
		credentialHeaders:  headers,
		accessDeniedTarget: config.AccessDeniedTarget,
		mappers:            config.Mappers,
	}, nil
}

// Routes preserves the current Auth Core effective methods. It is a source-only
// mount candidate; production composition does not call it yet.
func (h *CoreHandler) Routes() chi.Router {
	router := newRouter()
	h.registerRoutes(router)
	return router
}

func (h *CoreHandler) registerRoutes(router chi.Router) {
	router.MethodFunc(http.MethodGet, AuthPath, h.ServeHTTP)
	router.MethodFunc(http.MethodHead, AuthPath, head(h.ServeHTTP))
	router.MethodFunc(http.MethodOptions, AuthPath, options("GET, HEAD, OPTIONS"))
}

func (h *CoreHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	securityHeaders(writer)
	forwarded, err := h.sources.Resolve(request)
	if err != nil {
		h.writeDenied(writer, request)
		return
	}

	credentials := h.credentials(request.Header)
	browserSession := forwardapp.BrowserSessionInput{}
	if !hasPresentCredential(credentials) {
		sessionID, readErr := h.cookies.Read(request)
		switch {
		case readErr == nil:
			// The current service forwards the raw browser cookie as
			// X-Auth-Reference. In the merged service that value is bearer
			// material and is unnecessary for principal revalidation, so the
			// compatibility reference is intentionally redacted.
			browserSession = forwardapp.BrowserSessionInput{
				Present:   true,
				ID:        sessionID,
				Reference: "-",
			}
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
	}

	decision, err := h.kernel.Authorize(request.Context(), forwardapp.Request{
		Source:         applicationSource(forwarded),
		Credentials:    credentials,
		BrowserSession: browserSession,
		Traversal:      forwardapp.DirectHTTPTraversal,
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
		switch h.writeSuccess(writer, decision) {
		case successWritten:
		case successDenied:
			h.writeDenied(writer, request)
		case successInvalidAuthorizedData:
			writeProblem(writer, http.StatusServiceUnavailable)
		default:
			writeProblem(writer, http.StatusServiceUnavailable)
		}
	case forwardapp.DecisionDeny:
		h.writeDenied(writer, request)
	case forwardapp.DecisionLogin:
		h.writeLogin(writer, request, forwarded.URI)
	case forwardapp.DecisionDependencyFailure:
		writeProblem(writer, http.StatusServiceUnavailable)
	default:
		writeProblem(writer, http.StatusServiceUnavailable)
	}
}

func (h *CoreHandler) credentials(headers http.Header) []forwardapp.CredentialInput {
	return credentials(headers, h.credentialHeaders)
}

func credentials(headers http.Header, configured []CredentialHeader) []forwardapp.CredentialInput {
	credentials := make([]forwardapp.CredentialInput, 0, len(configured)+1)
	authorization, present, valid := optionalCredentialHeader(headers, "Authorization")
	input := forwardapp.CredentialInput{}
	if present {
		input.Present = true
		if valid {
			separator := strings.IndexByte(authorization, ' ')
			if separator >= 0 {
				credentialType := strings.ToLower(authorization[:separator])
				if len(credentialType) <= forwardapp.MaxCredentialTypeBytes {
					input.Type = credentialType
					input.Data = authorization[separator+1:]
				}
			}
		}
	}
	credentials = append(credentials, input)

	for _, header := range configured {
		value, headerPresent, headerValid := optionalCredentialHeader(headers, header.Name)
		input = forwardapp.CredentialInput{Present: headerPresent}
		if headerPresent && headerValid {
			input.Type = header.Type
			input.Data = value
		}
		credentials = append(credentials, input)
	}
	return credentials
}

type successDisposition uint8

const (
	successInvalidAuthorizedData successDisposition = iota
	successWritten
	successDenied
)

func (h *CoreHandler) writeSuccess(writer http.ResponseWriter, decision forwardapp.Decision) successDisposition {
	if !decision.Source.TargetPresent {
		writeEdgeAuthOK(writer)
		return successWritten
	}

	switch decision.Source.Target {
	case "rpc":
		switch decision.Authentication.Type {
		case forwardapp.AuthenticationToken:
			writer.Header().Set("X-Auth-Type", "token")
			writer.Header().Set("X-Auth-ID", decision.Authentication.Principal.TokenID)
			writer.Header().Set("X-Auth-User-ID", decision.Authentication.Principal.UserID)
			writer.Header().Set("X-Auth-Reference", "-")
		case forwardapp.AuthenticationUser:
			writer.Header().Set("X-Auth-Type", "user")
			writer.Header().Set("X-Auth-ID", decision.Authentication.Principal.UserID)
			writer.Header().Set("X-Auth-User-ID", decision.Authentication.Principal.UserID)
			writer.Header().Set("X-Auth-Reference", "-")
		case forwardapp.AuthenticationPublic:
			writer.Header().Set("X-Auth-Type", "public")
			writer.Header().Set("X-Auth-ID", "-")
			writer.Header().Set("X-Auth-Reference", "-")
		default:
			return successInvalidAuthorizedData
		}
	case "header":
		header, disposition := h.mappers.Header(decision)
		switch disposition {
		case MappingApplied:
			writer.Header().Set(header.Name, header.Value)
		case MappingNotApplicable:
			return successDenied
		case MappingInvalidAuthorizedData:
			return successInvalidAuthorizedData
		default:
			return successInvalidAuthorizedData
		}
	default:
		// Exposing the tracked JSON mapping through the current endpoint/session-ID
		// protocol would recreate a reusable browser bearer on an unauthenticated
		// data path.
		return successDenied
	}
	writeEdgeAuthOK(writer)
	return successWritten
}

func (h *CoreHandler) writeDenied(writer http.ResponseWriter, request *http.Request) {
	http.Redirect(writer, request, h.accessDeniedTarget, http.StatusFound)
}

func (h *CoreHandler) writeLogin(writer http.ResponseWriter, request *http.Request, target string) {
	query := url.Values{"target_to": {target}}
	http.Redirect(writer, request, BasePath+LoginPath+"?"+query.Encode(), http.StatusFound)
}

func writeEdgeAuthOK(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write([]byte("OK"))
}

func applicationSource(source ForwardedRequest) forwardapp.Source {
	return forwardapp.Source{
		Method:        source.Method,
		Proto:         source.Proto,
		Host:          source.Host,
		URI:           source.URI,
		IP:            source.ClientIP,
		Target:        source.Target,
		TargetPresent: source.TargetPresent,
		Scope:         source.Scope,
		ScopePresent:  source.ScopePresent,
	}
}

func hasPresentCredential(credentials []forwardapp.CredentialInput) bool {
	for _, credential := range credentials {
		if credential.Present {
			return true
		}
	}
	return false
}

func optionalCredentialHeader(headers http.Header, name string) (string, bool, bool) {
	var values []string
	for key, current := range headers {
		if strings.EqualFold(key, name) {
			values = append(values, current...)
		}
	}
	if len(values) == 0 {
		return "", false, true
	}
	if len(values) != 1 || len(values[0]) > forwardapp.MaxCredentialDataBytes ||
		!utf8.ValidString(values[0]) || !httpguts.ValidHeaderFieldValue(values[0]) {
		return "", true, false
	}
	return values[0], true, true
}
