package applications

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ADR-0043 decision 2: a version save with an invalid output contract (agent
// meta or a pipeline LLM node) is refused with a typed 422 before anything is
// written.
func TestCheckOutputContractsAnswersATyped422(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		body   map[string]any
		reason string
		node   string
	}{
		{"agent", map[string]any{"meta": map[string]any{"output_contract": map[string]any{"type": "format", "format": "xml"}}}, "format_unknown", ""},
		{"pipeline", map[string]any{"instructions": "nodes:\n  - id: N1\n    type: llm\n    output_contract: {type: regex, pattern: \"(a\"}\n"}, "pattern_invalid", "N1"},
	} {
		recorder := httptest.NewRecorder()
		if checkOutputContracts(recorder, tc.body) {
			t.Fatalf("%s: the invalid contract was accepted", tc.name)
		}
		if recorder.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: status = %d", tc.name, recorder.Code)
		}
		var body outputContractRefusal
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Code != "OUTPUT_CONTRACT_INVALID" || body.Reason != tc.reason || body.NodeID != tc.node || body.Error == "" {
			t.Fatalf("%s: body = %+v", tc.name, body)
		}
	}
	ok := map[string]any{
		"meta":         map[string]any{"output_contract": map[string]any{"type": "format", "format": "csv"}},
		"instructions": "Answer in CSV.",
	}
	if !checkOutputContracts(httptest.NewRecorder(), ok) || !checkOutputContracts(httptest.NewRecorder(), map[string]any{"meta": map[string]any{"output_contract": nil}}) {
		t.Fatal("a valid or a cleared contract was refused")
	}
}
