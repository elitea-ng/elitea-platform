// Package nodeevent converts the current browser-facing NodeEvent JSON shape
// to and from its language-neutral protobuf payload. It does not own transport,
// persistence, or SSE delivery.
package nodeevent

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
)

const (
	// MaxCurrentJSONBytes leaves headroom for the execution identity, fence,
	// digest and protobuf framing under the 64 KiB v1 output-frame limit. This
	// data-plane bound is independent from the 32 KiB bus command bound.
	MaxCurrentJSONBytes = 60 * 1024
	maxSafeStringBytes  = 256
	maxJSONNesting      = 64
	maxEventTypeBytes   = 128
)

var ErrInvalidCurrentNodeEvent = errors.New("invalid current node event")

type currentNodeEventJSON struct {
	Type                string          `json:"type"`
	StreamID            *string         `json:"stream_id"`
	MessageID           *string         `json:"message_id"`
	QuestionID          *string         `json:"question_id"`
	Content             json.RawMessage `json:"content"`
	Thinking            *string         `json:"thinking"`
	ResponseMetadata    json.RawMessage `json:"response_metadata"`
	References          json.RawMessage `json:"references"`
	SIOEvent            *string         `json:"sio_event"`
	CreatedAt           *string         `json:"created_at"`
	ParentMessageID     *string         `json:"parent_message_id"`
	AgentName           *string         `json:"agent_name"`
	ExecutionGeneration *string         `json:"execution_generation"`
}

// DecodeCurrentJSON validates one bounded current NodeEvent object and maps it
// to protobuf without converting arbitrary JSON numbers through float64.
// Missing optional fields are normalized to the current NodeEvent defaults.
func DecodeCurrentJSON(raw []byte) (*runtimev1.NodeEventV1, error) {
	if len(raw) == 0 || len(raw) > MaxCurrentJSONBytes || !utf8.Valid(raw) || !validJSONNesting(raw) || validateJSONValue(raw) != nil {
		return nil, ErrInvalidCurrentNodeEvent
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var wire currentNodeEventJSON
	if err := decoder.Decode(&wire); err != nil || !validEventType(wire.Type) ||
		!validOptionalSafeString(wire.StreamID, maxSafeStringBytes) ||
		!validOptionalSafeString(wire.MessageID, maxSafeStringBytes) ||
		!validOptionalSafeString(wire.QuestionID, maxSafeStringBytes) ||
		!validOptionalString(wire.Thinking, MaxCurrentJSONBytes) ||
		!validOptionalSafeString(wire.SIOEvent, maxSafeStringBytes) ||
		!validOptionalTimestamp(wire.CreatedAt) ||
		!validOptionalSafeString(wire.ParentMessageID, maxSafeStringBytes) ||
		!validOptionalSafeString(wire.AgentName, maxSafeStringBytes) ||
		!validOptionalSafeString(wire.ExecutionGeneration, maxSafeStringBytes) {
		return nil, ErrInvalidCurrentNodeEvent
	}

	content, err := normalizedFragment(wire.Content, []byte("null"), fragmentAny)
	if err != nil {
		return nil, ErrInvalidCurrentNodeEvent
	}
	responseMetadata, err := normalizedFragment(wire.ResponseMetadata, []byte("{}"), fragmentObjectOrNull)
	if err != nil {
		return nil, ErrInvalidCurrentNodeEvent
	}
	references, err := normalizedFragment(wire.References, []byte("[]"), fragmentArrayOrNull)
	if err != nil {
		return nil, ErrInvalidCurrentNodeEvent
	}

	return &runtimev1.NodeEventV1{
		Type:                wire.Type,
		StreamId:            wire.StreamID,
		MessageId:           wire.MessageID,
		QuestionId:          wire.QuestionID,
		Content:             content,
		Thinking:            wire.Thinking,
		ResponseMetadata:    responseMetadata,
		References:          references,
		SioEvent:            wire.SIOEvent,
		CreatedAt:           wire.CreatedAt,
		ParentMessageId:     wire.ParentMessageID,
		AgentName:           wire.AgentName,
		ExecutionGeneration: wire.ExecutionGeneration,
	}, nil
}

// EncodeCurrentJSON returns the exact current top-level field names in their
// established order. Optional protobuf strings are emitted as JSON null.
func EncodeCurrentJSON(event *runtimev1.NodeEventV1) (json.RawMessage, error) {
	if event == nil || len(event.ProtoReflect().GetUnknown()) != 0 || !validEventType(event.GetType()) {
		return nil, ErrInvalidCurrentNodeEvent
	}
	if !validOptionalSafeString(event.StreamId, maxSafeStringBytes) ||
		!validOptionalSafeString(event.MessageId, maxSafeStringBytes) ||
		!validOptionalSafeString(event.QuestionId, maxSafeStringBytes) ||
		!validOptionalString(event.Thinking, MaxCurrentJSONBytes) ||
		!validOptionalSafeString(event.SioEvent, maxSafeStringBytes) ||
		!validOptionalTimestamp(event.CreatedAt) ||
		!validOptionalSafeString(event.ParentMessageId, maxSafeStringBytes) ||
		!validOptionalSafeString(event.AgentName, maxSafeStringBytes) ||
		!validOptionalSafeString(event.ExecutionGeneration, maxSafeStringBytes) {
		return nil, ErrInvalidCurrentNodeEvent
	}

	content, err := normalizedFragment(event.GetContent(), []byte("null"), fragmentAny)
	if err != nil {
		return nil, ErrInvalidCurrentNodeEvent
	}
	responseMetadata, err := normalizedFragment(event.GetResponseMetadata(), []byte("{}"), fragmentObjectOrNull)
	if err != nil {
		return nil, ErrInvalidCurrentNodeEvent
	}
	references, err := normalizedFragment(event.GetReferences(), []byte("[]"), fragmentArrayOrNull)
	if err != nil {
		return nil, ErrInvalidCurrentNodeEvent
	}

	raw, err := json.Marshal(currentNodeEventJSON{
		Type:                event.GetType(),
		StreamID:            event.StreamId,
		MessageID:           event.MessageId,
		QuestionID:          event.QuestionId,
		Content:             content,
		Thinking:            event.Thinking,
		ResponseMetadata:    responseMetadata,
		References:          references,
		SIOEvent:            event.SioEvent,
		CreatedAt:           event.CreatedAt,
		ParentMessageID:     event.ParentMessageId,
		AgentName:           event.AgentName,
		ExecutionGeneration: event.ExecutionGeneration,
	})
	if err != nil || len(raw) > MaxCurrentJSONBytes {
		return nil, ErrInvalidCurrentNodeEvent
	}
	return json.RawMessage(raw), nil
}

type fragmentKind uint8

const (
	fragmentAny fragmentKind = iota
	fragmentObjectOrNull
	fragmentArrayOrNull
)

func normalizedFragment(raw, fallback []byte, kind fragmentKind) ([]byte, error) {
	if len(raw) == 0 {
		raw = fallback
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || len(trimmed) > MaxCurrentJSONBytes || !utf8.Valid(trimmed) || !validJSONNesting(trimmed) || validateJSONValue(trimmed) != nil {
		return nil, ErrInvalidCurrentNodeEvent
	}
	if !bytes.Equal(trimmed, []byte("null")) {
		switch kind {
		case fragmentObjectOrNull:
			if trimmed[0] != '{' {
				return nil, ErrInvalidCurrentNodeEvent
			}
		case fragmentArrayOrNull:
			if trimmed[0] != '[' {
				return nil, ErrInvalidCurrentNodeEvent
			}
		}
	}
	var compact bytes.Buffer
	compact.Grow(len(trimmed))
	if err := json.Compact(&compact, trimmed); err != nil {
		return nil, ErrInvalidCurrentNodeEvent
	}
	return compact.Bytes(), nil
}

func validateJSONValue(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := consumeJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return ErrInvalidCurrentNodeEvent
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			key, ok := keyToken.(string)
			if err != nil || !ok {
				return ErrInvalidCurrentNodeEvent
			}
			if _, duplicate := seen[key]; duplicate {
				return ErrInvalidCurrentNodeEvent
			}
			seen[key] = struct{}{}
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return ErrInvalidCurrentNodeEvent
		}
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return ErrInvalidCurrentNodeEvent
		}
	default:
		return ErrInvalidCurrentNodeEvent
	}
	return nil
}

func validJSONNesting(raw []byte) bool {
	depth := 0
	inString := false
	escaped := false
	for _, character := range raw {
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch character {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch character {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > maxJSONNesting {
				return false
			}
		case '}', ']':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0 && !inString && !escaped
}

func validEventType(value string) bool {
	if len(value) == 0 || len(value) > maxEventTypeBytes {
		return false
	}
	for index, character := range []byte(value) {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (index > 0 && character >= '0' && character <= '9') || (index > 0 && character == '_') {
			continue
		}
		return false
	}
	return true
}

func validOptionalSafeString(value *string, maxBytes int) bool {
	return validOptionalString(value, maxBytes) && (value == nil || !strings.ContainsAny(*value, "\r\n\x00"))
}

func validOptionalString(value *string, maxBytes int) bool {
	return value == nil || (utf8.ValidString(*value) && len(*value) <= maxBytes)
}

func validOptionalTimestamp(value *string) bool {
	if value == nil {
		return true
	}
	if *value == "" || len(*value) > 64 || strings.ContainsAny(*value, "\r\n\x00") {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, *value)
	return err == nil
}
