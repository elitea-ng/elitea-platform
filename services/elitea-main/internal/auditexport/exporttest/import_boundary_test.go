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
	importPattern := regexp.MustCompile(`"github\.com/elitea-ng/elitea-platform/services/elitea-main/internal/auditexport/exporttest"`)
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
