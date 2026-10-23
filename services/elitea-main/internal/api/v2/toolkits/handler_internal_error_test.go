package toolkits_test

// DEFECT: seven 500 answers in handler.go put the raw `err.Error()` in the
// response body. A pgx error names the database user, database, host and port
// when the server is unreachable, and table or constraint names otherwise. The
// caller is a browser, so the string crosses a trust boundary (AGENTS.md,
// "Keep API errors typed and safe for callers").
//
// The eighth site, index_meta_list, was already fixed and carries the same
// note.
//
// This test drives the four routes a mock repository reaches. The other three
// sites, ListTypeSchemas and the two writes in updateToolRelation, need a live
// pool or a broken snapshot.
//
// It fails before the change. Every case reads the seeded marker back.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
)

// databaseSecret stands for what a real pgx failure prints.
const databaseSecret = `dial tcp 10.0.7.3:5432: connect: connection refused (user=elitea db=elitea)`

func toolkitWriteRouter(repo toolkits.Repository) *chi.Mux {
	router := chi.NewRouter()
	handler := toolkits.NewHandlerWithRepo(repo)
	router.Post("/tools/prompt_lib/{projectID}", handler.Create)
	router.Put("/tool/prompt_lib/{projectID}/{toolkitID}", handler.Update)
	router.Delete("/tool/prompt_lib/{projectID}/{toolkitID}", handler.Delete)
	router.Post("/fork_toolkit/prompt_lib/{projectID}", handler.ForkToolkit)
	return router
}

func TestToolkitWriteFailuresHideTheDatabaseError(t *testing.T) {
	cases := []struct {
		name   string
		method string
		target string
		body   string
	}{
		{"create", http.MethodPost, "/tools/prompt_lib/1", `{"name":"t","type":"openai","settings":{}}`},
		{"update", http.MethodPut, "/tool/prompt_lib/1/7", `{"name":"t"}`},
		{"delete", http.MethodDelete, "/tool/prompt_lib/1/7", ""},
		{"fork", http.MethodPost, "/fork_toolkit/prompt_lib/1", `{"source_id":"7"}`},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			router := toolkitWriteRouter(&mockRepo{err: errors.New(databaseSecret)})
			request := httptest.NewRequest(testCase.method, testCase.target,
				strings.NewReader(testCase.body))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500 (body %s)", recorder.Code, recorder.Body.String())
			}
			body := recorder.Body.String()
			if strings.Contains(body, databaseSecret) || strings.Contains(body, "10.0.7.3") {
				t.Fatalf("the 500 body carries the raw database error: %s", body)
			}
			// The body still names the operation, so the client can report it.
			if !strings.Contains(body, "failed to") {
				t.Fatalf("the 500 body says nothing useful: %s", body)
			}
		})
	}
}
