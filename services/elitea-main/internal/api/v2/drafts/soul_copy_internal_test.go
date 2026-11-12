package drafts

import "testing"

// proposedSoul injects the schema key into a copy, never the caller's map.
func TestProposedSoulDoesNotMutateTheCallersMap(t *testing.T) {
	soul := map[string]any{"role": "Lead"}
	if normalized, failure := proposedSoul(soul); failure != nil || normalized == nil {
		t.Fatalf("normalized = %s failure = %v", normalized, failure)
	}
	if _, has := soul["schema"]; has || len(soul) != 1 {
		t.Errorf("the caller's map was written to: %v", soul)
	}
}
