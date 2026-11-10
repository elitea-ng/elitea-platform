// Package contextview is the rule of what one agent sees of a conversation
// (ADR-0050 decision 9, issue 1609): the per-version `context` block, the
// eligibility filter and the view digest. Main builds a view for the agent
// that answers, on every turn, and sends it as runtimev1.ContextViewV1.
//
// # What a view holds
//
// The resolver (agent_chat.sql) already projects only visible message items
// (text, attachments, canvases) and labels each peer and other person in
// `additional_kwargs.elitea_author`. Tool calls, tool results, thinking and
// trace steps live in other tables and never enter it. This package narrows
// that projection for one reader:
//
//   - side talk (visibility `side`) only when the reader wrote it or it is
//     addressed to the reader, unless `include_side_talk: none`;
//   - then the eligibility mode of the reader's version, or of its
//     participant ("share history from: now" sets `since_joined` there).
//
// # The block
//
// It is the `context` key of the version's meta, so it is part of the
// version and its release digest like `memory`:
//
//	context:
//	  history: full              # full | since_joined | addressed_only | none
//	  include_side_talk: party   # party | none
//	  activity_notes: false
//
// It narrows which messages are eligible and never sets a size: the size is
// the reader's model window, and the reader's own compaction fits the view
// into it. A version without the block is `full`, `party`, no notes.
package contextview

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
)

// MetaKey is the version meta key of the block.
const MetaKey = "context"

// ParticipantKey is the participant `entity_settings` key a person sets when
// adding an agent with "share history from: now".
const ParticipantKey = "context_history"

// History is an eligibility mode.
type History string

const (
	// HistoryFull is every visible message the reader may see.
	HistoryFull History = "full"
	// HistorySinceJoined is only what was said after the reader joined.
	HistorySinceJoined History = "since_joined"
	// HistoryAddressedOnly is the messages addressed to the reader, its own
	// answers, and the message each one replied to.
	HistoryAddressedOnly History = "addressed_only"
	// HistoryNone is the current message only.
	HistoryNone History = "none"
)

// Valid reports whether h is a known mode.
func (h History) Valid() bool {
	switch h {
	case HistoryFull, HistorySinceJoined, HistoryAddressedOnly, HistoryNone:
		return true
	}
	return false
}

// SideTalk is whether side talk the reader is a party to is eligible.
type SideTalk string

const (
	SideTalkParty SideTalk = "party"
	SideTalkNone  SideTalk = "none"
)

// Settings is a parsed block.
type Settings struct {
	History  History
	SideTalk SideTalk
	// ActivityNotes is stored and validated. The notes themselves come with
	// the turn-taking engine (issue 1611); no note is built yet.
	ActivityNotes bool
}

// Default is the settings of a version without a block.
var Default = Settings{History: HistoryFull, SideTalk: SideTalkParty}

// ErrInvalidBlock is a block that is not valid; the message names the field.
var ErrInvalidBlock = errors.New("invalid context block")

// Parse reads the block from a decoded meta value. Absent (nil) is Default.
// A field left out takes its default; an unknown key or a bad value is
// refused.
func Parse(raw any) (Settings, error) {
	if raw == nil {
		return Default, nil
	}
	object, ok := raw.(map[string]any)
	if !ok {
		return Settings{}, fmt.Errorf("%w: context must be an object", ErrInvalidBlock)
	}
	settings := Default
	for key, value := range object {
		switch key {
		case "history":
			mode, ok := value.(string)
			if !ok || !History(mode).Valid() {
				return Settings{}, fmt.Errorf("%w: context.history must be full, since_joined, addressed_only or none", ErrInvalidBlock)
			}
			settings.History = History(mode)
		case "include_side_talk":
			mode, ok := value.(string)
			if !ok || (SideTalk(mode) != SideTalkParty && SideTalk(mode) != SideTalkNone) {
				return Settings{}, fmt.Errorf("%w: context.include_side_talk must be party or none", ErrInvalidBlock)
			}
			settings.SideTalk = SideTalk(mode)
		case "activity_notes":
			notes, ok := value.(bool)
			if !ok {
				return Settings{}, fmt.Errorf("%w: context.activity_notes must be a boolean", ErrInvalidBlock)
			}
			settings.ActivityNotes = notes
		default:
			return Settings{}, fmt.Errorf("%w: unknown key context.%s", ErrInvalidBlock, key)
		}
	}
	return settings, nil
}

// WithParticipant applies the participant's own mode. Only `since_joined` is
// read there ("share history from: now"), and it only narrows: a version
// that already sees less (addressed_only, none) keeps its mode.
func (s Settings) WithParticipant(mode string) Settings {
	if History(mode) == HistorySinceJoined && s.History == HistoryFull {
		s.History = HistorySinceJoined
	}
	return s
}

// Facts is what the resolver projects about one visible message for the
// reader it builds the view for.
type Facts struct {
	// ReadersOwn: the reader's answer, or a message to the reader from the
	// person asking.
	ReadersOwn bool
	// OwnAgent: the reader wrote it.
	OwnAgent bool
	// Addressed: it is addressed to the reader.
	Addressed bool
	// Side: agent-to-agent side talk.
	Side bool
	// AfterJoined: written after the reader joined.
	AfterJoined bool
	// AnsweredByReader: the reader answered it, or a message to the reader
	// replied to it.
	AnsweredByReader bool
}

// Reason says why a message is not in a reader's view (issue 1669, the
// "What the agent saw" inspector). The empty Reason is "in the view".
type Reason string

const (
	// ReasonSideTalkOfOthers: side talk between two other agents, which is
	// never in a reader's view.
	ReasonSideTalkOfOthers Reason = "side_talk_of_others"
	// ReasonSideTalkOff: side talk the reader is a party to, with
	// include_side_talk none.
	ReasonSideTalkOff Reason = "side_talk_off"
	// ReasonHistoryNone: the mode is none (the current message only).
	ReasonHistoryNone Reason = "history_none"
	// ReasonBeforeJoined: written before the reader joined (since_joined).
	ReasonBeforeJoined Reason = "before_joined"
	// ReasonNotAddressed: neither to, from nor answered by the reader
	// (addressed_only).
	ReasonNotAddressed Reason = "not_addressed"
)

// Eligible reports whether the reader sees a message with these facts.
func (s Settings) Eligible(f Facts) bool {
	return s.Exclusion(f) == ""
}

// Exclusion is why the reader does not see a message with these facts, or ""
// when it does. It is the one rule; Eligible reads it.
func (s Settings) Exclusion(f Facts) Reason {
	party := f.OwnAgent || f.Addressed
	if f.Side && !party {
		return ReasonSideTalkOfOthers
	}
	if f.Side && s.SideTalk == SideTalkNone {
		return ReasonSideTalkOff
	}
	switch s.History {
	case HistoryNone:
		return ReasonHistoryNone
	case HistorySinceJoined:
		if f.AfterJoined || f.OwnAgent {
			return ""
		}
		return ReasonBeforeJoined
	case HistoryAddressedOnly:
		if party || f.ReadersOwn || f.AnsweredByReader {
			return ""
		}
		return ReasonNotAddressed
	default:
		return ""
	}
}

// DigestDomain separates a view digest from every other digest.
const DigestDomain = "elitea.context-view.v1"

// Ref is one history entry's reference, for the digest.
type Ref struct {
	MessageID  string
	ReadersOwn bool
}

// Digest is the lowercase hex SHA-256 the worker recomputes: the domain, the
// mode, the window, the exact `chat_history` bytes and every reference, each
// variable-length field prefixed with its big-endian 64-bit length.
func Digest(history History, windowTokens uint32, chatHistory []byte, refs []Ref) string {
	hash := sha256.New()
	field := func(value []byte) {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		hash.Write(size[:])
		hash.Write(value)
	}
	field([]byte(DigestDomain))
	field([]byte(history))
	var window [4]byte
	binary.BigEndian.PutUint32(window[:], windowTokens)
	hash.Write(window[:])
	field(chatHistory)
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], uint64(len(refs)))
	hash.Write(count[:])
	for _, ref := range refs {
		field([]byte(ref.MessageID))
		if ref.ReadersOwn {
			hash.Write([]byte{1})
		} else {
			hash.Write([]byte{0})
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}
