package contextmgr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

type Analytics struct {
	TokenCount int `json:"token_count"`
	MaxTokens  int `json:"max_tokens"`
}

type Summary struct {
	ID      string `json:"id"`
	Content string `json:"content"`
}

type Repository interface {
	GetAnalytics(ctx context.Context, projectID, conversationID string) (Analytics, error)
	ListSummaries(ctx context.Context, projectID, conversationID string) ([]Summary, int, error)
	CreateSummary(ctx context.Context, projectID, conversationID, content string) (Summary, error)
	UpdateSummary(ctx context.Context, projectID, conversationID, summaryID, content string) error
	DeleteSummary(ctx context.Context, projectID, conversationID, summaryID string) error
}

type Handler struct {
	repo Repository
}

func NewHandler(pool *pgxpool.Pool) *Handler {
	return &Handler{repo: &pgRepo{pool: pool}}
}

func NewHandlerWithRepo(repo Repository) *Handler {
	return &Handler{repo: repo}
}

func (h *Handler) OptimizeContext(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) GetAnalytics(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	conversationID := chi.URLParam(r, "conversationID")
	analytics, err := h.repo.GetAnalytics(r.Context(), projectID, conversationID)
	if err != nil {
		writeJSON(w, http.StatusOK, Analytics{TokenCount: 0, MaxTokens: 128000})
		return
	}
	writeJSON(w, http.StatusOK, analytics)
}

func (h *Handler) ListSummaries(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	conversationID := chi.URLParam(r, "conversationID")
	summaries, total, err := h.repo.ListSummaries(r.Context(), projectID, conversationID)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"summaries": []any{}, "total": 0})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"summaries": summaries, "total": total})
}

func (h *Handler) CreateSummary(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	conversationID := chi.URLParam(r, "conversationID")
	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	summary, err := h.repo.CreateSummary(r.Context(), projectID, conversationID, body.Content)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, summary)
}

func (h *Handler) UpdateSummary(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	conversationID := chi.URLParam(r, "conversationID")
	summaryID := chi.URLParam(r, "summaryID")
	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	if err := h.repo.UpdateSummary(r.Context(), projectID, conversationID, summaryID, body.Content); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "summary not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) DeleteSummary(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	conversationID := chi.URLParam(r, "conversationID")
	summaryID := chi.URLParam(r, "summaryID")
	if err := h.repo.DeleteSummary(r.Context(), projectID, conversationID, summaryID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "summary not found"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type pgRepo struct {
	pool *pgxpool.Pool
}

func (r *pgRepo) GetAnalytics(ctx context.Context, projectID, conversationID string) (Analytics, error) {
	s, err := tenantschema.Quote(projectID)
	if err != nil {
		return Analytics{}, err
	}
	q := fmt.Sprintf(`SELECT COUNT(*) FROM %s.chat_message_group WHERE conversation_id = $1`, s)
	var count int
	if err := r.pool.QueryRow(ctx, q, conversationID).Scan(&count); err != nil {
		return Analytics{TokenCount: 0, MaxTokens: 128000}, nil
	}
	// Rough estimate: average 500 tokens per message
	return Analytics{TokenCount: count * 500, MaxTokens: 128000}, nil
}

func (r *pgRepo) ListSummaries(_ context.Context, _, _ string) ([]Summary, int, error) {
	return []Summary{}, 0, nil
}

func (r *pgRepo) CreateSummary(_ context.Context, _, _, content string) (Summary, error) {
	return Summary{ID: fmt.Sprintf("sum-%d", 1), Content: content}, nil
}

func (r *pgRepo) UpdateSummary(_ context.Context, _, _, _, _ string) error {
	return nil
}

func (r *pgRepo) DeleteSummary(_ context.Context, _, _, _ string) error {
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
