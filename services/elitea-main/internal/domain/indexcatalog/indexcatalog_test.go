package indexcatalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
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

// The served-family list has one source of truth, read by the Rust runtime
// and worker tests too: a family added to one list and not the others fails
// here or there (review round 1).
func TestFamiliesAgreeWithTheSharedServedFamiliesFixture(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", "..", "..", "..", "libs", "rust", "agent-runtime", "src", "toolkits", "families", "index_served_families.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the shared families fixture must be readable: %v", err)
	}
	var fixture struct {
		Families []string `json:"families"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	got := append([]string(nil), Families...)
	sort.Strings(got)
	if !slices.Equal(got, fixture.Families) {
		t.Fatalf("Families = %v, fixture = %v", got, fixture.Families)
	}
}
