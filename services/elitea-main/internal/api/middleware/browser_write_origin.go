package middleware

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// BrowserWriteOrigin refuses a state-changing request that a browser cookie
// authenticated, unless the request shows that it came from this origin.
//
// WHY. The /llm group accepts the session cookie, and the browser voice client
// calls /llm/v1/audio/* with it. SameSite=Lax stops a cross-SITE POST from
// carrying the cookie. It does not stop a page on a same-site sibling (another
// app, or a user-content host under the same registrable domain): that page
// can auto-submit a multipart form to /llm/v1/audio/transcriptions. The
// victim's cookie is attached, the edge resolves the victim's project, and the
// provider call is billed to the victim. The realtime WebSocket route checks
// Origin; the unary routes did not.
//
// THE RULE. A request passes when any of these is true:
//   - the method is safe (GET, HEAD, OPTIONS);
//   - a bearer token, an API key or a forwarded token authenticated it, not a
//     browser cookie (no browser attaches those to a cross-site request);
//   - it carries the project selector header (X-Project-Id or
//     OpenAI-Organization). A form cannot set a header, and a cross-origin
//     fetch that sets one needs a CORS preflight, which a credentialed request
//     fails: the router's CORS policy does not allow credentials;
//   - Sec-Fetch-Site is "same-origin" (or "none", a navigation the user typed);
//   - there is no Sec-Fetch-Site, and Origin is absent or names this host.
//
// Everything else answers 403 `cross_origin_request`, before the handler runs.
func BrowserWriteOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if browserWriteAllowed(r) {
			next.ServeHTTP(w, r)
			return
		}
		writeJSONError(w, http.StatusForbidden, "permission_error", "cross_origin_request",
			"a cookie-authenticated request must come from this origin")
	})
}

func browserWriteAllowed(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	if !cookieAuthenticated(r) {
		return true
	}
	if r.Header.Get(HeaderProjectSelector) != "" || r.Header.Get("OpenAI-Organization") != "" {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))) {
	case "same-origin", "none":
		return true
	case "":
		return originMatchesHost(r)
	default:
		// "same-site" or "cross-site".
		return false
	}
}

// cookieAuthenticated reports whether a browser cookie authenticated r: the
// elitea_session cookie, or the edge session cookie that ForwardAuth projected
// as a forwarded USER identity. A forwarded TOKEN identity is a bearer
// credential the edge verified, not a cookie.
func cookieAuthenticated(r *http.Request) bool {
	source, ok := auth.AuthenticationSourceFromContext(r.Context())
	if !ok {
		return false
	}
	switch source {
	case auth.AuthenticationSourceSession:
		return true
	case auth.AuthenticationSourceForwarded:
		user, ok := auth.UserFromContext(r.Context())
		return ok && user.TokenID == ""
	default:
		return false
	}
}

func originMatchesHost(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}
