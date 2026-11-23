package admin

import (
	"net/http"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess/rolegrants"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/rolepolicy"
)

// grantOrigin is the history origin of a role change made through an admin
// route: the caller is the actor, and the source is `console` for a browser
// session and `api` for any other credential (a token). A caller always has a
// principal here: rolepolicy.ResolveCaller refuses a request without one
// before any write.
func grantOrigin(r *http.Request, caller rolepolicy.Caller) rolegrants.Origin {
	source := rolegrants.SourceAPI
	if principal, ok := auth.UserFromContext(r.Context()); ok && browserSession(r, principal) {
		source = rolegrants.SourceConsole
	}
	return rolegrants.Origin{ActorUserID: caller.UserID, Source: source}
}

// browserSession reports whether the principal signed in through a browser.
// Until the credential kind reaches the principal (R1.2, #2506) it is read
// from what each browser plane sets:
//
//   - the server-side session reader sets a positive BrowserSessionRef;
//   - the Form plane and the session-cookie path set AuthType "session" and no
//     token id, and the authentication middleware records the session source.
//
// A token principal carries a TokenID, and the middleware records a token or
// API-key source for it, so it is never a browser session.
func browserSession(r *http.Request, principal auth.User) bool {
	if principal.BrowserSessionRef > 0 {
		return true
	}
	if principal.TokenID != "" || principal.AuthType != "session" {
		return false
	}
	source, known := auth.AuthenticationSourceFromContext(r.Context())
	return !known || source == auth.AuthenticationSourceSession
}
