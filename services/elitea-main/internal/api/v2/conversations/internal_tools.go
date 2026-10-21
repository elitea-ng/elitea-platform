package conversations

// Narrow write of a conversation's internal tools (client contract 1.14,
// updateConversationInternalTools):
// PATCH /conversation_internal_tools/prompt_lib/{projectID}/{conversationID}.
//
// WHY A SEPARATE WRITE. The conversation PUT replaces the whole `meta`
// document with the one the client sends — the client reads the previous
// document, changes one key and sends it back. Two clients doing that at
// once (the web and a phone, say) each write the other's stale copy, so a
// toggle on one silently undoes a change on the other. This route changes
// `meta.internal_tools` and nothing else, in ONE statement (jsonb_set over
// the row's current value), so it can neither lose nor be lost to a write
// of any other key. The PUT, for its part, keeps the stored
// `internal_tools` when the `meta` it sends has no such key
// (ConversationsRepo.Update), so a native client that never sends the list
// never erases it.
//
// The list is validated against the internal-tools catalogue the agent
// admission applies (internal/db/queries/agent_chat.sql), so a toggle can
// never store a value that makes every later send answer 422. The other 422
// rule of that statement — a toolkit participant in an agent conversation —
// is about participants, not this list, and does not apply here.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// ConversationInternalToolsWriter replaces one conversation's
// `meta.internal_tools` atomically and answers the conversation as the PUT
// does. It validates every name against the internal-tools catalogue and
// answers a 400 for one outside it. *repos.ConversationsRepo implements it.
type ConversationInternalToolsWriter interface {
	UpdateInternalTools(ctx context.Context, projectID, conversationID string, tools []string) (Conversation, error)
}

// internalToolsBodyLimit bounds the request body: a short list of short names.
const internalToolsBodyLimit = 16 << 10

// maxInternalTools bounds the list. The catalogue has a dozen names, and a
// longer list can only repeat them.
const maxInternalTools = 64

// UpdateInternalTools serves updateConversationInternalTools.
func (h *Handler) UpdateInternalTools(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeConversation(w, r) {
		return
	}
	writer, ok := h.repo.(ConversationInternalToolsWriter)
	if !ok {
		apierr.Write(w, apierr.NotImplemented("conversation internal tools are not writable on this deployment"))
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, internalToolsBodyLimit+1))
	if err != nil || len(raw) > internalToolsBodyLimit {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}
	var body struct {
		InternalTools *[]string `json:"internal_tools"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || decoder.More() {
		apierr.Write(w, apierr.BadRequest("the body must be {\"internal_tools\": [names]}"))
		return
	}
	if body.InternalTools == nil {
		apierr.Write(w, apierr.BadRequest("internal_tools is required: an array of internal tool names"))
		return
	}
	if len(*body.InternalTools) > maxInternalTools {
		apierr.Write(w, apierr.BadRequest("internal_tools has too many entries"))
		return
	}
	// Repeats are dropped, first occurrence kept: the list is a set the
	// clients render as toggles.
	tools := make([]string, 0, len(*body.InternalTools))
	seen := make(map[string]bool, len(*body.InternalTools))
	for _, name := range *body.InternalTools {
		if seen[name] {
			continue
		}
		seen[name] = true
		tools = append(tools, name)
	}
	updated, err := writer.UpdateInternalTools(r.Context(), chi.URLParam(r, "projectID"), chi.URLParam(r, "conversationID"), tools)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}
