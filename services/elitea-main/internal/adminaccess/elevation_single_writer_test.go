package adminaccess_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// elevationWrite matches a statement that changes elitea_auth.admin_elevations,
// also when the verb and the table are on different lines.
var elevationWrite = regexp.MustCompile(`(?is)\b(insert\s+into|delete\s+from|update|truncate)\s+(table\s+)?(only\s+)?(elitea_auth\s*\.\s*)?"?admin_elevations\b`)

// ledgerCall matches a call of a ledger writer (not its declaration).
var ledgerCall = regexp.MustCompile(`\b(StartElevation|EndElevation)\s*\(`)

var blockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)

// withoutComments drops block comments and whole-line `//` and `--` comments:
// prose that names a statement is not a writer.
func withoutComments(body string) string {
	body = blockComment.ReplaceAllString(body, "")
	var kept []string
	for line := range strings.SplitSeq(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "--") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// runtimeSources walks root and calls visit with the comment-free body of
// every runtime source: non-test Go and Rust files, and sqlc queries.
// Migrations, schema projections, test data, dependencies and build output
// are not runtime writers.
func runtimeSources(t *testing.T, root string, visit func(path, body string)) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case "migrations", "schema", "testdata", "node_modules", "target", "vendor", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		name := entry.Name()
		isGo := strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
		isRust := strings.HasSuffix(name, ".rs") && !strings.HasSuffix(name, "_tests.rs") &&
			!strings.Contains(path, string(filepath.Separator)+"tests"+string(filepath.Separator))
		isQuery := strings.HasSuffix(name, ".sql") && strings.Contains(path, string(filepath.Separator)+"queries"+string(filepath.Separator))
		if !isGo && !isRust && !isQuery {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		visit(path, withoutComments(string(body)))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestThisPackageIsTheOnlyWriterOfElevations keeps the ledger in one place
// (ADR-0059 R2.1a, issue 2515): every write goes through the ledger functions
// of this package, which take the security audit appender the elevation's
// event is written with (R2.3). A write anywhere else in the services (Go,
// Rust or a sqlc query) would change an elevation with no event.
func TestThisPackageIsTheOnlyWriterOfElevations(t *testing.T) {
	services, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	self, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	runtimeSources(t, services, func(path, body string) {
		if filepath.Dir(path) == self {
			return
		}
		if elevationWrite.MatchString(body) {
			relative, _ := filepath.Rel(services, path)
			offenders = append(offenders, relative)
		}
	})
	if len(offenders) != 0 {
		t.Fatalf("elitea_auth.admin_elevations is written outside internal/adminaccess; "+
			"call StartElevation or EndElevation instead: %v", offenders)
	}
}

// TestNoProductionCallerBeforeTheAuditEvent: in this build the ledger
// writers take a security audit appender and write no event through it yet.
// R2.3 (issue 2517) adds the `admin.elevation` event on the same
// transaction; until then a production caller would start or end an
// elevation with no evidence. R2.3 deletes this test together with the gap.
func TestNoProductionCallerBeforeTheAuditEvent(t *testing.T) {
	module, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := filepath.Abs("elevation.go")
	if err != nil {
		t.Fatal(err)
	}
	var callers []string
	runtimeSources(t, module, func(path, body string) {
		if path == ledger || !strings.HasSuffix(path, ".go") {
			return
		}
		if ledgerCall.MatchString(body) {
			relative, _ := filepath.Rel(module, path)
			callers = append(callers, relative)
		}
	})
	if len(callers) != 0 {
		t.Fatalf("StartElevation or EndElevation is called by production code before R2.3 (#2517) "+
			"writes the admin.elevation event through the appender: %v", callers)
	}
}

// The scans see what they guard: the ledger's own statements match, on one
// line and across lines, and a comment does not.
func TestTheElevationScansMatchTheLedger(t *testing.T) {
	body, err := os.ReadFile("elevation.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(elevationWrite.FindAllString(withoutComments(string(body)), -1)); n < 2 {
		t.Fatalf("the scan matches %d statements of elevation.go, want the INSERT and the UPDATE", n)
	}
	for _, sample := range []string{
		"UPDATE\n\telitea_auth.admin_elevations SET x = 1",
		"delete from admin_elevations",
		"TRUNCATE TABLE ONLY elitea_auth.admin_elevations",
	} {
		if !elevationWrite.MatchString(withoutComments(sample)) {
			t.Errorf("the scan misses %q", sample)
		}
	}
	for _, sample := range []string{
		"/* UPDATE elitea_auth.admin_elevations */",
		"// UPDATE elitea_auth.admin_elevations",
		"-- DELETE FROM elitea_auth.admin_elevations",
		"SELECT 1 FROM elitea_auth.admin_elevations",
	} {
		if elevationWrite.MatchString(withoutComments(sample)) {
			t.Errorf("the scan matches %q", sample)
		}
	}
	if !ledgerCall.MatchString("adminaccess.StartElevation(ctx, tx, appender, start)") {
		t.Error("the caller scan misses a call")
	}
}
