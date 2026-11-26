package exporttest

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestExportTestImportBoundary is the gate that keeps exporttest a test-support
// package: no production file outside exporttest imports it. The receivers are
// in-process test doubles; a production file that names one is a mistake. The
// scan reads SOURCE rather than building a graph, because the property is "no
// production file names exporttest", which no runtime assertion can observe.
func TestExportTestImportBoundary(t *testing.T) {
	t.Parallel()
	// The service root, found by walking up from this test file.
	serviceRoot := findServiceRoot(t)
	// The test files of this package import exporttest legally; every other
	// file in the service must not.
	module := modulePath(t)
	importPattern := regexp.MustCompile(`"` + regexp.QuoteMeta(module) + `/internal/auditexport/exporttest"`)
	err := filepath.WalkDir(serviceRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if filepath.Base(path) == ".git" || filepath.Base(path) == "node_modules" || filepath.Base(path) == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		content, err := os.ReadFile(path) //nolint:gosec // walk of a fixed, test-local tree
		if err != nil {
			return err
		}
		if importPattern.MatchString(string(content)) {
			rel, _ := filepath.Rel(serviceRoot, path)
			t.Errorf("production file %s imports exporttest; the receivers are test support", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	// The pattern must actually match, so the gate cannot stay green because the
	// module path changed and the regex stopped matching anything.
	testsImported := false
	err = walkGoFiles(serviceRoot, func(path string, content []byte) {
		if strings.HasSuffix(path, "_test.go") && importPattern.MatchString(string(content)) {
			testsImported = true
		}
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if !testsImported {
		t.Error("no _test.go file imports exporttest; the import pattern must match")
	}
}

func modulePath(t *testing.T) string {
	t.Helper()
	serviceRoot := findServiceRoot(t)
	raw, err := os.ReadFile(filepath.Join(serviceRoot, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	t.Fatal("go.mod has no module line")
	return ""
}

func walkGoFiles(root string, visit func(path string, content []byte)) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if filepath.Base(path) == ".git" || filepath.Base(path) == "node_modules" || filepath.Base(path) == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		content, err := os.ReadFile(path) //nolint:gosec // walk of a fixed, test-local tree
		if err != nil {
			return err
		}
		visit(path, content)
		return nil
	})
}

func findServiceRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// internal/auditexport/exporttest -> service root is three levels up.
	return filepath.Clean(filepath.Join(wd, "..", "..", ".."))
}
