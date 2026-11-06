package agentexecution

import (
	"encoding/json"
	"errors"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
)

// ChatHistoryRef names one chat_history entry (ADR-0050 decision 9c,
// issue 1603). The reader's durable worker session is seeded from the
// history once and then holds only the reader's own turns; on each later
// turn the worker appends to it, once, the entries it has not seen and that
// are not ReadersOwn.
type ChatHistoryRef struct {
	// MessageID is the chat message group uuid.
	MessageID string `json:"message_id"`
	// ReadersOwn marks the reader's own answer, or a message sent to the
	// reader: the reader's own turns in the session already hold it.
	ReadersOwn bool `json:"readers_own"`
}

// ErrMalformedChatHistoryRefs is returned when the resolver's references do
// not name the history one to one.
var ErrMalformedChatHistoryRefs = errors.New("chat history references do not match the history")

// DecodeChatHistoryRefs reads the resolver's reference projection and checks
// it names every history entry, in order, with a valid uuid.
func DecodeChatHistoryRefs(history, refs json.RawMessage) ([]ChatHistoryRef, error) {
	var entries []json.RawMessage
	if err := json.Unmarshal(history, &entries); err != nil {
		return nil, ErrMalformedChatHistoryRefs
	}
	var decoded []ChatHistoryRef
	if err := json.Unmarshal(refs, &decoded); err != nil || len(decoded) != len(entries) {
		return nil, ErrMalformedChatHistoryRefs
	}
	for _, ref := range decoded {
		if !validUUID(ref.MessageID) {
			return nil, ErrMalformedChatHistoryRefs
		}
	}
	return decoded, nil
}

func chatHistoryRefsInput(refs []ChatHistoryRef) []*runtimev1.ChatHistoryRefV1 {
	if len(refs) == 0 {
		return nil
	}
	out := make([]*runtimev1.ChatHistoryRefV1, 0, len(refs))
	for _, ref := range refs {
		out = append(out, &runtimev1.ChatHistoryRefV1{MessageId: ref.MessageID, ReadersOwn: ref.ReadersOwn})
	}
	return out
}
