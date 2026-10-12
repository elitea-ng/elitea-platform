package indexcatalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The one entry rule, as the Rust worker's IndexCatalog::parse tests it too:
// both suites read the same fixture, so a case added on one side is checked
// on the other (review round 2).
func TestValidEntryAgreesWithTheWorkerOnEveryFixtureCase(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", "..", "..", "..", "libs", "rust", "agent-runtime", "src", "toolkits", "families", "index_catalog_rule_cases.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the shared rule fixture must be readable: %v", err)
	}
	var fixture struct {
		MaxEntries int `json:"max_entries"`
		Cases      []struct {
			Note      string `json:"note"`
			Name      string `json:"name"`
			IndexID   string `json:"index_id"`
			Model     string `json:"embedding_model"`
			Dimension int64  `json:"embedding_dimension"`
			Valid     bool   `json:"valid"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.MaxEntries != MaxEntries {
		t.Fatalf("fixture max_entries = %d, this side = %d", fixture.MaxEntries, MaxEntries)
	}
	if len(fixture.Cases) < 20 {
		t.Fatalf("the fixture lost its cases: %d", len(fixture.Cases))
	}
	for _, c := range fixture.Cases {
		if got := ValidEntry(c.Name, c.IndexID, c.Model, c.Dimension); got != c.Valid {
			t.Errorf("%s: ValidEntry = %v, want %v", c.Note, got, c.Valid)
		}
	}
}

func TestFamilyAndToolNames(t *testing.T) {
	t.Parallel()
	for _, family := range Families {
		if !Family(family) {
			t.Fatalf("%s is a family", family)
		}
	}
	if Family("figma") || Family("") {
		t.Fatal("figma is an I3 family and the empty type is none")
	}
	if len(ToolNames) != 5 {
		t.Fatalf("five index tools are served, not %d", len(ToolNames))
	}
}
