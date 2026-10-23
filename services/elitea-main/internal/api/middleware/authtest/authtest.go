// Package authtest is the test double for the Form session source of
// apimw.Auth. Route tests sign a request in with SignIn and compose
// apimw.AuthConfig{FormSessions: authtest.FormSessions{}}.
//
// It replaced the X-Auth-* headers those tests used to set (#1359). The
// headers were never a test-only convenience: they were the production
// forward-auth projection, and a route test that authenticated with them was
// exercising a plane that no longer exists.
package authtest

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// CookieName is the Form session cookie every shipped auth document names.
const CookieName = "elitea_browser_auth"

// FormSessions resolves the CookieName cookie to the user whose positive
// numeric id is its value, as the production reader resolves a live session.
// Err, when set, is returned for every request that carries the cookie, which
// is how a test drives a rejected or unreadable session.
type FormSessions struct {
	Err error
}

func (f FormSessions) AuthenticateFormSession(request *http.Request) (auth.User, error) {
	cookie, err := request.Cookie(CookieName)
	if err != nil {
		return auth.User{}, apimw.ErrFormSessionAbsent
	}
	if f.Err != nil {
		return auth.User{}, f.Err
	}
	id, err := strconv.ParseInt(cookie.Value, 10, 64)
	if err != nil || id <= 0 {
		return auth.User{}, apimw.ErrFormSessionRejected
	}
	return auth.User{ID: cookie.Value, UserID: cookie.Value, AuthType: "session"}, nil
}

// Cookie is the Form session cookie that FormSessions resolves to userID.
func Cookie(userID string) *http.Cookie {
	return &http.Cookie{Name: CookieName, Value: userID}
}

// SignIn adds the Form session cookie of userID to request.
func SignIn(request *http.Request, userID string) {
	request.AddCookie(Cookie(userID))
}

// tokenPrefix marks the bearer values Tokens accepts.
const tokenPrefix = "authtest-pat:"

// Tokens is a stub apimw.TokenValidator for the bearers BearerToken sets. It
// yields the principal a personal access token names before the principal
// validator normalizes it: the token's id as the compatibility ID, and its
// owner as UserID.
type Tokens struct{}

func (Tokens) ValidateToken(_ context.Context, token string) (auth.User, error) {
	rest, ok := strings.CutPrefix(token, tokenPrefix)
	if !ok {
		return auth.User{}, errors.New("authtest: not a test token")
	}
	tokenID, userID, ok := strings.Cut(rest, ":")
	if !ok || tokenID == "" || userID == "" {
		return auth.User{}, errors.New("authtest: malformed test token")
	}
	return auth.User{ID: tokenID, TokenID: tokenID, UserID: userID, AuthType: "token"}, nil
}

// BearerToken authenticates request as the personal access token tokenID
// owned by userID, for an AuthConfig whose Validator is Tokens.
func BearerToken(request *http.Request, tokenID, userID string) {
	request.Header.Set("Authorization", "Bearer "+tokenPrefix+tokenID+":"+userID)
}

// ForgeXAuth sets the X-Auth-* identity the retired forward-auth projection
// used (#1359), and the X-Elitea-* identity tuple of the gateway hop, naming
// userID. Nothing on a browser route reads either; a test sends them to
// prove that a request carrying them and no credential is still refused.
func ForgeXAuth(request *http.Request, userID string) {
	request.Header.Set("X-Auth-Type", "user")
	request.Header.Set("X-Auth-ID", userID)
	request.Header.Set("X-Auth-User-ID", userID)
	request.Header.Set("X-Elitea-User-Id", userID)
	request.Header.Set("X-Elitea-Project-Id", "7")
	request.Header.Set("X-Elitea-Identity-Signature", "forged")
}
