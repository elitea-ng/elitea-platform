package guardrail

import (
	"encoding/json"
	"os"
	"testing"
)

type rowCase struct {
	Name  string         `json:"name"`
	Valid bool           `json:"valid"`
	Data  map[string]any `json:"data"`
}

// TestRowCases runs ParseRow over testdata/rows.json. elitea-main runs its
// write-side check over the same file (governance_guardrail_test.go).
func TestRowCases(t *testing.T) {
	raw, err := os.ReadFile("testdata/rows.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []rowCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) < 10 {
		t.Fatalf("only %d cases", len(doc.Cases))
	}
	for _, c := range doc.Cases {
		_, err := ParseRow("r", c.Data)
		if c.Valid && err != nil {
			t.Errorf("%s: ParseRow refused a valid row: %v", c.Name, err)
		}
		if !c.Valid && err == nil {
			t.Errorf("%s: ParseRow accepted an invalid row", c.Name)
		}
	}
}
