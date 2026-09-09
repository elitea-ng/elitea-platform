// Package memories serves persistent, cross-conversation personal memory
// (#870) — Settings > Profile's "Long-term Memory" accordion
// (features/settings/ui/profile/ProfileLongTermMemory.tsx) has always
// rendered a dimmed "Coming soon" card, and Settings > Memory
// (features/settings/ui/memory/MemoryContextManagement.tsx) configures
// context-WINDOW management for the current conversation only. Neither page
// gave a user a place to keep a fact the assistant should recall in a LATER,
// unrelated conversation. This package is that place: CRUD over one row per
// remembered fact, scoped to (project, user).
//
// THIS IS NOT THE "memory" TOOLKIT. integrations/toolkits/memory_toolkit is
// an agent-scoped key/value store the LLM calls as a TOOL mid-turn, backed by
// pgvector, attached like any other toolkit. The store here is never a tool
// call target: it is read passively, before a turn starts, by the runtime
// context-injection path (internal/application/agentexecution's
// CurrentMemoryRecallResolver) and merged into the same `instructions` text
// both workers already consume — see start.go's and adhoc.go's own comments
// for why that mechanism, and not a new wire field, carries it.
package memories

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

const (
	// MaxContentBytes bounds one memory's text. Generous enough for a real
	// fact ("prefers TypeScript over Python for new services") while keeping
	// a user's full enabled set cheap to read every turn — the recall path
	// (agentexecution's resolver) additionally caps how much of this text
	// actually reaches a prompt.
	MaxContentBytes = 4000
	// MaxTags and MaxTagBytes bound the free-form label set the settings
	// panel's filter reads (#870 web unit — see MemoryEntryForm.tsx).
	MaxTags     = 16
	MaxTagBytes = 64
	// maxMemoryBodyBytes bounds the whole decoded request body, independent
	// of the per-field limits above, so a client cannot force a large decode
	// with padding or duplicate keys.
	maxMemoryBodyBytes = 64 * 1024
)

// MemoryEntry is one persistent, cross-conversation personal memory.
type MemoryEntry struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Content   string `json:"content"`
	// Tags is never nil on a served entry — always at least an empty slice,
	// so the wire shape a client decodes is stable (`[]`, not `null`).
	Tags []string `json:"tags"`
	// SourceConversationID records where a memory came from — the
	// "Remember this" message action stamps the owning conversation's uuid
	// (chat_conversations.uuid). Empty when a memory was typed directly into
	// the settings panel, or when the source conversation's identity was not
	// available. It is informational only: the source conversation may since
	// have been deleted (tenant/0137_personal_memory_entries.sql's own
	// header explains why this deliberately holds no foreign key).
	SourceConversationID string    `json:"source_conversation_id,omitempty"`
	Enabled              bool      `json:"enabled"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// Repository is the persistence contract this handler drives. Implemented by
// internal/infra/db/repos.MemoriesRepo (raw SQL over
// p_<project>.personal_memory_entries, tenant/0136).
type Repository interface {
	// List answers the caller's OWN memories in this project, most recent
	// first. search, when non-empty, is a case-insensitive substring filter
	// over content and tags — the settings panel's search box.
	List(ctx context.Context, projectID, userID, search string) ([]MemoryEntry, error)
	Create(ctx context.Context, projectID, userID string, entry MemoryEntry) (MemoryEntry, error)
	// Update is a full-record replace of the caller's OWN entry (matches the
	// settings panel's edit dialog and the per-row enable/disable Switch,
	// which both send the whole record — same shape webhooks' updateWebhook
	// already uses). A memoryID naming another user's entry, or no entry at
	// all, answers apierr.NotFound — a caller never learns whether an id
	// belongs to someone else. CreatedAt on entry is ignored; UpdatedAt is
	// always stamped `now()` by the repository.
	Update(ctx context.Context, projectID, userID, memoryID string, entry MemoryEntry) (MemoryEntry, error)
	Delete(ctx context.Context, projectID, userID, memoryID string) error
	// ClearAll deletes every one of the caller's OWN memories in this
	// project and answers how many rows were removed, for the settings
	// panel's "Clear all" confirmation toast.
	ClearAll(ctx context.Context, projectID, userID string) (int, error)
}

type Handler struct {
	repo Repository
}

func NewHandler(repo Repository) *Handler {
	return &Handler{repo: repo}
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	user, ok := requireMemoryUser(w, r)
	if !ok {
		return
	}
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	entries, err := h.repo.List(r.Context(), projectID, user.ID, search)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if entries == nil {
		entries = []MemoryEntry{}
	}
	writeMemoriesJSON(w, http.StatusOK, map[string]any{"items": entries, "total": len(entries)})
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	user, ok := requireMemoryUser(w, r)
	if !ok {
		return
	}
	var body memoryRequestBody
	if err := decodeMemoryBody(r, &body); err != nil {
		apierr.Write(w, err)
		return
	}
	entry, err := validatedMemoryEntry(body, true)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	created, err := h.repo.Create(r.Context(), projectID, user.ID, entry)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeMemoriesJSON(w, http.StatusCreated, created)
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	memoryID := chi.URLParam(r, "memoryID")
	user, ok := requireMemoryUser(w, r)
	if !ok {
		return
	}
	var body memoryRequestBody
	if err := decodeMemoryBody(r, &body); err != nil {
		apierr.Write(w, err)
		return
	}
	// PUT is a FULL-RECORD replace, same as webhooks' updateWebhook and
	// message_feedback's SetMessageFeedback: the settings panel's edit
	// dialog and its per-row enable/disable Switch both send the whole
	// record back, never a partial patch, so content is required here too.
	entry, err := validatedMemoryEntry(body, true)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	updated, err := h.repo.Update(r.Context(), projectID, user.ID, memoryID, entry)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeMemoriesJSON(w, http.StatusOK, updated)
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	memoryID := chi.URLParam(r, "memoryID")
	user, ok := requireMemoryUser(w, r)
	if !ok {
		return
	}
	if err := h.repo.Delete(r.Context(), projectID, user.ID, memoryID); err != nil {
		apierr.Write(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) ClearAll(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	user, ok := requireMemoryUser(w, r)
	if !ok {
		return
	}
	removed, err := h.repo.ClearAll(r.Context(), projectID, user.ID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeMemoriesJSON(w, http.StatusOK, map[string]any{"removed": removed})
}

// requireMemoryUser is the one place every handler above reads the caller's
// identity from. Memories are never readable or writable cross-user — there
// is no "admin sees everyone's memories" mode, matching the accordion's own
// promise ("what the AI remembers about YOU").
func requireMemoryUser(w http.ResponseWriter, r *http.Request) (auth.User, bool) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok || user.ID == "" {
		apierr.Write(w, apierr.Unauthorized("authentication required"))
		return auth.User{}, false
	}
	return user, true
}

type memoryRequestBody struct {
	Content              string   `json:"content"`
	Tags                 []string `json:"tags"`
	SourceConversationID string   `json:"source_conversation_id"`
	Enabled              *bool    `json:"enabled"`
}

func decodeMemoryBody(r *http.Request, body *memoryRequestBody) error {
	if err := json.NewDecoder(io.LimitReader(r.Body, maxMemoryBodyBytes)).Decode(body); err != nil {
		return apierr.BadRequest("invalid request body")
	}
	return nil
}

// validatedMemoryEntry turns a decoded request body into a MemoryEntry,
// refusing anything the table's own CHECK constraint
// (personal_memory_entries_content_nonempty_check, tenant/0136) would also
// refuse, plus the size bounds this handler owns. requireContent is true on
// both Create and Update (Update is a full-record replace, not a patch).
func validatedMemoryEntry(body memoryRequestBody, requireContent bool) (MemoryEntry, error) {
	content := strings.TrimSpace(body.Content)
	if requireContent && content == "" {
		return MemoryEntry{}, apierr.BadRequest("content is required")
	}
	if utf8.RuneCountInString(content) > MaxContentBytes {
		return MemoryEntry{}, apierr.BadRequest("content exceeds the maximum length")
	}
	if len(body.Tags) > MaxTags {
		return MemoryEntry{}, apierr.BadRequest("too many tags")
	}
	tags := make([]string, 0, len(body.Tags))
	for _, raw := range body.Tags {
		tag := strings.TrimSpace(raw)
		if tag == "" {
			continue
		}
		if utf8.RuneCountInString(tag) > MaxTagBytes {
			return MemoryEntry{}, apierr.BadRequest("a tag exceeds the maximum length")
		}
		tags = append(tags, tag)
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	return MemoryEntry{
		Content:              content,
		Tags:                 tags,
		SourceConversationID: strings.TrimSpace(body.SourceConversationID),
		Enabled:              enabled,
	}, nil
}

func writeMemoriesJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
