package guardrail

import (
	"strings"
	"testing"
)

// TestParseRowRefusesTheTopLevelShape pins the one data shape. The admin
// form writes and reads data.guardrail only. A row with stage and detectors
// at the top level of data showed defaults in the form, and a save then
// overwrote the authored values. elitea-main refuses the shape with a 400;
// the gateway refuses a row that reached the table another way.
func TestParseRowRefusesTheTopLevelShape(t *testing.T) {
	_, err := ParseRow("r", map[string]any{
		"stage": "output", "detectors": []any{map[string]any{"type": "secrets", "action": "mask"}},
	})
	if err == nil || !strings.Contains(err.Error(), "data.guardrail") {
		t.Fatalf("err = %v", err)
	}
	if _, err := ParseRow("r", map[string]any{"guardrail": "output"}); err == nil {
		t.Fatal("a non-object guardrail value was accepted")
	}
}
