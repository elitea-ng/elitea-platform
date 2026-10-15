package output

import "testing"

// #1292 round 2: the typed tool refusals are valid terminal summaries with the
// refusal rules of UNKNOWN_TOOL (a sentence, no result).
func TestToolRefusalSummariesValidateAsRefusals(t *testing.T) {
	for _, status := range []ToolkitCallToolStatus{ToolkitCallToolStatusEffectRefused, ToolkitCallToolStatusPolicyRefused} {
		if err := (ToolkitCallToolSummary{Status: status, ErrorMessage: "refused"}).Validate(); err != nil {
			t.Fatalf("%s: %v", status, err)
		}
		if (ToolkitCallToolSummary{Status: status}).Validate() == nil {
			t.Fatalf("%s without a sentence accepted", status)
		}
		if (ToolkitCallToolSummary{Status: status, ErrorMessage: "x", ResultJSON: "{}"}).Validate() == nil {
			t.Fatalf("%s with a result accepted", status)
		}
	}
}
