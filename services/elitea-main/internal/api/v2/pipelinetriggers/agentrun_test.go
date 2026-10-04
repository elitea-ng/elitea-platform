package pipelinetriggers

// The agent branch's two pure decisions (legacy issue 6656): what text an
// agent run starts with, and which caller variables reach it. The HTTP and
// database halves are in agent_postgres_integration_test.go.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAgentRunInputPrefersTheCallersInput(t *testing.T) {
	got, err := agentRunInput("summarise this", []byte(`{"input":"summarise this","ref":"main"}`))
	if err != nil || got != "summarise this" {
		t.Fatalf("agentRunInput = %q, %v; want the body's input", got, err)
	}
}

func TestAgentRunInputFallsBackToTheProviderPayload(t *testing.T) {
	payload := `{"object_kind":"push","ref":"refs/heads/main"}`
	got, err := agentRunInput("", []byte("  "+payload+"\n"))
	if err != nil {
		t.Fatalf("agentRunInput: %v", err)
	}
	if got != payload {
		t.Fatalf("agentRunInput = %q, want the trimmed payload %q", got, payload)
	}
	// A body that is not JSON at all is a payload too.
	got, err = agentRunInput("", []byte("deploy finished"))
	if err != nil || got != "deploy finished" {
		t.Fatalf("agentRunInput(text) = %q, %v", got, err)
	}
}

func TestAgentRunInputRefusesACallWithNothingToRead(t *testing.T) {
	for _, test := range []struct {
		name, input, payload string
	}{
		{"no body", "", ""},
		{"whitespace body", "   ", "  \n"},
		{"empty object", "", `{}`},
		{"empty input", "", `{"input":""}`},
		{"whitespace input", " ", `{"input":" "}`},
		{"only variables", "", `{"variables":{"topic":"x"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := agentRunInput(test.input, []byte(test.payload))
			if !errors.Is(err, ErrAgentInputRequired) {
				t.Fatalf("err = %v, want ErrAgentInputRequired", err)
			}
		})
	}
}

func TestAgentRunInputRefusesAPayloadTheRunCannotCarry(t *testing.T) {
	for _, payload := range [][]byte{
		{0xff, 0xfe, 'x'},
		[]byte("a\x00b"),
	} {
		if _, err := agentRunInput("", payload); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("agentRunInput(%q) err = %v, want ErrInvalidInput", payload, err)
		}
	}
}

func TestAgentRunInputTruncatesALargePayloadOnARuneBoundary(t *testing.T) {
	// A three-byte rune straddles the cut, so a byte cut would leave invalid
	// UTF-8 that the start use case refuses.
	payload := strings.Repeat("a", maxAgentPayloadInput-1) + "€" + strings.Repeat("b", 100)
	got, err := agentRunInput("", []byte(payload))
	if err != nil {
		t.Fatalf("agentRunInput: %v", err)
	}
	if !strings.HasSuffix(got, payloadTruncatedMarker) {
		t.Fatalf("a cut payload must say so; got suffix %q", got[len(got)-30:])
	}
	if !utf8.ValidString(got) {
		t.Fatal("the cut payload is not valid UTF-8")
	}
	if len(got) > maxAgentPayloadInput+len(payloadTruncatedMarker) {
		t.Fatalf("len = %d, want at most %d", len(got), maxAgentPayloadInput+len(payloadTruncatedMarker))
	}
}

func TestDeclaredVariableNamesReadsTheStoredRows(t *testing.T) {
	got := declaredVariableNames([]byte(`[{"name":"topic","value":"x"},{"name":""},{"value":"y"},{"name":"tone"}]`))
	if strings.Join(got, ",") != "topic,tone" {
		t.Fatalf("declared = %v, want [topic tone]", got)
	}
	for _, raw := range []string{`{}`, `null`, `"x"`, ``} {
		if got := declaredVariableNames([]byte(raw)); len(got) != 0 {
			t.Fatalf("declaredVariableNames(%q) = %v, want none", raw, got)
		}
	}
}

func TestAgentVariablesRevalueOnlyDeclaredNames(t *testing.T) {
	supplied := map[string]json.RawMessage{
		"topic":    json.RawMessage(`"release notes"`),
		"count":    json.RawMessage(`3`),
		"strict":   json.RawMessage(`true`),
		"nested":   json.RawMessage(`{"a":1}`),
		"list":     json.RawMessage(`[1]`),
		"nothing":  json.RawMessage(`null`),
		"undeclar": json.RawMessage(`"never declared"`),
		"long":     json.RawMessage(`"` + strings.Repeat("x", maxAgentVariableValue+1) + `"`),
	}
	declared := []string{"strict", "topic", "count", "nested", "list", "nothing", "long", "absent"}
	got := agentVariables(declared, supplied)
	want := []agentVariable{
		{Name: "strict", Value: "true"},
		{Name: "topic", Value: "release notes"},
		{Name: "count", Value: "3"},
	}
	if len(got) != len(want) {
		t.Fatalf("variables = %+v, want %+v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("variables[%d] = %+v, want %+v", index, got[index], want[index])
		}
	}
	if agentVariables(nil, supplied) != nil {
		t.Fatal("a version that declares nothing takes no variables")
	}
}

func TestBodyVariablesIgnoresAShapeThatIsNotAnObject(t *testing.T) {
	body := decodeInboundBody([]byte(`{"input":"go","variables":["not","an","object"]}`))
	if body.Input != "go" {
		t.Fatalf("input = %q — a foreign `variables` shape must not cost the body its input", body.Input)
	}
	if bodyVariables(body) != nil {
		t.Fatal("an array is not a variables object")
	}
	body = decodeInboundBody([]byte(`{"variables":{"topic":"x"}}`))
	if string(bodyVariables(body)["topic"]) != `"x"` {
		t.Fatalf("variables = %v", bodyVariables(body))
	}
}
