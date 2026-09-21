package nodeevent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// CHUNKED TOOL OUTPUT (#956).
//
// THE PROBLEM THIS SOLVES. A node event is one output frame, and the frame is
// bounded at 64 KiB by the runtime limits conformance document
// (`testdata/proto/runtime/v1/configuration-validation/conformance-limits.json`,
// `max_output_frame_bytes`, itself under the 48 KiB `max_redis_field_bytes`).
// Both workers enforce that bound on the WHOLE event — the Python worker at
// 60 KiB (`MAX_CURRENT_NODE_EVENT_JSON_BYTES`) and the native worker at 40 KiB
// for a tool value (`MAX_TOOL_EVENT_VALUE_BYTES`) — so a tool result larger
// than the frame could not be projected at all. It was REFUSED, and the
// refusal reached the user as a failed tool call: an 80,000-character artifact
// read, well inside the SDK toolkit's own 200,000-character cap, came back as
// `RESOURCE_EXHAUSTED: The agent event exceeds its output limit`.
//
// WHY NOT RAISE THE FRAME. The frame bound is not a local constant; it is the
// conformance limit every transport hop is sized against, Redis field bound
// included. Raising it to fit the largest tool result a toolkit may legally
// return (200k characters, which is ~800 KiB of worst-case UTF-8 and more once
// JSON-escaped) would move a bound that four components agree on, to a number
// chosen by one toolkit's cap.
//
// WHAT THIS DOES INSTEAD. A tool result too large for one frame is emitted as
// an ORDERED SEQUENCE of ordinary node events — same frame bound, same replay
// append, same ack accounting, nothing new on the transport — each carrying a
// slice of the result's text plus its position. The Go side reassembles them
// into the stored tool call before the trace is read back. The per-frame bound
// is unchanged; what changes is that one logical value may now span frames.
//
// WHY THE CHUNKS ARE REAL EVENTS rather than buffered fragments held in memory
// until a terminal marker: every node event is durably appended and acked one
// at a time, and a buffer would be state that a crash loses in the middle of a
// value the sender has already been told was accepted. As ordinary events they
// are replayed like everything else, and reassembly is a pure function of what
// is already stored.
const (
	// ToolOutputChunkEventType is the event type a chunk rides on. It is a
	// distinct type rather than a flag on `partial_message` so a consumer that
	// does not implement reassembly (an older browser bundle, a log tailer)
	// ignores it instead of rendering a fragment as though it were the whole
	// tool result.
	ToolOutputChunkEventType = "agent_tool_output_chunk"

	// ToolOutputChunkMetadataKey is where the chunk's position lives inside
	// the event's `response_metadata`.
	ToolOutputChunkMetadataKey = "tool_output_chunk"

	// ToolOutputChunkCountKey is the key a COMPLETED tool call carries in its
	// entry when its output was chunked: the number of chunks that precede it.
	// Its presence is what tells the reassembler not to overwrite the text it
	// has accumulated with the (empty) inline output.
	ToolOutputChunkCountKey = "tool_output_chunks"

	// ToolOutputChunkDigestKey carries the SHA-256 of the COMPLETE output, hex
	// encoded, on both the chunks and the completed entry. It is what makes a
	// lost or reordered chunk detectable rather than silently producing a
	// shorter result that reads like the whole one.
	ToolOutputChunkDigestKey = "tool_output_sha256"

	// MaxToolOutputChunkTextBytes is the text budget of ONE chunk.
	//
	// It is deliberately well under MaxCurrentJSONBytes (60 KiB): the text
	// travels as a JSON string, so a chunk of control characters escapes to
	// six bytes per byte, and the envelope carries the execution identity and
	// the chunk position besides. 32 KiB of text can escape to 192 KiB, which
	// would not fit — so a producer must SPLIT ON THE ENCODED SIZE, not on the
	// raw one. SplitToolOutput below does; this constant is the raw ceiling
	// that bounds the worst case before encoding is even considered.
	MaxToolOutputChunkTextBytes = 32 * 1024

	// MaxToolOutputChunks bounds one tool result at 64 chunks. With the
	// encoded-size split below that is comfortably more than the 200,000
	// characters the SDK's own agent-path cap admits, and it is the ceiling
	// that keeps a malformed or hostile producer from turning one tool call
	// into an unbounded event stream.
	MaxToolOutputChunks = 64

	// maxToolOutputBytes is the reassembled ceiling, restated so a consumer
	// refuses rather than allocates whatever arrives.
	maxToolOutputBytes = MaxToolOutputChunks * MaxToolOutputChunkTextBytes
)

// ToolOutputChunk is one slice of a tool result that did not fit in a frame.
type ToolOutputChunk struct {
	ToolCallID string
	Index      int
	Total      int
	// Digest is the hex SHA-256 of the COMPLETE output, repeated on every
	// chunk. Repeated rather than sent once at the end because a consumer that
	// has only some of the chunks must still be able to tell that they belong
	// to the same value.
	Digest string
	Text   string
}

// SplitToolOutput cuts one tool result into frame-sized chunks.
//
// The split is on the ENCODED size, not the raw one: a chunk's text becomes a
// JSON string, and the encoder is the thing whose output has to fit. Cutting
// at a fixed raw offset would produce a chunk that fits for ASCII and a frame
// violation for the same number of emoji or control characters.
//
// Chunks are cut on RUNE boundaries, so each chunk is independently valid
// UTF-8 and every hop that validates encoding (both workers, this codec, the
// browser) sees a well-formed string rather than half a code point.
func SplitToolOutput(toolCallID, output string) ([]ToolOutputChunk, error) {
	if toolCallID == "" || strings.ContainsAny(toolCallID, "\r\n\x00") ||
		len(toolCallID) > maxSafeStringBytes ||
		output == "" || len(output) > maxToolOutputBytes || !utf8.ValidString(output) {
		return nil, ErrInvalidCurrentNodeEvent
	}
	digest := sha256.Sum256([]byte(output))
	budget := chunkTextBudget()
	texts := make([]string, 0, 4)
	remaining := output
	for remaining != "" {
		cut := encodedPrefixLength(remaining, budget)
		if cut == 0 {
			// One rune whose ENCODED form alone exceeds the budget cannot
			// happen (a rune escapes to at most 12 bytes and the budget is
			// kilobytes), but a zero cut would loop forever, so it is refused
			// rather than trusted.
			return nil, ErrInvalidCurrentNodeEvent
		}
		texts = append(texts, remaining[:cut])
		remaining = remaining[cut:]
		if len(texts) > MaxToolOutputChunks {
			return nil, ErrInvalidCurrentNodeEvent
		}
	}
	chunks := make([]ToolOutputChunk, 0, len(texts))
	for index, text := range texts {
		chunks = append(chunks, ToolOutputChunk{
			ToolCallID: toolCallID,
			Index:      index,
			Total:      len(texts),
			Digest:     hex.EncodeToString(digest[:]),
			Text:       text,
		})
	}
	return chunks, nil
}

// ReassembleToolOutput joins chunks back into the exact original text.
//
// It is strict about everything that could produce a PLAUSIBLE but wrong
// result: the count must match `total`, the indices must be the complete
// sequence with no gap and no duplicate, every chunk must name the same call
// and the same digest, and the joined text must hash to that digest. A missing
// chunk yields an error, never a shorter output — a truncated tool result that
// reads like a complete one is the failure this whole mechanism exists to
// avoid.
func ReassembleToolOutput(chunks []ToolOutputChunk) (string, error) {
	if len(chunks) == 0 || len(chunks) > MaxToolOutputChunks {
		return "", ErrInvalidCurrentNodeEvent
	}
	total := chunks[0].Total
	if total != len(chunks) {
		return "", ErrInvalidCurrentNodeEvent
	}
	ordered := make([]string, total)
	seen := make([]bool, total)
	callID := chunks[0].ToolCallID
	digest := chunks[0].Digest
	size := 0
	for _, chunk := range chunks {
		if chunk.ToolCallID != callID || chunk.Digest != digest || chunk.Total != total ||
			chunk.Index < 0 || chunk.Index >= total || seen[chunk.Index] {
			return "", ErrInvalidCurrentNodeEvent
		}
		seen[chunk.Index] = true
		ordered[chunk.Index] = chunk.Text
		size += len(chunk.Text)
		if size > maxToolOutputBytes {
			return "", ErrInvalidCurrentNodeEvent
		}
	}
	joined := strings.Join(ordered, "")
	computed := sha256.Sum256([]byte(joined))
	if hex.EncodeToString(computed[:]) != digest {
		return "", ErrInvalidCurrentNodeEvent
	}
	return joined, nil
}

// ToolOutputDigest is the digest a producer stamps on the chunks and the
// completed entry, and the one a consumer checks the reassembled text against.
func ToolOutputDigest(output string) string {
	digest := sha256.Sum256([]byte(output))
	return hex.EncodeToString(digest[:])
}

// chunkMetadataJSON is the `response_metadata.tool_output_chunk` object. Its
// field names are the contract both workers encode and this package decodes.
type chunkMetadataJSON struct {
	ToolCallID string `json:"tool_call_id"`
	Index      int    `json:"index"`
	Total      int    `json:"total"`
	Digest     string `json:"tool_output_sha256"`
}

type chunkEventJSON struct {
	Type             string          `json:"type"`
	Content          *string         `json:"content"`
	ResponseMetadata json.RawMessage `json:"response_metadata"`
}

type chunkResponseMetadataJSON struct {
	Chunk *chunkMetadataJSON `json:"tool_output_chunk"`
}

// EncodeToolOutputChunkMetadata renders one chunk's position as the
// `response_metadata` fragment its event carries. The TEXT is not in here: it
// rides in the event's `content`, where every hop already bounds and validates
// a string.
func EncodeToolOutputChunkMetadata(chunk ToolOutputChunk) (json.RawMessage, error) {
	if !validChunkPosition(chunk) {
		return nil, ErrInvalidCurrentNodeEvent
	}
	raw, err := json.Marshal(map[string]any{
		ToolOutputChunkMetadataKey: chunkMetadataJSON{
			ToolCallID: chunk.ToolCallID,
			Index:      chunk.Index,
			Total:      chunk.Total,
			Digest:     chunk.Digest,
		},
	})
	if err != nil {
		return nil, ErrInvalidCurrentNodeEvent
	}
	return raw, nil
}

// DecodeToolOutputChunk reads one chunk out of an already-validated current
// NodeEvent JSON document.
//
// ok=false means "this event is not a chunk", which is the ordinary answer for
// every other event type and is not an error. An event that CLAIMS to be a
// chunk and is malformed IS an error: a consumer that skipped it would go on to
// reassemble a value with a hole in it.
func DecodeToolOutputChunk(browserData json.RawMessage) (ToolOutputChunk, bool, error) {
	var event chunkEventJSON
	if err := json.Unmarshal(browserData, &event); err != nil {
		return ToolOutputChunk{}, false, ErrInvalidCurrentNodeEvent
	}
	if event.Type != ToolOutputChunkEventType {
		return ToolOutputChunk{}, false, nil
	}
	var metadata chunkResponseMetadataJSON
	if len(bytes.TrimSpace(event.ResponseMetadata)) == 0 ||
		json.Unmarshal(event.ResponseMetadata, &metadata) != nil ||
		metadata.Chunk == nil || event.Content == nil || *event.Content == "" {
		return ToolOutputChunk{}, false, ErrInvalidCurrentNodeEvent
	}
	chunk := ToolOutputChunk{
		ToolCallID: metadata.Chunk.ToolCallID,
		Index:      metadata.Chunk.Index,
		Total:      metadata.Chunk.Total,
		Digest:     metadata.Chunk.Digest,
		Text:       *event.Content,
	}
	if !validChunkPosition(chunk) || len(chunk.Text) > MaxToolOutputChunkTextBytes ||
		!utf8.ValidString(chunk.Text) {
		return ToolOutputChunk{}, false, ErrInvalidCurrentNodeEvent
	}
	return chunk, true, nil
}

func validChunkPosition(chunk ToolOutputChunk) bool {
	return chunk.ToolCallID != "" && len(chunk.ToolCallID) <= maxSafeStringBytes &&
		!strings.ContainsAny(chunk.ToolCallID, "\r\n\x00") &&
		chunk.Total > 0 && chunk.Total <= MaxToolOutputChunks &&
		chunk.Index >= 0 && chunk.Index < chunk.Total &&
		len(chunk.Digest) == 2*sha256.Size && validHexDigest(chunk.Digest)
}

func validHexDigest(value string) bool {
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

// chunkTextBudget is the encoded-size budget one chunk's text may occupy.
//
// MaxCurrentJSONBytes is the whole event; the rest of the envelope (type,
// identity, the chunk position object) is bounded and small, and half the
// frame is left to it so a producer never has to reason about the exact
// overhead of the identity it happens to carry.
func chunkTextBudget() int {
	budget := MaxCurrentJSONBytes / 2
	if budget > MaxToolOutputChunkTextBytes {
		budget = MaxToolOutputChunkTextBytes
	}
	return budget
}

// encodedPrefixLength returns the longest prefix of value, cut on a rune
// boundary, whose JSON-STRING encoding fits in budget bytes.
//
// It walks runes and accumulates each one's encoded cost rather than encoding
// prefixes repeatedly, so splitting a 200,000-character result is linear in
// its length instead of quadratic.
func encodedPrefixLength(value string, budget int) int {
	if budget <= 0 {
		return 0
	}
	used := 0
	for offset, character := range value {
		cost := encodedRuneCost(character)
		if used+cost > budget {
			return offset
		}
		used += cost
	}
	return len(value)
}

// encodedRuneCost is what one rune costs inside a JSON string, matching
// encoding/json's escaping: the two-character escapes it emits, \u00XX for the
// other control characters, and the UTF-8 length otherwise. It deliberately
// OVERSTATES nothing and understates nothing for the characters json actually
// escapes — `<`, `>` and `&` are escaped by the standard encoder as < and
// friends, which is why they cost six.
func encodedRuneCost(character rune) int {
	switch character {
	case '"', '\\', '\n', '\r', '\t', '\b', '\f':
		return 2
	case '<', '>', '&':
		return 6
	}
	if character < 0x20 {
		return 6
	}
	if character == utf8.RuneError {
		// An invalid byte becomes the replacement character's escape; callers
		// have already rejected invalid UTF-8, so this is belt and braces.
		return 6
	}
	return utf8.RuneLen(character)
}
