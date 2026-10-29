package toolkits

import (
	"strings"
	"testing"
)

func TestEvidenceFieldsProblems(t *testing.T) {
	valid := []map[string]any{
		{},
		{"evidence_fields": nil},
		{"evidence_fields": []any{}},
		{"evidence_fields": []any{"status", "id"}},
	}
	for _, settings := range valid {
		if problems := evidenceFieldsProblems(settings); len(problems) != 0 {
			t.Fatalf("%v refused: %v", settings, problems)
		}
	}
	tooMany := make([]any, 65)
	for i := range tooMany {
		tooMany[i] = "f"
	}
	invalid := []map[string]any{
		{"evidence_fields": "status"},
		{"evidence_fields": []any{"status", 3}},
		{"evidence_fields": []any{""}},
		{"evidence_fields": []any{strings.Repeat("x", 129)}},
		{"evidence_fields": []any{"a\nb"}},
		{"evidence_fields": tooMany},
	}
	for _, settings := range invalid {
		problems := evidenceFieldsProblems(settings)
		if len(problems) != 1 || problems[0]["code"] != "invalid_evidence_fields" {
			t.Fatalf("%v: problems = %v", settings, problems)
		}
	}
}
