// Package localprobe answers one question: is this request a probe running
// inside elitea-main's own container?
//
// The readiness and startup endpoints (/readyz, /startupz) name each
// dependency's state, so they are container-local. Both the health routes and
// the browser edge (internal/api/webedge) ask this package, so the rule is
// written once.
package localprobe

import (
	"net"
	"net/http"
)

// ForwardingHeaders mark a request a proxy relayed. Any one of them means the
// loopback peer is a proxy, not a probe inside the container.
var ForwardingHeaders = []string{"Forwarded", "X-Forwarded-For", "X-Real-Ip"}

// FromLoopback answers whether r is a probe inside the container: a loopback
// peer that no proxy relayed. A reverse proxy on loopback (a mesh sidecar,
// which forwards inbound traffic from 127.0.0.6, or an nginx on the same host)
// makes every browser request a loopback peer, so a request carrying a
// forwarding header is never trusted as one; the `elitea-main -healthcheck`
// exec probe sends none.
func FromLoopback(r *http.Request) bool {
	for _, name := range ForwardingHeaders {
		if len(r.Header.Values(name)) > 0 {
			return false
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Only wraps next so that any caller but a container-local probe gets 404,
// the answer an unknown path gets.
func Only(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !FromLoopback(r) {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
