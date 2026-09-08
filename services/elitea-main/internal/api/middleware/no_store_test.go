package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
)

// A plain JSON read must carry the directive. This is the case the Context
// Budget panel met: the answer had no `Cache-Control` at all, and WebKit
// served the next page load its stored copy instead of asking again.
func TestNoStoreSetsTheDirectiveOnAJSONResponse(t *testing.T) {
	t.Parallel()

	handler := apimw.NoStore(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"max_tokens":32000}`))
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v2/elitea_core/context_analytics/prompt_lib/1/1", nil))

	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control: got %q, want %q", got, "no-store")
	}
}

// A handler that serves something genuinely cacheable keeps its own answer.
// The default is written before the handler runs precisely so the handler can
// still overwrite it; a middleware that clobbered the value would make every
// immutable asset uncacheable.
func TestNoStoreDoesNotReplaceAHandlersOwnValue(t *testing.T) {
	t.Parallel()

	const immutable = "public, max-age=31536000, immutable"
	handler := apimw.NoStore(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", immutable)
		w.WriteHeader(http.StatusOK)
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v2/branding/asset", nil))

	if got := recorder.Header().Get("Cache-Control"); got != immutable {
		t.Errorf("Cache-Control: got %q, want the handler's own %q", got, immutable)
	}
}

// The directive must be on the refusals too. A 404 or a 403 is an answer a
// browser can store just as happily as a 200, and a stored refusal outlives
// the permission change that lifted it.
func TestNoStoreCoversARefusal(t *testing.T) {
	t.Parallel()

	handler := apimw.NoStore(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v2/projects/project/default/1", nil))

	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control on a 403: got %q, want %q", got, "no-store")
	}
}
