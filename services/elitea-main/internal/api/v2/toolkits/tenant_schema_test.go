package toolkits_test

// The tenant-schema guard on every route that interpolates a schema name into
// its statement text.
//
// A query cannot bind a schema name as a parameter, so the name is written into
// the SQL. Issue #543 is what happens when that name is quoted with Go rules
// instead of PostgreSQL rules. The guard refuses anything that is not a plain
// decimal project id, and it must run BEFORE the pool is touched — these
// handlers are built here with no pool at all, so a route that reached the
// database would panic rather than answer 400.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
)

func TestTenantSchemaGuardRefusesANonDecimalProjectID(t *testing.T) {
	t.Parallel()

	handler := toolkits.NewHandlerWithRepo(&mockRepo{})
	router := chi.NewRouter()
	router.Get("/toolkit_export/{projectID}/{toolkitID}", handler.ExportToolkit)
	router.Get("/index_meta/{projectID}/{toolkitID}", handler.IndexMeta)
	router.Get("/index_meta/{projectID}/{toolkitID}/{indexName}", handler.IndexMetaGet)
	// List and Get are deliberately absent: they read through the repository
	// interface, which builds its own statement, so they never interpolate a
	// schema name here and answer 200 for a project id this guard would refuse.
	router.Delete("/index_meta/{projectID}/{toolkitID}/{indexName}", handler.IndexMetaDelete)
	router.Put("/index_meta/{projectID}/{toolkitID}/{indexName}", handler.IndexMetaUpdate)
	router.Post("/index_cancel/{projectID}/{toolkitID}/{indexName}", handler.IndexCancel)

	// Each of these is a project id a caller can put in the path today. The
	// last one is the shape #543 is about: an identifier that closes the quoted
	// schema name and continues the statement.
	for _, projectID := range []string{"abc", "1;DROP SCHEMA public", `1"; --`, "-1", "1.0", ""} {
		for _, route := range []struct {
			method string
			path   string
		}{
			{http.MethodGet, "/toolkit_export/%s/7"},
			{http.MethodGet, "/index_meta/%s/7"},
			{http.MethodGet, "/index_meta/%s/7/idx"},
			{http.MethodDelete, "/index_meta/%s/7/idx"},
			{http.MethodPut, "/index_meta/%s/7/idx"},
			{http.MethodPost, "/index_cancel/%s/7/idx"},
		} {
			path := strings.Replace(route.path, "%s", url.PathEscape(projectID), 1)
			response := httptest.NewRecorder()
			request := httptest.NewRequest(route.method, path, strings.NewReader("{}"))
			router.ServeHTTP(response, request)

			// An empty project id does not match the route at all, which is
			// also a refusal — it must never reach a handler.
			if response.Code == http.StatusNotFound && projectID == "" {
				continue
			}
			if response.Code != http.StatusBadRequest {
				t.Errorf("%s %s: status=%d, want 400", route.method, path, response.Code)
			}
			if !strings.Contains(response.Body.String(), "invalid project id") {
				t.Errorf("%s %s: body=%q", route.method, path, response.Body.String())
			}
		}
	}
}
