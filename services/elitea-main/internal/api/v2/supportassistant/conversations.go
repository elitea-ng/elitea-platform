package supportassistant

// The conversation routes — the port of `api/v2/conversations.py`,
// `api/v2/conversation.py`, `api/v2/messages.py` and `api/v2/attachments.py`.
//
// Every one of them resolves the conversation through
// `store.conversationOwnedByCaller`, which is where the "yours, and a support
// one" predicate lives. None of them takes a project from the request.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// ChatStore is the slice of the chat repository this package REUSES rather than
// reimplements — message-group reads, conversation and message deletion, and
// participant management. It is satisfied by
// `internal/infra/db/repos.ConversationsRepo`.
//
// Reuse is the point: a support transcript and a chat transcript are the same
// rows in the same tables, and a second implementation of "read a conversation's
// message groups" would be a second thing to keep in step with every future
// change to the message model.
type ChatStore interface {
	ListMessageGroups(ctx context.Context, projectID, conversationID string, limit int, sortOrder string) ([]map[string]any, error)
	AddParticipant(ctx context.Context, projectID, conversationID string, body map[string]any) error
	ListParticipants(ctx context.Context, projectID, conversationID string) ([]conversations.Participant, error)
	// UpdateEntitySettings repairs a mapping row that already exists.
	//
	// It is here because `AddParticipant`'s mapping insert is
	// ON CONFLICT DO NOTHING: a conversation that already holds an agent
	// mapping keeps whatever entity_settings it was created with, and this
	// package created every one of them empty. Without an update path those
	// conversations can never answer a question again. See participants.go.
	UpdateEntitySettings(ctx context.Context, projectID, conversationID, participantID string, settings map[string]any) error
}

// WithChatStore supplies the reused chat repository. Without it the routes that
// need it answer 503 rather than panicking on a nil interface.
func WithChatStore(chat ChatStore) Option {
	return func(h *Handler) { h.chat = chat }
}

// defaultConversationName is `conversations.py`'s `"New conversation"`.
const defaultConversationName = "New conversation"

// maxConversationNameLength is `ConversationCreateRequest`'s `max_length=255`.
const maxConversationNameLength = 255

// listLimits bound the history request. The reference defaults to 20 with no
// ceiling at all, so a client asking for `limit=100000` made the server build
// that page. The default is kept; the ceiling is added.
const (
	defaultListLimit = 20
	maxListLimit     = 100
)

// requestContext is the tuple every gated handler starts by unpacking: the
// resolved settings and the caller. Both are guaranteed present by `resolve`,
// so a failure here is a programming error in the middleware chain rather than
// anything a request can cause — which is why it answers 500 and not 401.
func (h *Handler) requestContext(w http.ResponseWriter, r *http.Request) (int64, int64, bool) {
	settings, ok := settingsFromContext(r.Context())
	if !ok {
		h.logger.Error("support assistant: settings missing from request context")
		apierr.WriteStatus(w, http.StatusInternalServerError, "support assistant is misconfigured")
		return 0, 0, false
	}
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		apierr.WriteStatus(w, http.StatusUnauthorized, "authentication required")
		return 0, 0, false
	}
	userID, ok := user.OwningUserID()
	if !ok {
		apierr.WriteStatus(w, http.StatusUnauthorized, "authentication required")
		return 0, 0, false
	}
	return settings.ProjectID, userID, true
}

// ListResponse is the widget's `TConversationsResponse`, plus the pagination
// fields the reference's listing returns.
type ListResponse struct {
	Items   []Conversation `json:"items"`
	Total   int            `json:"total"`
	Limit   int            `json:"limit"`
	Offset  int            `json:"offset"`
	HasMore bool           `json:"has_more"`
}

// ListConversations serves the widget's history drawer.
func (h *Handler) ListConversations(w http.ResponseWriter, r *http.Request) {
	projectID, userID, ok := h.requestContext(w, r)
	if !ok {
		return
	}

	limit := boundedQueryInt(r, "limit", defaultListLimit, 1, maxListLimit)
	offset := boundedQueryInt(r, "offset", 0, 0, 1<<30)
	query := r.URL.Query().Get("q")

	items, total, err := h.store.listConversations(r.Context(), projectID, userID, limit, offset, query)
	if err != nil {
		h.logger.Error("support assistant: list conversations", "err", err)
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to list conversations")
		return
	}
	writeJSON(w, http.StatusOK, ListResponse{
		Items:   items,
		Total:   total,
		Limit:   limit,
		Offset:  offset,
		HasMore: offset+len(items) < total,
	})
}

type createConversationRequest struct {
	Name string `json:"name"`
}

// CreateConversation opens a new support conversation for the caller.
func (h *Handler) CreateConversation(w http.ResponseWriter, r *http.Request) {
	projectID, userID, ok := h.requestContext(w, r)
	if !ok {
		return
	}

	// The widget POSTs `{}`. An absent, empty or whitespace body is the normal
	// case, not an error.
	var body createConversationRequest
	if raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<10)); err == nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			apierr.WriteStatus(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	name := body.Name
	if name == "" {
		name = defaultConversationName
	}
	if len(name) > maxConversationNameLength {
		apierr.WriteStatus(w, http.StatusBadRequest, "name is too long")
		return
	}

	conversation, err := h.store.createConversation(r.Context(), projectID, userID, name)
	if err != nil {
		h.logger.Error("support assistant: create conversation", "err", err)
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to create conversation")
		return
	}

	// THE AUTHOR IS A PARTICIPANT, and the conversation is unusable without it.
	//
	// `ResolveCurrentApplicationTurn` joins `chat_participants` on
	// `entity_name = 'user'` AND the caller's id. This route used to insert the
	// conversation row and stop, so every support conversation this service
	// created resolved to no row and every question it carried answered 502.
	//
	// The failure is LOGGED AND SURVIVED rather than returned. The conversation
	// exists at this point; answering 500 would leave the client with no id for
	// a row that was created, and the predict route repairs a missing author on
	// the next turn anyway (ensureTurnParticipants). Refusing here would trade a
	// recoverable gap for an unrecoverable orphan.
	if h.chat != nil {
		if err := h.ensureUserParticipant(r.Context(),
			strconv.FormatInt(projectID, 10),
			strconv.FormatInt(conversation.ID, 10),
			projectID, userID,
		); err != nil {
			h.logger.Warn("support assistant: attach author participant",
				"conversation_id", conversation.ID, "err", err)
		}
	}
	writeJSON(w, http.StatusCreated, conversation)
}

// ConversationDetails is the widget's `TRawConversation`: the conversation plus
// its transcript, in the same message-group shape the chat surface renders.
type ConversationDetails struct {
	Conversation
	MessageGroups []map[string]any            `json:"message_groups"`
	Participants  []conversations.Participant `json:"participants"`
}

// maxTranscriptGroups bounds one transcript read. The reference passes no limit
// at all to `chat_get_conversation_details`.
const maxTranscriptGroups = 200

// GetConversation returns one support conversation with its transcript.
func (h *Handler) GetConversation(w http.ResponseWriter, r *http.Request) {
	projectID, userID, ok := h.requestContext(w, r)
	if !ok {
		return
	}
	conversationUUID := chi.URLParam(r, "conversationUUID")
	conversationID, err := h.store.conversationOwnedByCaller(r.Context(), projectID, userID, conversationUUID)
	if err != nil {
		h.writeConversationError(w, err, "resolve conversation")
		return
	}
	if h.chat == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, unavailableMessage)
		return
	}

	conversation, err := h.store.conversationByID(r.Context(), projectID, conversationID)
	if err != nil {
		h.writeConversationError(w, err, "read conversation")
		return
	}

	projectKey := strconv.FormatInt(projectID, 10)
	conversationKey := strconv.FormatInt(conversationID, 10)
	groups, err := h.chat.ListMessageGroups(r.Context(), projectKey, conversationKey, maxTranscriptGroups, "asc")
	if err != nil {
		h.logger.Error("support assistant: read transcript", "err", err)
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to read conversation")
		return
	}
	if groups == nil {
		groups = []map[string]any{}
	}
	participants, err := h.chat.ListParticipants(r.Context(), projectKey, conversationKey)
	if err != nil || participants == nil {
		participants = []conversations.Participant{}
	}

	writeJSON(w, http.StatusOK, ConversationDetails{
		Conversation:  conversation,
		MessageGroups: groups,
		Participants:  participants,
	})
}

/*
 * DELETE /conversation/{uuid}, DELETE /messages/{uuid} AND POST
 * /attachments/{uuid} USED TO STAND HERE, and are removed rather than repaired.
 *
 * All three were unreachable — nothing in the widget called any of them — and
 * two could not have worked if it had:
 *
 *   - `ClearMessages` delegated to `ConversationsRepo.DeleteMessages`, which
 *     deletes from `chat_messages`. NO MIGRATION CREATES THAT TABLE: the
 *     transcript lives in chat_message_group / chat_message_items /
 *     chat_messages_text (migrations/tenant/0123), and neither 0123 nor
 *     001_initial's create_tenant_schema declares `chat_messages`. Every call
 *     would have answered 500 on SQLSTATE 42P01.
 *   - `DeleteConversation` delegated to `ConversationsRepo.Delete`, which drops
 *     chat_message_group rows while chat_message_items still references them
 *     (0123:117 declares that foreign key with NO ON DELETE action, unlike its
 *     siblings). Any conversation with a single message would have failed with
 *     a foreign-key violation; only empty ones could be deleted.
 *
 * Both root causes are in the SHARED chat repository, which the chat surface's
 * own routes hit too. Repairing them there is a real fix worth making, and it
 * is not this feature's to make: it changes behaviour for a surface this PR
 * does not touch, on routes the support widget does not call. Deleting the
 * support wrappers is the honest half — it removes three endpoints that
 * promised something they could not deliver, to a client that never asked.
 *
 * This is the same judgement the attachment tree got in this same change (see
 * ../../../../apps/elitea-web/src/widgets/support-assistant/vendor/components/chat/MessageInput.tsx):
 * a surface that cannot work is worse than an absent one. The three sibling
 * routes were left behind by oversight; this corrects that.
 *
 * When the widget grows a delete or clear affordance, the repository fix comes
 * first and the route comes back with a test that exercises its success path —
 * which is what would have caught this.
 */

// writeConversationError maps a store error to a status. "Not yours" and "does
// not exist" are ONE answer — see conversationOwnedByCaller.
func (h *Handler) writeConversationError(w http.ResponseWriter, err error, operation string) {
	if errors.Is(err, errConversationNotFound) {
		apierr.WriteStatus(w, http.StatusNotFound, "conversation not found")
		return
	}
	h.logger.Error("support assistant: "+operation, "err", err)
	apierr.WriteStatus(w, http.StatusInternalServerError, "failed to read conversation")
}

// boundedQueryInt reads a query integer, clamping it into range. An unparseable
// value takes the default rather than 400ing: these are pagination hints from a
// widget, not instructions.
func boundedQueryInt(r *http.Request, name string, fallback, minimum, maximum int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}
