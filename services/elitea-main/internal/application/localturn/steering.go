package localturn

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
)

// Steering messages of a local turn (client contract 1.27): the texts the
// person sent while the turn ran, which the agent took at a safe point. The
// commit stores each one as a user message after the question. The bounds
// are the cloud steering bounds (agentexecution steering).
const (
	MaxSteeringMessages     = 10
	MaxSteeringMessageBytes = 16 * 1024
)

// validSteeringMessages accepts at most MaxSteeringMessages texts, each 1 to
// MaxSteeringMessageBytes of UTF-8 without NUL that is not only white space.
func validSteeringMessages(messages []string) bool {
	if len(messages) > MaxSteeringMessages {
		return false
	}
	for _, message := range messages {
		if !validText(message, MaxSteeringMessageBytes, false) || strings.TrimSpace(message) == "" {
			return false
		}
	}
	return true
}

// steeringAuditPart is what the audit row says of the steering messages: how
// many, and the SHA-256 and size of their texts in order. Never the text.
// It is "" when there are none.
func steeringAuditPart(messages []string) string {
	if len(messages) == 0 {
		return ""
	}
	encoded, err := json.Marshal(messages)
	if err != nil {
		return ""
	}
	size := 0
	for _, message := range messages {
		size += len(message)
	}
	digest := sha256.Sum256(encoded)
	return strconv.Itoa(len(messages)) + " steering messages (content sha256 " +
		hex.EncodeToString(digest[:]) + ", " + strconv.Itoa(size) + " bytes)"
}

// withSteering puts the steering part first in a commit's audit action, so
// the 512-character cut never removes it.
func withSteering(action string, messages []string) string {
	part := steeringAuditPart(messages)
	const prefix = "local turn committed: "
	if part == "" || !strings.HasPrefix(action, prefix) {
		return action
	}
	action = prefix + part + ", " + strings.TrimPrefix(action, prefix)
	if runes := []rune(action); len(runes) > 512 {
		action = string(runes[:511]) + "…"
	}
	return action
}
