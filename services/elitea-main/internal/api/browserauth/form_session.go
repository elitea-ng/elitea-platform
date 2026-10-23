package browserauth

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	browserapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/browserauth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// FormSessionAuthorizer reads one Form browser session back. The Form
// browserauth service is the production implementation.
type FormSessionAuthorizer interface {
	Authorize(ctx context.Context, sessionID string) (browserapp.Authorization, error)
}

// FormSessions resolves the Form plane's session cookie into the principal
// apimw.Auth serves (apimw.FormSessionAuthenticator).
//
// It is the in-process replacement for the X-Auth-* projection (#1359). The
// cookie used to reach elitea-main only through an authenticating edge, which
// called /internal/auth/main, read the same session, and copied a signed
// identity onto the forwarded request. That needed an edge running ForwardAuth
// in front of every browser route, and no edge this repository ships ran one,
// so a Form sign-in authenticated nothing at all behind them.
type FormSessions struct {
	cookies  *CookiePolicy
	sessions FormSessionAuthorizer
}

func NewFormSessions(cookies *CookiePolicy, sessions FormSessionAuthorizer) (*FormSessions, error) {
	if cookies == nil || sessions == nil {
		return nil, ErrInvalidHandlerConfiguration
	}
	return &FormSessions{cookies: cookies, sessions: sessions}, nil
}

// AuthenticateFormSession implements apimw.FormSessionAuthenticator.
func (f *FormSessions) AuthenticateFormSession(request *http.Request) (auth.User, error) {
	sessionID, err := f.cookies.Read(request)
	switch {
	case err == nil:
	case errors.Is(err, ErrSessionCookieMissing):
		return auth.User{}, apimw.ErrFormSessionAbsent
	default:
		return auth.User{}, apimw.ErrFormSessionRejected
	}

	authorization, err := f.sessions.Authorize(request.Context(), sessionID)
	switch {
	case err == nil:
	case errors.Is(err, browserapp.ErrUnauthenticated),
		errors.Is(err, browserapp.ErrAuthenticationExpired),
		errors.Is(err, browserapp.ErrInvalidRequest):
		return auth.User{}, apimw.ErrFormSessionRejected
	default:
		return auth.User{}, err
	}
	if !coherentSessionPrincipal(authorization.Principal) {
		// The service already refuses an incoherent row. A principal of any
		// other shape here is a contract break between the two, and a refusal
		// is the only safe reading of it.
		return auth.User{}, apimw.ErrFormSessionRejected
	}
	return authorization.Principal, nil
}

// coherentSessionPrincipal is the shape browserapp.Service.Authorize returns:
// a session principal whose compatibility and owning ids are the same positive
// user id, and no token id.
func coherentSessionPrincipal(principal auth.User) bool {
	id, err := strconv.ParseInt(principal.UserID, 10, 64)
	return err == nil && id > 0 && principal.ID == principal.UserID &&
		principal.TokenID == "" && principal.AuthType == "session"
}
