package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
)

// TestRouterStripsClientIdentityHeaders proves the root router mounts
// apimw.StripClientIdentityHeaders ahead of every route (#1359). It is what
// keeps a caller-chosen identity out of elitea-main now that no edge is
// required in front of it; tests/deployedge holds the list to the edges'.
func TestRouterStripsClientIdentityHeaders(t *testing.T) {
	var seen http.Header
	reached := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.WriteHeader(http.StatusAccepted)
	})
	router := NewRouter(RouterConfig{CurrentAgentStart: reached})

	request := httptest.NewRequest(http.MethodPost, "/api/v2/elitea_core/messages/prompt_lib/2/conversation", nil)
	for _, name := range apimw.ClientIdentityHeaders {
		request.Header.Set(name, "client-chosen")
	}
	request.Header.Set("X-Elitea-Execution-Id", "exec-1")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted || seen == nil {
		t.Fatalf("status = %d: the probe route was not reached, so this test proves nothing", response.Code)
	}
	for _, name := range apimw.ClientIdentityHeaders {
		if value := seen.Get(name); value != "" {
			t.Errorf("%s reached the route as %q", name, value)
		}
	}
	if got := seen.Get("X-Elitea-Execution-Id"); got != "exec-1" {
		t.Errorf("X-Elitea-Execution-Id = %q, want it kept for /llm attribution", got)
	}
}
