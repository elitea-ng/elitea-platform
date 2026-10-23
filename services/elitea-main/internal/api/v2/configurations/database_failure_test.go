package configurations_test

// A DATABASE FAILURE WAS REPORTED AS SUCCESS.
//
// `List`, `ListModels` and `ListTypes` treated EVERY error as "the tenant
// schema may not exist yet" and answered HTTP 200 with an empty page. `Delete`
// treated every error as "the row is not there" and answered 404. Neither
// logged anything.
//
// So a saturated pool, a lost connection or a statement timeout rendered the
// AI-Configuration screen as "no credentials" for a project that has them. The
// user re-created a credential that already exists and hit the UNIQUE
// constraint on elitea_title; the operator received no signal at all. A delete
// that did not happen reported an already-absent row, and a client that treats
// DELETE-404 as idempotent success stopped retrying.
//
// The pool here is CLOSED, which is a pure connection failure and not a
// missing schema. That is exactly the case the old carve-out could not tell
// apart. A missing schema still answers an empty page — see
// TestOnlyAMissingSchemaCountsAsAnEmptyProject in list_parameters_test.go.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	handler "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/configurations"
)

func failingConfigurationRouter(t *testing.T) *chi.Mux {
	t.Helper()
	h := handler.NewHandler(closedPool(t), handler.WithPermissionResolver(entitledResolver()))
	router := chi.NewRouter()
	router.Use(withTestUser)
	router.Mount("/api/v2/configurations", h.Routes())
	return router
}

func TestADatabaseFailureIsNeverReportedAsAnEmptyProject(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		want   int
	}{
		{"list", http.MethodGet, "/api/v2/configurations/configurations/7", http.StatusInternalServerError},
		{"list mode twin", http.MethodGet, "/api/v2/configurations/configurations/administration/7", http.StatusInternalServerError},
		{"models", http.MethodGet, "/api/v2/configurations/models/7", http.StatusInternalServerError},
		{"types", http.MethodGet, "/api/v2/configurations/types/7", http.StatusInternalServerError},
		// A failed delete is not an absent row.
		{"delete", http.MethodDelete, "/api/v2/configurations/configuration/7/11", http.StatusInternalServerError},
	}
	router := failingConfigurationRouter(t)
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			req := httptest.NewRequest(testCase.method, testCase.path, nil)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != testCase.want {
				t.Fatalf("status = %d, want %d. The body was %s",
					rec.Code, testCase.want, rec.Body.String())
			}
			if contentType := rec.Header().Get("Content-Type"); contentType != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", contentType)
			}
		})
	}
}

// An oversized filter is refused BEFORE the database.
//
// The pool is closed, so a request that reaches the database answers 500. A
// 400 here proves that the bound runs first, and that the work never starts.
func TestAnOversizedFilterIsRefusedBeforeTheDatabase(t *testing.T) {
	values := url.Values{}
	for i := 0; i < 200; i++ {
		values.Add("type", "llm")
	}
	path := "/api/v2/configurations/configurations/7?" + values.Encode()

	router := failingConfigurationRouter(t)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d. The body was %s",
			rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid configuration request") {
		t.Fatalf("body = %s, want the reviewed route's message", rec.Body.String())
	}
}
