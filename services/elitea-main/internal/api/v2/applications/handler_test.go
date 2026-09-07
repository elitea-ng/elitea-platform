package applications_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/applications"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/applications"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

type mockRepo struct {
	apps     []applications.Application
	versions []applications.Version
	err      error
}

func (m *mockRepo) List(_ context.Context, req applications.ListRequest) (applications.ListResponse, error) {
	if m.err != nil {
		return applications.ListResponse{}, m.err
	}
	return applications.ListResponse{
		Rows:       m.apps,
		Total:      len(m.apps),
		Page:       req.Page,
		PageSize:   req.PageSize,
		TotalPages: 1,
	}, nil
}

func (m *mockRepo) Get(_ context.Context, _, _ string) (applications.Application, error) {
	if m.err != nil {
		return applications.Application{}, m.err
	}
	if len(m.apps) == 0 {
		return applications.Application{}, apierr.NotFound("application not found")
	}
	return m.apps[0], nil
}

func (m *mockRepo) Create(_ context.Context, req applications.CreateRequest) (applications.Application, error) {
	if m.err != nil {
		return applications.Application{}, m.err
	}
	return applications.Application{
		ID:        "new-id",
		ProjectID: req.ProjectID,
		Name:      req.Name,
		Type:      req.Type,
		Status:    "active",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}, nil
}

func (m *mockRepo) Update(_ context.Context, req applications.UpdateRequest) (applications.Application, error) {
	if m.err != nil {
		return applications.Application{}, m.err
	}
	app := m.apps[0]
	if req.Name != nil {
		app.Name = *req.Name
	}
	return app, nil
}

func (m *mockRepo) Delete(_ context.Context, _, _ string) error {
	return m.err
}

func (m *mockRepo) GetVersion(_ context.Context, _, _, _ string) (applications.Version, error) {
	if m.err != nil {
		return applications.Version{}, m.err
	}
	if len(m.versions) == 0 {
		return applications.Version{}, apierr.NotFound("version not found")
	}
	return m.versions[0], nil
}

func (m *mockRepo) ListVersions(_ context.Context, _, _ string) ([]applications.Version, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.versions, nil
}

func (m *mockRepo) CreateVersion(_ context.Context, _, _ string, v applications.Version) (applications.Version, error) {
	if m.err != nil {
		return applications.Version{}, m.err
	}
	v.ID = "new-ver-id"
	return v, nil
}

func (m *mockRepo) UpdateVersion(_ context.Context, _, _, _ string, v applications.Version) (applications.Version, error) {
	if m.err != nil {
		return applications.Version{}, m.err
	}
	if len(m.versions) == 0 {
		return applications.Version{}, apierr.NotFound("version not found")
	}
	m.versions[0].Name = v.Name
	return m.versions[0], nil
}

func (m *mockRepo) DeleteVersion(_ context.Context, _, _, _ string) error {
	return m.err
}

func (m *mockRepo) SetDefaultVersion(_ context.Context, _, _, _ string) error {
	return m.err
}

func (m *mockRepo) BatchReplaceVersion(_ context.Context, _, _, _ string, _ bool) error {
	return m.err
}

func setupRouter(repo applications.Repository) *chi.Mux {
	r := chi.NewRouter()
	h := handler.NewHandler(repo)
	// Every /elitea_core route is inside the authenticated group in
	// internal/api/router.go, so the handler now requires a principal and a
	// numeric tenant project id (#115). These doubles supply both.
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(auth.ContextWithUser(req.Context(),
				auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})))
		})
	})
	r.Route("/api/v2/projects/{projectID}/applications", func(r chi.Router) {
		r.Mount("/", h.Routes())
	})
	return r
}

func TestList_Success(t *testing.T) {
	repo := &mockRepo{
		apps: []applications.Application{
			{ID: "app-1", Name: "Agent 1", Status: "active"},
			{ID: "app-2", Name: "Agent 2", Status: "active"},
		},
	}
	r := setupRouter(repo)

	req := httptest.NewRequest("GET", "/api/v2/projects/1/applications?page=1&page_size=20", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp applications.ListResponse
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if len(resp.Rows) != 2 {
		t.Errorf("expected 2 items, got %d", len(resp.Rows))
	}
	if resp.Total != 2 {
		t.Errorf("expected total 2, got %d", resp.Total)
	}
}

// The Get() success path is covered by
// TestHandlerPostgres_CreateThenReadBackIsWhatJourney14Asserts in
// handler_postgres_integration_test.go, which drives the real pool and reads
// the created application back with its versions and version_details.
//
// A `TestGet_Success` used to sit here holding one unconditional `t.Skip`.
// It ran on every CI run, executed nothing, and printed `ok` (#423). A test
// that can never fail is not coverage; it is the appearance of coverage, so
// the real one above is now the only claim made.

func TestGet_NotFound(t *testing.T) {
	repo := &mockRepo{apps: nil}
	r := setupRouter(repo)

	req := httptest.NewRequest("GET", "/api/v2/projects/1/applications/nonexistent", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestCreate_Success(t *testing.T) {
	repo := &mockRepo{}
	r := setupRouter(repo)

	body, _ := json.Marshal(map[string]string{"name": "New Agent", "type": "chat"})
	req := httptest.NewRequest("POST", "/api/v2/projects/1/applications", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var app applications.Application
	_ = json.NewDecoder(rec.Body).Decode(&app)
	if app.Name != "New Agent" {
		t.Errorf("expected name 'New Agent', got %q", app.Name)
	}
}

func TestCreate_InvalidBody(t *testing.T) {
	repo := &mockRepo{}
	r := setupRouter(repo)

	req := httptest.NewRequest("POST", "/api/v2/projects/1/applications", bytes.NewReader([]byte("not json")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestDelete_Success(t *testing.T) {
	repo := &mockRepo{}
	r := setupRouter(repo)

	req := httptest.NewRequest("DELETE", "/api/v2/projects/1/applications/app-1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rec.Code)
	}
}

func TestUpdate_Success(t *testing.T) {
	name := "Updated Name"
	repo := &mockRepo{
		apps: []applications.Application{
			{ID: "app-1", Name: "Original", ProjectID: "proj-1"},
		},
	}
	r := setupRouter(repo)

	body, _ := json.Marshal(map[string]*string{"name": &name})
	req := httptest.NewRequest("PUT", "/api/v2/projects/1/applications/app-1", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// The Update handler writes http.StatusCreated (201), not 200 — this matches the
	// handler implementation at handler.go:593.
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var app applications.Application
	_ = json.NewDecoder(rec.Body).Decode(&app)
	if app.Name != "Updated Name" {
		t.Errorf("expected updated name, got %q", app.Name)
	}
}

// recordingRepo captures the applications.Version the handler builds from the
// request body, so a test can assert on what the handler decided to write
// rather than on what the repository happened to echo back.
type recordingRepo struct {
	mockRepo
	lastUpdate applications.Version
}

func (m *recordingRepo) UpdateVersion(_ context.Context, _, _, _ string, v applications.Version) (applications.Version, error) {
	m.lastUpdate = v
	return applications.Version{ID: "7", Name: v.Name}, nil
}

func setupVersionRouter(repo applications.Repository) *chi.Mux {
	r := chi.NewRouter()
	h := handler.NewHandler(repo)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(auth.ContextWithUser(req.Context(),
				auth.User{ID: "1", UserID: "1", Email: "one@elitea.ai"})))
		})
	})
	r.Put("/version/prompt_lib/{projectID}/{applicationID}/{versionID}", h.UpdateVersion)
	return r
}

// #135: the pipeline editor's Save returned 200 while the flow graph was
// discarded, because UpdateVersion read no pipeline_settings key off the body.
func TestUpdateVersion_ForwardsPipelineSettings(t *testing.T) {
	repo := &recordingRepo{}
	r := setupVersionRouter(repo)

	body, _ := json.Marshal(map[string]any{
		"name":         "base",
		"instructions": "nodes:\n  - id: Agent 1\n",
		"pipeline_settings": map[string]any{
			"nodes":          []any{map[string]any{"id": "Agent 1"}},
			"edges":          []any{},
			"orientation":    "vertical",
			"layout_version": "1.0",
		},
	})
	req := httptest.NewRequest("PUT", "/version/prompt_lib/1/2/3", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if repo.lastUpdate.PipelineSettings == nil {
		t.Fatalf("pipeline_settings was dropped; version = %+v", repo.lastUpdate)
	}
	nodes, ok := repo.lastUpdate.PipelineSettings["nodes"].([]any)
	if !ok || len(nodes) != 1 {
		t.Errorf("pipeline_settings.nodes = %v", repo.lastUpdate.PipelineSettings["nodes"])
	}
	if repo.lastUpdate.PipelineSettings["layout_version"] != "1.0" {
		t.Errorf("pipeline_settings.layout_version = %v", repo.lastUpdate.PipelineSettings["layout_version"])
	}
	if repo.lastUpdate.Instructions != "nodes:\n  - id: Agent 1\n" {
		t.Errorf("instructions = %q", repo.lastUpdate.Instructions)
	}
}

// #824 — an explicit empty string is a real edit and must reach the repository
// as PRESENT, so the UPDATE's SET list carries it and the column is cleared.
// Before the fix the handler decoded presence correctly and then threw it away
// by storing a plain string, and the repository read "" as "not sent".
func TestUpdateVersion_ExplicitEmptyStringsAreMarkedPresent(t *testing.T) {
	repo := &recordingRepo{}
	r := setupVersionRouter(repo)

	body, _ := json.Marshal(map[string]any{
		"instructions":    "",
		"welcome_message": "",
	})
	req := httptest.NewRequest("PUT", "/version/prompt_lib/1/2/3", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if !repo.lastUpdate.Present.Instructions {
		t.Error("instructions: an explicit \"\" did not reach the repository as present")
	}
	if !repo.lastUpdate.Present.WelcomeMessage {
		t.Error("welcome_message: an explicit \"\" did not reach the repository as present")
	}
	if repo.lastUpdate.Instructions != "" || repo.lastUpdate.WelcomeMessage != "" {
		t.Errorf("values were rewritten: %+v", repo.lastUpdate)
	}
}

// The mirror of the test above: a key the caller never sent must NOT be marked
// present, or every partial save would blank the fields it did not mention.
func TestUpdateVersion_OmittedStringsAreNotPresent(t *testing.T) {
	repo := &recordingRepo{}
	r := setupVersionRouter(repo)

	body, _ := json.Marshal(map[string]any{"name": "base"})
	req := httptest.NewRequest("PUT", "/version/prompt_lib/1/2/3", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if repo.lastUpdate.Present.Instructions || repo.lastUpdate.Present.WelcomeMessage ||
		repo.lastUpdate.Present.AgentType {
		t.Errorf("an unsent key was marked present: %+v", repo.lastUpdate.Present)
	}
	if !repo.lastUpdate.Present.Name {
		t.Error("name was sent and must be present")
	}
}

// `name` and `agent_type` are NOT NULL columns the create path defaults when
// it sees "", and the version name is unique per application. An explicit ""
// is therefore refused rather than written — and, above all, rather than
// silently dropped, which is the shape #824 is about.
func TestUpdateVersion_EmptyNameAndAgentTypeAreRefused(t *testing.T) {
	for _, field := range []string{"name", "agent_type"} {
		t.Run(field, func(t *testing.T) {
			repo := &recordingRepo{}
			r := setupVersionRouter(repo)

			body, _ := json.Marshal(map[string]any{field: ""})
			req := httptest.NewRequest("PUT", "/version/prompt_lib/1/2/3", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// A body with no pipeline_settings key must leave the stored value alone —
// nil, not an empty map that would blank the column.
func TestUpdateVersion_OmittedPipelineSettingsStaysNil(t *testing.T) {
	repo := &recordingRepo{}
	r := setupVersionRouter(repo)

	body, _ := json.Marshal(map[string]any{"name": "base"})
	req := httptest.NewRequest("PUT", "/version/prompt_lib/1/2/3", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if repo.lastUpdate.PipelineSettings != nil {
		t.Errorf("expected nil pipeline_settings, got %v", repo.lastUpdate.PipelineSettings)
	}
}

// #307: the agent editor sends `variables` on every save, and UpdateVersion
// had no branch for the key at all — the PUT answered 201 and the edit was
// gone. Variables have no column of their own; they are stored inside
// `meta` (see versionFromBody), so the assertion is on meta["variables"],
// and it deliberately runs with a `meta` in the body carrying the STALE
// variables the client spreads from the stored blob: the fold has to win
// over that or the round-trip still loses the edit.
func TestUpdateVersion_ForwardsVariablesIntoMeta(t *testing.T) {
	repo := &recordingRepo{}
	r := setupVersionRouter(repo)

	body, _ := json.Marshal(map[string]any{
		"name": "base",
		"meta": map[string]any{
			"step_limit": 25,
			"variables":  []any{map[string]any{"name": "stale", "value": "old"}},
		},
		"variables": []any{
			map[string]any{"name": "region", "value": "emea"},
			map[string]any{"name": "tier", "value": "gold"},
		},
	})
	req := httptest.NewRequest("PUT", "/version/prompt_lib/1/2/3", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if repo.lastUpdate.Meta == nil {
		t.Fatalf("meta was dropped; version = %+v", repo.lastUpdate)
	}
	vars, ok := repo.lastUpdate.Meta["variables"].([]any)
	if !ok || len(vars) != 2 {
		t.Fatalf("meta.variables = %v, want the 2 edited variables", repo.lastUpdate.Meta["variables"])
	}
	first, _ := vars[0].(map[string]any)
	if first["name"] != "region" || first["value"] != "emea" {
		t.Errorf("meta.variables[0] = %v, want {region emea} — the client's stale meta.variables won", vars[0])
	}
	if repo.lastUpdate.Meta["step_limit"] != float64(25) {
		t.Errorf("meta.step_limit = %v (%T), want 25 — folding variables must not drop the rest of meta",
			repo.lastUpdate.Meta["step_limit"], repo.lastUpdate.Meta["step_limit"])
	}
}

// Deleting the last variable is a real user action and must reach the
// repository as an empty list, not be swallowed the way a `len(vars) > 0`
// guard would swallow it.
func TestUpdateVersion_ClearsVariablesWhenEmptyListSent(t *testing.T) {
	repo := &recordingRepo{}
	r := setupVersionRouter(repo)

	body, _ := json.Marshal(map[string]any{
		"name":      "base",
		"meta":      map[string]any{"variables": []any{map[string]any{"name": "gone", "value": "x"}}},
		"variables": []any{},
	})
	req := httptest.NewRequest("PUT", "/version/prompt_lib/1/2/3", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
	vars, ok := repo.lastUpdate.Meta["variables"].([]any)
	if !ok || len(vars) != 0 {
		t.Errorf("meta.variables = %v, want an empty list", repo.lastUpdate.Meta["variables"])
	}
}

// A body with no `variables` key must leave the stored value alone rather
// than blanking it — the same "unset means untouched" contract every other
// branch of UpdateVersion keeps.
func TestUpdateVersion_OmittedVariablesLeavesMetaAlone(t *testing.T) {
	repo := &recordingRepo{}
	r := setupVersionRouter(repo)

	body, _ := json.Marshal(map[string]any{"name": "base"})
	req := httptest.NewRequest("PUT", "/version/prompt_lib/1/2/3", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if repo.lastUpdate.Meta != nil {
		t.Errorf("expected nil meta, got %v", repo.lastUpdate.Meta)
	}
}
