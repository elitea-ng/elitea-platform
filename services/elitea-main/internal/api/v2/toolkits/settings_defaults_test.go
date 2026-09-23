package toolkits_test

// The toolkit write path applies the catalogue's declared defaults (#978).
//
// Written over the REAL pinned catalogue snapshot rather than a fixture, for
// the reason type_catalogue_test.go gives: the whole point is that the stored
// settings track the admitted SDK, and a fixture cannot notice when the
// snapshot moves.
//
// The subject is `qtest`'s `no_of_tests_shown_in_dql_search` — `default: 10` in
// the snapshot, `int` in the SDK's own wrapper model. A toolkit saved without
// it reached the worker with the key null and died at materialization with
// `1 validation error … Input should be a valid integer`, which the user sees
// as an errored turn with nothing to point at. The toolkit FORM never hit it
// because it renders the defaults and therefore sends them; the API, the MCP
// surface and an import do not.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// defaultsRecordingRepo records the body each write RECEIVES, which is the
// only way to see what would have been persisted. Repository is embedded as a
// nil interface on purpose: a method this file does not implement panics
// loudly rather than quietly exercising another path.
type defaultsRecordingRepo struct {
	toolkits.Repository
	storedType string
	creates    []map[string]any
	updates    []map[string]any
}

func (r *defaultsRecordingRepo) CreateToolkit(
	_ context.Context, _ string, body map[string]any,
) (map[string]any, error) {
	r.creates = append(r.creates, body)
	return map[string]any{"id": "11"}, nil
}

func (r *defaultsRecordingRepo) UpdateToolkit(
	_ context.Context, _, _ string, body map[string]any,
) (map[string]any, error) {
	r.updates = append(r.updates, body)
	return map[string]any{"id": "11"}, nil
}

func (r *defaultsRecordingRepo) GetToolkit(_ context.Context, _, _ string) (map[string]any, error) {
	return map[string]any{"id": "11", "type": r.storedType}, nil
}

// newDefaultsRouter mounts the real Create and Update behind the real pinned
// catalogue, with no settings validator: the subject here is what is written,
// not what is refused.
func newDefaultsRouter(t *testing.T, repo toolkits.Repository) http.Handler {
	t.Helper()
	handler := toolkits.NewHandlerWithRepo(repo, toolkits.WithCatalogue(pinnedCatalogue(t)))
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(),
				auth.User{ID: "7", UserID: "7", AuthType: "user"})))
		})
	})
	router.Post("/tools/prompt_lib/{projectID}", handler.Create)
	router.Put("/tool/prompt_lib/{projectID}/{toolkitID}", handler.Update)
	return router
}

func writtenSettings(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	settings, ok := body["settings"].(map[string]any)
	if !ok {
		t.Fatalf("the write carried no settings object: %v", body)
	}
	return settings
}

// storedJSON renders the settings the way CreateToolkit does before the
// INSERT (`json.Marshal(body["settings"])`), which is the only form that
// answers what the column receives: the catalogue snapshot decodes its numbers
// as json.Number, so an in-memory comparison would be against the decoder's
// representation rather than against the stored value.
func storedJSON(t *testing.T, settings map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	return string(encoded)
}

func postToolkit(t *testing.T, router http.Handler, payload string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/tools/prompt_lib/1", bytes.NewBufferString(payload))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestCreateFillsTheCatalogueDefaultTheBodyOmits(t *testing.T) {
	t.Parallel()

	repo := &defaultsRecordingRepo{}
	recorder := postToolkit(t, newDefaultsRouter(t, repo), `{
		"name": "qtest-tk",
		"type": "qtest",
		"settings": {"qtest_project_id": 1, "selected_tools": ["update_test_run_status"]}
	}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", recorder.Code, recorder.Body.String())
	}
	if len(repo.creates) != 1 {
		t.Fatalf("writes = %d, want 1", len(repo.creates))
	}

	settings := writtenSettings(t, repo.creates[0])
	if _, present := settings["no_of_tests_shown_in_dql_search"]; !present {
		t.Fatalf("the defaulted key was not stored: %v", settings)
	}
	// The column receives a JSON NUMBER, not a string: the SDK's model types
	// this field `int` and a quoted 10 would fail materialization the same way
	// the missing key did.
	if stored := storedJSON(t, settings); !strings.Contains(stored, `"no_of_tests_shown_in_dql_search":10`) {
		t.Fatalf("stored settings = %s, want the catalogue's 10 as a number", stored)
	}
	// The caller's own values survive untouched, and nothing the catalogue
	// defaults to null or [] is invented beside them.
	if settings["qtest_project_id"] != float64(1) {
		t.Fatalf("qtest_project_id = %#v, want 1", settings["qtest_project_id"])
	}
	if _, present := settings["pgvector_configuration"]; present {
		t.Fatal("a null-defaulted credential reference was written")
	}
}

func TestCreateKeepsTheValueTheBodySends(t *testing.T) {
	t.Parallel()

	repo := &defaultsRecordingRepo{}
	recorder := postToolkit(t, newDefaultsRouter(t, repo), `{
		"name": "qtest-tk",
		"type": "qtest",
		"settings": {"qtest_project_id": 1, "no_of_tests_shown_in_dql_search": 25}
	}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", recorder.Code, recorder.Body.String())
	}
	if got := writtenSettings(t, repo.creates[0])["no_of_tests_shown_in_dql_search"]; got != float64(25) {
		t.Fatalf("no_of_tests_shown_in_dql_search = %#v, want the sent 25", got)
	}
}

// UpdateToolkit REPLACES the settings column, so an update that omits the key
// loses it exactly the way a create does.
func TestUpdateFillsTheCatalogueDefaultForTheTypeItRestates(t *testing.T) {
	t.Parallel()

	repo := &defaultsRecordingRepo{storedType: "qtest"}
	router := newDefaultsRouter(t, repo)

	request := httptest.NewRequest(http.MethodPut, "/tool/prompt_lib/1/11", bytes.NewBufferString(`{
		"settings": {"qtest_project_id": 2}
	}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if len(repo.updates) != 1 {
		t.Fatalf("writes = %d, want 1", len(repo.updates))
	}
	// The body restates no type — the stored row's type is what selects the
	// catalogue entry, the same read the settings validator makes.
	if stored := storedJSON(t, writtenSettings(t, repo.updates[0])); !strings.Contains(
		stored, `"no_of_tests_shown_in_dql_search":10`) {
		t.Fatalf("stored settings = %s, want the catalogue's 10 as a number", stored)
	}
}

// A type the catalogue does not describe — the elitea_core-native ones — is
// written exactly as it arrived.
func TestCreateLeavesANativeTypeUntouched(t *testing.T) {
	t.Parallel()

	repo := &defaultsRecordingRepo{}
	recorder := postToolkit(t, newDefaultsRouter(t, repo), `{
		"name": "custom-tk",
		"type": "custom",
		"settings": {"url": "https://example.invalid"}
	}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", recorder.Code, recorder.Body.String())
	}
	settings := writtenSettings(t, repo.creates[0])
	if len(settings) != 1 || settings["url"] != "https://example.invalid" {
		t.Fatalf("settings were changed for a non-catalogued type: %v", settings)
	}
}

// With no catalogue composed at all — the pre-catalogue deployment shape — a
// save still succeeds and stores what it was given.
func TestCreateWithoutACatalogueStoresTheBodyAsSent(t *testing.T) {
	t.Parallel()

	repo := &defaultsRecordingRepo{}
	handler := toolkits.NewHandlerWithRepo(repo)
	router := chi.NewRouter()
	router.Post("/tools/prompt_lib/{projectID}", handler.Create)

	request := httptest.NewRequest(http.MethodPost, "/tools/prompt_lib/1", bytes.NewBufferString(`{
		"name": "qtest-tk", "type": "qtest", "settings": {"qtest_project_id": 1}
	}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", recorder.Code, recorder.Body.String())
	}
	if got := writtenSettings(t, repo.creates[0]); len(got) != 1 {
		t.Fatalf("settings = %v, want the body as sent", got)
	}
}
