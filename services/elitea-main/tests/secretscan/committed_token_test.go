// Package secretscan holds one gate: no shipped file in the repository may
// carry a literal bearer token.
//
// Scripts, manifests and docs are copied, forked and pasted into terminals; a
// token committed in one of them is usable by anyone who reads the repository,
// and deleting it later does not un-publish it. Credentials reach scripts from
// the environment instead. Test sources are exempt: their tokens are built
// from in-test keys and authenticate nothing outside the test.
package secretscan

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// jwtPattern matches a compact JWS whose header and payload are both base64url
// JSON objects (`eyJ` is `{"`) and whose signature is long enough to be real.
var jwtPattern = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{16,}`)

// maxScannedFileBytes bounds the work per file; nothing hand-written is larger.
const maxScannedFileBytes = 2 << 20

// skippedDirs are build outputs, dependency trees and VCS metadata.
var skippedDirs = map[string]bool{
	".git": true, "node_modules": true, "target": true, "dist": true,
	"build": true, ".venv": true, "venv": true, "__pycache__": true,
	"coverage": true,
}

func isTestSource(rel string) bool {
	base := filepath.Base(rel)
	for _, suffix := range []string{"_test.go", "_tests.rs", "_test.py", ".test.ts", ".test.tsx", ".spec.ts", ".spec.tsx"} {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	if strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py") {
		return true
	}
	slashed := "/" + filepath.ToSlash(rel)
	for _, dir := range []string{"/testdata/", "/tests/", "/__tests__/", "/fixtures/"} {
		if strings.Contains(slashed, dir) {
			return true
		}
	}
	return false
}

// findTokens returns "path:line" for every token-shaped literal under root,
// test sources excepted.
func findTokens(t *testing.T, root string) []string {
	t.Helper()
	var hits []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if skippedDirs[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() || isTestSource(rel) {
			return nil
		}
		info, err := entry.Info()
		if err != nil || info.Size() > maxScannedFileBytes {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 0, 64<<10), maxScannedFileBytes)
		for line := 1; scanner.Scan(); line++ {
			if jwtPattern.Match(scanner.Bytes()) {
				hits = append(hits, filepath.ToSlash(rel)+":"+strconv.Itoa(line))
			}
		}
		// A file with an over-long line is reported, not silently skipped.
		if scanErr := scanner.Err(); scanErr != nil {
			hits = append(hits, filepath.ToSlash(rel)+": unscannable: "+scanErr.Error())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return hits
}

func TestNoShippedFileCarriesABearerToken(t *testing.T) {
	if hits := findTokens(t, repoRoot(t)); len(hits) > 0 {
		t.Fatalf("token-shaped literals in shipped files; read credentials from the environment instead:\n  %s",
			strings.Join(hits, "\n  "))
	}
}

// TestTheScanIsNotVacuous proves the gate fires on a planted token and
// exempts test sources and dependency trees, so a pattern or walk regression
// cannot pass silently.
func TestTheScanIsNotVacuous(t *testing.T) {
	root := t.TempDir()
	token := "eyJhbGciOiJIUzUxMiJ9.eyJ1dWlkIjoiMDAwMCJ9.c2lnbmF0dXJlLXNpZ25hdHVyZS1zaWc"
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("deploy/scripts/run.sh", "#!/bin/sh\nTOKEN=\""+token+"\"\n")
	write("services/x/auth_test.go", "const t = \""+token+"\"\n")
	write("node_modules/pkg/index.js", token)

	hits := findTokens(t, root)
	if len(hits) != 1 || hits[0] != "deploy/scripts/run.sh:2" {
		t.Fatalf("hits = %v, want exactly [deploy/scripts/run.sh:2]", hits)
	}
}

// repoRoot walks up from the test's working directory to the go.work that
// marks the repository root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, statErr := os.Stat(filepath.Join(dir, "go.work")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("found no go.work above %s; this test cannot locate the repository root", dir)
	return ""
}
