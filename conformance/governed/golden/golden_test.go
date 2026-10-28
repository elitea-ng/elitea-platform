// Package golden pins the flag-off golden command vectors (GOV-001, issue
// #1124 item 2.1).
//
// testdata/proto/runtime/v1 holds the byte-exact corpus that Main's and the
// worker's own suites encode against: command envelopes, input bundles,
// expected outputs, policies and parity corpora. With the governance flag off
// those bytes must stay exactly what they were before the governed profile
// existed. This test holds the SHA-256 of every legacy vector in
// ../testdata/golden-vectors.sha256, so a governed change that re-blesses a
// legacy vector fails here even when the suite that reads it was updated in
// the same change.
//
// The two governed corpora (governed-operations/, security-context/) are
// additions of the governed profile and are not flag-off vectors; they are
// excluded by name, and a NEW top-level directory fails until it is classified.
//
// Run without the stack: `go test ./golden/`. Regenerate deliberately, and say
// why in the change, with `go test ./golden/ -run TestGOV001 -update`.
package golden

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden-vector manifest from the current corpus")

const (
	corpus   = "../../../testdata/proto/runtime/v1"
	manifest = "../testdata/golden-vectors.sha256"
)

// governedCorpora are the corpus directories the governed profile added.
var governedCorpora = map[string]bool{"governed-operations": true, "security-context": true}

// legacyCorpora are the flag-off corpus entries at the top level. A new entry
// must be classified here or in governedCorpora.
var legacyCorpora = map[string]bool{
	"code_preparation_failure_policies.json": true,
	"code_runtime_lost_retry_policy.json":    true,
	"configuration-validation":               true,
	"model_failure_policies.json":            true,
	"node-event":                             true,
	"toolkit-available-tools":                true,
}

func TestGOV001GoldenCommandVectorsAreByteIdentical(t *testing.T) {
	entries, err := os.ReadDir(corpus)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !legacyCorpora[entry.Name()] && !governedCorpora[entry.Name()] {
			t.Errorf("%s is new in %s: classify it as a flag-off vector (legacyCorpora) or a governed addition (governedCorpora)",
				entry.Name(), corpus)
		}
	}
	current := map[string]string{}
	err = filepath.WalkDir(corpus, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(corpus, path)
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if governedCorpora[rel] {
				return filepath.SkipDir
			}
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		current[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(current) == 0 {
		t.Fatal("no flag-off vector was read: the corpus path is wrong, and an empty comparison would pass")
	}
	if *update {
		writeManifest(t, current)
		return
	}
	pinned := readManifest(t)
	for path, sum := range pinned {
		got, ok := current[path]
		switch {
		case !ok:
			t.Errorf("%s: a flag-off golden vector was removed", path)
		case got != sum:
			t.Errorf("%s: a flag-off golden vector changed (sha256 %s, pinned %s)", path, got, sum)
		}
	}
	for path := range current {
		if _, ok := pinned[path]; !ok {
			t.Errorf("%s: a new flag-off golden vector is not pinned; run with -update and say why", path)
		}
	}
}

func readManifest(t *testing.T) map[string]string {
	t.Helper()
	file, err := os.Open(manifest)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	out := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		sum, path, ok := strings.Cut(line, "  ")
		if !ok || len(sum) != 64 {
			t.Fatalf("malformed manifest line %q", line)
		}
		out[path] = sum
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("the golden-vector manifest is empty")
	}
	return out
}

func writeManifest(t *testing.T, current map[string]string) {
	t.Helper()
	paths := make([]string, 0, len(current))
	for path := range current {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var b strings.Builder
	b.WriteString("# SHA-256 of every flag-off golden vector under testdata/proto/runtime/v1\n")
	b.WriteString("# (GOV-001, conformance/governed/golden). Regenerate deliberately:\n")
	b.WriteString("#   go test ./golden/ -run TestGOV001 -update\n")
	for _, path := range paths {
		fmt.Fprintf(&b, "%s  %s\n", current[path], path)
	}
	if err := os.WriteFile(manifest, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}
