package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// routePatterns walks every route the router registers, including mounted
// subrouters, and returns the concatenated patterns.
func routePatterns(t *testing.T, router chi.Router) []string {
	t.Helper()
	var patterns []string
	walk := func(method string, route string, handler http.Handler, middlewares ...func(http.Handler) http.Handler) error {
		patterns = append(patterns, method+" "+route)
		return nil
	}
	if err := chi.Walk(router, walk); err != nil {
		t.Fatalf("walk router: %v", err)
	}
	return patterns
}

func hasRoute(patterns []string, method, prefix string) bool {
	for _, pattern := range patterns {
		if strings.HasPrefix(pattern, method+" "+prefix) {
			return true
		}
	}
	return false
}

// newProductionRouter (what NewRouter always builds; see production_router.go's
// NewRouter doc comment, #243) is the one EVERY deployment gets. Both routes
// below were unreachable there — the project stream because its then two-arm
// (NATS/Redis) gate had no arm wired, the notification stream because only
// production_router.go's now-deleted dead branch mounted it — so both
// answered 404 everywhere while their handlers, clients and tests all
// existed (#152).
//
// Asserted structurally (chi.Walk) rather than by driving a request: the defect
// is "this pattern is not registered", and a request would have to clear the
// group's auth middleware first, which is a different contract.
func TestPrototypeRouterRegistersTheSSEStreamsWhenTheirSourcesAreWired(t *testing.T) {
	t.Parallel()

	const (
		projectStream      = "/api/v2/events/prompt_lib/{projectID}"
		notificationStream = "/api/v2/notifications/events/prompt_lib/{projectID}"
	)

	prototypeTrigger := http.NotFoundHandler()
	// Never read: the route registration is what is under test.
	source := newEventSource()
	notificationEvents := http.NotFoundHandler()

	wired := routePatterns(t, NewRouter(RouterConfig{
		LLMProxy:                  prototypeTrigger,
		EventSource:               source,
		CurrentNotificationEvents: notificationEvents,
	}))
	if !hasRoute(wired, http.MethodGet, projectStream) {
		t.Errorf("%s is not registered even with EventSource wired.\n"+
			"  Its gate is `if cfg.EventSource != nil`; with it unassigned the route "+
			"exists in no deployment (#152).\n  registered: %v",
			projectStream, wired)
	}
	if !hasRoute(wired, http.MethodGet, notificationStream) {
		t.Errorf("%s is not registered even with CurrentNotificationEvents wired.\n"+
			"  It must be mounted by THIS router, not only by production_router.go, "+
			"which NewRouter never reaches once any prototype field is set (#152).\n  registered: %v",
			notificationStream, wired)
	}

	// The negative half: without a source, the routes are genuinely absent —
	// so the assertions above cannot pass by accident on a router that
	// registers everything unconditionally.
	bare := routePatterns(t, NewRouter(RouterConfig{LLMProxy: prototypeTrigger}))
	if hasRoute(bare, http.MethodGet, projectStream) {
		t.Errorf("%s is registered with no EventSource", projectStream)
	}
	if hasRoute(bare, http.MethodGet, notificationStream) {
		t.Errorf("%s is registered with no CurrentNotificationEvents", notificationStream)
	}
}
