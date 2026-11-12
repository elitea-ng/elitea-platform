package applications

import "testing"

// Get embeds the DEFAULT version's details, not versions[0]'s (issue 1568).
func TestDetailVersionID(t *testing.T) {
	versions := []map[string]any{
		{"id": "13", "name": "v3", "is_default": false},
		{"id": "12", "name": "v2", "is_default": true},
		{"id": "11", "name": "base", "is_default": false},
	}
	if got := detailVersionID(versions); got != "12" {
		t.Fatalf("the flagged default must win, got %q", got)
	}
	versions[1]["is_default"] = false
	if got := detailVersionID(versions); got != "11" {
		t.Fatalf("with no recorded default, base wins, got %q", got)
	}
	if got := detailVersionID(versions[:2]); got != "13" {
		t.Fatalf("with neither, the first listed, got %q", got)
	}
	if got := detailVersionID(nil); got != "" {
		t.Fatalf("an empty list has none, got %q", got)
	}
}
