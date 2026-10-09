package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/pipelinelimits"
)

// The save-time pipeline bound runs in the handler/repository, behind the
// route's permission gate. An over-bound pipeline body sent by a caller who may
// not write there is therefore answered by the authorization refusal alone: the
// size and node verdicts, which are a function of the submitted text, never
// reach an unauthorized caller, and the repositories (empty here, so a call
// would panic) are never touched.
func TestPipelineLimitRefusalNeverPrecedesAuthorization(t *testing.T) {
	pipeline := "nodes:\n" + strings.Repeat("  - {id: 1, type: llm}\n", pipelinelimits.MaxNodes+1) +
		"# " + strings.Repeat("x", pipelinelimits.MaxInstructionsBytes)
	body := `{"name":"p","type":"pipeline","instructions":` + quoteJSON(pipeline) +
		`,"agent_type":"pipeline","versions":[{"name":"base","agent_type":"pipeline","instructions":` + quoteJSON(pipeline) + `}],` +
		`"version":{"application_id":"1","id":"2","instructions":` + quoteJSON(pipeline) + `}}`

	writes := []struct{ method, path, permission string }{
		{http.MethodPost, "/api/v2/elitea_core/applications/prompt_lib/%s", "models.applications.applications.create"},
		{http.MethodPut, "/api/v2/elitea_core/application/prompt_lib/%s/1", "models.applications.application.update"},
		{http.MethodPost, "/api/v2/elitea_core/versions/prompt_lib/%s/1", "models.applications.versions.create"},
		{http.MethodPut, "/api/v2/elitea_core/version/prompt_lib/%s/1/2", "models.applications.version.update"},
		{http.MethodPost, "/api/v2/elitea_core/fork/prompt_lib/%s", "models.applications.fork.post"},
		{http.MethodPost, "/api/v2/elitea_core/export_import/prompt_lib/%s/1", "models.applications.export_import.import"},
	}
	for _, w := range writes {
		for name, setup := range map[string]struct {
			project  string
			resolver fakePermissionResolver
		}{
			"foreign project":      {"8", entitledForProjectSeven()},
			"permission withheld":  {"7", fakePermissionResolver{granted: permissionsExcept(w.permission), forProject: "7"}},
			"no permission at all": {"7", fakePermissionResolver{forProject: "7"}},
		} {
			t.Run(w.method+" "+w.path+" "+name, func(t *testing.T) {
				router := newEliteaCoreProjectScopeRouter(&memberOfProject{project: "7"}, setup.resolver)
				path := strings.Replace(w.path, "%s", setup.project, 1)
				request := testAuthHeader(httptest.NewRequest(w.method, path, strings.NewReader(body)))
				request.Header.Set("Content-Type", "application/json")
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, request)
				if recorder.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403; body=%.300s", recorder.Code, recorder.Body.String())
				}
				if text := recorder.Body.String(); strings.Contains(text, "PIPELINE_") || strings.Contains(text, "KiB") || strings.Contains(text, "nodes") {
					t.Fatalf("the authorization refusal carries a size verdict: %.300s", text)
				}
			})
		}
	}
}

func quoteJSON(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
