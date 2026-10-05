package middleware

import (
	"context"
	"net/http"
)

// AfterAuthentication schedules gates to run IMMEDIATELY AFTER the next
// successful Auth further down the chain — the place they would occupy in the
// /api/v2 group (Auth → Maintenance → ClientVersion), for a handler that
// carries its OWN Auth inside it.
//
// # Why it exists
//
// The reviewed production routes (internal/api/production_router.go) are
// root-mounted handlers that each wrap themselves in apimw.Auth with the same
// configuration the /api/v2 group uses. Mounting them under the group's Auth
// would authenticate every request twice; mounting them outside it — which is
// what the router did — skipped every cross-cutting gate the group applies
// after authentication. A caller authenticated by a native token on an
// outdated build was never answered 426 there, and a maintenance window let
// every non-administrator through.
//
// Both gates need the principal, so they cannot simply be placed in front of
// the handler: in front of its Auth there is no principal yet. This carries
// them on the request context instead, and Auth composes them between itself
// and the handler on success. The handler's authentication is untouched: the
// same credential sources, the same refusals, in the same order — a caller
// that Auth refuses is refused before any gate runs, exactly as in the group.
//
// The gates are CONSUMED by the first Auth that admits the request, so a
// handler that nests a second Auth cannot run them twice. A handler that never
// authenticates never runs them; the router guard test
// (router_cross_cutting_gates_test.go) is what proves every reviewed route
// does.
func AfterAuthentication(gates ...func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	if len(gates) == 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), afterAuthenticationKey{}, gates)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

type afterAuthenticationKey struct{}

// withScheduledGates wraps next in the gates AfterAuthentication scheduled, and
// removes them from the context so no later Auth applies them again.
func withScheduledGates(ctx context.Context, next http.Handler) (context.Context, http.Handler) {
	gates, ok := ctx.Value(afterAuthenticationKey{}).([]func(http.Handler) http.Handler)
	if !ok || len(gates) == 0 {
		return ctx, next
	}
	for i := len(gates) - 1; i >= 0; i-- {
		next = gates[i](next)
	}
	return context.WithValue(ctx, afterAuthenticationKey{}, []func(http.Handler) http.Handler(nil)), next
}
