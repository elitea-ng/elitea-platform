package agentexecution

import (
	"bytes"
	"encoding/json"
	"errors"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/contextview"
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
	// The facts the reader's view is filtered on (ADR-0050 decision 9,
	// issue 1609); contextview.Facts says what each means.
	OwnAgent         bool `json:"own_agent"`
	Addressed        bool `json:"addressed"`
	Side             bool `json:"side"`
	AfterJoined      bool `json:"after_joined"`
	AnsweredByReader bool `json:"answered_by_reader"`
}

func (ref ChatHistoryRef) facts() contextview.Facts {
	return contextview.Facts{
		ReadersOwn: ref.ReadersOwn, OwnAgent: ref.OwnAgent, Addressed: ref.Addressed,
		Side: ref.Side, AfterJoined: ref.AfterJoined, AnsweredByReader: ref.AnsweredByReader,
	}
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

// ReaderView is one reader's view of the conversation (ADR-0050 decision 9,
// issue 1609): the resolver's projection filtered by the reader's context
// settings. History and Refs replace the projection on the turn's input.
type ReaderView struct {
	Settings contextview.Settings
	History  json.RawMessage
	Refs     []ChatHistoryRef
}

// BuildReaderView keeps the entries the reader may see, in order. The
// projection holds visible items only (agent_chat.sql); this applies side
// talk and the eligibility mode. history and refs must already agree
// (DecodeChatHistoryRefs).
func BuildReaderView(settings contextview.Settings, history json.RawMessage, refs []ChatHistoryRef) (ReaderView, error) {
	var entries []json.RawMessage
	if err := json.Unmarshal(history, &entries); err != nil || len(entries) != len(refs) {
		// History without the references that carry its facts is refused,
		// never sent unfiltered: a view fails closed.
		return ReaderView{}, ErrMalformedChatHistoryRefs
	}
	keptEntries := make([]json.RawMessage, 0, len(entries))
	keptRefs := make([]ChatHistoryRef, 0, len(refs))
	for index, ref := range refs {
		if !settings.Eligible(ref.facts()) {
			continue
		}
		keptEntries = append(keptEntries, entries[index])
		keptRefs = append(keptRefs, ref)
	}
	if len(keptEntries) == len(entries) {
		// Nothing filtered: the projection's own bytes, so a one-agent
		// conversation's input is unchanged.
		return ReaderView{Settings: settings, History: bytes.Clone(history), Refs: refs}, nil
	}
	encoded, err := json.Marshal(keptEntries)
	if err != nil {
		return ReaderView{}, ErrMalformedChatHistoryRefs
	}
	return ReaderView{Settings: settings, History: encoded, Refs: keptRefs}, nil
}

// readerViewSettings is the reader's context settings: the frozen version's
// `meta.context` under the participant's own mode. A malformed stored block
// reads as the default, as a malformed `session_compaction` does: an
// authored typo never fails a turn, and the save path refuses new ones.
func readerViewSettings(version map[string]any, participantMode string) contextview.Settings {
	settings := contextview.Default
	if meta, _ := version["meta"].(map[string]any); meta != nil {
		if parsed, err := contextview.Parse(meta[contextview.MetaKey]); err == nil {
			settings = parsed
		}
	}
	return settings.WithParticipant(participantMode)
}

// contextViewInput is the input's ContextViewV1 for history and refs as
// they go on the wire.
func contextViewInput(settings contextview.Settings, limits *runtimev1.ModelContextLimitsV1, history []byte, refs []*runtimev1.ChatHistoryRefV1) *runtimev1.ContextViewV1 {
	window := limits.GetContextWindowTokens()
	digestRefs := make([]contextview.Ref, 0, len(refs))
	for _, ref := range refs {
		digestRefs = append(digestRefs, contextview.Ref{MessageID: ref.GetMessageId(), ReadersOwn: ref.GetReadersOwn()})
	}
	return &runtimev1.ContextViewV1{
		History:      string(settings.History),
		WindowTokens: window,
		Digest:       contextview.Digest(settings.History, window, history, digestRefs),
	}
}
