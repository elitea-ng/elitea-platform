package browserauth

// The Form password prompt (ADR-0059 decision 5, path 1 step 4; story R1.10b,
// issue 2594).
//
// A route that needs a recent sign-in answers 401 `reauthentication_required`
// (apimw.RecentAuthentication). On a Form deployment the console then sends
// the browser to
//
//	GET /auth/form/reauthenticate?target_to=<same-origin path>
//
// a server-rendered step of the Form sign-in pages: the same card, asking only
// for the password of the login the session already holds. The POST verifies
// it with the sign-in's own verifier, counted by the sign-in's own credential
// limiter (the same stage and the same login digest), and on success rotates
// the session with a new authentication time and returns to the target. A
// wrong password leaves the session untouched and shows the prompt again.
//
// A browser with no live Form session is sent to the ordinary sign-in with the
// same target: a fresh sign-in is a password entry too.
//
// The POST is bound to the session that rendered it by a token derived from
// the session identifier (formReauthenticationToken), so a cross-site form
// cannot spend the person's attempts. The rotation on success makes the token
// single use.

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"errors"
	"html/template"
	"mime"
	"net/http"
	"net/url"

	browserapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/browserauth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/browserflow"
)

// FormReauthenticatePath is the password prompt, relative to BasePath.
const FormReauthenticatePath = "/form/reauthenticate"

// formReauthenticationTokenDomain separates the prompt's token from every
// other digest of a session identifier.
const formReauthenticationTokenDomain = "form-reauthenticate\x00"

//go:embed templates/reauthenticate.html
var reauthenticateTemplateSource string

// reauthenticatePage is the prompt template's data.
type reauthenticatePage struct {
	Login  string
	Target string
	Token  string
	Error  bool
	Style  template.CSS
	Script template.JS
	Brand  loginBrand
}

// formReauthenticationToken binds a rendered prompt to the session that
// rendered it. It is a one-way digest of the session identifier, which has
// 256 bits of entropy, so it reveals nothing of the cookie and cannot be
// produced without it.
func formReauthenticationToken(sessionID string) string {
	digest := sha256.Sum256([]byte(formReauthenticationTokenDomain + sessionID))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// renderReauthenticate answers GET /auth/form/reauthenticate.
func (h *Handler) renderReauthenticate(writer http.ResponseWriter, request *http.Request) {
	securityHeaders(writer)
	target := queryReturnTarget(request.URL.Query(), h.defaultLoginTarget)
	sessionID, err := h.cookies.Read(request)
	if err != nil {
		h.redirectSignIn(writer, request, target)
		return
	}
	subject, err := h.flow.ReauthenticationSubject(request.Context(), sessionID)
	if err != nil {
		h.reauthenticationSubjectFailure(writer, request, target, err)
		return
	}

	brand := h.loginBrand(request.Context())
	var body bytes.Buffer
	if err := h.reauthenticateTemplate.Execute(&body, reauthenticatePage{
		Login:  subject.Login,
		Target: target,
		Token:  formReauthenticationToken(sessionID),
		Error:  hasQueryKey(request.URL.Query(), "error"),
		Style:  authPageStyle(),
		Script: authPageScript(),
		Brand:  brand,
	}); err != nil {
		writeProblem(writer, http.StatusServiceUnavailable)
		return
	}
	writer.Header().Set("Content-Security-Policy", authPageCSP("'self'", brand.StyleSource))
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(body.Bytes())
}

// submitReauthenticate answers POST /auth/form/reauthenticate.
func (h *Handler) submitReauthenticate(writer http.ResponseWriter, request *http.Request) {
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
	if len(request.PostForm) != 4 {
		writeProblem(writer, http.StatusBadRequest)
		return
	}
	rawTarget, targetOK := singleValue(request.PostForm, "target")
	token, tokenOK := singleValue(request.PostForm, "token")
	login, loginOK := singleValue(request.PostForm, "login")
	password, passwordOK := singleValue(request.PostForm, "password")
	target, targetErr := browserflow.CanonicalReturnTarget(rawTarget)
	if !targetOK || !tokenOK || !loginOK || !passwordOK || targetErr != nil ||
		!validFormText(password, maxPasswordBytes) || len(token) > 64 || len(login) > browserflow.MaxProviderReferenceBytes {
		writeProblem(writer, http.StatusBadRequest)
		return
	}

	sessionID, err := h.cookies.Read(request)
	if err != nil {
		h.redirectSignIn(writer, request, target)
		return
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(formReauthenticationToken(sessionID))) != 1 {
		// Not the prompt this session rendered: a stale page (the session
		// rotated in another tab) or a cross-site form. Nothing is counted
		// or verified; the prompt is shown again for the current session.
		h.redirectReauthenticate(writer, request, target, false)
		return
	}
	subject, err := h.flow.ReauthenticationSubject(request.Context(), sessionID)
	if err != nil {
		h.reauthenticationSubjectFailure(writer, request, target, err)
		return
	}
	if subtle.ConstantTimeCompare([]byte(login), []byte(subject.Login)) != 1 {
		h.redirectReauthenticate(writer, request, target, false)
		return
	}
	// The sign-in's own limiter: the same stage and the same login digest,
	// so a re-entry and a sign-in share one budget per login. Admitted
	// BEFORE the password is verified, so a wrong one is counted.
	if !h.admit(writer, request, BrowserAttempt{
		Stage:       BrowserAttemptFormCredential,
		LoginDigest: sha256.Sum256([]byte(subject.Login)),
	}) {
		return
	}
	result, err := h.flow.Reauthenticate(request.Context(), sessionID, subject.Login,
		h.form.NewVerifier(subject.Login, password))
	if err != nil {
		switch {
		case errors.Is(err, browserapp.ErrUnauthenticated):
			// A wrong password: the session is unchanged; ask again.
			h.redirectReauthenticate(writer, request, target, true)
		case errors.Is(err, browserapp.ErrAuthenticationExpired):
			h.redirectSignIn(writer, request, target)
		case errors.Is(err, browserapp.ErrInvalidRequest):
			writeProblem(writer, http.StatusBadRequest)
		default:
			writeProblem(writer, http.StatusServiceUnavailable)
		}
		return
	}
	if result.SessionID == sessionID || browserflow.ValidateOpaqueID(result.SessionID) != nil {
		_ = h.cookies.Clear(writer)
		writeProblem(writer, http.StatusServiceUnavailable)
		return
	}
	if err := h.cookies.Set(writer, result.SessionID); err != nil {
		_ = h.cookies.Clear(writer)
		writeProblem(writer, http.StatusServiceUnavailable)
		return
	}
	http.Redirect(writer, request, target, http.StatusFound)
}

// reauthenticationSubjectFailure answers a prompt for a session that cannot
// re-enter a password: none, expired, inactive, or not a Form session. The
// remedy for all of them is the ordinary sign-in, with the same target.
func (h *Handler) reauthenticationSubjectFailure(
	writer http.ResponseWriter, request *http.Request, target string, err error,
) {
	switch {
	case errors.Is(err, browserapp.ErrUnauthenticated),
		errors.Is(err, browserapp.ErrAuthenticationExpired),
		errors.Is(err, browserapp.ErrInvalidRequest):
		h.redirectSignIn(writer, request, target)
	default:
		writeProblem(writer, http.StatusServiceUnavailable)
	}
}

// redirectSignIn sends the browser to the ordinary Form sign-in for target.
func (h *Handler) redirectSignIn(writer http.ResponseWriter, request *http.Request, target string) {
	query := url.Values{"target_to": {target}}
	http.Redirect(writer, request, BasePath+LoginPath+"?"+query.Encode(), http.StatusFound)
}

// redirectReauthenticate shows the prompt again for target.
func (h *Handler) redirectReauthenticate(
	writer http.ResponseWriter, request *http.Request, target string, failed bool,
) {
	query := url.Values{"target_to": {target}}
	if failed {
		query.Set("error", "true")
	}
	http.Redirect(writer, request, BasePath+FormReauthenticatePath+"?"+query.Encode(), http.StatusFound)
}
