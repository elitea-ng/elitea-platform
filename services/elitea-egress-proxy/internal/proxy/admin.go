package proxy

import (
	"io"
	"net/http"
)

// AdminHandler serves the admin port: /healthz (the process is up),
// /readyz (keys are loaded and the replica is not draining) and /metrics.
// The admin port is separate from the proxy port, so the sandbox, which the
// NetworkPolicy lets reach only the proxy port, never reaches it.
func AdminHandler(ready func() bool, metrics http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if !ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "not ready\n")
			return
		}
		_, _ = io.WriteString(w, "ready\n")
	})
	mux.Handle("GET /metrics", metrics)
	return mux
}
