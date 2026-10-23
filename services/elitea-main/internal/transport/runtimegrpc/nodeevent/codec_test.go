package nodeevent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/protobuf/proto"
)

type parityCorpus struct {
	ContractRevision  string   `json:"contract_revision"`
	CurrentEventTypes []string `json:"current_event_types"`
	Cases             []struct {
		Name       string          `json:"name"`
		WireSHA256 string          `json:"wire_sha256"`
		Event      json.RawMessage `json:"event"`
	} `json:"cases"`
}

var expectedCurrentJSONFields = []string{
	"type",
	"stream_id",
	"message_id",
	"question_id",
	"content",
	"thinking",
	"response_metadata",
	"references",
	"sio_event",
	"created_at",
	"parent_message_id",
	"agent_name",
	"execution_generation",
}

func TestCurrentNodeEventParityCorpusRoundTripsWithoutChangingUISemantics(t *testing.T) {
	corpus := loadParityCorpus(t)
	if corpus.ContractRevision != "elitea.runtime.node-event.v1" || len(corpus.Cases) < 2 {
		t.Fatal("current NodeEvent parity corpus is incomplete")
	}
	if len(corpus.CurrentEventTypes) != 36 {
		t.Fatalf("current NodeEvent type catalog changed: %v", corpus.CurrentEventTypes)
	}
	seenTypes := make(map[string]struct{}, len(corpus.CurrentEventTypes))
	for _, eventType := range corpus.CurrentEventTypes {
		if _, duplicate := seenTypes[eventType]; duplicate {
			t.Fatalf("duplicate current NodeEvent type %q", eventType)
		}
		seenTypes[eventType] = struct{}{}
		if _, err := DecodeCurrentJSON([]byte(`{"type":"` + eventType + `"}`)); err != nil {
			t.Fatalf("current NodeEvent type %q is not representable: %v", eventType, err)
		}
	}

	for _, testCase := range corpus.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			event, err := DecodeCurrentJSON(testCase.Event)
			if err != nil {
				t.Fatal(err)
			}
			wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			if len(wire) >= 64*1024 {
				t.Fatalf("NodeEvent protobuf leaves no frame headroom: %d", len(wire))
			}
			if testCase.WireSHA256 == "" {
				t.Fatal("cross-language wire digest is missing")
			}
			digest := sha256.Sum256(wire)
			if got := hex.EncodeToString(digest[:]); got != testCase.WireSHA256 {
				t.Fatalf("cross-language wire digest changed: got %s want %s", got, testCase.WireSHA256)
			}

			encoded, err := EncodeCurrentJSON(event)
			if err != nil {
				t.Fatal(err)
			}
			if !sameJSON(testCase.Event, encoded) {
				t.Fatalf("UI JSON semantics changed:\ninput=%s\noutput=%s", testCase.Event, encoded)
			}
			assertCurrentFieldNames(t, encoded)
		})
	}
}

func TestCurrentNodeEventCodecAppliesDefaultsAndStrictBounds(t *testing.T) {
	minimal := []byte(`{"type":"agent_exception","content":"safe failure"}`)
	event, err := DecodeCurrentJSON(minimal)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeCurrentJSON(event)
	if err != nil {
		t.Fatal(err)
	}
	assertCurrentFieldNames(t, encoded)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fields["response_metadata"], []byte("{}")) || !bytes.Equal(fields["references"], []byte("[]")) || !bytes.Equal(fields["stream_id"], []byte("null")) {
		t.Fatalf("current defaults changed: %s", encoded)
	}

	deep := `{"type":"agent_response","content":` + strings.Repeat("[", maxJSONNesting+1) + "null" + strings.Repeat("]", maxJSONNesting+1) + `}`
	oversized := `{"type":"agent_response","content":"` + strings.Repeat("x", MaxCurrentJSONBytes) + `"}`
	invalid := []string{
		`[]`,
		`{"content":null}`,
		`{"type":"agent_response","unknown":true}`,
		`{"type":"agent_response","type":"agent_exception"}`,
		`{"type":"agent_response","response_metadata":[]}`,
		`{"type":"agent_response","references":{}}`,
		`{"type":"agent_response","created_at":"not-a-time"}`,
		`{"type":"agent_response","stream_id":"unsafe\nroom"}`,
		`{"type":"agent_response","content":NaN}`,
		`{"type":"agent_response","response_metadata":{"state":"first","state":"second"}}`,
		deep,
		oversized,
	}
	for index, raw := range invalid {
		if _, err := DecodeCurrentJSON([]byte(raw)); !errors.Is(err, ErrInvalidCurrentNodeEvent) {
			t.Fatalf("invalid case %d was accepted: %v", index, err)
		}
	}
}

func TestCurrentNodeEventEncoderRejectsMalformedFragmentsAndOversizedText(t *testing.T) {
	invalidMetadata := &runtimev1.NodeEventV1{
		Type:             "agent_response",
		Content:          []byte("null"),
		ResponseMetadata: []byte(`[]`),
		References:       []byte(`[]`),
	}
	if _, err := EncodeCurrentJSON(invalidMetadata); !errors.Is(err, ErrInvalidCurrentNodeEvent) {
		t.Fatalf("invalid response_metadata was accepted: %v", err)
	}

	oversizedThinking := strings.Repeat("x", MaxCurrentJSONBytes)
	oversized := &runtimev1.NodeEventV1{
		Type:             "agent_response",
		Content:          []byte("null"),
		Thinking:         &oversizedThinking,
		ResponseMetadata: []byte(`{}`),
		References:       []byte(`[]`),
	}
	if _, err := EncodeCurrentJSON(oversized); !errors.Is(err, ErrInvalidCurrentNodeEvent) {
		t.Fatalf("oversized current event was accepted: %v", err)
	}
}

func TestCurrentNodeEventCodecAcceptsProductionSizedToolOutput(t *testing.T) {
	const outputBytes = 51_979
	const escapedQuoteCount = 5_385
	toolOutput := strings.Repeat(`"`, escapedQuoteCount) +
		strings.Repeat("x", outputBytes-escapedQuoteCount)
	metadata, err := json.Marshal(map[string]any{
		"tool_name":        "list_initiatives",
		"tool_run_id":      "run-production-sized",
		"tool_inputs":      map[string]any{"max_records": 100},
		"tool_output":      toolOutput,
		"metadata":         map[string]any{"toolkit_name": "aha"},
		"finish_reason":    "stop",
		"timestamp_start":  "2026-08-03T18:47:20Z",
		"timestamp_finish": "2026-08-03T18:47:21Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	event := &runtimev1.NodeEventV1{
		Type:             "agent_tool_end",
		Content:          []byte("null"),
		ResponseMetadata: metadata,
		References:       []byte(`[]`),
	}
	encoded, err := EncodeCurrentJSON(event)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > MaxCurrentJSONBytes {
		t.Fatalf("production-sized tool event exceeded browser bound: %d", len(encoded))
	}
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if len(wire) >= 64*1024 {
		t.Fatalf("production-sized NodeEvent leaves no frame headroom: %d", len(wire))
	}
}

func assertCurrentFieldNames(t *testing.T, raw []byte) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != len(expectedCurrentJSONFields) {
		t.Fatalf("current NodeEvent field count changed: %v", fields)
	}
	for _, name := range expectedCurrentJSONFields {
		if _, ok := fields[name]; !ok {
			t.Fatalf("current NodeEvent field %q is missing", name)
		}
	}
}

func sameJSON(left, right []byte) bool {
	decode := func(raw []byte) (any, error) {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		return value, nil
	}
	leftValue, leftErr := decode(left)
	rightValue, rightErr := decode(right)
	return leftErr == nil && rightErr == nil && reflect.DeepEqual(leftValue, rightValue)
}

func loadParityCorpus(t *testing.T) parityCorpus {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate NodeEvent parity test")
	}
	path := filepath.Clean(filepath.Join(
		filepath.Dir(source),
		"../../../../../../testdata/proto/runtime/v1/node-event/current-parity-corpus.json",
	))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var corpus parityCorpus
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	return corpus
}
