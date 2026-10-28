package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/elitea-ng/elitea-platform/conformance/governed/canary"
)

// TestSelfTestDetectsThePlantedLeak runs the command's own negative self-test,
// the one CI runs before the real scan.
func TestSelfTestDetectsThePlantedLeak(t *testing.T) {
	if code := run([]string{"-selftest"}); code != 0 {
		t.Fatalf("-selftest exited %d: the scanner did not fail on a planted leak", code)
	}
}

func evidence(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c, err := canary.New("cmd")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal([]canary.Canary{c})
	list := filepath.Join(dir, "canaries.json")
	if err := os.WriteFile(list, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, list
}

func TestCleanEvidencePasses(t *testing.T) {
	dir, list := evidence(t, map[string]string{"logs.txt": "nothing\n"})
	if code := run([]string{"-canaries", list, "-dir", dir, "-require", "logs.txt"}); code != 0 {
		t.Fatalf("clean evidence exited %d", code)
	}
}

func TestMissingRequiredEvidenceIsNotClean(t *testing.T) {
	dir, list := evidence(t, map[string]string{"logs.txt": "nothing\n", "empty.bin": ""})
	for _, required := range []string{"nats-tap.bin", "empty.bin"} {
		if code := run([]string{"-canaries", list, "-dir", dir, "-require", required}); code != 2 {
			t.Fatalf("required %s: exit %d, want 2", required, code)
		}
	}
}

func TestOnlyTheCanaryListIsNotEvidence(t *testing.T) {
	// The list itself holds every value; it is skipped, so a directory with
	// nothing else in it read zero evidence and must not pass.
	dir, list := evidence(t, nil)
	if code := run([]string{"-canaries", list, "-dir", dir}); code != 2 {
		t.Fatalf("a directory holding only the canary list exited %d, want 2", code)
	}
}
