package folders_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	handler "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/folders"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// mockFolderRepo implements folders.Repository.
type mockFolderRepo struct {
	folders []handler.Folder
	err     error

	// updated records the Folder the handler asked to persist, so the
	// reorder tests can assert on the position it resolved.
	updated *handler.Folder
	// rebalanced is the folder set List returns after Rebalance is called,
	// and rebalanceCalls counts the calls.
	rebalanced     []handler.Folder
	rebalanceCalls int
}

func (m *mockFolderRepo) List(_ context.Context, _ string) ([]handler.Folder, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.folders, nil
}

func (m *mockFolderRepo) Create(_ context.Context, projectID string, f handler.Folder) (handler.Folder, error) {
	if m.err != nil {
		return handler.Folder{}, m.err
	}
	f.ID = "new-folder-id"
	f.ProjectID = projectID
	f.CreatedAt = time.Now()
	f.UpdatedAt = time.Now()
	return f, nil
}

func (m *mockFolderRepo) Update(_ context.Context, projectID, folderID string, f handler.Folder) (handler.Folder, error) {
	if m.err != nil {
		return handler.Folder{}, m.err
	}
	f.ID = folderID
	f.ProjectID = projectID
	f.UpdatedAt = time.Now()
	stored := f
	m.updated = &stored
	return f, nil
}

func (m *mockFolderRepo) Delete(_ context.Context, _, _ string) error {
	return m.err
}

func (m *mockFolderRepo) Rebalance(_ context.Context, _ string) ([]handler.Folder, error) {
	if m.err != nil {
		return nil, m.err
	}
	m.rebalanceCalls++
	m.folders = m.rebalanced
	return m.folders, nil
}

func pos(v int) *int { return &v }

func setupFoldersRouter(repo handler.Repository) *chi.Mux {
	r := chi.NewRouter()
	h := handler.NewHandler(repo)
	r.Route("/api/v2/projects/{projectID}/folders", func(r chi.Router) {
		r.Mount("/", h.Routes())
	})
	return r
}

// ---- List -------------------------------------------------------------------

func TestFolderList_Success(t *testing.T) {
	repo := &mockFolderRepo{
		folders: []handler.Folder{
			{ID: "f-1", ProjectID: "proj-1", Name: "Folder A"},
			{ID: "f-2", ProjectID: "proj-1", Name: "Folder B"},
		},
	}
	r := setupFoldersRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/api/v2/projects/proj-1/folders/", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatal("expected 'items' key in response")
	}
	if len(items) != 2 {
		t.Errorf("expected 2 folders, got %d", len(items))
	}
}

func TestFolderList_Empty(t *testing.T) {
	repo := &mockFolderRepo{folders: []handler.Folder{}}
	r := setupFoldersRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/api/v2/projects/proj-1/folders/", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var body map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&body)
	items, _ := body["items"].([]any)
	if len(items) != 0 {
		t.Errorf("expected empty items, got %d", len(items))
	}
}

func TestFolderList_Error(t *testing.T) {
	repo := &mockFolderRepo{err: errors.New("db failure")}
	r := setupFoldersRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/api/v2/projects/proj-1/folders/", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// ---- Create -----------------------------------------------------------------

func TestFolderCreate_Success(t *testing.T) {
	repo := &mockFolderRepo{}
	r := setupFoldersRouter(repo)

	payload, _ := json.Marshal(handler.Folder{Name: "New Folder"})
	req := httptest.NewRequest(http.MethodPost, "/api/v2/projects/proj-1/folders/", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var folder handler.Folder
	if err := json.NewDecoder(rec.Body).Decode(&folder); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if folder.ID != "new-folder-id" {
		t.Errorf("expected ID 'new-folder-id', got %q", folder.ID)
	}
	if folder.Name != "New Folder" {
		t.Errorf("expected Name 'New Folder', got %q", folder.Name)
	}
}

func TestFolderCreate_InvalidBody(t *testing.T) {
	repo := &mockFolderRepo{}
	r := setupFoldersRouter(repo)

	req := httptest.NewRequest(http.MethodPost, "/api/v2/projects/proj-1/folders/", bytes.NewReader([]byte("bad json")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestFolderCreate_RepoError(t *testing.T) {
	repo := &mockFolderRepo{err: apierr.Internal("create failed")}
	r := setupFoldersRouter(repo)

	payload, _ := json.Marshal(handler.Folder{Name: "X"})
	req := httptest.NewRequest(http.MethodPost, "/api/v2/projects/proj-1/folders/", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// ---- Update (PUT) -----------------------------------------------------------

func TestFolderUpdate_Success(t *testing.T) {
	repo := &mockFolderRepo{}
	r := setupFoldersRouter(repo)

	payload, _ := json.Marshal(handler.Folder{Name: "Renamed Folder"})
	req := httptest.NewRequest(http.MethodPut, "/api/v2/projects/proj-1/folders/f-1", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var folder handler.Folder
	if err := json.NewDecoder(rec.Body).Decode(&folder); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if folder.ID != "f-1" {
		t.Errorf("expected ID 'f-1', got %q", folder.ID)
	}
	if folder.Name != "Renamed Folder" {
		t.Errorf("expected updated name, got %q", folder.Name)
	}
}

func TestFolderUpdate_NotFound(t *testing.T) {
	repo := &mockFolderRepo{err: apierr.NotFound("folder not found")}
	r := setupFoldersRouter(repo)

	payload, _ := json.Marshal(handler.Folder{Name: "X"})
	req := httptest.NewRequest(http.MethodPut, "/api/v2/projects/proj-1/folders/f-missing", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestFolderUpdate_InvalidBody(t *testing.T) {
	repo := &mockFolderRepo{}
	r := setupFoldersRouter(repo)

	req := httptest.NewRequest(http.MethodPut, "/api/v2/projects/proj-1/folders/f-1", bytes.NewReader([]byte("{{invalid")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// ---- Update: reorder (#128 defect 3) ----------------------------------------

// putFolder issues the PUT and returns the recorder, so each reorder case is
// one line of setup.
func putFolder(t *testing.T, repo *mockFolderRepo, folderID, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := setupFoldersRouter(repo)
	req := httptest.NewRequest(http.MethodPut, "/api/v2/projects/proj-1/folders/"+folderID, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	return rec
}

// The literal payload useReorderFolders.ts:79-97 sends when a folder is
// dragged to the very top: no neighbour above, the old first folder below.
// The resolved position must be ABOVE that folder's, since positions sort
// descending.
func TestFolderUpdate_ReorderToTop(t *testing.T) {
	repo := &mockFolderRepo{folders: []handler.Folder{
		{ID: "f-1", Name: "First", Position: pos(2 * handler.PositionGap)},
		{ID: "f-2", Name: "Second", Position: pos(handler.PositionGap)},
	}}

	putFolder(t, repo, "f-2", `{"name":"Second","position":0,"neighbor_above_id":null,"neighbor_below_id":"f-1"}`)

	if repo.updated == nil || repo.updated.Position == nil {
		t.Fatal("handler persisted no position")
	}
	if got := *repo.updated.Position; got <= 2*handler.PositionGap {
		t.Errorf("reorder to top must resolve above f-1's %d, got %d", 2*handler.PositionGap, got)
	}
}

// The neighbours are authoritative: a `position` that contradicts them (here
// 0, which would sort the folder LAST) must not win. This is the deviation
// from the legacy runtime documented on resolveReorder, and the exact reason
// the E2E reorder journey failed while returning 200.
func TestFolderUpdate_NeighborsOverrideContradictoryPosition(t *testing.T) {
	repo := &mockFolderRepo{folders: []handler.Folder{
		{ID: "f-1", Name: "First", Position: pos(5000)},
		{ID: "f-2", Name: "Second", Position: pos(1000)},
	}}

	putFolder(t, repo, "f-2", `{"name":"Second","position":0,"neighbor_above_id":null,"neighbor_below_id":"f-1"}`)

	if got := *repo.updated.Position; got == 0 {
		t.Error("contradictory position:0 was stored verbatim; neighbours must win")
	}
}

// Dropped between two folders → strictly between their positions.
func TestFolderUpdate_ReorderBetweenNeighbors(t *testing.T) {
	repo := &mockFolderRepo{folders: []handler.Folder{
		{ID: "f-1", Name: "First", Position: pos(3000)},
		{ID: "f-2", Name: "Second", Position: pos(1000)},
		{ID: "f-3", Name: "Third", Position: pos(500)},
	}}

	putFolder(t, repo, "f-3", `{"name":"Third","neighbor_above_id":"f-1","neighbor_below_id":"f-2"}`)

	got := *repo.updated.Position
	if got <= 1000 || got >= 3000 {
		t.Errorf("expected a position strictly between 1000 and 3000, got %d", got)
	}
}

// Adjacent integers leave no room, so the handler must rebalance and retry
// rather than storing a colliding position.
func TestFolderUpdate_RebalancesWhenNoRoomBetweenNeighbors(t *testing.T) {
	repo := &mockFolderRepo{
		folders: []handler.Folder{
			{ID: "f-1", Name: "First", Position: pos(1001)},
			{ID: "f-2", Name: "Second", Position: pos(1000)},
		},
		rebalanced: []handler.Folder{
			{ID: "f-1", Name: "First", Position: pos(2 * handler.PositionGap)},
			{ID: "f-2", Name: "Second", Position: pos(handler.PositionGap)},
		},
	}

	putFolder(t, repo, "f-3", `{"name":"Third","neighbor_above_id":"f-1","neighbor_below_id":"f-2"}`)

	if repo.rebalanceCalls != 1 {
		t.Fatalf("expected exactly 1 rebalance, got %d", repo.rebalanceCalls)
	}
	got := *repo.updated.Position
	if got <= handler.PositionGap || got >= 2*handler.PositionGap {
		t.Errorf("post-rebalance position %d is not between the respaced neighbours", got)
	}
}

// A rename carries no position and no neighbours, so the stored order must be
// left alone — nil, which the repo turns into "keep the current position".
func TestFolderUpdate_RenameLeavesPositionUntouched(t *testing.T) {
	repo := &mockFolderRepo{folders: []handler.Folder{
		{ID: "f-1", Name: "First", Position: pos(1000)},
	}}

	putFolder(t, repo, "f-1", `{"name":"Renamed"}`)

	if repo.updated.Position != nil {
		t.Errorf("a rename must not carry a position, got %d", *repo.updated.Position)
	}
}

// Without neighbours an explicit position is the caller's whole intent.
func TestFolderUpdate_ExplicitPositionWithoutNeighborsIsStored(t *testing.T) {
	repo := &mockFolderRepo{folders: []handler.Folder{
		{ID: "f-1", Name: "First", Position: pos(1000)},
	}}

	putFolder(t, repo, "f-1", `{"name":"First","position":4242}`)

	if repo.updated.Position == nil || *repo.updated.Position != 4242 {
		t.Errorf("expected the explicit position 4242 to be stored, got %v", repo.updated.Position)
	}
}

// A numeric neighbour id — the wire shape when the client has not stringified
// its ids — must resolve the same as the string form.
func TestFolderUpdate_NumericNeighborID(t *testing.T) {
	repo := &mockFolderRepo{folders: []handler.Folder{
		{ID: "7", Name: "Seven", Position: pos(9000)},
		{ID: "8", Name: "Eight", Position: pos(1000)},
	}}

	putFolder(t, repo, "8", `{"name":"Eight","neighbor_above_id":null,"neighbor_below_id":7}`)

	if got := *repo.updated.Position; got <= 9000 {
		t.Errorf("expected a position above 9000, got %d", got)
	}
}

// ---- Update (PATCH) ---------------------------------------------------------

func TestFolderPatch_Success(t *testing.T) {
	repo := &mockFolderRepo{
		folders: []handler.Folder{
			{ID: "f-1", ProjectID: "proj-1", Name: "Original Folder"},
		},
	}
	r := setupFoldersRouter(repo)

	payload, _ := json.Marshal(handler.Folder{Name: "Patched Folder"})
	req := httptest.NewRequest(http.MethodPatch, "/api/v2/projects/proj-1/folders/f-1", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// ---- Delete -----------------------------------------------------------------

func TestFolderDelete_Success(t *testing.T) {
	repo := &mockFolderRepo{}
	r := setupFoldersRouter(repo)

	req := httptest.NewRequest(http.MethodDelete, "/api/v2/projects/proj-1/folders/f-1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestFolderDelete_NotFound(t *testing.T) {
	repo := &mockFolderRepo{err: apierr.NotFound("folder not found")}
	r := setupFoldersRouter(repo)

	req := httptest.NewRequest(http.MethodDelete, "/api/v2/projects/proj-1/folders/missing", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestFolderDelete_Error(t *testing.T) {
	repo := &mockFolderRepo{err: apierr.Internal("db error")}
	r := setupFoldersRouter(repo)

	req := httptest.NewRequest(http.MethodDelete, "/api/v2/projects/proj-1/folders/f-1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d; body: %s", rec.Code, rec.Body.String())
	}
}
