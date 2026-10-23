package api

import (
	"context"
	"net/http"
	"testing"

	v2predict "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/predict"
)

// `POST /api/v2/elitea_core/predict_llm/prompt_lib/{projectID}` is the
// AI-draft-generation sender: apps/elitea-web's features/agents aiEdit and the
// canvas mermaid quick fix both post to it.
//
// It used to stand inside a group gated on `RouterConfig.Predictor`, a field
// no composition root ever assigned, so chi answered 404 in every deployment
// and nothing said so — router.go's NOTE(#126) is the record of it. Issue #194
// serves it.
//
// Asserted structurally (chi.Walk) rather than by driving a request, for the
// reason the chat_config test gives: the defect is "this pattern is not
// registered", which no handler unit test can observe, and a request would
// additionally have to clear the route's auth+RBAC chain — a different
// contract, covered by router_elitea_core_permission_map_test.go for this very
// permission.
func TestRouterRegistersPredictLLMRegardlessOfWhetherAnLLMPlaneIsComposed(t *testing.T) {
	t.Parallel()

	const predictLLM = "/api/v2/elitea_core/predict_llm/prompt_lib/{projectID}"

	// The empty config is the point of this test, not a convenience. #126 was
	// a route group that disappeared when its dependency was absent; this
	// route must be present in exactly that situation, because the handler
	// answers 503 naming LLM_GATEWAY_URL and a 503 is something an operator
	// can act on where a 404 is not.
	unconfigured := routePatterns(t, NewRouter(RouterConfig{}))
	if !hasRoute(unconfigured, http.MethodPost, predictLLM) {
		t.Errorf("%s is not registered on a router with no PredictCompleter.\n"+
			"  Gating the registration on the dependency is what produced #126: an\n"+
			"  invisible 404 nobody can tell from a typo'd path.\n  registered: %v",
			predictLLM, unconfigured)
	}

	// And it is registered exactly once — a second registration would shadow
	// the first silently.
	if count := countRoute(unconfigured, http.MethodPost, predictLLM); count != 1 {
		t.Errorf("%s is registered %d times, want exactly 1", predictLLM, count)
	}

	// Composing a completer must not add a second registration either.
	withPlane := routePatterns(t, NewRouter(RouterConfig{PredictCompleter: stubCompleter{}}))
	if count := countRoute(withPlane, http.MethodPost, predictLLM); count != 1 {
		t.Errorf("with a PredictCompleter composed, %s is registered %d times, want exactly 1", predictLLM, count)
	}
}

// stubCompleter satisfies v2predict.Completer at compile time and is never
// invoked: this test only walks the route table.
type stubCompleter struct{}

func (stubCompleter) Complete(_ context.Context, _ v2predict.CompletionRequest) (string, error) {
	return "", nil
}
