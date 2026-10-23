package conversations

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// ConversationArchiver is the optional repository half of the conversation
// archive (client contract 1.10). The archive is the project's, as the pin
// is. Optional so test doubles of Repository keep compiling; the production
// repository implements it, and a composition without it answers 501.
//
// Both methods resolve the conversation under the chat authority's detail
// rule and answer apierr 404 for a conversation the caller cannot see, with
// nothing written. Both are idempotent: a repeated archive keeps the first
// archive (who and when), and a repeated unarchive removes nothing. Any
// member who can see the conversation may unarchive it. Only a change of
// state stamps the conversation's sync_at, so only a change re-delivers the
// list row in the `changes_since` delta of every member who can see it.
type ConversationArchiver interface {
	ArchiveConversation(ctx context.Context, projectID, conversationID string) error
	UnarchiveConversation(ctx context.Context, projectID, conversationID string) error
}

// Archive serves archiveConversation: the conversation is archived in its
// project.
func (h *Handler) Archive(w http.ResponseWriter, r *http.Request) {
	h.setArchived(w, r, true)
}

// Unarchive serves unarchiveConversation: the project's archive is removed.
func (h *Handler) Unarchive(w http.ResponseWriter, r *http.Request) {
	h.setArchived(w, r, false)
}

func (h *Handler) setArchived(w http.ResponseWriter, r *http.Request, archived bool) {
	archiver, ok := h.repo.(ConversationArchiver)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"ok": false, "error": "conversation archive is not supported by this composition"})
		return
	}
	projectID := chi.URLParam(r, "projectID")
	conversationID := chi.URLParam(r, "conversationID")
	var err error
	if archived {
		err = archiver.ArchiveConversation(r.Context(), projectID, conversationID)
	} else {
		err = archiver.UnarchiveConversation(r.Context(), projectID, conversationID)
	}
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
