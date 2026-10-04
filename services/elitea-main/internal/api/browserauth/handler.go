package browserauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"errors"
	"html/template"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	browserapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/browserauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browserflow"
)

// The browser authentication URLs, relative to BasePath: `/auth/login`,
// `/auth/form/login` and so on. `/auth` itself (no trailing segment) is the
// edge auth check (router.go). The browser routes are below it.
const (
	BasePath = "/auth"

	LoginPath         = "/login"
	LogoutPath        = "/logout"
	FormLoginPath     = "/form/login"
	FormAuthorizePath = "/form/authorize"
	FormLogoutPath    = "/form/logout"

	DefaultMaxFormBodyBytes = int64(8 << 10)
	maxMaxFormBodyBytes     = int64(64 << 10)
	maxPasswordBytes        = 4096
)

var (
	ErrInvalidHandlerConfiguration = errors.New("invalid browser authentication HTTP configuration")
	ErrAttemptLimited              = browserapp.ErrAttemptLimited
)

//go:embed templates/login.html
var loginTemplateSource string

// loginPage is the Form page template's data.
type loginPage struct {
	Target string
	Error  bool
	Style  template.CSS
	Script template.JS
	Brand  loginBrand
}

type Flow interface {
	Begin(context.Context, browserapp.BeginRequest) (browserapp.BeginResult, error)
	Complete(
		context.Context,
		browserapp.CompleteRequest,
		browserapp.AssertionVerifier,
	) (browserapp.CompleteResult, error)
	Logout(context.Context, string) (browserapp.LogoutContext, error)
}

// ClientKeyResolver owns the trusted-proxy policy used for cross-replica rate
// admission. It must not trust forwarded client metadata from an arbitrary
// peer. The handler remains unconstructable without this policy.
type ClientKeyResolver interface {
	ResolveClientKey(*http.Request) (string, error)
}

type BrowserAttempt = browserapp.BrowserAttempt
type BrowserAttemptStage = browserapp.BrowserAttemptStage
type AttemptAdmitter = browserapp.AttemptAdmitter

const (
	BrowserAttemptFormBegin      = browserapp.BrowserAttemptFormBegin
	BrowserAttemptFormCredential = browserapp.BrowserAttemptFormCredential
	BrowserAttemptOIDCBegin      = browserapp.BrowserAttemptOIDCBegin
	BrowserAttemptOIDCCallback   = browserapp.BrowserAttemptOIDCCallback
)

type Config struct {
	DefaultLoginTarget  string
	DefaultLogoutTarget string
	MaxFormBodyBytes    int64
	// Brand supplies the resolved brand pack the login page renders
	// (branding.go). Nil renders the default presentation.
	Brand BrandSource
}

type Handler struct {
	flow                Flow
	form                *browserapp.FormProvider
	attempts            AttemptAdmitter
	clientKeys          ClientKeyResolver
	cookies             *CookiePolicy
	defaultLoginTarget  string
	defaultLogoutTarget string
	maxFormBodyBytes    int64
	loginTemplate       *template.Template
	brand               BrandSource
}

func NewHandler(
	flow Flow,
	form *browserapp.FormProvider,
	attempts AttemptAdmitter,
	clientKeys ClientKeyResolver,
	cookies *CookiePolicy,
	config Config,
) (*Handler, error) {
	if flow == nil || form == nil || attempts == nil || clientKeys == nil || cookies == nil {
		return nil, ErrInvalidHandlerConfiguration
	}
	if config.DefaultLoginTarget == "" {
		config.DefaultLoginTarget = "/"
	}
	if config.DefaultLogoutTarget == "" {
		config.DefaultLogoutTarget = "/"
	}
	if browserflow.ValidateReturnTarget(config.DefaultLoginTarget) != nil ||
		browserflow.ValidateReturnTarget(config.DefaultLogoutTarget) != nil {
		return nil, ErrInvalidHandlerConfiguration
	}
	if config.MaxFormBodyBytes == 0 {
		config.MaxFormBodyBytes = DefaultMaxFormBodyBytes
	}
	if config.MaxFormBodyBytes < 1024 || config.MaxFormBodyBytes > maxMaxFormBodyBytes {
		return nil, ErrInvalidHandlerConfiguration
	}
	loginTemplate, err := parseAuthPage("login.html", loginTemplateSource)
	if err != nil {
		return nil, ErrInvalidHandlerConfiguration
	}
	return &Handler{
		flow:                flow,
		form:                form,
		attempts:            attempts,
		clientKeys:          clientKeys,
		cookies:             cookies,
		defaultLoginTarget:  config.DefaultLoginTarget,
		defaultLogoutTarget: config.DefaultLogoutTarget,
		maxFormBodyBytes:    config.MaxFormBodyBytes,
		loginTemplate:       loginTemplate,
		brand:               config.Brand,
	}, nil
}

// Routes preserves the current Form route paths and effective methods. The
// production Form graph mounts this provider-only router without exposing the
// compatibility Auth Core handler through an ordinary reverse proxy.
func (h *Handler) Routes() chi.Router {
	router := newRouter()
	h.registerRoutes(router)
	return router
}

func (h *Handler) registerRoutes(router chi.Router) {
	h.registerReadRoute(router, LoginPath, h.beginLogin)
	h.registerReadRoute(router, FormLoginPath, h.renderForm)
	router.MethodFunc(http.MethodPost, FormAuthorizePath, h.authorizeForm)
	router.MethodFunc(http.MethodOptions, FormAuthorizePath, options("POST, OPTIONS"))
	h.registerReadRoute(router, LogoutPath, h.beginLogout)
	h.registerReadRoute(router, FormLogoutPath, h.logout)
}

// FormPaths are every route of the Form router (NewFormRoutes), relative to
// BasePath. The composition root registers each one under BasePath.
func FormPaths() []string {
	return []string{AuthPath, LoginPath, LogoutPath, FormLoginPath, FormAuthorizePath, FormLogoutPath}
}

func (h *Handler) registerReadRoute(router chi.Router, path string, handler http.HandlerFunc) {
	router.MethodFunc(http.MethodGet, path, handler)
	router.MethodFunc(http.MethodHead, path, head(handler))
	router.MethodFunc(http.MethodOptions, path, options("GET, HEAD, OPTIONS"))
}

func (h *Handler) beginLogin(writer http.ResponseWriter, request *http.Request) {
	securityHeaders(writer)
	if !h.admit(writer, request, BrowserAttempt{Stage: BrowserAttemptFormBegin}) {
		return
	}
	if !h.resetExistingSession(writer, request) {
		return
	}
	target := queryReturnTarget(request.URL.Query(), h.defaultLoginTarget)
	result, err := h.flow.Begin(request.Context(), browserapp.BeginRequest{
		Provider:     "form",
		ReturnTarget: target,
	})
	if err != nil {
		h.writeFlowError(writer, err)
		return
	}
	if browserflow.ValidateTransactionID(result.TransactionID) != nil ||
		browserflow.ValidateOpaqueID(result.SessionID) != nil || result.ExpiresAt.IsZero() {
		writeProblem(writer, http.StatusServiceUnavailable)
		return
	}
	if err := h.cookies.Set(writer, result.SessionID); err != nil {
		writeProblem(writer, http.StatusServiceUnavailable)
		return
	}
	query := url.Values{"target_to": {result.TransactionID}}
	if _, present := request.URL.Query()["error"]; present {
		query.Set("error", "true")
	}
	http.Redirect(writer, request, BasePath+FormLoginPath+"?"+query.Encode(), http.StatusFound)
}

func (h *Handler) renderForm(writer http.ResponseWriter, request *http.Request) {
	securityHeaders(writer)
	target, ok := singleValue(request.URL.Query(), "target_to")
	if !ok || browserflow.ValidateTransactionID(target) != nil {
		writeProblem(writer, http.StatusBadRequest)
		return
	}
	if _, err := h.cookies.Read(request); err != nil {
		_ = h.cookies.Clear(writer)
		writeProblem(writer, http.StatusBadRequest)
		return
	}

	brand := h.loginBrand(request.Context())
	var body bytes.Buffer
	if err := h.loginTemplate.Execute(&body, loginPage{
		Target: target,
		Error:  hasQueryKey(request.URL.Query(), "error"),
		Style:  authPageStyle(),
		Script: authPageScript(),
		Brand:  brand,
	}); err != nil {
		writeProblem(writer, http.StatusServiceUnavailable)
		return
	}
	// The brand stylesheet's hash joins the static one (branding.go); the
	// theme script is pinned by its own hash (page.go).
	writer.Header().Set("Content-Security-Policy", authPageCSP("'self'", brand.StyleSource))
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(body.Bytes())
}

func (h *Handler) authorizeForm(writer http.ResponseWriter, request *http.Request) {
	securityHeaders(writer)
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		writeProblem(writer, http.StatusUnsupportedMediaType)
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, h.maxFormBodyBytes)
	if err := request.ParseForm(); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeProblem(writer, http.StatusRequestEntityTooLarge)
			return
		}
		writeProblem(writer, http.StatusBadRequest)
		return
	}
	if len(request.PostForm) != 3 {
		writeProblem(writer, http.StatusBadRequest)
		return
	}
	target, targetOK := singleValue(request.PostForm, "target")
	login, loginOK := singleValue(request.PostForm, "login")
	password, passwordOK := singleValue(request.PostForm, "password")
	if !targetOK || !loginOK || !passwordOK ||
		browserflow.ValidateTransactionID(target) != nil || !validFormText(login, browserflow.MaxProviderReferenceBytes) ||
		!validFormText(password, maxPasswordBytes) {
		writeProblem(writer, http.StatusBadRequest)
		return
	}
	sessionID, err := h.cookies.Read(request)
	if err != nil {
		// The session cookie went away between the form render and this POST.
		// That is a recoverable state, not a client defect: send the browser
		// back to beginLogin, which mints a new session and a new transaction.
		// A bare 400 leaves the user on a dead page with no way back.
		_ = h.cookies.Clear(writer)
		h.redirectLoginFailure(writer, request)
		return
	}
	if !h.admit(writer, request, BrowserAttempt{
		Stage:       BrowserAttemptFormCredential,
		LoginDigest: sha256.Sum256([]byte(login)),
	}) {
		return
	}
	verifier := h.form.NewVerifier(login, password)
	result, err := h.flow.Complete(request.Context(), browserapp.CompleteRequest{
		SessionID:     sessionID,
		TransactionID: target,
		Provider:      "form",
	}, verifier)
	if err != nil {
		switch {
		case errors.Is(err, browserapp.ErrUnauthenticated),
			errors.Is(err, browserapp.ErrAuthenticationExpired),
			// A rejected transaction means the rendered form went stale: it
			// expired, it was already consumed, or beginLogin ran again and
			// bound a newer transaction to the session cookie. The SPA makes
			// that routine. An unauthenticated visit to /app/ redirects to
			// the login page from the router AND from the first API call.
			// Two begins therefore race, and the last Set-Cookie wins. The
			// user then submits a form the server no longer accepts. Send
			// them back to beginLogin, which mints a fresh session and
			// transaction, instead of answering a bare "Bad Request" page
			// with no way forward.
			errors.Is(err, browserapp.ErrTransactionRejected):
			h.redirectLoginFailure(writer, request)
		case errors.Is(err, browserapp.ErrInvalidRequest):
			writeProblem(writer, http.StatusBadRequest)
		default:
			writeProblem(writer, http.StatusServiceUnavailable)
		}
		return
	}
	returnTarget, targetErr := browserflow.CanonicalReturnTarget(result.ReturnTarget)
	if targetErr != nil ||
		result.SessionID == sessionID || browserflow.ValidateOpaqueID(result.SessionID) != nil {
		_ = h.cookies.Clear(writer)
		writeProblem(writer, http.StatusServiceUnavailable)
		return
	}
	if err := h.cookies.Set(writer, result.SessionID); err != nil {
		_ = h.cookies.Clear(writer)
		writeProblem(writer, http.StatusServiceUnavailable)
		return
	}
	http.Redirect(writer, request, returnTarget, http.StatusFound)
}

func (h *Handler) admit(writer http.ResponseWriter, request *http.Request, attempt BrowserAttempt) bool {
	return admitAttempt(writer, request, h.attempts, h.clientKeys, attempt)
}

func (h *Handler) resetExistingSession(writer http.ResponseWriter, request *http.Request) bool {
	sessionID, err := h.cookies.Read(request)
	if errors.Is(err, ErrSessionCookieMissing) {
		return true
	}
	if errors.Is(err, ErrSessionCookieInvalid) {
		if clearErr := h.cookies.Clear(writer); clearErr != nil {
			writeProblem(writer, http.StatusServiceUnavailable)
			return false
		}
		return true
	}
	if err != nil {
		writeProblem(writer, http.StatusServiceUnavailable)
		return false
	}
	if _, err := h.flow.Logout(request.Context(), sessionID); err != nil {
		_ = h.cookies.Clear(writer)
		writeProblem(writer, http.StatusServiceUnavailable)
		return false
	}
	return true
}

func (h *Handler) beginLogout(writer http.ResponseWriter, request *http.Request) {
	securityHeaders(writer)
	target := queryReturnTarget(request.URL.Query(), h.defaultLogoutTarget)
	query := url.Values{"target_to": {target}}
	http.Redirect(writer, request, BasePath+FormLogoutPath+"?"+query.Encode(), http.StatusFound)
}

func (h *Handler) logout(writer http.ResponseWriter, request *http.Request) {
	securityHeaders(writer)
	target := queryReturnTarget(request.URL.Query(), h.defaultLogoutTarget)
	sessionID, readErr := h.cookies.Read(request)
	if readErr == nil {
		_, logoutErr := h.flow.Logout(request.Context(), sessionID)
		if clearErr := h.cookies.Clear(writer); clearErr != nil {
			writeProblem(writer, http.StatusServiceUnavailable)
			return
		}
		if logoutErr != nil {
			writeProblem(writer, http.StatusServiceUnavailable)
			return
		}
		http.Redirect(writer, request, target, http.StatusFound)
		return
	}
	if clearErr := h.cookies.Clear(writer); clearErr != nil {
		writeProblem(writer, http.StatusServiceUnavailable)
		return
	}
	if !errors.Is(readErr, ErrSessionCookieMissing) && !errors.Is(readErr, ErrSessionCookieInvalid) {
		writeProblem(writer, http.StatusServiceUnavailable)
		return
	}
	http.Redirect(writer, request, target, http.StatusFound)
}

func (h *Handler) redirectLoginFailure(writer http.ResponseWriter, request *http.Request) {
	http.Redirect(writer, request, BasePath+LoginPath+"?error=true", http.StatusFound)
}

func (h *Handler) writeFlowError(writer http.ResponseWriter, err error) {
	if errors.Is(err, browserapp.ErrInvalidRequest) {
		writeProblem(writer, http.StatusBadRequest)
		return
	}
	writeProblem(writer, http.StatusServiceUnavailable)
}

func queryReturnTarget(values url.Values, fallback string) string {
	value, ok := singleValue(values, "target_to")
	if !ok {
		return fallback
	}
	canonical, err := browserflow.CanonicalReturnTarget(value)
	if err != nil {
		return fallback
	}
	return canonical
}

func singleValue(values url.Values, key string) (string, bool) {
	items, present := values[key]
	return first(items), present && len(items) == 1
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func hasQueryKey(values url.Values, key string) bool {
	_, present := values[key]
	return present
}

func validFormText(value string, maxBytes int) bool {
	return value != "" && len(value) <= maxBytes && utf8.ValidString(value) &&
		!strings.ContainsRune(value, '\x00')
}

func setRetryAfter(writer http.ResponseWriter, retryAfter time.Duration) {
	if retryAfter <= 0 {
		return
	}
	if retryAfter > browserapp.MaxBrowserAttemptRetryAfter {
		retryAfter = browserapp.MaxBrowserAttemptRetryAfter
	}
	seconds := int64((retryAfter + time.Second - 1) / time.Second)
	writer.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
}

func securityHeaders(writer http.ResponseWriter) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Pragma", "no-cache")
	writer.Header().Set("Content-Security-Policy", "default-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; style-src "+authStyleCSPSource)
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("X-Frame-Options", "DENY")
	writer.Header().Set("Server", "Centry")
}

func writeProblem(writer http.ResponseWriter, status int) {
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.WriteHeader(status)
	_, _ = writer.Write([]byte(http.StatusText(status)))
}

func options(allow string) http.HandlerFunc {
	return func(writer http.ResponseWriter, _ *http.Request) {
		securityHeaders(writer)
		writer.Header().Set("Allow", allow)
		writer.WriteHeader(http.StatusOK)
	}
}

func head(handler http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		handler(headResponseWriter{ResponseWriter: writer}, request)
	}
}

type headResponseWriter struct {
	http.ResponseWriter
}

func (writer headResponseWriter) Write(value []byte) (int, error) {
	return len(value), nil
}
