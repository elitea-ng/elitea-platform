package rolegrants

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// assignmentWrite matches a statement that changes public.auth_core__user_role.
var assignmentWrite = regexp.MustCompile(`(?i)\b(insert\s+into|delete\s+from|update)\s+(public\.)?auth_core__user_role\b`)

// TestThisPackageIsTheOnlyWriterOfCentralAssignments keeps the history whole:
// a central role assignment written anywhere else in elitea-main (Go code or a
// sqlc query) would change a role and leave no admin_role_grants row. Tests,
// migrations and the schema projections are not runtime writers.
func TestThisPackageIsTheOnlyWriterOfCentralAssignments(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
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
			if path == self {
				return filepath.SkipDir
			}
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
			if assignmentWrite.MatchString(line) {
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
		t.Fatalf("auth_core__user_role is written outside internal/adminaccess/rolegrants, "+
			"so those changes leave no history; call rolegrants.Grant, Revoke or Set instead: %v", offenders)
	}
}
