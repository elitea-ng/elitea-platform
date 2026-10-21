package spi

import (
	"io"
	"net/http"
	"sort"
	"strings"
)

// DefaultAdminAddr is where the host's admin port listens when
// <PREFIX>ADMIN_ADDR is not set.
const DefaultAdminAddr = ":9090"

// AdminHandler serves the admin port: /healthz (the process is up) and
// /readyz (every readiness check passes — Server.Readiness, the same checks
// the provider service's Health reports). Plain HTTP and no mTLS, like every
// other service's health port: the kubelet and the container probe present
// no client certificate, and the gRPC listener requires one, so a pod whose
// readiness is false (a lost owner claim, #1359) could otherwise never leave
// the Service. Nothing else is served here.
func AdminHandler(server *Server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		ready, checks := server.Readiness(r.Context())
		if !ready {
			var failing []string
			for name, ok := range checks {
				if !ok {
					failing = append(failing, name)
				}
			}
			sort.Strings(failing)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "not ready: "+strings.Join(failing, ",")+"\n")
			return
		}
		_, _ = io.WriteString(w, "ready\n")
	})
	return mux
}
