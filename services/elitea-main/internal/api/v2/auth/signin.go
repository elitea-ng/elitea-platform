package auth

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

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

// ensurePersonalProject asks for the caller's personal project at SIGN-IN and
// waits a BOUNDED moment for it.
//
// WHY HERE AND NOT ONLY ON THE READ PATH. `GET /social/author` and the `/llm`
// project resolver already ask for it, and both are lazy: the project appears
// only once the browser or the SDK happens to call one of them. A new user
// whose first screen does not is left with no personal project, which is the
// state the onboarding screen waits on forever. A login is the one moment
// every account passes through exactly once, so asking here makes the project
// a consequence of signing in rather than of which page loaded first.
//
// WHY IT WAITS AT ALL (issue 843). It used to ask and walk away. The ensurer
// runs one provisioning attempt at a time, so a burst of first logins — a team
// onboarded together — left every login but one with the work merely QUEUED,
// and the browser reached the product before its project existed. Waiting a
// few seconds here costs nothing on a quiet deployment, where the attempt this
// login started is the only one running, and it is where the wait belongs: the
// browser is between the identity provider and the redirect, with nothing on
// screen to invalidate.
//
// IT NEVER BLOCKS THE LOGIN. The wait is bounded by `wait`
// (defaultSignInProvisionWait when zero), it never cancels the provisioning —
// see personalproject.EnsureStarted — and it changes nothing about the
// response. A login that outruns the bound continues, and the first
// `GET /social/author` afterwards asks again and waits again, exactly as it
// did before this function waited at all.
func ensurePersonalProject(ensurer personalproject.AsyncEnsurer, userID string, wait time.Duration) {
	if ensurer == nil {
		return
	}
	id, err := strconv.ParseInt(userID, 10, 64)
	if err != nil || id <= 0 {
		return
	}
	// The fire-and-forget interface is the one both request-path consumers
	// hold, so a composition that supplies something narrower than the real
	// ensurer keeps working — it simply does not wait.
	awaitable, canWait := ensurer.(startedEnsurer)
	if !canWait {
		ensurer.EnsureAsync(id)
		return
	}
	done := awaitable.EnsureStarted(id)
	if done == nil {
		// Refused outright: the queue is full to its bound. There is nothing to
		// wait for, and the account is not marked, so the next authenticated
		// request queues it again.
		return
	}
	if wait <= 0 {
		wait = defaultSignInProvisionWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

// startedEnsurer is the completion-aware half of *personalproject.Ensurer,
// declared at the consumer.
//
// The handler fields stay `personalproject.AsyncEnsurer` because that is what
// the composition root hands every consumer of "the caller's personal
// project", and internal/api/middleware's project resolver wants exactly the
// fire-and-forget method. internal/api/v2/social declares the same shape for
// the same reason.
type startedEnsurer interface {
	EnsureStarted(userID int64) <-chan struct{}
}

// defaultSignInProvisionWait bounds the wait above.
//
// FIVE SECONDS, and the number is a browser budget rather than a provisioning
// one. The person is looking at the identity provider's redirect, so a few
// seconds is invisible; a provisioning run that applies the whole tenant
// migration corpus can take far longer than this, and waiting for THAT would
// hold the callback open long enough for a load balancer to cut it. What the
// wait buys is the common case — an idle deployment, where the attempt this
// login started is the only one and finishes well inside the bound.
const defaultSignInProvisionWait = 5 * time.Second

// sessionUserID converts the id a provisioning path returns into the numeric
// one a session row holds. A value that is not a positive integer is a
// provisioning defect, and minting a session for it would create a row that
// names no account.
func sessionUserID(userID string) (int64, bool) {
	id, err := strconv.ParseInt(userID, 10, 64)
	return id, err == nil && id > 0
}
