package browserauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"errors"
	"html/template"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	browserapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/browserauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browserflow"
)

const (
	OIDCLoginPath         = "/oidc/login"
	OIDCLoginCallbackPath = "/oidc/login_callback"
	OIDCPKCEChallengeS256 = browserapp.OIDCPKCEChallengeS256

	DefaultMaxOIDCCallbackBytes = int64(8 << 10)
	maxOIDCBeginQueryBytes      = 8 << 10
	maxMaxOIDCCallbackBytes     = int64(64 << 10)
	maxOIDCCallbackParameters   = 16
	maxOIDCCallbackKeyBytes     = 128
	maxOIDCCallbackValueBytes   = 4096
	maxOIDCAuthorizationURL     = 16 << 10
	maxOIDCAuthorizationValue   = 4096
	maxOIDCAuthorizationForm    = 64 << 10
	oidcAutoSubmitScriptHash    = "sha256-vZjApXuMAArOlGLmcMVkBoEfwkeoKlzMWYxslp0fG9M="
)

var errOIDCCallbackRejected = errors.New("OIDC authorization response rejected")

//go:embed templates/oidc_redirect.html
var oidcRedirectTemplateSource string

type OIDCAuthorization = browserapp.OIDCAuthorization
type OIDCAuthorizationRequest = browserapp.OIDCAuthorizationRequest
type OIDCProtocol = browserapp.OIDCProtocol

const (
	OIDCAuthorizationGET  = browserapp.OIDCAuthorizationGET
	OIDCAuthorizationPOST = browserapp.OIDCAuthorizationPOST
)

type OIDCHandlerConfig struct {
	DefaultLoginTarget string
	MaxCallbackBytes   int64
}

// OIDCHandler preserves the current auth_oidc route boundary without mounting
// it in the production router. Construction requires a trusted client-key
// policy and a cross-replica attempt admitter so admission happens before
// Service.Begin allocates a session or transaction.
type OIDCHandler struct {
	flow               Flow
	protocol           OIDCProtocol
	attempts           AttemptAdmitter
	clientKeys         ClientKeyResolver
	cookies            *CookiePolicy
	defaultLoginTarget string
	maxCallbackBytes   int64
	redirectTemplate   *template.Template
}

func NewOIDCHandler(
	flow Flow,
	protocol OIDCProtocol,
	attempts AttemptAdmitter,
	clientKeys ClientKeyResolver,
	cookies *CookiePolicy,
	config OIDCHandlerConfig,
) (*OIDCHandler, error) {
	if flow == nil || protocol == nil || attempts == nil || clientKeys == nil || cookies == nil ||
		cookies.sameSite != http.SameSiteLaxMode {
		return nil, ErrInvalidHandlerConfiguration
	}
	if config.DefaultLoginTarget == "" {
		config.DefaultLoginTarget = "/"
	}
	if browserflow.ValidateReturnTarget(config.DefaultLoginTarget) != nil {
		return nil, ErrInvalidHandlerConfiguration
	}
	if config.MaxCallbackBytes == 0 {
		config.MaxCallbackBytes = DefaultMaxOIDCCallbackBytes
	}
	if config.MaxCallbackBytes < 1024 || config.MaxCallbackBytes > maxMaxOIDCCallbackBytes {
		return nil, ErrInvalidHandlerConfiguration
	}
	redirectTemplate, err := template.New("oidc_redirect.html").Parse(oidcRedirectTemplateSource)
	if err != nil {
		return nil, ErrInvalidHandlerConfiguration
	}
	return &OIDCHandler{
		flow:               flow,
		protocol:           protocol,
		attempts:           attempts,
		clientKeys:         clientKeys,
		cookies:            cookies,
		defaultLoginTarget: config.DefaultLoginTarget,
		maxCallbackBytes:   config.MaxCallbackBytes,
		redirectTemplate:   redirectTemplate,
	}, nil
}

// Routes preserves the current OIDC paths and callback transports. HEAD is an
// intentional security correction: unlike the current Flask boundary it never
// aliases a mutating GET. The router remains unmounted in production.
func (h *OIDCHandler) Routes() chi.Router {
	router := newRouter()
	h.registerRoutes(router)
	return router
}

func (h *OIDCHandler) registerRoutes(router chi.Router) {
	h.registerReadRoute(router, OIDCLoginPath, h.beginLogin)
	h.registerCallbackRoute(router, OIDCLoginCallbackPath, h.completeLogin)
}

func (h *OIDCHandler) registerReadRoute(router chi.Router, path string, handler http.HandlerFunc) {
	router.MethodFunc(http.MethodGet, path, handler)
	router.MethodFunc(http.MethodHead, path, head(rejectOIDCHead("GET, OPTIONS")))
	router.MethodFunc(http.MethodOptions, path, options("GET, OPTIONS"))
}

func (h *OIDCHandler) registerCallbackRoute(router chi.Router, path string, handler http.HandlerFunc) {
	router.MethodFunc(http.MethodGet, path, handler)
	router.MethodFunc(http.MethodHead, path, head(rejectOIDCHead("GET, POST, OPTIONS")))
	router.MethodFunc(http.MethodPost, path, handler)
	router.MethodFunc(http.MethodOptions, path, options("GET, POST, OPTIONS"))
}

func (h *OIDCHandler) beginLogin(writer http.ResponseWriter, request *http.Request) {
	securityHeaders(writer)
	if !admitAttempt(writer, request, h.attempts, h.clientKeys, BrowserAttempt{Stage: BrowserAttemptOIDCBegin}) {
		return
	}
	if hasUnexpectedReadBody(request) {
		writeProblem(writer, http.StatusBadRequest)
		return
	}
	if len(request.URL.RawQuery) > maxOIDCBeginQueryBytes {
		writeProblem(writer, http.StatusRequestURITooLong)
		return
	}
	authorization, err := h.protocol.NewAuthorization(request.Context())
	if err != nil || !validOIDCAuthorization(authorization) {
		writeProblem(writer, http.StatusServiceUnavailable)
		return
	}
	if !resetSession(writer, request, h.flow, h.cookies) {
		return
	}
	target := queryReturnTarget(request.URL.Query(), h.defaultLoginTarget)
	result, err := h.flow.Begin(request.Context(), browserapp.BeginRequest{
		Provider:      "oidc",
		ReturnTarget:  target,
		Correlation:   authorization.Correlation,
		ProviderState: authorization.ProviderState,
	})
	if err != nil {
		_ = h.cookies.Clear(writer)
		writeOIDCFlowError(writer, err)
		return
	}
	if browserflow.ValidateTransactionID(result.TransactionID) != nil ||
		browserflow.ValidateOpaqueID(result.SessionID) != nil || result.ExpiresAt.IsZero() {
		h.failStartedLogin(writer, request.Context(), result.SessionID)
		return
	}
	authorizationRequest, err := h.protocol.AuthorizationRequest(result.TransactionID, authorization)
	if err != nil || !validOIDCAuthorizationRequest(authorizationRequest, result.TransactionID, authorization) {
		h.failStartedLogin(writer, request.Context(), result.SessionID)
		return
	}

	var authorizationURL string
	var formBody []byte
	var formOrigin string
	switch authorizationRequest.Transport {
	case OIDCAuthorizationGET:
		authorizationURL, err = buildOIDCAuthorizationURL(authorizationRequest)
	case OIDCAuthorizationPOST:
		formBody, formOrigin, err = h.renderOIDCAuthorizationForm(authorizationRequest)
	default:
		err = ErrInvalidHandlerConfiguration
	}
	if err != nil {
		h.failStartedLogin(writer, request.Context(), result.SessionID)
		return
	}
	if err := h.cookies.Set(writer, result.SessionID); err != nil {
		h.failStartedLogin(writer, request.Context(), result.SessionID)
		return
	}
	if authorizationRequest.Transport == OIDCAuthorizationGET {
		http.Redirect(writer, request, authorizationURL, http.StatusFound)
		return
	}
	setOIDCPostHeaders(writer, formOrigin)
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(formBody)
}

func (h *OIDCHandler) revokeStartedSession(ctx context.Context, sessionID string) {
	if browserflow.ValidateOpaqueID(sessionID) == nil {
		_, _ = h.flow.Logout(ctx, sessionID)
	}
}

func (h *OIDCHandler) failStartedLogin(
	writer http.ResponseWriter,
	ctx context.Context,
	sessionID string,
) {
	h.revokeStartedSession(ctx, sessionID)
	_ = h.cookies.Clear(writer)
	writeProblem(writer, http.StatusServiceUnavailable)
}

func (h *OIDCHandler) completeLogin(writer http.ResponseWriter, request *http.Request) {
	securityHeaders(writer)
	// Gate all callback traffic before query/body parsing so malformed floods
	// cannot bypass the shared cross-replica attempt policy.
	if !admitAttempt(writer, request, h.attempts, h.clientKeys, BrowserAttempt{Stage: BrowserAttemptOIDCCallback}) {
		return
	}
	values, status := h.callbackValues(writer, request)
	if status != 0 {
		writeProblem(writer, status)
		return
	}
	response, ok := parseOIDCCallback(values)
	if !ok {
		writeProblem(writer, http.StatusBadRequest)
		return
	}
	sessionID, err := h.cookies.Read(request)
	if err != nil {
		_ = h.cookies.Clear(writer)
		writeProblem(writer, http.StatusBadRequest)
		return
	}
	var verifier browserapp.AssertionVerifier = rejectedOIDCVerifier{}
	if response.code != "" {
		verifier = h.protocol.NewVerifier(response.code)
		if verifier == nil {
			writeProblem(writer, http.StatusServiceUnavailable)
			return
		}
	}
	result, err := h.flow.Complete(request.Context(), browserapp.CompleteRequest{
		SessionID:     sessionID,
		TransactionID: response.state,
		Provider:      "oidc",
	}, verifier)
	if err != nil {
		switch {
		case errors.Is(err, browserapp.ErrUnauthenticated),
			errors.Is(err, browserapp.ErrAuthenticationExpired):
			http.Redirect(writer, request, BasePath+LoginPath+"?error=true", http.StatusFound)
		case errors.Is(err, browserapp.ErrInvalidRequest),
			errors.Is(err, browserapp.ErrTransactionRejected):
			writeProblem(writer, http.StatusBadRequest)
		default:
			writeProblem(writer, http.StatusServiceUnavailable)
		}
		return
	}
	if browserflow.ValidateReturnTarget(result.ReturnTarget) != nil ||
		result.SessionID == sessionID || browserflow.ValidateOpaqueID(result.SessionID) != nil {
		h.revokeStartedSession(request.Context(), result.SessionID)
		_ = h.cookies.Clear(writer)
		writeProblem(writer, http.StatusServiceUnavailable)
		return
	}
	if err := h.cookies.Set(writer, result.SessionID); err != nil {
		h.revokeStartedSession(request.Context(), result.SessionID)
		_ = h.cookies.Clear(writer)
		writeProblem(writer, http.StatusServiceUnavailable)
		return
	}
	http.Redirect(writer, request, result.ReturnTarget, http.StatusFound)
}

func (h *OIDCHandler) callbackValues(writer http.ResponseWriter, request *http.Request) (url.Values, int) {
	switch request.Method {
	case http.MethodGet:
		if hasUnexpectedReadBody(request) {
			return nil, http.StatusBadRequest
		}
		if int64(len(request.URL.RawQuery)) > h.maxCallbackBytes {
			return nil, http.StatusRequestURITooLong
		}
		values, err := url.ParseQuery(request.URL.RawQuery)
		if err != nil {
			return nil, http.StatusBadRequest
		}
		return values, 0
	case http.MethodPost:
		if request.URL.RawQuery != "" {
			return nil, http.StatusBadRequest
		}
		contentTypes := request.Header.Values("Content-Type")
		if len(contentTypes) != 1 {
			return nil, http.StatusUnsupportedMediaType
		}
		mediaType, _, err := mime.ParseMediaType(contentTypes[0])
		if err != nil || mediaType != "application/x-www-form-urlencoded" {
			return nil, http.StatusUnsupportedMediaType
		}
		request.Body = http.MaxBytesReader(writer, request.Body, h.maxCallbackBytes)
		if err := request.ParseForm(); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				return nil, http.StatusRequestEntityTooLarge
			}
			return nil, http.StatusBadRequest
		}
		return request.PostForm, 0
	default:
		return nil, http.StatusBadRequest
	}
}

func rejectOIDCHead(allow string) http.HandlerFunc {
	return func(writer http.ResponseWriter, _ *http.Request) {
		securityHeaders(writer)
		writer.Header().Set("Allow", allow)
		writeProblem(writer, http.StatusMethodNotAllowed)
	}
}

func hasUnexpectedReadBody(request *http.Request) bool {
	return request.ContentLength != 0 || len(request.TransferEncoding) != 0 ||
		(request.Body != nil && request.Body != http.NoBody)
}

type oidcCallback struct {
	state string
	code  string
}

func parseOIDCCallback(values url.Values) (oidcCallback, bool) {
	if len(values) == 0 || len(values) > maxOIDCCallbackParameters {
		return oidcCallback{}, false
	}
	allowed := map[string]struct{}{
		"state": {}, "code": {}, "error": {}, "error_description": {},
		"error_uri": {}, "session_state": {},
	}
	for key, items := range values {
		if _, ok := allowed[key]; !ok || !validOIDCCallbackText(key, maxOIDCCallbackKeyBytes) ||
			len(items) != 1 || !validOIDCCallbackText(items[0], maxOIDCCallbackValueBytes) {
			return oidcCallback{}, false
		}
	}
	state, stateOK := singleValue(values, "state")
	code, codePresent := singleValue(values, "code")
	providerError, errorPresent := singleValue(values, "error")
	if !stateOK || browserflow.ValidateTransactionID(state) != nil || codePresent == errorPresent {
		return oidcCallback{}, false
	}
	if errorPresent {
		if providerError == "" {
			return oidcCallback{}, false
		}
		return oidcCallback{state: state}, true
	}
	if code == "" {
		return oidcCallback{}, false
	}
	if _, present := values["error_description"]; present {
		return oidcCallback{}, false
	}
	if _, present := values["error_uri"]; present {
		return oidcCallback{}, false
	}
	return oidcCallback{state: state, code: code}, true
}

func validOIDCAuthorization(authorization OIDCAuthorization) bool {
	return authorization.Correlation.Nonce != "" && authorization.Correlation.RequestID == "" &&
		authorization.Correlation.Validate() == nil && authorization.ProviderState.PKCEVerifier != "" &&
		authorization.ProviderState.Validate() == nil &&
		authorization.PKCEChallengeMethod == OIDCPKCEChallengeS256
}

func validOIDCAuthorizationRequest(
	request OIDCAuthorizationRequest,
	state string,
	authorization OIDCAuthorization,
) bool {
	if request.Transport != OIDCAuthorizationGET && request.Transport != OIDCAuthorizationPOST {
		return false
	}
	if request.ResponseType != "code" || request.State != state ||
		request.Nonce != authorization.Correlation.Nonce ||
		request.CodeChallengeMethod != OIDCPKCEChallengeS256 {
		return false
	}
	expectedChallengeBytes := sha256.Sum256([]byte(authorization.ProviderState.PKCEVerifier))
	expectedChallenge := base64.RawURLEncoding.EncodeToString(expectedChallengeBytes[:])
	if request.CodeChallenge != expectedChallenge ||
		!validOIDCAuthorizationText(request.ClientID, maxOIDCAuthorizationValue) ||
		!validOIDCAuthorizationScope(request.Scope) ||
		!validOIDCAuthorizationURI(request.RedirectURI) {
		return false
	}
	_, _, ok := parseOIDCAuthorizationEndpoint(request.Endpoint)
	return ok
}

func buildOIDCAuthorizationURL(request OIDCAuthorizationRequest) (string, error) {
	parsed, _, ok := parseOIDCAuthorizationEndpoint(request.Endpoint)
	if !ok {
		return "", ErrInvalidHandlerConfiguration
	}
	parameters, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return "", ErrInvalidHandlerConfiguration
	}
	for key, values := range oidcAuthorizationParameters(request) {
		parameters[key] = values
	}
	parsed.RawQuery = parameters.Encode()
	result := parsed.String()
	if len(result) > maxOIDCAuthorizationURL {
		return "", ErrInvalidHandlerConfiguration
	}
	return result, nil
}

func (h *OIDCHandler) renderOIDCAuthorizationForm(
	request OIDCAuthorizationRequest,
) ([]byte, string, error) {
	parsed, origin, ok := parseOIDCAuthorizationEndpoint(request.Endpoint)
	if !ok {
		return nil, "", ErrInvalidHandlerConfiguration
	}
	request.Endpoint = parsed.String()
	rendered := boundedOIDCBuffer{remaining: maxOIDCAuthorizationForm}
	if err := h.redirectTemplate.Execute(&rendered, request); err != nil ||
		rendered.buffer.Len() == 0 {
		return nil, "", ErrInvalidHandlerConfiguration
	}
	return rendered.buffer.Bytes(), origin, nil
}

type boundedOIDCBuffer struct {
	buffer    bytes.Buffer
	remaining int
}

func (buffer *boundedOIDCBuffer) Write(value []byte) (int, error) {
	if len(value) > buffer.remaining {
		return 0, ErrInvalidHandlerConfiguration
	}
	written, err := buffer.buffer.Write(value)
	buffer.remaining -= written
	return written, err
}

func oidcAuthorizationParameters(request OIDCAuthorizationRequest) url.Values {
	return url.Values{
		"response_type":         {request.ResponseType},
		"client_id":             {request.ClientID},
		"redirect_uri":          {request.RedirectURI},
		"scope":                 {request.Scope},
		"state":                 {request.State},
		"nonce":                 {request.Nonce},
		"code_challenge":        {request.CodeChallenge},
		"code_challenge_method": {request.CodeChallengeMethod},
	}
}

func parseOIDCAuthorizationEndpoint(value string) (url.URL, string, bool) {
	if value == "" || len(value) > maxOIDCAuthorizationURL || !utf8.ValidString(value) {
		return url.URL{}, "", false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.Opaque != "" || parsed.Fragment != "" {
		return url.URL{}, "", false
	}
	parameters, err := url.ParseQuery(parsed.RawQuery)
	if err != nil || len(parameters) > maxOIDCCallbackParameters {
		return url.URL{}, "", false
	}
	parameterCount := 0
	for key, values := range parameters {
		if isOIDCAuthorizationParameter(key) || forbiddenOIDCAuthorizationEndpointParameter(key) ||
			!validOIDCAuthorizationText(key, maxOIDCCallbackKeyBytes) || len(values) != 1 {
			return url.URL{}, "", false
		}
		parameterCount += len(values)
		for _, item := range values {
			if len(item) > maxOIDCAuthorizationValue || !utf8.ValidString(item) ||
				strings.ContainsFunc(item, unicode.IsControl) {
				return url.URL{}, "", false
			}
		}
	}
	if parameterCount > maxOIDCCallbackParameters {
		return url.URL{}, "", false
	}
	origin := parsed.Scheme + "://" + parsed.Host
	for index := 0; index < len(origin); index++ {
		if origin[index] <= 0x20 || origin[index] >= 0x7f || strings.ContainsRune(";,'\"", rune(origin[index])) {
			return url.URL{}, "", false
		}
	}
	return *parsed, origin, true
}

func forbiddenOIDCAuthorizationEndpointParameter(value string) bool {
	switch strings.ToLower(value) {
	case "client_secret", "code_verifier", "access_token", "refresh_token", "id_token":
		return true
	default:
		return false
	}
}

func isOIDCAuthorizationParameter(value string) bool {
	switch value {
	case "response_type", "client_id", "redirect_uri", "scope", "state", "nonce",
		"code_challenge", "code_challenge_method":
		return true
	default:
		return false
	}
}

func validOIDCAuthorizationText(value string, maxBytes int) bool {
	return value != "" && len(value) <= maxBytes && utf8.ValidString(value) &&
		strings.TrimSpace(value) == value && !strings.ContainsFunc(value, unicode.IsControl)
}

func validOIDCAuthorizationScope(value string) bool {
	parts := strings.Fields(value)
	if len(parts) == 0 || len(parts) > 16 || strings.Join(parts, " ") != value {
		return false
	}
	hasOpenID := false
	for _, part := range parts {
		if !validOIDCAuthorizationText(part, 256) {
			return false
		}
		hasOpenID = hasOpenID || part == "openid"
	}
	return hasOpenID
}

func validOIDCAuthorizationURI(value string) bool {
	if len(value) > maxOIDCAuthorizationValue || !utf8.ValidString(value) {
		return false
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil &&
		parsed.Opaque == "" && parsed.Fragment == ""
}

func setOIDCPostHeaders(writer http.ResponseWriter, origin string) {
	writer.Header().Set(
		"Content-Security-Policy",
		"default-src 'none'; base-uri 'none'; form-action "+origin+
			"; frame-ancestors 'none'; script-src '"+oidcAutoSubmitScriptHash+"'",
	)
}

func validOIDCCallbackText(value string, maxBytes int) bool {
	return value != "" && len(value) <= maxBytes && utf8.ValidString(value) &&
		!strings.ContainsFunc(value, unicode.IsControl)
}

func admitAttempt(
	writer http.ResponseWriter,
	request *http.Request,
	attempts AttemptAdmitter,
	clientKeys ClientKeyResolver,
	attempt BrowserAttempt,
) bool {
	clientKey, err := clientKeys.ResolveClientKey(request)
	if err != nil {
		writeProblem(writer, http.StatusServiceUnavailable)
		return false
	}
	attempt.ClientKey = clientKey
	if err := attempt.Validate(); err != nil {
		writeProblem(writer, http.StatusServiceUnavailable)
		return false
	}
	retryAfter, err := attempts.Admit(request.Context(), attempt)
	if err == nil {
		return true
	}
	if errors.Is(err, ErrAttemptLimited) {
		setRetryAfter(writer, retryAfter)
		writeProblem(writer, http.StatusTooManyRequests)
		return false
	}
	writeProblem(writer, http.StatusServiceUnavailable)
	return false
}

func resetSession(
	writer http.ResponseWriter,
	request *http.Request,
	flow Flow,
	cookies *CookiePolicy,
) bool {
	sessionID, err := cookies.Read(request)
	if errors.Is(err, ErrSessionCookieMissing) {
		return true
	}
	if errors.Is(err, ErrSessionCookieInvalid) {
		if clearErr := cookies.Clear(writer); clearErr != nil {
			writeProblem(writer, http.StatusServiceUnavailable)
			return false
		}
		return true
	}
	if err != nil {
		writeProblem(writer, http.StatusServiceUnavailable)
		return false
	}
	if _, err := flow.Logout(request.Context(), sessionID); err != nil {
		_ = cookies.Clear(writer)
		writeProblem(writer, http.StatusServiceUnavailable)
		return false
	}
	return true
}

func writeOIDCFlowError(writer http.ResponseWriter, err error) {
	if errors.Is(err, browserapp.ErrInvalidRequest) {
		writeProblem(writer, http.StatusBadRequest)
		return
	}
	writeProblem(writer, http.StatusServiceUnavailable)
}

type rejectedOIDCVerifier struct{}

func (rejectedOIDCVerifier) Verify(
	context.Context,
	browserflow.VerificationContext,
) (browserflow.VerifiedAssertion, error) {
	return browserflow.VerifiedAssertion{}, errOIDCCallbackRejected
}
