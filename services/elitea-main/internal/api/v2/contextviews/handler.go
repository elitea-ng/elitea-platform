// Package contextviews serves the "What the agent saw" read (ADR-0050
// decision 9, issue 1669):
//
//	GET /elitea_core/message_context_view/prompt_lib/{projectID}/{messageID}
//
// It answers the view one agent answer was given: the mode, the model
// window, the digest the worker checked, and every message the resolver
// projected, in order, with why each one the view dropped was dropped. It is
// read-only, and it is the conversation's own read: the route takes
// models.chat.messages.details (as reading the message does) and the
// handler applies the conversation's visibility rule to the message first,
// so a caller who cannot read the conversation gets the same 404 as for a
// missing message.
//
// It never answers another reader's view. The record is keyed by the answer
// and the store checks the answer's author is the record's reader; there is
// no parameter that names a participant. No message body is in the
// response: the client shows the text of the messages it already holds.
package contextviews

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// Path is the route.
const Path = "/message_context_view/prompt_lib/{projectID}/{messageID}"

// Permission gates the route: the message read's own permission.
const Permission = "models.chat.messages.details"

// Store reads a recorded view.
type Store interface {
	MessageContextView(ctx context.Context, projectID, messageUUID string) (repos.ContextView, error)
}

// Conversations is the conversation read rule.
type Conversations interface {
	AuthorizeChatResource(ctx context.Context, projectID, resourceKind, resourceID string) error
}

var _ Store = (*repos.ContextViews)(nil)

// Handler serves the route.
type Handler struct {
	store Store
	chat  Conversations
}

// NewHandler builds the handler. A nil store or chat answers 503.
func NewHandler(store Store, chat Conversations) *Handler {
	if typed, ok := store.(*repos.ContextViews); ok && typed == nil {
		store = nil
	}
	return &Handler{store: store, chat: chat}
}

// Get answers one answer's recorded view.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.store == nil || h.chat == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	projectID := chi.URLParam(r, "projectID")
	messageID := chi.URLParam(r, "messageID")
	// The conversation's rule first: a message of a private conversation the
	// caller is not in is a 404, like a missing one.
	if err := h.chat.AuthorizeChatResource(r.Context(), projectID, "message", messageID); err != nil {
		apierr.Write(w, err)
		return
	}
	view, err := h.store.MessageContextView(r.Context(), projectID, messageID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(view)
}
