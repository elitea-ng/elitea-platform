package nodeevent_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/nodeevent"
)

// encodeChunkEvent renders one chunk exactly as a worker does: an ordinary
// node event whose content is the chunk text and whose response_metadata
// carries the position. It goes through the REAL codec, so a chunk that could
// not survive the frame bound fails here rather than in production.
func encodeChunkEvent(t *testing.T, chunk nodeevent.ToolOutputChunk) json.RawMessage {
	t.Helper()
	metadata, err := nodeevent.EncodeToolOutputChunkMetadata(chunk)
	require.NoError(t, err)
	raw, err := json.Marshal(map[string]any{
		"type":                 nodeevent.ToolOutputChunkEventType,
		"stream_id":            "execution-1:2",
		"message_id":           "message-1",
		"execution_generation": "2",
		"sio_event":            "chat_predict",
		"content":              chunk.Text,
		"response_metadata":    json.RawMessage(metadata),
	})
	require.NoError(t, err)
	decoded, err := nodeevent.DecodeCurrentJSON(raw)
	require.NoError(t, err, "a chunk event must survive the frame bound")
	encoded, err := nodeevent.EncodeCurrentJSON(decoded)
	require.NoError(t, err)
	return encoded
}

// The round trip is the whole contract: an output far past the frame goes out
// as chunks, comes back through the codec, and reassembles BYTE FOR BYTE.
func TestToolOutputChunkRoundTripRestoresTheExactOutput(t *testing.T) {
	t.Parallel()

	for name, output := range map[string]string{
		"an 80k-character read (the case #956 was filed for)": strings.Repeat(
			"AUTOTESTMED the quick brown fox jumps over the lazy dog 0123456789\n", 1_213,
		),
		"one that fits in a single chunk":  "a short tool result",
		"one that is exactly escape-heavy": strings.Repeat("\"\\\n\t<&>", 4_096),
		"one that is not ASCII":            strings.Repeat("проверка ✅ 🙂 ", 4_096),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			chunks, err := nodeevent.SplitToolOutput("call-1", output)
			require.NoError(t, err)
			require.NotEmpty(t, chunks)

			decoded := make([]nodeevent.ToolOutputChunk, 0, len(chunks))
			for _, chunk := range chunks {
				event := encodeChunkEvent(t, chunk)
				require.LessOrEqual(t, len(event), nodeevent.MaxCurrentJSONBytes,
					"every chunk event must fit one output frame")
				back, ok, err := nodeevent.DecodeToolOutputChunk(event)
				require.NoError(t, err)
				require.True(t, ok)
				decoded = append(decoded, back)
			}

			reassembled, err := nodeevent.ReassembleToolOutput(decoded)
			require.NoError(t, err)
			require.Equal(t, output, reassembled)
		})
	}
}

// Every chunk of an escape-heavy value must fit the frame ENCODED, which is
// the property a raw-offset split gets wrong: 32 KiB of quotes is 64 KiB of
// JSON.
func TestToolOutputChunksAreSplitOnTheEncodedSize(t *testing.T) {
	t.Parallel()

	output := strings.Repeat("\"", 200_000)
	chunks, err := nodeevent.SplitToolOutput("call-1", output)
	require.NoError(t, err)
	for _, chunk := range chunks {
		encoded, err := json.Marshal(chunk.Text)
		require.NoError(t, err)
		require.LessOrEqual(t, len(encoded), nodeevent.MaxCurrentJSONBytes/2+2)
		require.LessOrEqual(t, len(encodeChunkEvent(t, chunk)), nodeevent.MaxCurrentJSONBytes)
	}
	reassembled, err := nodeevent.ReassembleToolOutput(chunks)
	require.NoError(t, err)
	require.Equal(t, output, reassembled)
}

// A chunk boundary never lands inside a code point, so every chunk is
// independently valid UTF-8 — which every hop between the worker and the
// browser validates.
func TestToolOutputChunksCutOnRuneBoundaries(t *testing.T) {
	t.Parallel()

	chunks, err := nodeevent.SplitToolOutput("call-1", strings.Repeat("🙂", 40_000))
	require.NoError(t, err)
	require.Greater(t, len(chunks), 1)
	for _, chunk := range chunks {
		require.True(t, json.Valid(mustMarshal(t, chunk.Text)))
		require.Equal(t, chunk.Text, strings.ToValidUTF8(chunk.Text, "?"))
	}
}

// A missing, duplicated, reordered or foreign chunk must FAIL rather than
// produce a shorter result: a truncated tool output that reads like a complete
// one is exactly what the digest is carried for.
func TestToolOutputReassemblyRefusesAnIncompleteSequence(t *testing.T) {
	t.Parallel()

	output := strings.Repeat("payload ", 20_000)
	chunks, err := nodeevent.SplitToolOutput("call-1", output)
	require.NoError(t, err)
	require.Greater(t, len(chunks), 2)

	for name, mutate := range map[string]func([]nodeevent.ToolOutputChunk) []nodeevent.ToolOutputChunk{
		"a dropped chunk": func(in []nodeevent.ToolOutputChunk) []nodeevent.ToolOutputChunk {
			return append(append([]nodeevent.ToolOutputChunk{}, in[0]), in[2:]...)
		},
		"a duplicated chunk": func(in []nodeevent.ToolOutputChunk) []nodeevent.ToolOutputChunk {
			out := append([]nodeevent.ToolOutputChunk{}, in...)
			out[1] = out[0]
			return out
		},
		"a chunk from another call": func(in []nodeevent.ToolOutputChunk) []nodeevent.ToolOutputChunk {
			out := append([]nodeevent.ToolOutputChunk{}, in...)
			out[1].ToolCallID = "call-2"
			return out
		},
		"a chunk whose text was altered": func(in []nodeevent.ToolOutputChunk) []nodeevent.ToolOutputChunk {
			out := append([]nodeevent.ToolOutputChunk{}, in...)
			out[1].Text = strings.Repeat("x", len(out[1].Text))
			return out
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := nodeevent.ReassembleToolOutput(mutate(chunks))
			require.ErrorIs(t, err, nodeevent.ErrInvalidCurrentNodeEvent)
		})
	}

	// Reordering is NOT an error: the index is the order, not the arrival, so
	// a consumer that collected events out of order still reassembles exactly.
	shuffled := append([]nodeevent.ToolOutputChunk{}, chunks...)
	shuffled[0], shuffled[len(shuffled)-1] = shuffled[len(shuffled)-1], shuffled[0]
	reassembled, err := nodeevent.ReassembleToolOutput(shuffled)
	require.NoError(t, err)
	require.Equal(t, output, reassembled)
}

// Every other event type answers "not a chunk" without an error, and an event
// that claims to be a chunk and is malformed answers with one — a consumer
// that skipped the second would reassemble a value with a hole in it.
func TestDecodeToolOutputChunkSeparatesAbsenceFromMalformation(t *testing.T) {
	t.Parallel()

	ordinary, err := json.Marshal(map[string]any{
		"type": "partial_message", "content": "hello",
	})
	require.NoError(t, err)
	_, ok, err := nodeevent.DecodeToolOutputChunk(ordinary)
	require.NoError(t, err)
	require.False(t, ok)

	for name, raw := range map[string]map[string]any{
		"no chunk metadata": {
			"type": nodeevent.ToolOutputChunkEventType, "content": "x",
			"response_metadata": map[string]any{},
		},
		"no text": {
			"type": nodeevent.ToolOutputChunkEventType, "content": "",
			"response_metadata": map[string]any{
				nodeevent.ToolOutputChunkMetadataKey: map[string]any{
					"tool_call_id": "call-1", "index": 0, "total": 1,
					"tool_output_sha256": nodeevent.ToolOutputDigest("x"),
				},
			},
		},
		"an index outside its own total": {
			"type": nodeevent.ToolOutputChunkEventType, "content": "x",
			"response_metadata": map[string]any{
				nodeevent.ToolOutputChunkMetadataKey: map[string]any{
					"tool_call_id": "call-1", "index": 3, "total": 2,
					"tool_output_sha256": nodeevent.ToolOutputDigest("x"),
				},
			},
		},
		"a digest that is not one": {
			"type": nodeevent.ToolOutputChunkEventType, "content": "x",
			"response_metadata": map[string]any{
				nodeevent.ToolOutputChunkMetadataKey: map[string]any{
					"tool_call_id": "call-1", "index": 0, "total": 1,
					"tool_output_sha256": "not-a-digest",
				},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			encoded, err := json.Marshal(raw)
			require.NoError(t, err)
			_, ok, err := nodeevent.DecodeToolOutputChunk(encoded)
			require.ErrorIs(t, err, nodeevent.ErrInvalidCurrentNodeEvent)
			require.False(t, ok)
		})
	}
}

// The producer side refuses what it cannot carry, rather than emitting a
// sequence no consumer will accept.
func TestSplitToolOutputRefusesWhatItCannotCarry(t *testing.T) {
	t.Parallel()

	for name, call := range map[string]func() ([]nodeevent.ToolOutputChunk, error){
		"an empty output": func() ([]nodeevent.ToolOutputChunk, error) {
			return nodeevent.SplitToolOutput("call-1", "")
		},
		"no call id": func() ([]nodeevent.ToolOutputChunk, error) {
			return nodeevent.SplitToolOutput("", "value")
		},
		"an output past the ceiling": func() ([]nodeevent.ToolOutputChunk, error) {
			return nodeevent.SplitToolOutput("call-1", strings.Repeat("x",
				nodeevent.MaxToolOutputChunks*nodeevent.MaxToolOutputChunkTextBytes+1))
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := call()
			require.ErrorIs(t, err, nodeevent.ErrInvalidCurrentNodeEvent)
		})
	}
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	return raw
}
