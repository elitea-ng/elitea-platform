package eliteacore_test

// Real-PostgreSQL coverage for the `?format=md` DISPATCH.
//
// The renderer's own rules are unit-tested next door in
// export_markdown_internal_test.go, without a database, because they are pure.
// The one claim that file cannot make is the one this defect was actually
// about: that the ROUTE looks at `format` at all. The handler read the
// parameter nowhere and answered the JSON export to every request, so the
// product's Export-to-Markdown control saved a `.md` file containing an export
// document — a download that succeeded, with the right filename, and the wrong
// bytes inside.
//
// Both cases here run over the SAME seeded agent as the round-trip file and
// assert on the response a browser would receive, not on a helper's return
// value: the media type, the filename, and the first bytes of the body.
//
// The pool, the migration corpus and the seed come from
// export_import_roundtrip_postgres_integration_test.go — see its header.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// exportWithQuery performs the export the way the browser does, with an
// arbitrary query string, and hands back the whole recorder so a case can
// assert on headers as well as on the body.
func exportWithQuery(t *testing.T, handler *eliteacore.Handler, applicationID int, query string) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	router.Get("/elitea_core/export_import/prompt_lib/{projectID}/{entityID}", handler.ExportImportGet)

	target := fmt.Sprintf("/elitea_core/export_import/prompt_lib/1/%d%s", applicationID, query)
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request = request.WithContext(auth.ContextWithUser(request.Context(),
		auth.User{ID: strconv.Itoa(importLinkPrincipal)}))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// The defect, stated as a test: ?format=md must produce MARKDOWN.
func TestExportHonoursTheMarkdownFormat(t *testing.T) {
	pool := newImportLinkPool(t)
	handler := eliteacore.NewHandler(pool)
	seeded := seedRoundTripAgent(t, pool)

	recorder := exportWithQuery(t, handler, seeded.applicationID, "?format=md")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", recorder.Code, recorder.Body.String())
	}

	// The media type is the whole point. Before the repair this said
	// application/json while the client saved the blob as `.md`.
	if got := recorder.Header().Get("Content-Type"); got != "text/markdown; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/markdown; charset=utf-8", got)
	}

	body := recorder.Body.String()
	if !strings.HasPrefix(body, "---\n") {
		t.Fatalf("the body does not open with YAML frontmatter:\n%s", body)
	}
	// A JSON document would satisfy "starts with something" but never this:
	// `{` cannot open a frontmatter block, and `"ok":true` is what the old
	// response led with.
	if strings.Contains(body, `"ok"`) || strings.Contains(body, `"applications"`) {
		t.Errorf("the JSON export document reached a markdown response:\n%s", body)
	}

	// The SEEDED TOOLKIT must be in the file. This is the assertion the first
	// version of this test lacked: it checked the media type and the
	// frontmatter opener, both of which a renderer that silently dropped every
	// toolkit still satisfies — and one did, because `version["tools"]` is a
	// `[]map[string]any` that no `.([]any)` assertion accepts.
	if !strings.Contains(body, "toolkits:") {
		t.Errorf("the seeded agent's toolkit is missing from its export:\n%s", body)
	}

	// The filename the browser will use, and the header that lets it read it
	// cross-origin. Without the latter the download silently falls back to the
	// client's guessed name.
	disposition := recorder.Header().Get("Content-Disposition")
	if !strings.Contains(disposition, ".agent.md") {
		t.Errorf("Content-Disposition = %q, want a .agent.md filename", disposition)
	}
	if got := recorder.Header().Get("Access-Control-Expose-Headers"); got != "Content-Disposition" {
		t.Errorf("Access-Control-Expose-Headers = %q, want Content-Disposition", got)
	}
}

// The other half of the same claim: adding the branch must not have changed
// what every existing caller gets. `format` absent — and `format` set to
// anything else — is still the JSON export.
func TestExportStillAnswersJSONWithoutTheMarkdownFormat(t *testing.T) {
	pool := newImportLinkPool(t)
	handler := eliteacore.NewHandler(pool)
	seeded := seedRoundTripAgent(t, pool)

	for _, query := range []string{"", "?format=json", "?format=zip"} {
		recorder := exportWithQuery(t, handler, seeded.applicationID, query)
		if recorder.Code != http.StatusOK {
			t.Fatalf("query %q: status = %d, want 200, body = %s", query, recorder.Code, recorder.Body.String())
		}
		if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
			t.Errorf("query %q: Content-Type = %q, want application/json", query, got)
		}
		if !strings.Contains(recorder.Body.String(), `"applications"`) {
			t.Errorf("query %q: the JSON export lost its applications key:\n%s", query, recorder.Body.String())
		}
	}
}
