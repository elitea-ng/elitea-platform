package repos

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

const maxAgentToolResultBytes = 1024 * 1024

// This is the tool_output_chunk_v1 contract declared in node_event.proto.
type agentToolOutputChunk struct {
	Offset int    `json:"offset_bytes"`
	Total  int    `json:"total_bytes"`
	Digest string `json:"sha256"`
	Final  bool   `json:"final"`
}

func mergeAgentToolOutputChunk(previous, incoming map[string]any) (map[string]any, error) {
	raw, present := incoming["tool_output_chunk_v1"]
	if !present {
		return incoming, nil
	}
	invalid := errors.New("invalid agent tool output chunk")
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, invalid
	}
	var chunk agentToolOutputChunk
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&chunk) != nil {
		return nil, invalid
	}
	data, ok := incoming["tool_output"].(string)
	digest, err := hex.DecodeString(chunk.Digest)
	if !ok || !utf8.ValidString(data) || len(data) == 0 || len(data) > 8192 || err != nil || len(digest) != sha256.Size || strings.ToLower(chunk.Digest) != chunk.Digest || chunk.Offset < 0 || chunk.Total <= 0 || chunk.Total > maxAgentToolResultBytes || chunk.Offset > chunk.Total-len(data) || chunk.Final != (chunk.Offset+len(data) == chunk.Total) {
		return nil, invalid
	}
	old, _ := previous["tool_output"].(string)
	if prior, exists := previous["tool_output_chunk_v1"]; exists {
		b, _ := json.Marshal(prior)
		var metadata agentToolOutputChunk
		if json.Unmarshal(b, &metadata) != nil || metadata.Total != chunk.Total || metadata.Digest != chunk.Digest {
			return nil, invalid
		}
	} else if chunk.Offset != 0 || old != "" {
		return nil, invalid
	}
	if chunk.Offset < len(old) {
		if chunk.Offset+len(data) > len(old) || old[chunk.Offset:chunk.Offset+len(data)] != data {
			return nil, invalid
		}
		return previous, nil
	}
	if chunk.Offset != len(old) {
		return nil, invalid
	}
	combined := old + data
	if chunk.Final {
		actual := sha256.Sum256([]byte(combined))
		if hex.EncodeToString(actual[:]) != chunk.Digest || !json.Valid([]byte(combined)) {
			return nil, invalid
		}
	}
	result := cloneCurrentAgentMap(incoming)
	result["tool_output"] = combined
	if _, exists := result["tool_inputs"]; !exists {
		result["tool_inputs"] = previous["tool_inputs"]
	}
	return result, nil
}
