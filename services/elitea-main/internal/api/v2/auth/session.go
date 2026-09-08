package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browserflow"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browsersession"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// sessionUsers is the one read Info makes. *pgxpool.Pool satisfies it. The
// interface exists so a test can supply a store that fails. This handler must
// not report that state as "you are not authenticated".
type sessionUsers interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type SessionHandler struct {
	users         sessionUsers
	secretKey     string
	secureCookies bool
	// sessions is the server-side session store (migrations/shared/0117). Nil
	// on a deployment that has none, in which case this handler reads only the
	// legacy signed cookie, exactly as it always did.
	sessions *browsersession.Manager
	// rejectLegacy ends the legacy-cookie window. See browsersession's package
	// header for why the default is off.
	rejectLegacy bool
}

func NewSessionHandler(pool *pgxpool.Pool, secretKey string) *SessionHandler {
	handler := &SessionHandler{secretKey: secretKey, secureCookies: os.Getenv("COOKIE_SECURE") != "false"}
	// A nil *pgxpool.Pool in an interface field is not a nil interface, and
	// every later nil test then passes while the call panics.
	if pool != nil {
		handler.users = pool
	}
	return handler
}

// WithSessionManager gives the handler the server-side session store.
//
// It is a setter rather than a constructor parameter because the manager is
// built where the pool and the policy are — one place — while this handler is
// built in the single-sign-on block above it. A nil manager leaves the handler
// exactly as it was.
func (h *SessionHandler) WithSessionManager(manager *browsersession.Manager) *SessionHandler {
	if h == nil || manager == nil {
		return h
	}
	h.sessions = manager
	h.rejectLegacy = manager.Policy().RejectLegacyCookies
	return h
}

// SessionManager reports the manager this handler holds, so the OIDC and SAML
// planes can mint through the SAME one rather than each composing its own.
func (h *SessionHandler) SessionManager() *browsersession.Manager {
	if h == nil {
		return nil
	}
	return h.sessions
}

func (h *SessionHandler) Logout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	// REVOKE BEFORE CLEARING. Deleting the browser's copy of the cookie is not
	// a sign-out: any other holder of the same value keeps using it until the
	// absolute deadline. The row is what ends the session, and a revocation
	// that fails must still clear the cookie — the user asked to sign out, and
	// leaving them signed in because a write failed is the wrong answer.
	if cookie, err := r.Cookie(browsersession.CookieName); err == nil && cookie.Value != "" {
		if revokeErr := h.sessions.Revoke(r.Context(), cookie.Value); revokeErr != nil {
			slog.Error("logout could not revoke the browser session", "err", revokeErr)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "elitea_session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   h.secureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})

	target := "/"
	if canonical, err := browserflow.CanonicalReturnTarget(r.URL.Query().Get("target_to")); err == nil {
		target = canonical
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// Info reports whether the browser holds a usable session.
//
// The answer has three outcomes, and they must stay apart. `authenticated:
// false` is a statement about the CALLER: there is no session, or the session
// is not usable. The web app acts on it by sending the user to the identity
// provider. A database that cannot answer says nothing about the caller, so
// reporting it as `authenticated: false` signs out a user who holds a valid,
// unexpired cookie, and the identity provider bounces the browser straight
// back. A 503 lets the app keep the session and retry.
func (h *SessionHandler) Info(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(browsersession.CookieName)
	if err != nil || cookie.Value == "" {
		// NO CREDENTIAL AT ALL. This is not an expiry: nobody was signed in,
		// and the app shell shows its sign-in affordance rather than
		// navigating. It keeps the 200 it always answered.
		writeSessionJSON(w, http.StatusOK, map[string]any{"authenticated": false})
		return
	}

	userID, email, refusal := h.resolveCookie(r, cookie.Value)
	switch refusal {
	case sessionRefusalNone:
	case sessionRefusalUnavailable:
		// The store did not answer. See the doc comment: a 503 lets the app
		// keep the session and retry.
		w.Header().Set("Retry-After", "5")
		writeSessionJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": map[string]any{
				"code": "session_store_unavailable", "message": "session store unavailable"}})
		return
	case sessionRefusalNotConfigured:
		slog.Error("session info: the handler has no user store")
		writeSessionJSON(w, http.StatusInternalServerError,
			map[string]any{"error": map[string]any{
				"code": "session_service_not_configured", "message": "session service is not configured"}})
		return
	default:
		writeSessionExpired(w, r)
		return
	}

	if h.users == nil {
		// Composition error, not an outage and not a missing session. The
		// OIDC branch of main.go always passes a pool.
		slog.Error("session info: the handler has no user store")
		writeSessionJSON(w, http.StatusInternalServerError,
			map[string]any{"error": map[string]any{
				"code": "session_service_not_configured", "message": "session service is not configured"}})
		return
	}

	var activeUserID int64
	if err := h.users.QueryRow(r.Context(),
		`SELECT id FROM public.auth_core__user WHERE id = $1 AND suspended = false`,
		userID,
	).Scan(&activeUserID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The user is absent or suspended. That IS an answer about the
			// caller, and the caller HELD a session, so it is an expiry: the
			// browser must go back to the identity provider, not sit on a
			// screen it can no longer load.
			writeSessionExpired(w, r)
			return
		}
		slog.Error("session info: the user lookup failed", "err", err)
		w.Header().Set("Retry-After", "5")
		writeSessionJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": map[string]any{
				"code": "session_store_unavailable", "message": "session store unavailable"}})
		return
	}

	writeSessionJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"user_id":       strconv.FormatInt(activeUserID, 10),
		"email":         email,
	})
}

// sessionRefusal names what resolveCookie found. The three that are not a
// plain refusal are separated because each takes a different status.
type sessionRefusal int

const (
	sessionRefusalNone sessionRefusal = iota
	// sessionRefusalExpired covers every way a PRESENTED cookie fails to name
	// a usable session: unknown, revoked, past its absolute deadline, idle,
	// unparseable, or a legacy cookie this deployment no longer accepts. The
	// app shell acts identically on all of them, so they share one code.
	sessionRefusalExpired
	sessionRefusalUnavailable
	sessionRefusalNotConfigured
)

// resolveCookie reads whichever of the two cookie formats arrived.
//
// The PREFIX decides, not a parse of both. browsersession.LooksServerSide is
// the single discriminator; see its doc comment for why the two formats cannot
// be confused.
func (h *SessionHandler) resolveCookie(
	r *http.Request, value string,
) (userID int64, email string, refusal sessionRefusal) {
	if h.sessions != nil && browsersession.LooksServerSide(value) {
		session, err := h.sessions.Validate(r.Context(), value)
		switch {
		case err == nil:
			return session.UserID, session.Email, sessionRefusalNone
		case errors.Is(err, browsersession.ErrNotFound),
			errors.Is(err, browsersession.ErrMalformedCookie),
			errors.Is(err, browsersession.ErrRevoked),
			errors.Is(err, browsersession.ErrExpired),
			errors.Is(err, browsersession.ErrIdle):
			return 0, "", sessionRefusalExpired
		default:
			slog.Error("session info: the browser session store could not be read", "err", err)
			return 0, "", sessionRefusalUnavailable
		}
	}
	if h.rejectLegacy {
		return 0, "", sessionRefusalExpired
	}
	claims, err := h.parseSessionToken(value)
	if err != nil {
		return 0, "", sessionRefusalExpired
	}
	id, ok := sessionClaimUserID(claims)
	if !ok {
		return 0, "", sessionRefusalExpired
	}
	address, _ := claims["email"].(string)
	return id, address, sessionRefusalNone
}

// SessionExpiredCode is the ONE string the app shell acts on.
//
// It is exported so that a test in another package — and any future reader of
// this contract — names the same value the handler writes. The shell redirects
// to the login start on this code and on nothing else: a 401 from the
// notification bell or any other peripheral call must not move the browser
// (see apps/elitea-web/src/shared/api/http.ts, and the login loop that a
// peripheral 401 caused once already).
const SessionExpiredCode = "session_expired"

// writeSessionExpired is the expiry contract: 401, the code above, and where
// to send the browser.
//
// THE HINT IS A HINT, NOT A REDIRECT. A 401 with a Location header is not
// followed by any browser, and this response is read by an XMLHttpRequest in
// any case. Both the header and the body field carry the same URL so that a
// caller can use whichever it already reads.
//
// `target_to` is preserved when the probe carried one. The value goes through
// browserflow.CanonicalReturnTarget first: it is caller-supplied, and this URL
// ends up in a navigation.
func writeSessionExpired(w http.ResponseWriter, r *http.Request) {
	target := "/"
	if canonical, err := browserflow.CanonicalReturnTarget(r.URL.Query().Get("target_to")); err == nil {
		target = canonical
	}
	loginURL := "/forward-auth/login?target_to=" + url.QueryEscape(target)
	w.Header().Set("Location", loginURL)
	writeSessionJSON(w, http.StatusUnauthorized, map[string]any{
		"authenticated": false,
		"error": map[string]any{
			"code":    SessionExpiredCode,
			"message": "the browser session is no longer valid",
		},
		"login_url": loginURL,
	})
}

func (h *SessionHandler) parseSessionToken(token string) (map[string]any, error) {
	return verifySessionToken(h.secretKey, token)
}

func sessionClaimUserID(claims map[string]any) (int64, bool) {
	switch value := claims["uid"].(type) {
	case string:
		id, err := strconv.ParseInt(value, 10, 64)
		return id, err == nil && id > 0
	case float64:
		id := int64(value)
		return id, value == float64(id) && id > 0
	default:
		return 0, false
	}
}

func makeSessionToken(secretKey, userID, email string) string {
	payload := map[string]any{
		"uid":   userID,
		"email": email,
		"exp":   time.Now().Add(24 * time.Hour).Unix(),
	}
	payloadBytes, _ := json.Marshal(payload)
	encoded := base64.RawURLEncoding.EncodeToString(payloadBytes)

	mac := hmac.New(sha256.New, []byte(secretKey))
	mac.Write([]byte(encoded))
	sig := hex.EncodeToString(mac.Sum(nil))

	return encoded + "." + sig
}

func verifySessionToken(secretKey, token string) (map[string]any, error) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid token format")
	}

	mac := hmac.New(sha256.New, []byte(secretKey))
	mac.Write([]byte(parts[0]))
	expectedSig := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(parts[1]), []byte(expectedSig)) {
		return nil, fmt.Errorf("invalid signature")
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, err
	}

	var claims map[string]any
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return nil, err
	}

	exp, ok := claims["exp"].(float64)
	if !ok || exp != float64(int64(exp)) {
		return nil, fmt.Errorf("session expiration is missing or invalid")
	}
	if time.Now().Unix() > int64(exp) {
		return nil, fmt.Errorf("token expired")
	}

	return claims, nil
}

func writeSessionJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
