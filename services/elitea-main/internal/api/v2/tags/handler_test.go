package tags_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/tags"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// mockRepo implements tags.Repository for testing.
//
// It RECORDS what the handler asked it for. The Create and Delete tests below
// used to assert the handler's own echo, which was the whole defect: both
// verbs answered success without a repository call, so a test that read the
// response could not tell a stored tag from an invented one.
type mockRepo struct {
	tags []handler.Tag
	err  error

	listedCoverage handler.EntityCoverage
	created        *handler.Tag
	deletedID      string
	createErr      error
	deleteErr      error
}

func (m *mockRepo) List(_ context.Context, _ string, coverage handler.EntityCoverage) ([]handler.Tag, error) {
	m.listedCoverage = coverage
	if m.err != nil {
		return nil, m.err
	}
	return m.tags, nil
}

func (m *mockRepo) Create(_ context.Context, _ string, tag handler.Tag) (handler.Tag, error) {
	if m.createErr != nil {
		return handler.Tag{}, m.createErr
	}
	m.created = &tag
	// A real repository answers the STORED row, whose id the database chose.
	stored := tag
	stored.ID = 77
	return stored, nil
}

func (m *mockRepo) Delete(_ context.Context, _ string, tagID string) error {
	if m.deleteErr != nil {
		return m.deleteErr
	}
	m.deletedID = tagID
	return nil
}

func setupTagsRouter(repo handler.Repository) *chi.Mux {
	r := chi.NewRouter()
	h := handler.NewHandler(repo)
	r.Route("/api/v2/projects/{projectID}/tags", func(r chi.Router) {
		r.Mount("/", h.Routes())
	})
	return r
}

// ---- List -------------------------------------------------------------------

func TestTagList_Success(t *testing.T) {
	repo := &mockRepo{
		tags: []handler.Tag{
			{ID: 1, Name: "go"},
			{ID: 2, Name: "python"},
		},
	}
	r := setupTagsRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/api/v2/projects/proj-1/tags/", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	rows, ok := body["rows"]
	if !ok {
		t.Fatal("expected 'rows' key in response")
	}
	rowSlice, ok := rows.([]any)
	if !ok {
		t.Fatalf("expected 'rows' to be a slice, got %T", rows)
	}
	if len(rowSlice) != 2 {
		t.Errorf("expected 2 tags, got %d", len(rowSlice))
	}
}

func TestTagList_Empty(t *testing.T) {
	repo := &mockRepo{tags: []handler.Tag{}}
	r := setupTagsRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/api/v2/projects/proj-1/tags/", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var body map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&body)
	rows, _ := body["rows"].([]any)
	if len(rows) != 0 {
		t.Errorf("expected empty rows, got %d", len(rows))
	}
}

func TestTagList_RepoError(t *testing.T) {
	repo := &mockRepo{err: errors.New("database connection lost")}
	r := setupTagsRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/api/v2/projects/proj-1/tags/", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Non-APIError → internal server error
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// ---- Create -----------------------------------------------------------------

func TestTagCreate_StoresTheTagAndAnswersTheStoredRow(t *testing.T) {
	repo := &mockRepo{}

	payload, _ := json.Marshal(handler.Tag{Name: "new-tag", Data: map[string]any{"color": "blue"}})

	testRouter := chi.NewRouter()
	h := handler.NewHandler(repo)
	testRouter.Post("/tags/{projectID}", h.Create)

	req2 := httptest.NewRequest(http.MethodPost, "/tags/1", bytes.NewReader(payload))
	req2.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	testRouter.ServeHTTP(rec, req2)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}
	// The REPOSITORY was asked to store it. The handler used to answer 201
	// with the request echoed back and an id of 0, touching no table.
	if repo.created == nil {
		t.Fatal("the create answered 201 without asking the repository to store anything")
	}
	if repo.created.Name != "new-tag" {
		t.Errorf("stored name = %q, want \"new-tag\"", repo.created.Name)
	}

	var tag handler.Tag
	if err := json.NewDecoder(rec.Body).Decode(&tag); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if tag.ID != 77 {
		t.Errorf("response id = %d, want the id the store chose (77) — an id of 0 addresses nothing", tag.ID)
	}
	if tag.Name != "new-tag" {
		t.Errorf("expected name 'new-tag', got %q", tag.Name)
	}
}

func TestTagCreate_InvalidBody(t *testing.T) {
	repo := &mockRepo{}
	h := handler.NewHandler(repo)
	testRouter := chi.NewRouter()
	testRouter.Post("/tags/{projectID}", h.Create)

	req := httptest.NewRequest(http.MethodPost, "/tags/1", bytes.NewReader([]byte("not json{")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	testRouter.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestTagCreate_NamelessTagIsRefused(t *testing.T) {
	repo := &mockRepo{}
	h := handler.NewHandler(repo)
	testRouter := chi.NewRouter()
	testRouter.Post("/tags/{projectID}", h.Create)

	payload, _ := json.Marshal(map[string]any{"name": "   "})
	req := httptest.NewRequest(http.MethodPost, "/tags/1", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	testRouter.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if repo.created != nil {
		t.Error("a nameless tag reached the store; tags.name is NOT NULL")
	}
}

// ---- Delete -----------------------------------------------------------------

func TestTagDelete_RemovesTheRow(t *testing.T) {
	repo := &mockRepo{}
	h := handler.NewHandler(repo)
	testRouter := chi.NewRouter()
	testRouter.Delete("/tags/{projectID}/{tagID}", h.Delete)

	req := httptest.NewRequest(http.MethodDelete, "/tags/1/42", nil)
	rec := httptest.NewRecorder()
	testRouter.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d; body: %s", rec.Code, rec.Body.String())
	}
	// The 204 used to be written by the handler alone. The tag it claimed to
	// remove stayed in every list.
	if repo.deletedID != "42" {
		t.Errorf("the repository was asked to delete %q, want \"42\"", repo.deletedID)
	}
}

func TestTagDelete_AbsentTagIsNotFound(t *testing.T) {
	repo := &mockRepo{deleteErr: apierr.NotFound("tag not found")}
	h := handler.NewHandler(repo)
	testRouter := chi.NewRouter()
	testRouter.Delete("/tags/{projectID}/{tagID}", h.Delete)

	req := httptest.NewRequest(http.MethodDelete, "/tags/1/999", nil)
	rec := httptest.NewRecorder()
	testRouter.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// ---- entity_coverage --------------------------------------------------------

func TestTagList_PassesTheCoverageThrough(t *testing.T) {
	for _, coverage := range []handler.EntityCoverage{
		handler.CoverageApplication, handler.CoveragePipeline, handler.CoverageSkill, handler.CoverageAll,
	} {
		repo := &mockRepo{tags: []handler.Tag{}}
		r := setupTagsRouter(repo)
		req := httptest.NewRequest(http.MethodGet,
			"/api/v2/projects/1/tags/?entity_coverage="+string(coverage), nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d", coverage, rec.Code)
		}
		if repo.listedCoverage != coverage {
			t.Errorf("%s: the repository was asked for %q", coverage, repo.listedCoverage)
		}
	}
}

func TestTagList_NoCoverageMeansEveryTagInTheProject(t *testing.T) {
	repo := &mockRepo{tags: []handler.Tag{}}
	r := setupTagsRouter(repo)
	req := httptest.NewRequest(http.MethodGet, "/api/v2/projects/1/tags/", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if repo.listedCoverage != handler.CoverageAll {
		t.Errorf("an unfiltered list asked for %q, want the whole project", repo.listedCoverage)
	}
}

// An unknown coverage is REFUSED. Legacy answers an empty list, which reads
// exactly like a project with no tags — a misspelt filter and an empty project
// must not be the same answer.
func TestTagList_UnknownCoverageIsRefused(t *testing.T) {
	repo := &mockRepo{tags: []handler.Tag{{ID: 1, Name: "go"}}}
	r := setupTagsRouter(repo)
	req := httptest.NewRequest(http.MethodGet, "/api/v2/projects/1/tags/?entity_coverage=agents", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if repo.listedCoverage != "" {
		t.Errorf("the refused request still reached the store as %q", repo.listedCoverage)
	}
}

// ---- Content-Type -----------------------------------------------------------

func TestTagList_ContentTypeJSON(t *testing.T) {
	repo := &mockRepo{tags: []handler.Tag{}}
	r := setupTagsRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/api/v2/projects/proj-1/tags/", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	ct := rec.Header().Get("Content-Type")
	if ct == "" {
		t.Error("expected Content-Type header")
	}
}
