package conversations

import (
	"encoding/json"
	"testing"
)

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

func TestParticipantLLMAutoOutputPreservesIdentityValidation(t *testing.T) {
	for _, read := range []bool{false, true} {
		for _, value := range []any{-1, int64(-1), float64(-1), "-1", json.Number("-1")} {
			got, err := normalizeParticipantLLM(map[string]any{"max_tokens": value}, read)
			if err != nil || got["max_tokens"] != int64(-1) {
				t.Fatalf("Auto read=%v value=%v: got=%v err=%v", read, value, got, err)
			}
			if _, err := normalizeParticipantLLM(map[string]any{"model_project_id": value}, read); err == nil {
				t.Fatalf("accepted Auto as project identity: %v", value)
			}
		}
		for _, value := range []any{0, -2, -1.5, true, "Auto", json.Number("2147483648")} {
			if _, err := normalizeParticipantLLM(map[string]any{"max_tokens": value}, read); err == nil {
				t.Fatalf("accepted invalid output cap: %v", value)
			}
		}
		for _, value := range []any{8192, "8192", json.Number("8192")} {
			got, err := normalizeParticipantLLM(map[string]any{"max_tokens": value}, read)
			if err != nil || got["max_tokens"] != int64(8192) {
				t.Fatalf("explicit cap: got=%v err=%v", got, err)
			}
		}
	}
}
