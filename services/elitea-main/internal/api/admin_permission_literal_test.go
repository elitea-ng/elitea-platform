package api_test

// An administration-mode permission is named through the registry, never as a
// string literal (ADR-0059 decision 7, story R0.1a).
//
// The permission table of internal/adminaccess says which administration names
// exist and what each one is. That holds only while every check names a
// constant of the table: a literal at a gate is a name the table may not know,
// and a constant declared in another package is the same literal one hop
// away. So the check follows each permission argument to the string literal it
// ends at, and fails unless that literal is in internal/adminaccess.
//
// Two shapes of check resolve the administration mode, and both are read:
//
//   - a route gate (RequireCentralPermissions and every helper that wraps a
//     gate, found the way TestEveryGatedPermissionHasAGrant finds them) whose
//     mode is `administration`;
//   - an in-application check: a function that calls ResolvePermissions with
//     the `administration` mode and compares a granted name with `==` or `!=`
//     (the maintenance bypass, Marketplace moderation, the platform_settings
//     bypass flag).
//
// A permission passed in as a parameter (config_values.go's section gate) is
// not resolvable here; the section list is held to the registry by
// TestEveryConfigSectionGateIsARegistryName in internal/api/v2/admin.
//
// Finding the administration surface by driving the composed router is story
// R0.1b (#2581); this test reads the source.

import (
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const registryPackage = "internal/adminaccess"

// adminModes are the mode values an in-application comparison may name
// without being a permission.
var adminModes = map[string]bool{"": true, "administration": true, "default": true, "developer": true}

type literalReport struct {
	gates      int
	inApp      int
	violations []string
}

func TestNoAdministrationPermissionLiteralOutsideTheRegistry(t *testing.T) {
	t.Parallel()
	root := repoRootFrom(t)
	if _, err := os.Stat(filepath.Join(root, registryPackage)); err != nil {
		t.Fatalf("%s is missing: %v", registryPackage, err)
	}
	report := administrationLiterals(t, root)
	if report.gates == 0 {
		t.Fatal("found no administration-mode gate; the gate discovery is broken")
	}
	if report.inApp == 0 {
		t.Fatal("found no in-application administration check; the ResolvePermissions shape changed")
	}
	t.Logf("administration-mode gate permissions: %d; in-application comparisons: %d",
		report.gates, report.inApp)
	for _, violation := range report.violations {
		t.Error(violation)
	}
}

// TestAdministrationLiteralFixtureFails proves the check fails: a synthetic
// package passes one literal to an administration gate, names one constant of
// its own, and compares one literal in an in-application check. Each is one
// violation; the registry constant beside them is none.
func TestAdministrationLiteralFixtureFails(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	registryDir := filepath.Join(root, registryPackage)
	packageDir := filepath.Join(root, "internal", "synthetic")
	for _, dir := range []string{registryDir, packageDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}
	write(t, filepath.Join(registryDir, "permissions.go"), `package adminaccess

const Registered = "fixture.registered"
`)
	write(t, filepath.Join(packageDir, "routes.go"), `package synthetic

import "`+modulePrefix+registryPackage+`"

const modeAdministration = "administration"

const ownConstant = "fixture.own_constant"

func RequireCentralPermissions(_ any, _ string, _ ...string) func() { return nil }

func Routes() []func() {
	return []func(){
		RequireCentralPermissions(nil, modeAdministration, adminaccess.Registered),
		RequireCentralPermissions(nil, modeAdministration, "fixture.literal"),
		RequireCentralPermissions(nil, modeAdministration, ownConstant),
		RequireCentralPermissions(nil, "default", "fixture.default_mode"),
	}
}

type resolver interface {
	ResolvePermissions(_ any, _ any, _ string, _ string) ([]string, error)
}

func holds(r resolver) bool {
	granted, _ := r.ResolvePermissions(nil, nil, modeAdministration, "")
	for _, name := range granted {
		if name == adminaccess.Registered || name == "fixture.in_app" {
			return true
		}
	}
	return false
}
`)

	report := administrationLiterals(t, root)
	want := []string{"fixture.in_app", "fixture.literal", "fixture.own_constant"}
	var got []string
	for _, violation := range report.violations {
		for _, name := range want {
			if strings.Contains(violation, fmt.Sprintf("%q", name)) {
				got = append(got, name)
			}
		}
	}
	sort.Strings(got)
	if fmt.Sprint(got) != fmt.Sprint(want) || len(report.violations) != len(want) {
		t.Fatalf("violations = %q, want one each for %v", report.violations, want)
	}
	if report.gates != 3 || report.inApp != 2 {
		t.Errorf("checked %d gate permissions and %d comparisons, want 3 and 2", report.gates, report.inApp)
	}
}

// TestAdministrationLiteralFixtureCoversTheOtherShapes proves the check does
// not skip what it cannot read: a gate whose permission or mode does not
// resolve fails, and a literal tested in a switch case, a Contains call or an
// index expression fails like one tested with ==.
func TestAdministrationLiteralFixtureCoversTheOtherShapes(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	packageDir := filepath.Join(root, "internal", "synthetic")
	for _, dir := range []string{filepath.Join(root, registryPackage), packageDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}
	write(t, filepath.Join(packageDir, "routes.go"), `package synthetic

import "slices"

const modeAdministration = "administration"

func RequireCentralPermissions(_ any, _ string, _ ...string) func() { return nil }

func name() string { return "fixture.computed" }

func mode() string { return "administration" }

func Routes() []func() {
	return []func(){
		RequireCentralPermissions(nil, modeAdministration, name()),
		RequireCentralPermissions(nil, mode(), "fixture.unknown_mode"),
	}
}

type resolver interface {
	ResolvePermissions(_ any, _ any, _ string, _ string) ([]string, error)
}

func holds(r resolver, held map[string]bool) bool {
	granted, _ := r.ResolvePermissions(nil, nil, modeAdministration, "")
	for _, name := range granted {
		switch name {
		case "fixture.case":
			return true
		}
	}
	return slices.Contains(granted, "fixture.contains") || held["fixture.index"]
}
`)

	report := administrationLiterals(t, root)
	wants := []string{
		"cannot resolve the permission argument",
		"cannot resolve the permission mode",
		`"fixture.case"`,
		`"fixture.contains"`,
		`"fixture.index"`,
	}
	if len(report.violations) != len(wants) {
		t.Fatalf("violations = %q, want %d", report.violations, len(wants))
	}
	for _, want := range wants {
		found := false
		for _, violation := range report.violations {
			found = found || strings.Contains(violation, want)
		}
		if !found {
			t.Errorf("no violation mentions %s; violations = %q", want, report.violations)
		}
	}
}

func administrationLiterals(t *testing.T, root string) literalReport {
	t.Helper()
	index := &sourceIndex{
		fileSet:     token.NewFileSet(),
		root:        root,
		constants:   map[string]map[string]constantDefinition{},
		files:       map[string][]*ast.File{},
		imports:     map[*ast.File]map[string]string{},
		helperCache: map[string]map[string]helperSignature{},
	}
	if err := index.load(filepath.Join(root, "internal")); err != nil {
		t.Fatalf("parse the service source: %v", err)
	}
	registryDir := filepath.Join(root, registryPackage)

	var report literalReport
	for dir, files := range index.files {
		if dir == registryDir {
			continue
		}
		for _, file := range files {
			gates, unresolved := index.gatesIn(dir, file)
			// A gate this check cannot read is a gate it cannot hold to the
			// registry, so it fails rather than being skipped.
			for _, site := range unresolved {
				report.violations = append(report.violations, fmt.Sprintf(
					"cannot resolve the permission argument at %s, so this check cannot tell whether it is\n"+
						"    an administration permission outside the registry. Pass a constant of %s.",
					site, registryPackage))
			}
			for _, g := range gates {
				if g.mode == "" && g.takesMode {
					report.violations = append(report.violations, fmt.Sprintf(
						"%s:%d: cannot resolve the permission mode of the gate %s, so this check cannot tell\n"+
							"    whether it resolves the administration mode. Pass a mode constant.",
						relativeTo(root, g.position), g.position.Line, g.helper))
					continue
				}
				if g.mode != "administration" {
					continue
				}
				for i, permission := range g.permissions {
					report.gates++
					if g.origins[i] != registryDir {
						report.violations = append(report.violations, fmt.Sprintf(
							"%s:%d passes %q to the administration-mode gate %s as a string defined in %s.\n"+
								"    Name it through a constant of %s, and add a row to its permission table\n"+
								"    if the name is new.",
							relativeTo(root, g.position), g.position.Line, permission, g.helper,
							relativeDir(root, g.origins[i]), registryPackage))
					}
				}
			}
			index.inAppComparisons(dir, file, registryDir, &report)
		}
	}
	sort.Strings(report.violations)
	return report
}

// inAppComparisons reads every function of file that resolves the
// administration mode and records each resolvable string that is not a mode
// name and that the function tests a granted name against: an operand of `==`
// or `!=`, a `case` of a switch, an argument of a `Contains` or `Index` call
// (slices.Contains(resolution.Permissions, ...)) and the key of an index
// expression (held[...]).
//
// Its limit: it reads the function that calls ResolvePermissions. A function
// that receives the resolved list and tests it elsewhere is not read; such a
// test must take its name from a parameter or a registry constant, which code
// review holds. R0.1b (#2581) drives the composed router instead.
func (index *sourceIndex) inAppComparisons(dir string, file *ast.File, registryDir string, report *literalReport) {
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil || !index.resolvesAdministration(function.Body, file, dir) {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			for _, operand := range testedOperands(node) {
				value, origin, ok := index.resolveStringOrigin(operand, file, dir, 0)
				if !ok || adminModes[value] {
					continue
				}
				report.inApp++
				if origin != registryDir {
					position := index.fileSet.Position(operand.Pos())
					report.violations = append(report.violations, fmt.Sprintf(
						"%s:%d compares a granted administration permission with %q, a string defined in %s.\n"+
							"    Name it through a constant of %s.",
						relativeTo(index.root, position), position.Line, value,
						relativeDir(index.root, origin), registryPackage))
				}
			}
			return true
		})
	}
}

// resolvesAdministration reports whether body calls ResolvePermissions with
// the `administration` mode as its third argument.
func (index *sourceIndex) resolvesAdministration(body *ast.BlockStmt, file *ast.File, dir string) bool {
	found := false
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || found {
			return !found
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "ResolvePermissions" || len(call.Args) < 3 {
			return true
		}
		if mode, ok := index.resolveString(call.Args[2], file, dir, 0); ok && mode == "administration" {
			found = true
		}
		return !found
	})
	return found
}

func relativeDir(root, dir string) string {
	relative, err := filepath.Rel(root, dir)
	if err != nil {
		return dir
	}
	return relative
}

// testedOperands returns the expressions of node that a granted name may be
// tested against.
func testedOperands(node ast.Node) []ast.Expr {
	switch typed := node.(type) {
	case *ast.BinaryExpr:
		if typed.Op == token.EQL || typed.Op == token.NEQ {
			return []ast.Expr{typed.X, typed.Y}
		}
	case *ast.CaseClause:
		return typed.List
	case *ast.IndexExpr:
		return []ast.Expr{typed.Index}
	case *ast.CallExpr:
		var name string
		switch function := typed.Fun.(type) {
		case *ast.SelectorExpr:
			name = function.Sel.Name
		case *ast.Ident:
			name = function.Name
		}
		if name == "Contains" || name == "Index" {
			return typed.Args
		}
	}
	return nil
}
