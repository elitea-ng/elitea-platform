package conversations

import "testing"

func TestParticipantSettingsRejectMalformedIdentityAndReasoningConflict(t *testing.T) {
	for _, value := range []any{true, 1.5, "abc", -1, 0} {
		if normalizeParticipantSettings(map[string]any{"version_id": value}) == nil {
			t.Fatalf("accepted version_id=%v", value)
		}
	}
	for _, raw := range []any{"not an object", map[string]any{"reasoning_effort": "none"}, map[string]any{"model_name": 9}, map[string]any{"temperature": 0.2, "reasoning_effort": "high"}} {
		if _, err := normalizeParticipantLLM(raw, false); err == nil {
			t.Fatalf("accepted settings %+v", raw)
		}
	}
	for _, p := range []Participant{{EntityName: "user", EntityMeta: map[string]any{}}, {EntityName: "llm", EntityMeta: map[string]any{}}, {EntityName: "application", EntityMeta: map[string]any{"id": 2}}, {EntityName: "unknown", EntityMeta: map[string]any{}}} {
		if p.Validate() == nil {
			t.Fatalf("accepted participant %+v", p)
		}
	}
}
