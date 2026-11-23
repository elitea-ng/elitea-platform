package admin

import (
	"net/http"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess/rolegrants"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/rolepolicy"
)

// grantOrigin is the history origin of a role change made through an admin
// route: the caller is the actor, and the source is `console` for a browser
// session and `api` for any other credential (a token). Until the credential
// kind reaches the principal (R1.2, #2506), a non-zero BrowserSessionRef is
// what marks a browser session; only the browser session reader sets it.
func grantOrigin(r *http.Request, caller rolepolicy.Caller) rolegrants.Origin {
	source := rolegrants.SourceAPI
	if principal, ok := auth.UserFromContext(r.Context()); ok && principal.BrowserSessionRef != 0 {
		source = rolegrants.SourceConsole
	}
	return rolegrants.Origin{ActorUserID: caller.UserID, Source: source}
}
