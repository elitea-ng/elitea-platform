package repos

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// The worker reports, on the final message of a turn, every capability the
// turn was configured with but could not serve (an unported toolkit family,
// a refused toolkit setting, an unserved built-in tool). Main keeps the list
// on the message group's meta so a reloaded conversation shows the same
// notice the live stream did.
//
// The list is display data, not authority: it is rebuilt into a closed shape
// (kind, name, optional type, reason_code, reason) with bounded strings, and
// anything malformed is dropped rather than failing the answer it rides on.

const (
	maxSkippedCapabilities         = 32
	maxSkippedCapabilityLabelRunes = 256
)

type skippedCapability struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Type       string `json:"type,omitempty"`
	ReasonCode string `json:"reason_code"`
	Reason     string `json:"reason"`
}

func validSkippedCapabilityText(value string, required bool) bool {
	if required && value == "" {
		return false
	}
	return utf8.RuneCountInString(value) <= maxSkippedCapabilityLabelRunes &&
		!strings.ContainsAny(value, "\x00\r\n")
}

// sanitizeSkippedCapabilities returns the canonical JSON array to persist, or
// nil when there is nothing valid to keep.
func sanitizeSkippedCapabilities(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var entries []skippedCapability
	if json.Unmarshal(raw, &entries) != nil {
		return nil
	}
	kept := make([]skippedCapability, 0, len(entries))
	for _, entry := range entries {
		switch entry.Kind {
		case "toolkit", "internal_tool", "application":
		default:
			continue
		}
		if !validSkippedCapabilityText(entry.Name, true) ||
			!validSkippedCapabilityText(entry.Type, false) ||
			!validSkippedCapabilityText(entry.ReasonCode, true) ||
			!validSkippedCapabilityText(entry.Reason, true) {
			continue
		}
		kept = append(kept, entry)
		if len(kept) == maxSkippedCapabilities {
			break
		}
	}
	if len(kept) == 0 {
		return nil
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return nil
	}
	return encoded
}
