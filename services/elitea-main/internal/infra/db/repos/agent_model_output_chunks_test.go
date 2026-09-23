package repos

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestAgentModelChunksSurviveStorageAndExactReplay(t *testing.T) {
	text := strings.Repeat("界\n\"", 20_000)
	hash := sha256.Sum256([]byte(text))
	var rows []currentAgentTraceRow
	for offset := 0; offset < len(text); {
		end := min(offset+8000, len(text))
		// This fixture repeats five-byte units.
		end -= (end - offset) % 5
		chunk := map[string]any{"offset_bytes": offset, "total_bytes": len(text), "sha256": hex.EncodeToString(hash[:]), "final": end == len(text)}
		step := map[string]any{"tool_run_id": "long-model", "text": text[offset:end], "text_chunk_v1": chunk}
		delta := currentAgentTraceDelta{thinkingSteps: []map[string]any{step}}
		var err error
		rows, err = mergeCurrentAgentTraceRows(1, rows, delta)
		if err != nil {
			t.Fatal(err)
		}
		rows, err = mergeCurrentAgentTraceRows(1, rows, delta)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].text == nil || *rows[0].text != text[:end] {
			t.Fatal("incomplete or duplicate model output")
		}
		// Simulate the attrs JSON round trip across process replacement.
		encoded, err := json.Marshal(rows[0].attrs)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, &rows[0].attrs); err != nil {
			t.Fatal(err)
		}
		offset = end
	}
	if *rows[0].text != text {
		t.Fatal("lost model text")
	}
}

func TestAgentResultChunksRejectGapsAndDigestMismatch(t *testing.T) {
	sum := sha256.Sum256([]byte("abcd"))
	makePart := func(offset int, text string, final bool) map[string]any {
		return map[string]any{
			"content": text, "result_chunk_v1": map[string]any{"offset_bytes": offset, "total_bytes": 4, "sha256": hex.EncodeToString(sum[:]), "final": final},
		}
	}
	first, err := mergeAgentTextChunk(nil, makePart(0, "ab", false), "content", "result_chunk_v1", maxAgentModelOutputBytes, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []map[string]any{makePart(3, "d", true), makePart(2, "xx", true), makePart(0, "zz", false)} {
		if _, err := mergeAgentTextChunk(first, invalid, "content", "result_chunk_v1", maxAgentModelOutputBytes, false); err == nil {
			t.Fatal("accepted invalid result chunk")
		}
	}
	complete, err := mergeAgentTextChunk(first, makePart(2, "cd", true), "content", "result_chunk_v1", maxAgentModelOutputBytes, false)
	if err != nil || complete["content"] != "abcd" {
		t.Fatal("lost complete result", err)
	}
}
