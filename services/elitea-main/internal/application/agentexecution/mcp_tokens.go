package agentexecution

import (
	"bytes"
	"encoding/json"
)

// Session tokens use the existing encrypted input bundle, never chat history or Redis fields.
// Toolkit admission and exact credential matching remain required before each operation.
func validCurrentMCPTokens(tokens json.RawMessage) bool {
	trimmed := bytes.TrimSpace(tokens)
	return len(tokens) <= 64*1024 && (len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || validJSONObject(tokens))
}

func currentMCPTokens(tokens json.RawMessage) []byte {
	trimmed := bytes.TrimSpace(tokens)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return []byte(`{}`)
	}
	return bytes.Clone(tokens)
}
