package nativeauth

import (
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/browserauth"
	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/nativeauth"
)

var (
	codeChallengePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	clientVersionPattern = regexp.MustCompile(`^[0-9A-Za-z.+-]{0,32}$`)
)

// The parameters /authorize reads. `scope`, `nonce` and `prompt` are ignored in
// v1; `prompt=login` and `max_age` are not supported (coordinator decision 12).
var authorizeParameters = []string{
	"response_type", "client_id", "redirect_uri", "code_challenge", "code_challenge_method",
	"state", "device_name", "platform", "client_version",
}

// authorize validates an authorization request in RFC 6749 §4.1.2.1 order:
// nothing that could be an open redirect ever redirects.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	// The three parameters the redirect itself depends on: a repeat of any of
	// them is a page, never a redirect.
	for _, name := range []string{"client_id", "redirect_uri", "state"} {
		if len(query[name]) > 1 {
			h.errorPage(w, r, http.StatusBadRequest, "The sign-in request repeats the parameter "+name+".")
			return
		}
	}
	clientID, redirectURI := query.Get("client_id"), query.Get("redirect_uri")
	client, active, err := h.cfg.Registry.Active(r.Context(), clientID)
	if err != nil {
		slog.ErrorContext(r.Context(), "native authorization: client registry unavailable", "err", err)
		h.errorPage(w, r, http.StatusServiceUnavailable, "The sign-in service is temporarily unavailable.")
		return
	}
	if !active {
		h.errorPage(w, r, http.StatusBadRequest, "This app is not registered with this server.")
		return
	}
	if redirectURI == "" {
		h.errorPage(w, r, http.StatusBadRequest, "The sign-in request names no return address for the app.")
		return
	}
	if _, ok := domain.MatchingRedirect(client.RedirectURIs, redirectURI); !ok {
		h.errorPage(w, r, http.StatusBadRequest, "The app's return address is not registered with this server.")
		return
	}
	if h.cfg.PublicOrigin == "" {
		slog.ErrorContext(r.Context(),
			"native authorization refused: DEPLOYMENT_URL is not set, so no verifiable issuer exists")
		h.errorPage(w, r, http.StatusServiceUnavailable,
			"This server is missing its public address configuration.")
		return
	}
	state := query.Get("state")
	if !validState(state) {
		h.errorPage(w, r, http.StatusBadRequest, "The sign-in request carries no valid state.")
		return
	}

	redirectError := func(code, description string) {
		http.Redirect(w, r, h.redirectWith(redirectURI, url.Values{
			"error": {code}, "error_description": {description}, "state": {state},
		}), http.StatusFound)
	}
	for _, name := range authorizeParameters {
		if len(query[name]) > 1 {
			redirectError("invalid_request", name+" is repeated")
			return
		}
	}
	if query.Get("response_type") != "code" {
		redirectError("unsupported_response_type", "only response_type=code is supported")
		return
	}
	if query.Get("code_challenge_method") != "S256" {
		redirectError("invalid_request", "code_challenge_method must be S256")
		return
	}
	challenge := query.Get("code_challenge")
	if !codeChallengePattern.MatchString(challenge) {
		redirectError("invalid_request", "code_challenge must be 43 base64url characters")
		return
	}
	deviceName := strings.TrimSpace(query.Get("device_name"))
	if !validDeviceName(deviceName) {
		redirectError("invalid_request", "device_name must be 1 to 64 bytes without control characters")
		return
	}
	platform := query.Get("platform")
	if !domain.ValidPlatform(platform) {
		redirectError("invalid_request", "platform must be ios, android, macos, windows, linux or other")
		return
	}
	clientVersion := query.Get("client_version")
	if !clientVersionPattern.MatchString(clientVersion) {
		redirectError("invalid_request", "client_version is malformed")
		return
	}

	address, known := h.address(r)
	key := addressKey(address, known)
	if blocked, _ := h.authorizations.Blocked(key); blocked {
		redirectError("temporarily_unavailable", "too many sign-in requests; retry later")
		return
	}
	pending, err := h.pending(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "native authorization: pending count unavailable", "err", err)
		redirectError("temporarily_unavailable", "the sign-in service is temporarily unavailable")
		return
	}
	if pending >= pendingCeiling {
		redirectError("temporarily_unavailable", "too many sign-in requests are pending; retry later")
		return
	}
	handle, binder, err := h.cfg.Store.CreateAuthorization(r.Context(), domain.AuthorizationRequest{
		ClientID: client.ClientID, RedirectURI: redirectURI, CodeChallenge: challenge, State: state,
		DeviceName: deviceName, Platform: platform, ClientVersion: clientVersion,
	})
	if err != nil {
		slog.ErrorContext(r.Context(), "native authorization: request not stored", "err", err)
		redirectError("temporarily_unavailable", "the sign-in service is temporarily unavailable")
		return
	}
	h.authorizations.Fail(key)
	h.setBinder(w, binder)
	http.Redirect(w, r, ContinuePath+"?"+url.Values{"request": {handle}}.Encode(), http.StatusFound)
}

// redirectWith appends the response parameters and `iss` (RFC 9207) to the
// registered redirect URI, which carries no query of its own.
func (h *Handler) redirectWith(redirectURI string, values url.Values) string {
	values.Set("iss", h.cfg.PublicOrigin)
	return redirectURI + "?" + values.Encode()
}

func validState(state string) bool {
	if state == "" || len(state) > 512 {
		return false
	}
	for index := 0; index < len(state); index++ {
		if state[index] < 0x20 || state[index] > 0x7e {
			return false
		}
	}
	return true
}

func validDeviceName(name string) bool {
	return name != "" && len(name) <= 64 && utf8.ValidString(name) &&
		!strings.ContainsFunc(name, unicode.IsControl)
}

/* ── continue: resume after the existing browser sign-in ─────────────────── */

// loadBound loads the authorization the request names and requires the binder
// cookie set by /authorize in THIS browser. ok is false when a page was
// written.
func (h *Handler) loadBound(w http.ResponseWriter, r *http.Request, handle string) (domain.Authorization, bool) {
	authorization, err := h.cfg.Store.AuthorizationByHandle(r.Context(), handle)
	if errors.Is(err, domain.ErrNotFound) || (err == nil && !authorization.BinderMatches(h.binder(r))) {
		h.errorPage(w, r, http.StatusBadRequest,
			"This sign-in was started in a different browser, or it is no longer valid.")
		return domain.Authorization{}, false
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "native authorization: request unreadable", "err", err)
		h.errorPage(w, r, http.StatusServiceUnavailable, "The sign-in service is temporarily unavailable.")
		return domain.Authorization{}, false
	}
	return authorization, true
}

func (h *Handler) continueRoute(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if len(query["request"]) != 1 {
		h.errorPage(w, r, http.StatusBadRequest, "The sign-in request is missing.")
		return
	}
	handle := query.Get("request")
	authorization, ok := h.loadBound(w, r, handle)
	if !ok {
		return
	}
	if authorization.Expired(h.now()) || authorization.Status != "pending" {
		h.clearBinder(w)
		http.Redirect(w, r, h.redirectWith(authorization.RedirectURI, url.Values{
			"error": {"access_denied"}, "error_description": {"authorization_expired"},
			"state": {authorization.State},
		}), http.StatusFound)
		return
	}
	h.browser(authorization, handle, func(w http.ResponseWriter, r *http.Request) {
		h.renderConsent(w, r, authorization, handle)
	}).ServeHTTP(w, r)
}

// browser wraps a page in the composed Auth middleware through
// apimw.BrowserSignInRequired: an unauthenticated browser is bounced to the
// deployment's own sign-in with a constant same-origin return path.
func (h *Handler) browser(authorization domain.Authorization, handle string, page http.HandlerFunc) http.Handler {
	if h.cfg.Authenticate == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.errorPage(w, r, http.StatusServiceUnavailable, "Browser sign-in is not configured on this server.")
		})
	}
	return apimw.BrowserSignInRequired(
		h.cfg.Authenticate,
		func(w http.ResponseWriter, r *http.Request) { h.bounceToSignIn(w, r, authorization, handle) },
		func(w http.ResponseWriter, r *http.Request) {
			h.errorPage(w, r, http.StatusBadRequest,
				"A new device can only be approved from a signed-in browser, not with a token.")
		},
	)(page)
}

// bounceToSignIn sends the browser through the deployment's existing sign-in.
// `target_to` is the constant continue path plus the opaque handle: it passes
// every existing same-origin target_to rule unchanged.
func (h *Handler) bounceToSignIn(w http.ResponseWriter, r *http.Request, authorization domain.Authorization, handle string) {
	count, err := h.cfg.Store.BumpLoginRedirects(r.Context(), authorization.ID)
	if errors.Is(err, domain.ErrNotPending) {
		h.errorPage(w, r, http.StatusBadRequest, "This sign-in is no longer valid.")
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "native authorization: sign-in bounce not counted", "err", err)
		h.errorPage(w, r, http.StatusServiceUnavailable, "The sign-in service is temporarily unavailable.")
		return
	}
	if count > maxLoginBounces {
		slog.WarnContext(r.Context(),
			"native authorization: sign-in completed but the browser session is not readable here",
			"bounces", count)
		h.errorPage(w, r, http.StatusBadRequest,
			"Sign-in completed but this server could not read the browser session.")
		return
	}
	target := ContinuePath + "?" + url.Values{"request": {handle}}.Encode()
	http.Redirect(w, r, "/auth/login?"+url.Values{"target_to": {target}}.Encode(), http.StatusFound)
}

func (h *Handler) renderConsent(w http.ResponseWriter, r *http.Request, authorization domain.Authorization, handle string) {
	user, _ := auth.UserFromContext(r.Context())
	userID, ok := user.OwningUserID()
	if !ok {
		h.errorPage(w, r, http.StatusBadRequest, "The signed-in account could not be identified.")
		return
	}
	client, _, err := h.cfg.Registry.Active(r.Context(), authorization.ClientID)
	if err != nil {
		h.errorPage(w, r, http.StatusServiceUnavailable, "The sign-in service is temporarily unavailable.")
		return
	}
	name := client.DisplayName
	if name == "" {
		name = authorization.ClientID
	}
	target := ContinuePath + "?" + url.Values{"request": {handle}}.Encode()
	h.cfg.Pages.RenderConsent(w, r, browserauth.NativeConsent{
		ClientName:       name,
		ClientID:         authorization.ClientID,
		Email:            user.Email,
		DeviceName:       authorization.DeviceName,
		Platform:         authorization.Platform,
		Origin:           h.cfg.PublicOrigin,
		Action:           DecisionPath,
		Request:          handle,
		UserID:           strconv.FormatInt(userID, 10),
		SwitchAccountURL: "/auth/logout?" + url.Values{"target_to": {target}}.Encode(),
		FormAction:       domain.FormActionSource(authorization.RedirectURI),
	})
}

/* ── decision ───────────────────────────────────────────────────────────── */

func (h *Handler) decision(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		h.errorPage(w, r, http.StatusUnsupportedMediaType, "The answer was not sent as a form.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		h.errorPage(w, r, http.StatusBadRequest, "The answer could not be read.")
		return
	}
	form := r.PostForm
	for _, name := range []string{"request", "uid", "decision"} {
		if len(form[name]) != 1 {
			h.errorPage(w, r, http.StatusBadRequest, "The answer is incomplete.")
			return
		}
	}
	// Defence in depth on top of the SameSite=Lax session cookie a cross-site
	// POST does not carry. A missing Origin is allowed; "null" is not.
	if origin, present := r.Header["Origin"]; present &&
		(len(origin) != 1 || origin[0] != h.cfg.PublicOrigin) {
		h.errorPage(w, r, http.StatusForbidden, "The answer did not come from this server's page.")
		return
	}
	choice := form.Get("decision")
	if choice != "allow" && choice != "deny" {
		h.errorPage(w, r, http.StatusBadRequest, "The answer is not one this page offers.")
		return
	}
	handle := form.Get("request")
	authorization, ok := h.loadBound(w, r, handle)
	if !ok {
		return
	}
	page := func(w http.ResponseWriter, r *http.Request) {
		user, _ := auth.UserFromContext(r.Context())
		userID, resolved := user.OwningUserID()
		if !resolved || strconv.FormatInt(userID, 10) != form.Get("uid") {
			h.errorPage(w, r, http.StatusConflict,
				"The signed-in account changed after this page was shown. No sign-in was approved.")
			return
		}
		decision, err := h.cfg.Store.Decide(r.Context(), authorization.ID, choice == "allow", userID)
		if errors.Is(err, domain.ErrNotPending) {
			h.errorPage(w, r, http.StatusBadRequest, "This sign-in was already answered or has expired.")
			return
		}
		if err != nil {
			slog.ErrorContext(r.Context(), "native authorization: decision not recorded", "err", err)
			h.errorPage(w, r, http.StatusServiceUnavailable, "The sign-in service is temporarily unavailable.")
			return
		}
		action := "Native sign-in denied: " + authorization.ClientID + " on " + authorization.DeviceName
		if choice == "allow" {
			action = "Native sign-in approved: " + authorization.ClientID + " on " + authorization.DeviceName
		}
		audit.Annotate(r.Context(), audit.Annotation{
			Action: action, EntityType: "native_device", EntityName: authorization.DeviceName,
		})
		h.clearBinder(w)
		values := url.Values{"state": {decision.State}}
		if choice == "allow" {
			values.Set("code", decision.Code)
		} else {
			values.Set("error", "access_denied")
		}
		http.Redirect(w, r, h.redirectWith(decision.RedirectURI, values), http.StatusFound)
	}
	h.browser(authorization, handle, apimw.Audit(h.cfg.Audit)(http.HandlerFunc(page)).ServeHTTP).ServeHTTP(w, r)
}
