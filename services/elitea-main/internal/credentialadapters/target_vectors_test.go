package credentialadapters

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Main's canonical target equals the worker's for every shared vector: the
// worker compares the envelope's target_identity by exact string.
func TestCanonicalBaseURLMatchesTheSharedVectors(t *testing.T) {
	data, err := os.ReadFile(filepath.FromSlash("../../../../testdata/proto/runtime/v1/governed-operations/targets.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Vectors []struct {
			Input     string `json:"input"`
			Canonical string `json:"canonical"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(data, &file); err != nil || len(file.Vectors) == 0 {
		t.Fatalf("vectors: %v", err)
	}
	for _, v := range file.Vectors {
		got, err := CanonicalBaseURL(v.Input)
		if err != nil || got != v.Canonical {
			t.Errorf("CanonicalBaseURL(%q) = %q, %v; want %q", v.Input, got, err, v.Canonical)
		}
	}
}
