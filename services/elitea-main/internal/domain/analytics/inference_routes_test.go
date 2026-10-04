package analytics

import (
	"regexp"
	"strings"
	"testing"
)

// The list is written into SQL text, so every entry must be one quoted route
// pattern and nothing else.
func TestInferenceRouteSQLListIsWellFormed(t *testing.T) {
	entry := regexp.MustCompile(`^'/llm/v1/[a-z/_]+'$`)
	for _, part := range strings.Split(InferenceRouteSQLList, ",") {
		if !entry.MatchString(strings.TrimSpace(part)) {
			t.Errorf("entry %q is not one quoted /llm/v1 route pattern", part)
		}
	}
}

// Embeddings are model calls (legacy issue 6879: the Usage page counted them
// and Analytics must too). The operator and catalogue routes are not.
func TestInferenceRoutesIncludeEmbeddingsAndExcludeOperatorRoutes(t *testing.T) {
	routes := map[string]bool{}
	for _, route := range InferenceRoutes() {
		routes[route] = true
	}
	for _, want := range []string{"/llm/v1/chat/completions", "/llm/v1/embeddings", "/llm/v1/images/generations", "/llm/v1/messages"} {
		if !routes[want] {
			t.Errorf("%s is a model call and is missing", want)
		}
	}
	for _, excluded := range []string{
		"/llm/v1/models", "/llm/v1/models/*", "/llm/v1/messages/count_tokens", "/llm/v1/messages/*",
		"/llm/v1/check_connection", "/llm/v1/list_provider_models", "/llm/v1/list_provider_voices", "(unmatched)",
	} {
		if routes[excluded] {
			t.Errorf("%s is not a model call and must not be counted", excluded)
		}
	}
}
