package conversations

// The per-user read marker (client contract 1.12, markConversationRead):
// PUT /conversation_read/prompt_lib/{projectID}/{conversationID}.

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// ReadState is the caller's read marker after a markConversationRead.
type ReadState struct {
	ConversationID   int64
	ConversationUUID string
	// ReaderID is the caller's user id: the only receiver of the read frame.
	ReaderID int64
	// ReadAt is RFC 3339, UTC.
	ReadAt            string
	LastReadMessageID *int64
	// Unread is the row's `unread` after the write: true only when the
	// request named a message older than the newest answer.
	Unread bool
}

// ConversationReadMarker is the optional repository half of the read
// marker. Optional so test doubles of Repository keep compiling; the
// production repository implements it, and a composition without it answers
// 501.
//
// MarkConversationRead resolves the conversation under the chat authority's
// detail rule: a conversation the caller cannot see is apierr 404, as is a
// messageID that is not a message group of it. It is idempotent and the
// marker never moves back.
type ConversationReadMarker interface {
	MarkConversationRead(ctx context.Context, projectID, conversationID, messageID string) (ReadState, error)
}

// ReadEvents tells the reader's other devices about a new read marker
// (conversationactivity.Notifier.Read). Optional.
type ReadEvents interface {
	Read(ctx context.Context, projectID, readerID, conversationID int64, conversationUUID string, readAt string)
}

// WithReadEvents wires the read frame. A nil value (or a typed nil) sends
// none, which the other devices repair on their next list read.
func (h *Handler) WithReadEvents(events ReadEvents) *Handler {
	if events == nil || !presentValue(events) {
		return h
	}
	h.readEvents = events
	return h
}

// readBodyLimit bounds the request body: one short string.
const readBodyLimit = 16 << 10

// MarkRead serves markConversationRead.
func (h *Handler) MarkRead(w http.ResponseWriter, r *http.Request) {
	marker, ok := h.repo.(ConversationReadMarker)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"ok": false, "error": "conversation read markers are not supported by this composition"})
		return
	}
	projectID := chi.URLParam(r, "projectID")
	conversationID := chi.URLParam(r, "conversationID")
	project, err := strconv.ParseInt(projectID, 10, 64)
	if err != nil || project <= 0 || project > math.MaxInt32 {
		apierr.Write(w, apierr.NotFound("chat resource not found"))
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, readBodyLimit+1))
	if err != nil || len(raw) > readBodyLimit {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}
	body := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil || body == nil {
			apierr.Write(w, apierr.BadRequest("the body must be a JSON object"))
			return
		}
	}
	messageID := ""
	if value, present := body["message_id"]; present && value != nil {
		switch typed := value.(type) {
		case string:
			if typed == "" {
				apierr.Write(w, apierr.NotFound("message not found"))
				return
			}
			messageID = typed
		case float64:
			if typed != math.Trunc(typed) || typed <= 0 || typed > math.MaxInt32 {
				apierr.Write(w, apierr.NotFound("message not found"))
				return
			}
			messageID = strconv.FormatInt(int64(typed), 10)
		default:
			apierr.Write(w, apierr.BadRequest("message_id must be a string"))
			return
		}
	}

	state, err := marker.MarkConversationRead(r.Context(), projectID, conversationID, messageID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if h.readEvents != nil {
		h.readEvents.Read(r.Context(), project, state.ReaderID, state.ConversationID, state.ConversationUUID, state.ReadAt)
	}
	response := map[string]any{
		"conversation_id": state.ConversationID,
		"read_at":         state.ReadAt,
		"unread":          state.Unread,
	}
	if state.LastReadMessageID != nil {
		response["last_read_message_id"] = *state.LastReadMessageID
	}
	writeJSON(w, http.StatusOK, response)
}
