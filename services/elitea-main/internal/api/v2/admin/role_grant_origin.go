package admin

import (
	"net/http"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess/rolegrants"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// grantOrigin is the history origin of a role change made through an admin
// route: the caller is the actor, and the source is `console` for a browser
// session and `api` for any other credential (a token). A caller always has a
// principal here: adminaccess.ResolveCaller refuses a request without one
// before any write.
func grantOrigin(r *http.Request, caller adminaccess.Caller) rolegrants.Origin {
	source := rolegrants.SourceAPI
	if principal, ok := auth.UserFromContext(r.Context()); ok && browserSession(principal) {
		source = rolegrants.SourceConsole
	}
	return rolegrants.Origin{ActorUserID: caller.UserID, Source: source}
}

// browserSession reports whether the principal signed in through a browser:
// its credential kind (ADR-0059 R1.2a) is a browser session (`session`,
// `form`, `sealed`) and it names no token row. Every other kind, the zero
// kind included, is `api`.
func browserSession(principal auth.User) bool {
	credential := principal.Credential()
	return credential.IsBrowserSession() && credential.TokenID == ""
}
