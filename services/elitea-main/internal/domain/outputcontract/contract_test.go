package outputcontract

import (
	"errors"
	"strings"
	"testing"
)

func reason(t *testing.T, err error) string {
	t.Helper()
	var refusal *Error
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want *Error", err)
	}
	return refusal.Reason
}

func TestCheckAcceptsTheThreeTypes(t *testing.T) {
	t.Parallel()
	for _, contract := range []any{
		nil,
		map[string]any{"type": "json_schema", "schema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"confidence": map[string]any{"enum": []any{"low", "medium", "high"}}},
			"required":   []any{"confidence"},
			"$defs":      map[string]any{"a": map[string]any{"type": "string"}},
		}},
		map[string]any{"type": "json_schema", "schema": map[string]any{"$ref": "#/$defs/a", "$defs": map[string]any{"a": map[string]any{"type": "string"}}}},
		map[string]any{"type": "regex", "pattern": `[A-Z]{3}-\d+`},
		map[string]any{"type": "format", "format": "json"},
		map[string]any{"type": "format", "format": "markdown"},
		map[string]any{"type": "format", "format": "csv"},
	} {
		if err := Check(contract); err != nil {
			t.Fatalf("Check(%v) = %v", contract, err)
		}
	}
}

func TestCheckRefusesWithAReason(t *testing.T) {
	t.Parallel()
	deep := map[string]any{"type": "string"}
	for range MaxSchemaDepth {
		deep = map[string]any{"type": "array", "items": deep}
	}
	for _, tc := range []struct {
		contract any
		reason   string
	}{
		{"json", ReasonShape},
		{map[string]any{"type": "xml"}, ReasonShape},
		{map[string]any{"type": "regex", "pattern": "a", "format": "csv"}, ReasonShape},
		{map[string]any{"type": "regex"}, ReasonShape},
		{map[string]any{"type": "json_schema", "schema": "object"}, ReasonShape},
		{map[string]any{"type": "json_schema", "schema": map[string]any{"type": 7}}, ReasonSchemaInvalid},
		{map[string]any{"type": "json_schema", "schema": map[string]any{"$ref": "https://example.com/s.json"}}, ReasonSchemaInvalid},
		{map[string]any{"type": "json_schema", "schema": deep}, ReasonSchemaTooLarge},
		{map[string]any{"type": "json_schema", "schema": map[string]any{"description": strings.Repeat("x", MaxSchemaBytes)}}, ReasonSchemaTooLarge},
		{map[string]any{"type": "regex", "pattern": "(a"}, ReasonPatternInvalid},
		{map[string]any{"type": "regex", "pattern": `(a)\1`}, ReasonPatternInvalid},
		{map[string]any{"type": "regex", "pattern": "(?=a)"}, ReasonPatternInvalid},
		{map[string]any{"type": "regex", "pattern": ""}, ReasonPatternInvalid},
		{map[string]any{"type": "regex", "pattern": strings.Repeat("a", MaxPatternBytes+1)}, ReasonPatternTooLong},
		{map[string]any{"type": "format", "format": "xml"}, ReasonFormatUnknown},
	} {
		if got := reason(t, Check(tc.contract)); got != tc.reason {
			t.Fatalf("Check(%v) reason = %s, want %s", tc.contract, got, tc.reason)
		}
	}
}

func TestCheckPipelineNamesTheNode(t *testing.T) {
	t.Parallel()
	valid := `
nodes:
  - id: Extract
    type: llm
    output_contract:
      type: json_schema
      schema:
        type: object
        required: [answer]
  - id: Code
    type: llm
    output_contract: {type: regex, pattern: "[0-9]+"}
`
	if err := CheckPipeline(valid); err != nil {
		t.Fatalf("CheckPipeline(valid) = %v", err)
	}
	if !HasContract(nil, valid) || HasContract(nil, "nodes: []") || !HasContract(map[string]any{MetaKey: map[string]any{}}, "") {
		t.Fatal("HasContract did not see the contracts")
	}
	invalid := "nodes:\n  - id: Extract\n    type: llm\n    output_contract: {type: regex, pattern: \"(a\"}\n"
	err := CheckPipeline(invalid)
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Reason != ReasonPatternInvalid || refusal.NodeID != "Extract" || !strings.Contains(err.Error(), `"Extract"`) {
		t.Fatalf("CheckPipeline(invalid) = %v", err)
	}
	structured := "nodes:\n  - id: S\n    type: llm\n    structured_output: true\n    output_contract: {type: format, format: json}\n"
	if got := reason(t, CheckPipeline(structured)); got != ReasonStructuredOutput {
		t.Fatalf("structured reason = %s", got)
	}
	// A document that does not parse is left to the worker compiler.
	if err := CheckPipeline("output_contract: [unclosed"); err != nil {
		t.Fatalf("CheckPipeline(unparsable) = %v", err)
	}
}
