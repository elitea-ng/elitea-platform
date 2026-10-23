package api

import (
	"net/http"
	"strings"
	"testing"

	v2promptcontextreads "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/promptcontextreads"
)

// `GET /api/v2/elitea_core/chat_config/prompt_lib/{projectID}` is what
// `apps/elitea-web/src/features/artifacts/api/chatConfigApi.ts` calls on every
// artifacts page load. Its only registration used to be the prototype
// eliteacore handler behind the never-assigned `ChatService` gate, so it
// answered 404 in every deployment — and it had to be mounted by
// mountReviewedProductionRoutes (production_router.go), the single
// registration source newProductionRouter calls (#243), for it to be reached
// by the router main.go actually builds (#194, same class as #152).
//
// Asserted structurally (chi.Walk) rather than by driving a request: the defect
// is "this pattern is not registered", and a request would additionally have to
// clear the route's own auth+RBAC chain, which is a different contract already
// covered in internal/api/v2/promptcontextreads.
func TestPrototypeRouterRegistersChatConfigWhenPromptContextReadsAreWired(t *testing.T) {
	t.Parallel()

	const (
		chatConfig     = "/api/v2/elitea_core/chat_config/prompt_lib/{projectID}"
		projectContext = "/api/v2/elitea_core/project_context/prompt_lib/{projectID}/project-context"
	)

	prototypeTrigger := http.NotFoundHandler()

	wired := routePatterns(t, NewRouter(RouterConfig{
		LLMProxy:                  prototypeTrigger,
		CurrentPromptContextReads: &v2promptcontextreads.CurrentRoutes{},
	}))
	if !hasRoute(wired, http.MethodGet, chatConfig) {
		t.Errorf("%s is not registered even with CurrentPromptContextReads wired.\n"+
			"  It must be mounted by THIS router, not only by production_router.go (#194).\n"+
			"  registered: %v", chatConfig, wired)
	}
	// The negative half: without the field the route is genuinely absent — so
	// the assertion above cannot pass on a router that registers everything
	// unconditionally.
	bare := routePatterns(t, NewRouter(RouterConfig{LLMProxy: prototypeTrigger}))
	if hasRoute(bare, http.MethodGet, chatConfig) {
		t.Errorf("%s is registered with no CurrentPromptContextReads", chatConfig)
	}

	// Scope guard: CurrentPromptContextReads also carries the project-context
	// read, and wiring it here must not add a SECOND registration for that
	// path. The compatibility router already serves it from the prototype
	// eliteacore handler (router.go:918) — unlike chat_config, it was never
	// dark — so switching the current implementation on there would be a
	// behaviour change beyond #194's chat_config half, and one that silently
	// shadows a live handler. Counted rather than merely tested for presence,
	// which would pass either way.
	if before, after := countRoute(bare, http.MethodGet, projectContext), countRoute(wired, http.MethodGet, projectContext); after != before {
		t.Errorf("wiring CurrentPromptContextReads changed %s registrations from %d to %d; #194 covers chat_config only",
			projectContext, before, after)
	}
}

func countRoute(patterns []string, method, prefix string) int {
	total := 0
	for _, pattern := range patterns {
		if strings.HasPrefix(pattern, method+" "+prefix) {
			total++
		}
	}
	return total
}
