package adminaccess_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// elevationWrite matches a statement that changes elitea_auth.admin_elevations.
var elevationWrite = regexp.MustCompile(`(?i)\b(insert\s+into|delete\s+from|update|truncate)\s+(table\s+)?(elitea_auth\.)?admin_elevations\b`)

// TestThisPackageIsTheOnlyWriterOfElevations keeps the ledger in one place
// (ADR-0059 R2.1a, issue 2515): every write goes through the ledger functions
// of this package, which take the security audit appender the elevation's
// event is written with (R2.3). A write anywhere else in elitea-main (Go code
// or a sqlc query) would change an elevation with no event. Tests, migrations
// and the schema projections are not runtime writers.
func TestThisPackageIsTheOnlyWriterOfElevations(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	self, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case "migrations", "schema", "testdata", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Dir(path) == self {
			return nil
		}
		name := entry.Name()
		isGo := strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
		isQuery := strings.HasSuffix(name, ".sql") && strings.Contains(path, string(filepath.Separator)+"queries"+string(filepath.Separator))
		if !isGo && !isQuery {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for line := range strings.SplitSeq(string(body), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "--") {
				continue // prose, not a statement
			}
			if elevationWrite.MatchString(line) {
				relative, _ := filepath.Rel(root, path)
				offenders = append(offenders, relative)
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) != 0 {
		t.Fatalf("elitea_auth.admin_elevations is written outside internal/adminaccess; "+
			"call StartElevation or EndElevation instead: %v", offenders)
	}
}

// The scan sees the writers it guards: the ledger's own statements match.
func TestTheElevationWriterScanMatchesTheLedger(t *testing.T) {
	body, err := os.ReadFile("elevation.go")
	if err != nil {
		t.Fatal(err)
	}
	var writes int
	for line := range strings.SplitSeq(string(body), "\n") {
		if elevationWrite.MatchString(line) {
			writes++
		}
	}
	if writes < 2 {
		t.Fatalf("the scan matches %d statements of elevation.go, want the INSERT and the UPDATE", writes)
	}
}
