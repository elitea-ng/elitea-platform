package toolkits_test

// The two toolkit-INSTANCE reads that answer from the repository: the paged
// list and the single read.
//
// They had no test at all. The list turns the client's limit/offset into the
// repository's page/size, and that arithmetic is the only place a caller's
// paging is interpreted — an off-by-one here shows up as a page of toolkits
// the user cannot reach, with nothing in a log to say so.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
)

// pagingRepo records the page and size the handler asks for.
type pagingRepo struct {
	mockRepo
	page     int
	size     int
	filter   toolkits.InstanceListFilter
	calls    int
	rows     []map[string]any
	total    int
	listErr  error
	getErr   error
	getValue map[string]any
}

func (r *pagingRepo) ListToolkitInstances(
	_ context.Context, _ string, filter toolkits.InstanceListFilter, page, size int,
) ([]map[string]any, int, error) {
	r.calls++
	r.filter = filter
	r.page = page
	r.size = size
	if r.listErr != nil {
		return nil, 0, r.listErr
	}
	return r.rows, r.total, nil
}

func (r *pagingRepo) GetToolkit(_ context.Context, _, _ string) (map[string]any, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	return r.getValue, nil
}

func instanceRouter(repo toolkits.Repository) *chi.Mux {
	handler := toolkits.NewHandlerWithRepo(repo)
	router := chi.NewRouter()
	router.Get("/tools/prompt_lib/{projectID}", handler.List)
	router.Get("/tool/prompt_lib/{projectID}/{toolkitID}", handler.Get)
	return router
}

func TestListToolkitsTranslatesLimitAndOffsetIntoAPage(t *testing.T) {
	t.Parallel()

	for name, testCase := range map[string]struct {
		query    string
		wantPage int
		wantSize int
	}{
		"no paging asks for the first page of twenty": {query: "", wantPage: 1, wantSize: 20},
		"an offset of one page asks for page two":     {query: "?limit=10&offset=10", wantPage: 2, wantSize: 10},
		"a partial offset stays on the page it lands in": {
			query: "?limit=10&offset=15", wantPage: 2, wantSize: 10,
		},
		"a limit above the ceiling falls back to twenty": {query: "?limit=500", wantPage: 1, wantSize: 20},
		"a limit below one falls back to twenty":         {query: "?limit=0", wantPage: 1, wantSize: 20},
		"a negative offset starts at the beginning":      {query: "?limit=5&offset=-9", wantPage: 1, wantSize: 5},
		"a non-numeric limit falls back to twenty":       {query: "?limit=many", wantPage: 1, wantSize: 20},
	} {
		t.Run(name, func(t *testing.T) {
			repo := &pagingRepo{}
			response := httptest.NewRecorder()
			instanceRouter(repo).ServeHTTP(response,
				httptest.NewRequest(http.MethodGet, "/tools/prompt_lib/1"+testCase.query, nil))

			if response.Code != http.StatusOK {
				t.Fatalf("status=%d: %s", response.Code, response.Body.String())
			}
			if repo.page != testCase.wantPage || repo.size != testCase.wantSize {
				t.Errorf("asked the repository for page %d size %d, want page %d size %d",
					repo.page, repo.size, testCase.wantPage, testCase.wantSize)
			}
		})
	}
}

func TestListToolkitsPassesTheMCPAndQueryFilterThrough(t *testing.T) {
	t.Parallel()

	for name, testCase := range map[string]struct {
		query     string
		wantMCP   *bool
		wantQuery string
	}{
		"absent params apply no filter":          {query: ""},
		"mcp=true keeps MCP":                     {query: "?mcp=true", wantMCP: boolRef(true)},
		"mcp=false drops MCP":                    {query: "?mcp=false", wantMCP: boolRef(false)},
		"the query is trimmed":                   {query: "?query=%20ctx7%20", wantQuery: "ctx7"},
		"a blank query applies no text filter":   {query: "?query=%20%20"},
		"both filters travel together with page": {query: "?mcp=true&query=doc&limit=10&offset=10", wantMCP: boolRef(true), wantQuery: "doc"},
	} {
		t.Run(name, func(t *testing.T) {
			repo := &pagingRepo{}
			response := httptest.NewRecorder()
			instanceRouter(repo).ServeHTTP(response,
				httptest.NewRequest(http.MethodGet, "/tools/prompt_lib/1"+testCase.query, nil))

			if response.Code != http.StatusOK {
				t.Fatalf("status=%d: %s", response.Code, response.Body.String())
			}
			if (repo.filter.MCP == nil) != (testCase.wantMCP == nil) ||
				(repo.filter.MCP != nil && *repo.filter.MCP != *testCase.wantMCP) {
				t.Errorf("mcp=%v, want %v", repo.filter.MCP, testCase.wantMCP)
			}
			if repo.filter.Query != testCase.wantQuery {
				t.Errorf("query=%q, want %q", repo.filter.Query, testCase.wantQuery)
			}
		})
	}
}

func TestListToolkitsRefusesABadFilterBeforeTheRepository(t *testing.T) {
	t.Parallel()

	for name, rawQuery := range map[string]string{
		"mcp is not a boolean":       "?mcp=yes",
		"mcp is empty":               "?mcp=",
		"mcp is a different case":    "?mcp=True",
		"query is one rune too long": "?query=" + strings.Repeat("a", 129),
		"query has a NUL byte":       "?query=a%00b",
		"query is not valid UTF-8":   "?query=%ff",
		"long multi-byte query":      "?query=" + strings.Repeat("%C3%A9", 129),
	} {
		t.Run(name, func(t *testing.T) {
			repo := &pagingRepo{}
			response := httptest.NewRecorder()
			instanceRouter(repo).ServeHTTP(response,
				httptest.NewRequest(http.MethodGet, "/tools/prompt_lib/1"+rawQuery, nil))

			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400: %s", response.Code, response.Body.String())
			}
			var body map[string]string
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body["error"] == "" {
				t.Errorf("want a readable {\"error\": ...}, got %s", response.Body.String())
			}
			if repo.calls != 0 {
				t.Errorf("the repository was called %d times for a refused request", repo.calls)
			}
		})
	}
}

func TestListToolkitsAcceptsAQueryAtExactlyTheLimit(t *testing.T) {
	t.Parallel()

	for name, query := range map[string]string{
		"128 ASCII characters":      strings.Repeat("a", 128),
		"128 multi-byte characters": strings.Repeat("é", 128),
	} {
		t.Run(name, func(t *testing.T) {
			repo := &pagingRepo{}
			response := httptest.NewRecorder()
			instanceRouter(repo).ServeHTTP(response,
				httptest.NewRequest(http.MethodGet, "/tools/prompt_lib/1?query="+url.QueryEscape(query), nil))

			if response.Code != http.StatusOK {
				t.Fatalf("status=%d: %s", response.Code, response.Body.String())
			}
			if repo.filter.Query != query {
				t.Errorf("the query did not reach the repository intact")
			}
		})
	}
}

func boolRef(v bool) *bool { return &v }

func TestListToolkitsAnswersAnArrayWhenThereAreNoRows(t *testing.T) {
	t.Parallel()

	// nil rows must serve `[]`, not `null`: the client indexes the value.
	response := httptest.NewRecorder()
	instanceRouter(&pagingRepo{}).ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, "/tools/prompt_lib/1", nil))

	var body struct {
		Rows  []map[string]any `json:"rows"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, response.Body.String())
	}
	if body.Rows == nil {
		t.Errorf("rows=null, want an empty array: %s", response.Body.String())
	}
	if body.Total != 0 {
		t.Errorf("total=%d", body.Total)
	}
}

func TestListToolkitsServesTheRowsAndTotal(t *testing.T) {
	t.Parallel()

	repo := &pagingRepo{
		rows:  []map[string]any{{"id": "7", "name": "one"}, {"id": "8", "name": "two"}},
		total: 42,
	}
	response := httptest.NewRecorder()
	instanceRouter(repo).ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, "/tools/prompt_lib/1", nil))

	var body struct {
		Rows  []map[string]any `json:"rows"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Rows) != 2 || body.Total != 42 {
		t.Errorf("rows=%d total=%d, want 2 and 42", len(body.Rows), body.Total)
	}
}

func TestListToolkitsHidesTheDriverErrorFromTheCaller(t *testing.T) {
	t.Parallel()

	repo := &pagingRepo{listErr: errors.New("dial tcp 10.0.7.3:5432: connect: connection refused")}
	response := httptest.NewRecorder()
	instanceRouter(repo).ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, "/tools/prompt_lib/1", nil))

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", response.Code)
	}
	// The driver names the database host, user and port. None of it crosses
	// this boundary.
	for _, leak := range []string{"10.0.7.3", "5432", "connection refused"} {
		if bodyContains(response.Body.String(), leak) {
			t.Errorf("the response body leaks %q: %s", leak, response.Body.String())
		}
	}
}

func TestGetToolkitAnswersNotFoundForAReadFailure(t *testing.T) {
	t.Parallel()

	repo := &pagingRepo{getErr: errors.New("no rows in result set")}
	response := httptest.NewRecorder()
	instanceRouter(repo).ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, "/tool/prompt_lib/1/7", nil))

	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", response.Code)
	}
	if bodyContains(response.Body.String(), "no rows") {
		t.Errorf("the response body repeats the driver message: %s", response.Body.String())
	}
}

func TestGetToolkitServesTheStoredRow(t *testing.T) {
	t.Parallel()

	repo := &pagingRepo{getValue: map[string]any{"id": "7", "type": "github"}}
	response := httptest.NewRecorder()
	instanceRouter(repo).ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, "/tool/prompt_lib/1/7", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["type"] != "github" {
		t.Errorf("body=%#v", body)
	}
}

func TestDeleteToolkitAnswersNoContentAndHidesTheDriverError(t *testing.T) {
	t.Parallel()

	handler := toolkits.NewHandlerWithRepo(&pagingRepo{})
	router := chi.NewRouter()
	router.Delete("/tool/prompt_lib/{projectID}/{toolkitID}", handler.Delete)

	response := httptest.NewRecorder()
	router.ServeHTTP(response,
		httptest.NewRequest(http.MethodDelete, "/tool/prompt_lib/1/7", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("status=%d, want 204", response.Code)
	}
	if response.Body.Len() != 0 {
		t.Errorf("a 204 carried a body: %s", response.Body.String())
	}

	failing := &pagingRepo{}
	failing.err = errors.New(
		"dial tcp 10.0.7.3:5432: connect: connection refused (user=elitea db=elitea)")
	response = httptest.NewRecorder()
	router = chi.NewRouter()
	router.Delete("/tool/prompt_lib/{projectID}/{toolkitID}",
		toolkits.NewHandlerWithRepo(failing).Delete)
	router.ServeHTTP(response,
		httptest.NewRequest(http.MethodDelete, "/tool/prompt_lib/1/7", nil))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", response.Code)
	}
	for _, leak := range []string{"10.0.7.3", "user=elitea", "connection refused"} {
		if bodyContains(response.Body.String(), leak) {
			t.Errorf("the response body leaks %q: %s", leak, response.Body.String())
		}
	}
}

func bodyContains(body, needle string) bool {
	return indexOf(body, needle) >= 0
}
