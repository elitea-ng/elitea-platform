// Package agentmemory is the rules of an agent's own memory (ADR-0050
// decision 1b, issue 1607): the per-version `memory` block, the write mode
// decision, the content checks (the secret scanner among them) and recall
// selection. The tables are tenant/0163; storage is repos.AgentMemory.
//
// # The memory block
//
// It is the `memory` key of the version's meta, so it is part of the version
// and of the release digest like every other meta key:
//
//	memory:
//	  enabled: true
//	  write: auto_attended   # auto_attended | auto | review | off
//	  recall: { max_entries: 12, max_tokens: 1500 }
//
// A version WITHOUT the block has memory off: that is every agent saved before
// this package existed. A new agent gets DefaultNew written into its first
// version, so memory is on for new agents and off for existing ones until the
// owner turns it on.
package agentmemory

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// MetaKey is the version meta key of the block.
const MetaKey = "memory"

// Kind is what an entry records.
type Kind string

const (
	KindFact       Kind = "fact"
	KindPreference Kind = "preference"
	KindEpisode    Kind = "episode"
	// KindCursor is the heartbeat's last-seen marker (decision 2). The
	// platform writes it; the agent's remember tool cannot.
	KindCursor Kind = "cursor"
)

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool {
	switch k {
	case KindFact, KindPreference, KindEpisode, KindCursor:
		return true
	}
	return false
}

// AgentWritable reports whether the agent's remember tool may write k.
func (k Kind) AgentWritable() bool { return k.Valid() && k != KindCursor }

// Status is where an entry is in review.
type Status string

const (
	// StatusPending waits for a person and is never recalled.
	StatusPending Status = "pending"
	StatusActive  Status = "active"
	// StatusArchived is kept for review and never recalled.
	StatusArchived Status = "archived"
)

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	return s == StatusPending || s == StatusActive || s == StatusArchived
}

// WriteMode is how the agent's writes land.
type WriteMode string

const (
	// WriteAutoAttended is active when a person is present, pending
	// otherwise (owner decision 11, the default).
	WriteAutoAttended WriteMode = "auto_attended"
	WriteAuto         WriteMode = "auto"
	WriteReview       WriteMode = "review"
	// WriteOff recalls but never writes.
	WriteOff WriteMode = "off"
)

// Valid reports whether m is a known mode.
func (m WriteMode) Valid() bool {
	switch m {
	case WriteAutoAttended, WriteAuto, WriteReview, WriteOff:
		return true
	}
	return false
}

// Bounds of the block and of an entry.
const (
	DefaultMaxEntries = 12
	DefaultMaxTokens  = 1500
	MaxRecallEntries  = 50
	MinRecallTokens   = 100
	MaxRecallTokens   = 8000
	// BytesPerToken turns the token budget into a byte budget. A coarse
	// measure on purpose: recall runs before any tokenizer is known.
	BytesPerToken = 4
	// MaxContentBytes bounds one entry (the tenant CHECK says the same).
	MaxContentBytes = 2048
	// DefaultActiveLimit is how many active entries one agent keeps in one
	// project. When full, the oldest unpinned episodes are archived first.
	DefaultActiveLimit = 2000
)

// Settings is a parsed memory block.
type Settings struct {
	Enabled    bool
	Write      WriteMode
	MaxEntries int
	MaxTokens  int
}

// Off is the settings of a version without a block.
var Off = Settings{Enabled: false, Write: WriteOff, MaxEntries: DefaultMaxEntries, MaxTokens: DefaultMaxTokens}

// DefaultNew is the block a new agent's first version gets.
var DefaultNew = Settings{Enabled: true, Write: WriteAutoAttended, MaxEntries: DefaultMaxEntries, MaxTokens: DefaultMaxTokens}

// Block renders s as the meta value.
func (s Settings) Block() map[string]any {
	return map[string]any{
		"enabled": s.Enabled,
		"write":   string(s.Write),
		"recall":  map[string]any{"max_entries": s.MaxEntries, "max_tokens": s.MaxTokens},
	}
}

// Writes reports whether the agent may write at all.
func (s Settings) Writes() bool { return s.Enabled && s.Write != WriteOff }

// RecallBytes is the recall byte budget.
func (s Settings) RecallBytes() int { return s.MaxTokens * BytesPerToken }

// ErrInvalidBlock is a block that is not valid; the message names the field.
var ErrInvalidBlock = errors.New("invalid memory block")

// Parse reads the block from a decoded meta value. Absent (nil) is Off. A
// field left out takes its default; an unknown key or a bad value is refused.
func Parse(raw any) (Settings, error) {
	if raw == nil {
		return Off, nil
	}
	object, ok := raw.(map[string]any)
	if !ok {
		return Settings{}, fmt.Errorf("%w: memory must be an object", ErrInvalidBlock)
	}
	settings := DefaultNew
	for key, value := range object {
		switch key {
		case "enabled":
			enabled, ok := value.(bool)
			if !ok {
				return Settings{}, fmt.Errorf("%w: memory.enabled must be a boolean", ErrInvalidBlock)
			}
			settings.Enabled = enabled
		case "write":
			mode, ok := value.(string)
			if !ok || !WriteMode(mode).Valid() {
				return Settings{}, fmt.Errorf("%w: memory.write must be auto_attended, auto, review or off", ErrInvalidBlock)
			}
			settings.Write = WriteMode(mode)
		case "recall":
			if err := parseRecall(value, &settings); err != nil {
				return Settings{}, err
			}
		default:
			return Settings{}, fmt.Errorf("%w: unknown key memory.%s", ErrInvalidBlock, key)
		}
	}
	return settings, nil
}

func parseRecall(raw any, settings *Settings) error {
	if raw == nil {
		return nil
	}
	object, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("%w: memory.recall must be an object", ErrInvalidBlock)
	}
	for key, value := range object {
		number, ok := wholeNumber(value)
		switch {
		case key == "max_entries" && ok && number >= 1 && number <= MaxRecallEntries:
			settings.MaxEntries = number
		case key == "max_tokens" && ok && number >= MinRecallTokens && number <= MaxRecallTokens:
			settings.MaxTokens = number
		case key == "max_entries":
			return fmt.Errorf("%w: memory.recall.max_entries must be a whole number from 1 to %d", ErrInvalidBlock, MaxRecallEntries)
		case key == "max_tokens":
			return fmt.Errorf("%w: memory.recall.max_tokens must be a whole number from %d to %d", ErrInvalidBlock, MinRecallTokens, MaxRecallTokens)
		default:
			return fmt.Errorf("%w: unknown key memory.recall.%s", ErrInvalidBlock, key)
		}
	}
	return nil
}

// wholeNumber accepts a JSON number (float64 from encoding/json, or an int
// from a Go literal) that is whole.
func wholeNumber(value any) (int, bool) {
	switch number := value.(type) {
	case float64:
		if number != float64(int(number)) {
			return 0, false
		}
		return int(number), true
	case int:
		return number, true
	case int64:
		return int(number), true
	}
	return 0, false
}

// WriteContext is what the platform knows about one agent write.
type WriteContext struct {
	// Attended: a person started this run in the product and sees the
	// chip (a manual trigger, not a continuation of an unattended chain).
	Attended bool
	// FromToolOutput: the run read tool or document output before the
	// write, so the entry may carry text nobody in the conversation wrote.
	FromToolOutput bool
	// PersonalProject: the project is a person's own.
	PersonalProject bool
}

// ErrWritesOff refuses a write of an agent whose mode is off or whose
// memory is disabled.
var ErrWritesOff = errors.New("this agent's memory does not accept writes")

// DecideStatus is the status an agent write lands in.
func DecideStatus(settings Settings, write WriteContext) (Status, error) {
	if !settings.Writes() {
		return "", ErrWritesOff
	}
	switch settings.Write {
	case WriteAuto:
		return StatusActive, nil
	case WriteReview:
		return StatusPending, nil
	}
	// auto_attended.
	if write.PersonalProject || (write.Attended && !write.FromToolOutput) {
		return StatusActive, nil
	}
	return StatusPending, nil
}

// Content refusals. Each is a model- or person-readable reason.
var (
	ErrContentEmpty   = errors.New("the entry is empty")
	ErrContentTooLong = fmt.Errorf("the entry is longer than %d bytes", MaxContentBytes)
	ErrContentInvalid = errors.New("the entry is not valid text")
	ErrContentSecret  = errors.New("the entry looks like it holds a credential, which memory never stores")
)

// CheckContent is the one content rule of every write, the agent's and a
// person's edit alike. It answers the text trimmed.
func CheckContent(text string) (string, error) {
	text = strings.TrimSpace(text)
	switch {
	case text == "":
		return "", ErrContentEmpty
	case !utf8.ValidString(text) || strings.ContainsRune(text, 0):
		return "", ErrContentInvalid
	case len(text) > MaxContentBytes:
		return "", ErrContentTooLong
	case LooksLikeSecret(text):
		return "", ErrContentSecret
	}
	return text, nil
}
