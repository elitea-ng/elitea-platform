package llmproxy

import (
	"net/http"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/failmode"
)

// DegradedHoldsScope gives each request its set of degraded-mode reservations
// (failmode.Holds, human decision, DECISIONS.md 2026-10-22), and drops the
// request's own pin when the next handler RETURNS.
//
// The end of the handler is the one point where it is certain that every
// biller of the request has pinned the holds or will bill nothing:
//
//   - a unary call bills (and pins) before the handler returns;
//   - a stream settler pins in the handler's goroutine, before it returns;
//   - a realtime session returns only after its last turn handed its bill off.
//
// A context cancel is NOT that point. A client that disconnects cancels the
// request context while the handler still runs, and a release then would come
// before the bill. So the holds are not tied to any context.
//
// Mount it on the router root (internal/api/router.go), so every route is
// covered. A request that reaches a handler without it carries no holds, and
// CheckBudget then reserves nothing for it.
func (h *Handler) DegradedHoldsScope(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		holds := failmode.NewHolds()
		defer holds.End()
		next.ServeHTTP(w, r.WithContext(failmode.WithHolds(r.Context(), holds)))
	})
}
