package configurations

import "testing"

func TestNormalizeAIDisclosureSetting(t *testing.T) {
	t.Parallel()
	normalized, err := normalizeAIDisclosureSetting(map[string]any{
		"marker_text": "  Team bot ", "opt_out_toolkit_types": []any{"Slack", "slack", "jira"}, "extra": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if normalized["marker_text"] != "Team bot" || len(normalized["opt_out_toolkit_types"].([]any)) != 2 || normalized["extra"] != nil {
		t.Fatalf("normalized = %v", normalized)
	}
	for _, data := range []map[string]any{
		nil,
		{"marker_text": 7},
		{"marker_text": "line\nbreak"},
		{"opt_out_toolkit_types": []any{"sharepoint"}},
		{"opt_out_toolkit_types": "slack"},
	} {
		if _, err := normalizeAIDisclosureSetting(data); err == nil {
			t.Errorf("%v: want a refusal", data)
		}
	}
}
