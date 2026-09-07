package auth

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/personalproject"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browsersession"
)

// issueBrowserSession writes the credential a successful login answers with.
//
// ONE FUNCTION, TWO PLANES. OIDC and SAML both reach it, and before it existed
// each spelled the same cookie out for itself. The two copies had already
// drifted once — nothing but review kept `MaxAge` and `SameSite` in step — and
// with a session ROW to create as well there would have been two places to get
// the provider name, the lifetime and the failure handling right.
//
// A FAILED SESSION CREATE FAILS THE LOGIN. It does not fall back to the legacy
// signed cookie. A deployment that composed a session store means to have
// revocable sessions, and quietly handing out an unrevocable one instead is
// the kind of silent downgrade that is only discovered when somebody needs the
// revocation.
//
// It returns false when it has already answered the browser.
func issueBrowserSession(
	w http.ResponseWriter,
	manager *browsersession.Manager,
	secretKey string,
	secureCookies bool,
	request browsersession.NewSession,
	r *http.Request,
) bool {
	if manager != nil {
		value, err := manager.Create(r.Context(), request)
		if err != nil {
			slog.Error("the browser session could not be created",
				"err", err, "provider", request.Provider, "user_id", request.UserID)
			http.Error(w, "session could not be created", http.StatusInternalServerError)
			return false
		}
		browsersession.SetCookie(w, value, secureCookies, manager.Policy().AbsoluteLifetime)
		return true
	}

	// No store composed. The legacy signed cookie is still the credential, and
	// apimw.Auth still reads it. See browsersession's package header.
	http.SetCookie(w, &http.Cookie{
		Name:     browsersession.CookieName,
		Value:    makeSessionToken(secretKey, strconv.FormatInt(request.UserID, 10), request.Email),
		Path:     "/",
		HttpOnly: true,
		Secure:   secureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400,
	})
	return true
}

// ensurePersonalProject asks for the caller's personal project at SIGN-IN.
//
// WHY HERE AND NOT ONLY ON THE READ PATH. `GET /social/author` and the `/llm`
// project resolver already ask for it, and both are lazy: the project appears
// only once the browser or the SDK happens to call one of them. A new user
// whose first screen does not is left with no personal project, which is the
// state the onboarding screen waits on forever. A login is the one moment
// every account passes through exactly once, so asking here makes the project
// a consequence of signing in rather than of which page loaded first.
//
// It NEVER blocks and never changes this response. Provisioning applies a
// tenant migration corpus; see personalproject.EnsureAsync.
func ensurePersonalProject(ensurer personalproject.AsyncEnsurer, userID string) {
	if ensurer == nil {
		return
	}
	id, err := strconv.ParseInt(userID, 10, 64)
	if err != nil || id <= 0 {
		return
	}
	ensurer.EnsureAsync(id)
}

// sessionUserID converts the id a provisioning path returns into the numeric
// one a session row holds. A value that is not a positive integer is a
// provisioning defect, and minting a session for it would create a row that
// names no account.
func sessionUserID(userID string) (int64, bool) {
	id, err := strconv.ParseInt(userID, 10, 64)
	return id, err == nil && id > 0
}
