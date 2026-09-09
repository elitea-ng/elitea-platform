package memories_test

// Unit coverage for the HTTP layer (#870): validation, authentication, and
// wire shape. Persisted-effect coverage (real SQL against real ownership
// scoping and the recall path) lives in
// internal/infra/db/repos/memories_postgres_integration_test.go — this file
// uses a mock Repository so it can run without a database.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/memories"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

type mockMemoriesRepo struct {
	entries []handler.MemoryEntry
	err     error

	lastListSearch string
	created        *handler.MemoryEntry
	cleared        int
}

func (m *mockMemoriesRepo) List(_ context.Context, _, _, search string) ([]handler.MemoryEntry, error) {
	m.lastListSearch = search
	if m.err != nil {
		return nil, m.err
	}
	return m.entries, nil
}

func (m *mockMemoriesRepo) Create(_ context.Context, projectID, _ string, entry handler.MemoryEntry) (handler.MemoryEntry, error) {
	if m.err != nil {
		return handler.MemoryEntry{}, m.err
	}
	entry.ID = "new-memory-id"
	entry.ProjectID = projectID
	entry.CreatedAt = time.Now()
	entry.UpdatedAt = time.Now()
	stored := entry
	m.created = &stored
	return entry, nil
}

func (m *mockMemoriesRepo) Update(_ context.Context, projectID, _, memoryID string, entry handler.MemoryEntry) (handler.MemoryEntry, error) {
	if m.err != nil {
		return handler.MemoryEntry{}, m.err
	}
	entry.ID = memoryID
	entry.ProjectID = projectID
	entry.UpdatedAt = time.Now()
	return entry, nil
}

func (m *mockMemoriesRepo) Delete(_ context.Context, _, _, _ string) error {
	return m.err
}

func (m *mockMemoriesRepo) ClearAll(_ context.Context, _, _ string) (int, error) {
	if m.err != nil {
		return 0, m.err
	}
	return m.cleared, nil
}

func mountMemories(repo handler.Repository) http.Handler {
	h := handler.NewHandler(repo)
	router := chi.NewRouter()
	router.Route("/api/v2/elitea_core", func(r chi.Router) {
		r.Get("/memories/prompt_lib/{projectID}", h.List)
		r.Post("/memories/prompt_lib/{projectID}", h.Create)
		r.Delete("/memories/prompt_lib/{projectID}", h.ClearAll)
		r.Put("/memory/prompt_lib/{projectID}/{memoryID}", h.Update)
		r.Delete("/memory/prompt_lib/{projectID}/{memoryID}", h.Delete)
	})
	return router
}

func withUser(req *http.Request, userID string) *http.Request {
	if userID == "" {
		return req
	}
	return req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: userID}))
}

func TestMemoriesHandlerRequiresAuthentication(t *testing.T) {
	router := mountMemories(&mockMemoriesRepo{})
	req := httptest.NewRequest(http.MethodGet, "/api/v2/elitea_core/memories/prompt_lib/1", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list = %d, want 401: %s", rec.Code, rec.Body.String())
	}
}

func TestMemoriesHandlerListAnswersOwnEntriesAndSearch(t *testing.T) {
	repo := &mockMemoriesRepo{entries: []handler.MemoryEntry{
		{ID: "1", Content: "prefers dark mode", Tags: []string{"ui"}, Enabled: true},
	}}
	router := mountMemories(repo)

	req := withUser(httptest.NewRequest(http.MethodGet, "/api/v2/elitea_core/memories/prompt_lib/1?q=dark", nil), "7")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	if repo.lastListSearch != "dark" {
		t.Errorf("search forwarded = %q, want %q", repo.lastListSearch, "dark")
	}
	var body struct {
		Items []handler.MemoryEntry `json:"items"`
		Total int                   `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Total != 1 || len(body.Items) != 1 {
		t.Fatalf("list body = %+v, want 1 item", body)
	}
}

func TestMemoriesHandlerCreateValidatesContent(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"empty content refused", `{"content": "  "}`, http.StatusBadRequest},
		{"content too long refused", `{"content": "` + strings.Repeat("a", handler.MaxContentBytes+1) + `"}`, http.StatusBadRequest},
		{"too many tags refused", tagsBody(handler.MaxTags + 1), http.StatusBadRequest},
		{"a too-long tag refused", `{"content": "ok", "tags": ["` + strings.Repeat("t", handler.MaxTagBytes+1) + `"]}`, http.StatusBadRequest},
		{"valid content accepted", `{"content": "remember this", "tags": ["a", "b"]}`, http.StatusCreated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &mockMemoriesRepo{}
			router := mountMemories(repo)
			req := withUser(httptest.NewRequest(http.MethodPost, "/api/v2/elitea_core/memories/prompt_lib/1", bytes.NewBufferString(tc.body)), "7")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("create(%s) = %d, want %d: %s", tc.name, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestMemoriesHandlerCreateDefaultsEnabledTrue(t *testing.T) {
	repo := &mockMemoriesRepo{}
	router := mountMemories(repo)
	req := withUser(httptest.NewRequest(http.MethodPost, "/api/v2/elitea_core/memories/prompt_lib/1", bytes.NewBufferString(`{"content": "remember this"}`)), "7")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	if repo.created == nil || !repo.created.Enabled {
		t.Fatalf("create defaulted enabled = %+v, want true", repo.created)
	}
	if repo.created.Tags == nil {
		t.Fatalf("create left tags nil, want an empty (non-nil) slice on the wire shape")
	}
}

func TestMemoriesHandlerCreateHonoursExplicitDisabled(t *testing.T) {
	repo := &mockMemoriesRepo{}
	router := mountMemories(repo)
	req := withUser(httptest.NewRequest(http.MethodPost, "/api/v2/elitea_core/memories/prompt_lib/1", bytes.NewBufferString(`{"content": "remember this", "enabled": false}`)), "7")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	if repo.created == nil || repo.created.Enabled {
		t.Fatalf("create ignored an explicit enabled:false: %+v", repo.created)
	}
}

func TestMemoriesHandlerUpdateRequiresFullRecord(t *testing.T) {
	repo := &mockMemoriesRepo{}
	router := mountMemories(repo)
	req := withUser(httptest.NewRequest(http.MethodPut, "/api/v2/elitea_core/memory/prompt_lib/1/9", bytes.NewBufferString(`{"content": ""}`)), "7")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update with empty content = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestMemoriesHandlerNotFoundOnAnotherOwnersEntry(t *testing.T) {
	repo := &mockMemoriesRepo{err: apierr.NotFound("memory not found")}
	router := mountMemories(repo)
	req := withUser(httptest.NewRequest(http.MethodPut, "/api/v2/elitea_core/memory/prompt_lib/1/9", bytes.NewBufferString(`{"content": "hijack"}`)), "7")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("update on a repository NotFound = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestMemoriesHandlerDeleteAndClearAll(t *testing.T) {
	repo := &mockMemoriesRepo{cleared: 4}
	router := mountMemories(repo)

	delReq := withUser(httptest.NewRequest(http.MethodDelete, "/api/v2/elitea_core/memory/prompt_lib/1/9", nil), "7")
	delRec := httptest.NewRecorder()
	router.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204: %s", delRec.Code, delRec.Body.String())
	}

	clearReq := withUser(httptest.NewRequest(http.MethodDelete, "/api/v2/elitea_core/memories/prompt_lib/1", nil), "7")
	clearRec := httptest.NewRecorder()
	router.ServeHTTP(clearRec, clearReq)
	if clearRec.Code != http.StatusOK {
		t.Fatalf("clear all = %d: %s", clearRec.Code, clearRec.Body.String())
	}
	var body struct {
		Removed int `json:"removed"`
	}
	if err := json.Unmarshal(clearRec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode clear-all body: %v", err)
	}
	if body.Removed != 4 {
		t.Errorf("clear all removed = %d, want 4", body.Removed)
	}
}

func tagsBody(count int) string {
	tags := make([]string, count)
	for i := range tags {
		tags[i] = "t" + strconv.Itoa(i)
	}
	encoded, _ := json.Marshal(map[string]any{"content": "ok", "tags": tags})
	return string(encoded)
}
