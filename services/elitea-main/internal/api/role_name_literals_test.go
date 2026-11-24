package api_test

// Every check by role NAME is reviewed (ADR-0059 decision 7, "Checks by role
// name", story R0.13).
//
// Some code tests a role name and not a permission. With several roles per
// person, custom roles and elevations, a name check either misses a narrow
// role or admits something it should not, so each site carries the decision
// the ADR made for it, in testdata/role_name_literals.reviewed. This test
// lists every site and fails on a site the file does not list, and on a
// listed site that is gone (a stale row would let the next site of the same
// name through unreviewed).
//
// # What is a site
//
// The two names are `super_admin` and `admin`. A site is, in a non-test Go
// file of this service or a SQL query file:
//
//   - a Go string literal whose value is one of the names (a comparison, a
//     list of names, a constant);
//   - a Go string literal that carries one of them as a SQL literal
//     ('super_admin', 'admin'): SQL decides by name as surely as Go does;
//   - a comparison (==, !=, a switch case, a Contains or Index argument) with
//     a constant whose value is one of the names, which the first two shapes
//     would not see;
//   - a SQL literal in a .sql file (internal/db/queries), outside a comment.
//
// Every directory named migrations is excluded by path (migrations/ and the
// bootstrap schema internal/infra/db/migrations/001_initial.sql): an applied
// migration is immutable, so a name it seeds cannot be "reviewed away"; the
// roles it seeds are the roles the reviewed sites test. testdata/ is excluded
// too. A comment is never a site.
//
// The web client has the same test: apps/elitea-web/src/pages/admin/
// roleNameLiterals.test.ts.

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const reviewedRoleNameSites = "testdata/role_name_literals.reviewed"

// roleNames are the names whose checks are reviewed.
var roleNames = map[string]bool{"super_admin": true, "admin": true}

// sqlRoleName finds a role name written as a SQL string literal.
var sqlRoleName = regexp.MustCompile(`'(super_admin|admin)'`)

// The decisions a reviewed row may carry.
var roleNameDecisions = map[string]bool{
	// kept: the check tests the name on purpose (ADR-0059 decision 7).
	"kept": true,
	// compat: kept for one release beside the role set (R0.8b), removed
	// with the field it serves (R0.8c, issue 2586).
	"compat": true,
	// seed: the name is data (a built-in role, a default) and no check
	// decides access by it here.
	"seed": true,
	// project: the name of a PROJECT role (auth_core__project_role), the
	// owner role of a project; ADR-0059 does not change the project roles.
	"project": true,
	// unrelated: the string is not an administration role name.
	"unrelated": true,
}

type roleNameSite struct {
	file  string
	scope string
	form  string
}

func (s roleNameSite) String() string { return s.file + " | " + s.scope + " | " + s.form }

func TestEveryRoleNameCheckIsReviewed(t *testing.T) {
	t.Parallel()
	root := repoRootFrom(t)
	found := roleNameSites(t, root)
	if len(found) == 0 {
		t.Fatal("found no role name site; the scan is broken")
	}
	reviewed := readReviewedRoleNameSites(t, reviewedRoleNameSites)
	for _, problem := range compareRoleNameSites(found, reviewed) {
		t.Error(problem)
	}
}

// TestRoleNameScanFixture proves the scan sees each shape and skips what it
// must: a literal, a SQL literal, a comparison with a constant, a .sql query;
// not a comment, a test file or a migration.
func TestRoleNameScanFixture(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, dir := range []string{"internal/synthetic", "internal/db/queries", "migrations"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(root, "internal/synthetic/roles.go"), `package synthetic

// "super_admin" in a comment is no site.
const Owner = "super_admin"

func isOwner(role string) bool { return role == Owner }

func isAdmin(role string) bool {
	switch role {
	case "admin":
		return true
	}
	return false
}

const query = `+"`"+`SELECT 1 WHERE name = 'super_admin'`+"`"+`
`)
	write(t, filepath.Join(root, "internal/synthetic/roles_test.go"), `package synthetic

var _ = "super_admin"
`)
	write(t, filepath.Join(root, "migrations/0001.sql"), "SELECT 'super_admin';\n")
	write(t, filepath.Join(root, "internal/db/queries/q.sql"), `-- name: First :one
-- a comment naming 'admin' is no site
SELECT 1 WHERE name IN ('admin', 'super_admin');
`)
	got := map[string]int{}
	for site, count := range roleNameSites(t, root) {
		got[site.String()] = count
	}
	want := map[string]int{
		"internal/synthetic/roles.go | const Owner | literal:super_admin": 1,
		"internal/synthetic/roles.go | func isOwner | const:Owner":        1,
		"internal/synthetic/roles.go | func isAdmin | literal:admin":      1,
		"internal/synthetic/roles.go | const query | sql:super_admin":     1,
		"internal/db/queries/q.sql | query First | sql:admin":             1,
		"internal/db/queries/q.sql | query First | sql:super_admin":       1,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("sites = %v\nwant    %v", got, want)
	}

	reviewed := map[roleNameSite]reviewedRoleNameSite{}
	for site := range roleNameSites(t, root) {
		reviewed[site] = reviewedRoleNameSite{count: 1, decision: "kept"}
	}
	delete(reviewed, roleNameSite{"internal/synthetic/roles.go", "func isAdmin", "literal:admin"})
	reviewed[roleNameSite{"internal/synthetic/gone.go", "func gone", "literal:admin"}] =
		reviewedRoleNameSite{count: 1, decision: "kept"}
	problems := compareRoleNameSites(roleNameSites(t, root), reviewed)
	joined := strings.Join(problems, "\n")
	if len(problems) != 2 || !strings.Contains(joined, "func isAdmin") || !strings.Contains(joined, "gone.go") {
		t.Fatalf("problems = %q, want one unreviewed site (isAdmin) and one stale row (gone.go)", problems)
	}
}

type reviewedRoleNameSite struct {
	count    int
	decision string
}

func compareRoleNameSites(found map[roleNameSite]int, reviewed map[roleNameSite]reviewedRoleNameSite) []string {
	var problems []string
	for site, count := range found {
		row, ok := reviewed[site]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("unreviewed role name site: %s (%d). A check by role "+
				"name misses a custom role and admits nothing an elevation gives (ADR-0059 decision 7). "+
				"Test a permission instead, or add a row with the decision to internal/api/%s.",
				site, count, reviewedRoleNameSites))
		case row.count != count:
			problems = append(problems, fmt.Sprintf("%s: %d sites, the reviewed row says %d; review the new "+
				"one and update the count in internal/api/%s", site, count, row.count, reviewedRoleNameSites))
		}
	}
	for site := range reviewed {
		if _, ok := found[site]; !ok {
			problems = append(problems, fmt.Sprintf("stale reviewed row: %s is no longer in the source; "+
				"remove it from internal/api/%s", site, reviewedRoleNameSites))
		}
	}
	sort.Strings(problems)
	return problems
}

// readReviewedRoleNameSites reads `file | scope | form | count | decision |
// why` rows; `#` starts a comment line.
func readReviewedRoleNameSites(t *testing.T, path string) map[roleNameSite]reviewedRoleNameSite {
	t.Helper()
	handle, err := os.Open(path) //nolint:gosec // fixed testdata path
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = handle.Close() }()
	rows := map[roleNameSite]reviewedRoleNameSite{}
	scanner := bufio.NewScanner(handle)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Split(text, " | ")
		if len(fields) != 6 {
			t.Fatalf("%s:%d: want 6 fields separated by \" | \", got %d: %q", path, line, len(fields), text)
		}
		count, err := strconv.Atoi(fields[3])
		if err != nil || count < 1 {
			t.Fatalf("%s:%d: count %q is not a positive number", path, line, fields[3])
		}
		if !roleNameDecisions[fields[4]] {
			t.Fatalf("%s:%d: decision %q is not one of kept, compat, seed, project, unrelated", path, line, fields[4])
		}
		if strings.TrimSpace(fields[5]) == "" {
			t.Fatalf("%s:%d: a reviewed row says why", path, line)
		}
		site := roleNameSite{file: fields[0], scope: fields[1], form: fields[2]}
		if _, duplicate := rows[site]; duplicate {
			t.Fatalf("%s:%d: %s is listed twice", path, line, site)
		}
		rows[site] = reviewedRoleNameSite{count: count, decision: fields[4]}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return rows
}

// roleNameSites scans root (a service root) and counts the sites per
// (file, scope, form).
func roleNameSites(t *testing.T, root string) map[roleNameSite]int {
	t.Helper()
	type parsed struct {
		dir  string
		file *ast.File
		path string
	}
	fileSet := token.NewFileSet()
	var goFiles []parsed
	sites := map[roleNameSite]int{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(root, path)
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			if entry.Name() == "migrations" || entry.Name() == "testdata" || entry.Name() == "node_modules" ||
				(strings.HasPrefix(entry.Name(), ".") && relative != ".") {
				return filepath.SkipDir
			}
			return nil
		}
		switch {
		case strings.HasSuffix(path, "_test.go"):
		case strings.HasSuffix(path, ".go"):
			file, err := parser.ParseFile(fileSet, path, nil, parser.ParseComments|parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			goFiles = append(goFiles, parsed{dir: filepath.Dir(relative), file: file, path: relative})
		case strings.HasSuffix(path, ".sql"):
			sqlFileSites(t, path, relative, sites)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", root, err)
	}

	// Package-level constants whose value is a role name, by package dir.
	constants := map[string]map[string]string{}
	for _, f := range goFiles {
		for name, value := range roleNameConstants(f.file) {
			if constants[f.dir] == nil {
				constants[f.dir] = map[string]string{}
			}
			constants[f.dir][name] = value
		}
	}
	for _, f := range goFiles {
		imports := importedDirs(f.file)
		for _, unit := range scopedUnits(f.file) {
			scope := unit.scope
			ast.Inspect(unit.node, func(node ast.Node) bool {
				if literal, ok := node.(*ast.BasicLit); ok && literal.Kind == token.STRING {
					value, err := strconv.Unquote(literal.Value)
					if err != nil {
						return true
					}
					if roleNames[value] {
						sites[roleNameSite{f.path, scope, "literal:" + value}]++
					}
					for _, name := range sqlNamesIn(value) {
						sites[roleNameSite{f.path, scope, "sql:" + name}]++
					}
					return true
				}
				for _, operand := range testedOperands(node) {
					if name, ok := roleNameConstant(operand, f.dir, imports, constants); ok {
						sites[roleNameSite{f.path, scope, "const:" + name}]++
					}
				}
				return true
			})
		}
	}
	return sites
}

// sqlNamesIn returns each role name s carries as a SQL literal, once per
// occurrence, sorted.
func sqlNamesIn(s string) []string {
	var names []string
	for _, match := range sqlRoleName.FindAllStringSubmatch(s, -1) {
		names = append(names, match[1])
	}
	sort.Strings(names)
	return names
}

// sqlFileSites records the SQL literals of a query file, scoped by the
// `-- name:` of the query they belong to. Comment text is not read.
func sqlFileSites(t *testing.T, path, relative string, sites map[roleNameSite]int) {
	t.Helper()
	body, err := os.ReadFile(path) //nolint:gosec // a file of the scanned tree
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	scope := "file"
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "-- name:") {
			if fields := strings.Fields(strings.TrimPrefix(trimmed, "-- name:")); len(fields) > 0 {
				scope = "query " + fields[0]
			}
			continue
		}
		if comment := strings.Index(line, "--"); comment >= 0 {
			line = line[:comment]
		}
		for _, name := range sqlNamesIn(line) {
			sites[roleNameSite{relative, scope, "sql:" + name}]++
		}
	}
}

type scopedUnit struct {
	scope string
	node  ast.Node
}

// scopedUnits splits file into the units a site is scoped by: each function
// (`func Name`, `func (Type).Name`) and each spec of a declaration
// (`const Name`, `var Name`, `type Name`; the first name of a spec that
// declares several). Imports are no unit.
func scopedUnits(file *ast.File) []scopedUnit {
	var units []scopedUnit
	for _, declaration := range file.Decls {
		switch typed := declaration.(type) {
		case *ast.FuncDecl:
			units = append(units, scopedUnit{scope: functionScope(typed), node: typed})
		case *ast.GenDecl:
			for _, spec := range typed.Specs {
				switch s := spec.(type) {
				case *ast.ValueSpec:
					if len(s.Names) > 0 {
						units = append(units, scopedUnit{scope: typed.Tok.String() + " " + s.Names[0].Name, node: s})
					}
				case *ast.TypeSpec:
					units = append(units, scopedUnit{scope: "type " + s.Name.Name, node: s})
				}
			}
		}
	}
	return units
}

func functionScope(function *ast.FuncDecl) string {
	if function.Recv != nil && len(function.Recv.List) > 0 {
		receiver := function.Recv.List[0].Type
		if star, ok := receiver.(*ast.StarExpr); ok {
			receiver = star.X
		}
		if ident, ok := receiver.(*ast.Ident); ok {
			return "func (" + ident.Name + ")." + function.Name.Name
		}
	}
	return "func " + function.Name.Name
}

// roleNameConstants maps the package-level constants of file whose value is
// a role name.
func roleNameConstants(file *ast.File) map[string]string {
	constants := map[string]string{}
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.CONST {
			continue
		}
		for _, spec := range general.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Names) != len(value.Values) {
				continue
			}
			for i, name := range value.Names {
				literal, ok := value.Values[i].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					continue
				}
				if text, err := strconv.Unquote(literal.Value); err == nil && roleNames[text] {
					constants[name.Name] = text
				}
			}
		}
	}
	return constants
}

// importedDirs maps the names file refers to this module's packages by to
// their directories relative to the service root.
func importedDirs(file *ast.File) map[string]string {
	dirs := map[string]string{}
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || !strings.HasPrefix(path, modulePrefix) {
			continue
		}
		name := path[strings.LastIndex(path, "/")+1:]
		if spec.Name != nil {
			name = spec.Name.Name
		}
		dirs[name] = strings.TrimPrefix(path, modulePrefix)
	}
	return dirs
}

// roleNameConstant resolves operand to a role name constant of this module:
// a bare name of the file's own package, or pkg.Name of an imported one.
func roleNameConstant(
	operand ast.Expr, dir string, imports map[string]string, constants map[string]map[string]string,
) (string, bool) {
	switch typed := operand.(type) {
	case *ast.Ident:
		if _, ok := constants[dir][typed.Name]; ok {
			return typed.Name, true
		}
	case *ast.SelectorExpr:
		x, ok := typed.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		target, ok := imports[x.Name]
		if !ok {
			return "", false
		}
		if _, ok := constants[target][typed.Sel.Name]; ok {
			return x.Name + "." + typed.Sel.Name, true
		}
	}
	return "", false
}
