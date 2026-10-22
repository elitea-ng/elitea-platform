// Package webedge makes elitea-main the browser edge (#1359 P2 for compose,
// P3 for the chart).
//
// The compose stacks used to run a Traefik in front of elitea-main and the web
// SPA container (deploy/traefik/dynamic*.yml, now deleted), and a cluster ran a
// Gateway API HTTPRoute with one rule per root-mounted Go route plus a second
// route for the SPA. Both edges did four things for a browser, and this package
// does the same four, in the same order of precedence:
//
//  1. `/` answers a NON-permanent redirect to `/app/`, keeping the query
//     string (a link of the form `/?utm_source=x` once skipped the redirect and
//     reached the SPA container at `/`, which its nginx does not serve).
//  2. `/app…` and `/docs…` go to the web container, EXCEPT the paths
//     elitea-main itself serves under `/app`: the MCP server at
//     `/app/<numeric project id>/mcp[/…]` and the two icon file servers
//     `/app/application_icon/` and `/app/application_tool_icon/`.
//  3. `/readyz` and `/startupz` are not published at this edge. Their bodies
//     name each dependency's state; a probe inside the container (loopback)
//     still reaches them. The chart runs its startup and readiness probes as
//     `elitea-main -healthcheck <path>` for that reason: a kubelet httpGet
//     arrives from the node's address and would be refused.
//  4. Everything else reaches elitea-main's own router, which strips every
//     client identity header itself (apimw.StripClientIdentityHeaders) — the
//     job Traefik's `strip-client-identity` middleware did.
//
// It is off unless ELITEA_WEB_UPSTREAM names the web container. The compose
// stacks set it, and the chart sets it to the elitea-web Service whenever
// web.enabled is true. The cluster's Ingress or HTTPRoute is then ONE `/` rule
// to elitea-main: the MCP carve-out needs a path regular expression, which
// Gateway API leaves implementation-specific and a networking.k8s.io Ingress
// cannot express at all, so it lives here, in Go, where one test covers every
// deployment shape.
//
// The browser → elitea-main → elitea-web hop is browser traffic relayed, not a
// service-to-service call, so it stays plain HTTP like the edge it replaces.
//
// services/elitea-main/tests/deployedge drives the real router with this edge
// enabled against a real upstream and holds every rule above; webedge_test.go
// holds the routing decision itself.
package webedge

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
)

// UpstreamEnv names the environment variable that turns the edge on.
const UpstreamEnv = "ELITEA_WEB_UPSTREAM"

// WebPrefixes are the path prefixes served by the web container. They are
// STRING prefixes, as Traefik's PathPrefix was: `/app` also takes
// `/application…`, which no elitea-main route uses.
var WebPrefixes = []string{"/app", "/docs"}

// MainPrefixesUnderWeb are the elitea-main file servers inside the SPA's
// `/app` namespace (internal/api/router.go: application_icon and
// application_tool_icon). apps/elitea-web builds those URLs itself.
var MainPrefixesUnderWeb = []string{"/app/application_icon/", "/app/application_tool_icon/"}

// MCPPath is the MCP server elitea-main serves inside the SPA's namespace
// (issue 252). The internal-toolkit URLs already written into projects
// hardcode it, so the path is not ours to move. It carves `/app/<n>/mcp` out
// of the SPA's project-switch deep link `/app/<n>/<rest>`;
// apps/elitea-web/src/routes/__tests__/mcpPathReservation.test.ts fails if a
// web route ever claims it.
var MCPPath = regexp.MustCompile(`^/app/[0-9]+/mcp(/.*)?$`)

// NotPublished are elitea-main paths this edge refuses to anyone but a
// loopback caller.
var NotPublished = []string{"/readyz", "/startupz"}

// ParseUpstream validates the configured web upstream. Empty means the edge is
// off and returns nil.
func ParseUpstream(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	upstream, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", UpstreamEnv, err)
	}
	if upstream.Scheme != "http" && upstream.Scheme != "https" {
		return nil, fmt.Errorf("%s: scheme %q, want http or https", UpstreamEnv, upstream.Scheme)
	}
	if upstream.Host == "" {
		return nil, errors.New(UpstreamEnv + ": no host")
	}
	if upstream.Path != "" && upstream.Path != "/" || upstream.RawQuery != "" || upstream.User != nil {
		return nil, fmt.Errorf("%s: %q must be an origin (scheme://host[:port]) with no path, query or credentials", UpstreamEnv, raw)
	}
	upstream.Path = ""
	return upstream, nil
}

// RoutesToWeb answers whether the edge sends path to the web container.
func RoutesToWeb(path string) bool {
	if MCPPath.MatchString(path) {
		return false
	}
	for _, prefix := range MainPrefixesUnderWeb {
		if strings.HasPrefix(path, prefix) {
			return false
		}
	}
	for _, prefix := range WebPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// Middleware returns the edge in front of next, or next itself when upstream
// is nil. It belongs FIRST in the router's chain: the web container's answers
// carry their own security headers, and nothing elitea-main adds to an API
// response belongs on them.
func Middleware(upstream *url.URL) func(http.Handler) http.Handler {
	if upstream == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(upstream)
			// The browser's Host survives, as it did through Traefik
			// (passHostHeader), so the SPA's own redirects name the origin
			// the browser used.
			request.Out.Host = request.In.Host
			request.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "web upstream unavailable", http.StatusBadGateway)
		},
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path := r.URL.Path
			switch {
			case path == "/":
				target := "/app/"
				if r.URL.RawQuery != "" {
					target += "?" + r.URL.RawQuery
				}
				status := http.StatusFound
				if r.Method != http.MethodGet && r.Method != http.MethodHead {
					status = http.StatusTemporaryRedirect
				}
				http.Redirect(w, r, target, status)
			case RoutesToWeb(path):
				proxy.ServeHTTP(w, r)
			case isNotPublished(path) && !fromLoopback(r):
				http.NotFound(w, r)
			default:
				next.ServeHTTP(w, r)
			}
		})
	}
}

func isNotPublished(path string) bool {
	for _, candidate := range NotPublished {
		if path == candidate {
			return true
		}
	}
	return false
}

func fromLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
