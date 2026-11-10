package middleware

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/libs/go/faults"
	"github.com/elitea-ng/elitea-platform/libs/go/faults/httperr"
)

// Recover turns a panic into one fault line (internal.panic: the method,
// the route pattern, the support reference and a bounded, rate-limited
// stack; never the panic value) and a JSON 500 holding the sentence, the
// code and the support reference only (FU-299 C2).
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				if recovered == http.ErrAbortHandler {
					panic(recovered)
				}
				route := r.URL.Path
				if routeCtx := chi.RouteContext(r.Context()); routeCtx != nil && routeCtx.RoutePattern() != "" {
					route = routeCtx.RoutePattern()
				}
				fault := faults.Panic(recovered, slog.String("method", r.Method), slog.String("route", route))
				httperr.Write(w, r, slog.Default(), http.StatusInternalServerError, fault)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
