package contextmgr_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/contextmgr"
)

// mockRepo implements contextmgr.Repository for testing.
type mockRepo struct {
	analytics     contextmgr.Analytics
	analyticsErr  error
	summaries     []contextmgr.Summary
	total         int
	listErr       error
	createSummary contextmgr.Summary
	createErr     error
	updateErr     error
	deleteErr     error
}

func (m *mockRepo) GetAnalytics(_ context.Context, _, _ string) (contextmgr.Analytics, error) {
	return m.analytics, m.analyticsErr
}

func (m *mockRepo) ListSummaries(_ context.Context, _, _ string) ([]contextmgr.Summary, int, error) {
	if m.listErr != nil {
		return nil, 0, m.listErr
	}
	return m.summaries, m.total, nil
}

func (m *mockRepo) CreateSummary(_ context.Context, _, _, content string) (contextmgr.Summary, error) {
	if m.createErr != nil {
		return contextmgr.Summary{}, m.createErr
	}
	s := m.createSummary
	if s.Content == "" {
		s.Content = content
	}
	return s, nil
}

func (m *mockRepo) UpdateSummary(_ context.Context, _, _, _, _ string) error {
	return m.updateErr
}

func (m *mockRepo) DeleteSummary(_ context.Context, _, _, _ string) error {
	return m.deleteErr
}

func newTestRouter(h *contextmgr.Handler) chi.Router {
	r := chi.NewRouter()
	r.Post("/projects/{projectID}/conversations/{conversationID}/optimize", h.OptimizeContext)
	r.Get("/projects/{projectID}/conversations/{conversationID}/analytics", h.GetAnalytics)
	r.Get("/projects/{projectID}/conversations/{conversationID}/summaries", h.ListSummaries)
	r.Post("/projects/{projectID}/conversations/{conversationID}/summaries", h.CreateSummary)
	r.Put("/projects/{projectID}/conversations/{conversationID}/summaries/{summaryID}", h.UpdateSummary)
	r.Delete("/projects/{projectID}/conversations/{conversationID}/summaries/{summaryID}", h.DeleteSummary)
	return r
}

func TestOptimizeContext_ReturnsOK(t *testing.T) {
	repo := &mockRepo{}
	h := contextmgr.NewHandlerWithRepo(repo)
	r := newTestRouter(h)

	req := httptest.NewRequest(http.MethodPost, "/projects/p1/conversations/c1/optimize", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if v, _ := resp["ok"].(bool); !v {
		t.Errorf("expected ok=true in response, got %v", resp)
	}
}

func TestGetAnalytics_ReturnsData(t *testing.T) {
	repo := &mockRepo{
		analytics: contextmgr.Analytics{TokenCount: 1500, MaxTokens: 128000},
	}
	h := contextmgr.NewHandlerWithRepo(repo)
	r := newTestRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/projects/p1/conversations/c1/analytics", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var resp contextmgr.Analytics
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.TokenCount != 1500 {
		t.Errorf("expected token_count 1500, got %d", resp.TokenCount)
	}
	if resp.MaxTokens != 128000 {
		t.Errorf("expected max_tokens 128000, got %d", resp.MaxTokens)
	}
}

func TestGetAnalytics_ErrorFallsBackToDefaults(t *testing.T) {
	repo := &mockRepo{
		analyticsErr: fmt.Errorf("db error"),
	}
	h := contextmgr.NewHandlerWithRepo(repo)
	r := newTestRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/projects/p1/conversations/c1/analytics", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var resp contextmgr.Analytics
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.TokenCount != 0 {
		t.Errorf("expected token_count 0, got %d", resp.TokenCount)
	}
	if resp.MaxTokens != 128000 {
		t.Errorf("expected max_tokens 128000, got %d", resp.MaxTokens)
	}
}

func TestListSummaries_Empty(t *testing.T) {
	repo := &mockRepo{summaries: []contextmgr.Summary{}, total: 0}
	h := contextmgr.NewHandlerWithRepo(repo)
	r := newTestRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/projects/p1/conversations/c1/summaries", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	items, ok := resp["summaries"].([]interface{})
	if !ok {
		t.Fatalf("expected summaries array, got %T", resp["summaries"])
	}
	if len(items) != 0 {
		t.Errorf("expected empty summaries, got %d", len(items))
	}
	if total := resp["total"].(float64); total != 0 {
		t.Errorf("expected total 0, got %v", total)
	}
}

func TestListSummaries_ErrorFallsBackToEmpty(t *testing.T) {
	repo := &mockRepo{listErr: fmt.Errorf("db error")}
	h := contextmgr.NewHandlerWithRepo(repo)
	r := newTestRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/projects/p1/conversations/c1/summaries", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["summaries"] == nil {
		t.Error("expected summaries field in response")
	}
}

func TestCreateSummary_ReturnsCreated(t *testing.T) {
	repo := &mockRepo{
		createSummary: contextmgr.Summary{ID: "sum-1", Content: "hello"},
	}
	h := contextmgr.NewHandlerWithRepo(repo)
	r := newTestRouter(h)

	body := bytes.NewBufferString(`{"content":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/projects/p1/conversations/c1/summaries", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", rr.Code)
	}
	var summary contextmgr.Summary
	if err := json.NewDecoder(rr.Body).Decode(&summary); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if summary.ID != "sum-1" {
		t.Errorf("expected id %q, got %q", "sum-1", summary.ID)
	}
	if summary.Content != "hello" {
		t.Errorf("expected content %q, got %q", "hello", summary.Content)
	}
}

func TestUpdateSummary_OK(t *testing.T) {
	repo := &mockRepo{}
	h := contextmgr.NewHandlerWithRepo(repo)
	r := newTestRouter(h)

	body := bytes.NewBufferString(`{"content":"updated"}`)
	req := httptest.NewRequest(http.MethodPut, "/projects/p1/conversations/c1/summaries/s1", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if v, _ := resp["ok"].(bool); !v {
		t.Errorf("expected ok=true, got %v", resp)
	}
}

func TestUpdateSummary_NotFound(t *testing.T) {
	repo := &mockRepo{updateErr: fmt.Errorf("not found")}
	h := contextmgr.NewHandlerWithRepo(repo)
	r := newTestRouter(h)

	body := bytes.NewBufferString(`{"content":"updated"}`)
	req := httptest.NewRequest(http.MethodPut, "/projects/p1/conversations/c1/summaries/s1", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

func TestDeleteSummary_NoContent(t *testing.T) {
	repo := &mockRepo{}
	h := contextmgr.NewHandlerWithRepo(repo)
	r := newTestRouter(h)

	req := httptest.NewRequest(http.MethodDelete, "/projects/p1/conversations/c1/summaries/s1", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rr.Code)
	}
}

func TestDeleteSummary_NotFound(t *testing.T) {
	repo := &mockRepo{deleteErr: fmt.Errorf("not found")}
	h := contextmgr.NewHandlerWithRepo(repo)
	r := newTestRouter(h)

	req := httptest.NewRequest(http.MethodDelete, "/projects/p1/conversations/c1/summaries/s1", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}
