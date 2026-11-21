package repos

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// legacyToolsSQL matches SQL that names the legacy-only `application_tools`
// relation: a statement keyword, then a (possibly %s- or p_N-qualified)
// relation ending in application_tools.
var legacyToolsSQL = regexp.MustCompile(`(?is)\b(from|join|into|update|delete\s+from)\s+[^\s;]*\.?application_tools\b`)

// legacyToolsProbes are the ways a function proves the table exists before it
// touches it (LegacyApplicationToolsExists and its unexported twin, or the
// same to_regclass probe inline). A query naming the table in a function with
// none of them reaches a relation that exists only in legacy-migrated tenants,
// and on every other tenant fails: the bug class that made application GETs
// log `relation "p_N.application_tools" does not exist` on every call.
var legacyToolsProbes = []string{"LegacyApplicationToolsExists", "legacyApplicationToolsExists", "to_regclass"}

// legacyToolsGuarded are functions that touch the table only when their caller
// has probed it, with the reason. Keep it short: each entry is a place a new
// caller could forget the probe.
var legacyToolsGuarded = map[string]string{}

// TestLegacyApplicationToolsReadsAreProbeGuarded holds every function in this
// module that names `application_tools` in SQL to a probe in the same body.
func TestLegacyApplicationToolsReadsAreProbeGuarded(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(here), "..", "..", "..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found from %s: %v", here, err)
	}
	var offenders []string
	sites := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "vendor" || name == "testdata" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			names := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && legacyToolsSQL.MatchString(lit.Value) {
					names = true
				}
				return !names
			})
			if !names {
				continue
			}
			sites++
			name := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) > 0 {
				name = receiverName(fn.Recv.List[0].Type) + "." + name
			}
			if _, ok := legacyToolsGuarded[name]; ok {
				continue
			}
			body := string(source[fset.Position(fn.Body.Pos()).Offset:fset.Position(fn.Body.End()).Offset])
			probed := false
			for _, probe := range legacyToolsProbes {
				if strings.Contains(body, probe) {
					probed = true
				}
			}
			if !probed {
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders, rel+": "+name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A rule that can match nothing passes by finding nothing: the sites exist.
	if sites == 0 {
		t.Fatal("no function names application_tools in SQL; the matcher is broken")
	}
	sort.Strings(offenders)
	for _, offender := range offenders {
		t.Errorf("%s names application_tools in SQL without probing for it first (LegacyApplicationToolsExists): the table exists only in legacy-migrated tenants", offender)
	}
}

func receiverName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.StarExpr:
		return receiverName(e.X)
	case *ast.Ident:
		return e.Name
	case *ast.IndexExpr:
		return receiverName(e.X)
	}
	return "?"
}
