package middleware

import (
	"bytes"
	"net/http"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// BrowserSignInRequired adapts an authentication middleware for a BROWSER page
// (ADR-0025 WP2: the native authorization `continue` and `decision` routes).
//
// authenticate is apimw.Auth over the SAME composed AuthConfig the /api/v2
// group uses — no second AuthConfig literal exists for this. The adapter adds
// three rules on top of it:
//
//  1. A request carrying `Authorization` or `X-API-Key` is refused with
//     refuse(w, r) before anything is read. A native or personal access token
//     must never be able to authorize a NEW device: that would turn a stolen
//     15-minute access token into a 30-day refresh family.
//  2. When authenticate answers 401 — the inner handler was never reached —
//     the 401 is swallowed and unauthenticated(w, r) answers instead (the
//     caller's 302 to its sign-in, or its own loop-guard page). A 503 (a
//     store that did not answer) and every other status pass through
//     unchanged: an outage is not a reason to bounce a person to sign-in.
//  3. After authenticate succeeds, the request must have been authenticated by
//     a browser credential: the session cookie or the edge-forwarded browser
//     identity, with no token principal. A forwarded TOKEN principal is an
//     edge-validated bearer and is refused like rule 1.
func BrowserSignInRequired(
	authenticate func(http.Handler) http.Handler,
	unauthenticated func(http.ResponseWriter, *http.Request),
	refuse func(http.ResponseWriter, *http.Request),
) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "" || r.Header.Get("X-API-Key") != "" {
				refuse(w, r)
				return
			}
			reached := false
			inner := http.HandlerFunc(func(_ http.ResponseWriter, authenticated *http.Request) {
				reached = true
				if !browserPrincipal(authenticated) {
					refuse(w, authenticated)
					return
				}
				next.ServeHTTP(w, authenticated)
			})
			capture := &refusalCapture{header: http.Header{}}
			authenticate(inner).ServeHTTP(capture, r)
			if reached {
				return
			}
			if capture.status == http.StatusUnauthorized {
				unauthenticated(w, r)
				return
			}
			capture.replay(w)
		})
	}
}

func browserPrincipal(r *http.Request) bool {
	source, ok := auth.AuthenticationSourceFromContext(r.Context())
	if !ok || (source != auth.AuthenticationSourceSession && source != auth.AuthenticationSourceForwarded) {
		return false
	}
	user, ok := auth.UserFromContext(r.Context())
	if !ok || user.TokenID != "" {
		return false
	}
	return user.AuthType == "session" || user.AuthType == "user"
}

// refusalCapture buffers what authenticate writes when it refuses, so the
// adapter can decide what the browser sees instead. Once the inner handler is
// reached nothing is written here: the inner handler writes to the real writer.
type refusalCapture struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (c *refusalCapture) Header() http.Header { return c.header }

func (c *refusalCapture) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}

func (c *refusalCapture) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.body.Write(p)
}

func (c *refusalCapture) replay(w http.ResponseWriter) {
	for key, values := range c.header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	status := c.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(c.body.Bytes())
}
