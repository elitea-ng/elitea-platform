package api_test

// The legacy names may only shrink (ADR-0059 decision 2, story R0.14a).
//
// runtime.plugins and configuration.governance end on the sunset date, and
// story R4.7 removes every site that names them. Until then no new site may
// name either, or the removal has more to undo every week. This test counts
// the sites and holds the count to legacyGateBaseline, in both directions:
//
//   - a count above the baseline fails: a change added a site. Name the
//     narrow permission of ADR-0059 decision 2 instead.
//   - a count below the baseline fails too: a change removed a site and must
//     lower the baseline in the same commit, so the headroom cannot be spent
//     by the next change.
//
// # What counts
//
// The legacy names are the rows the registry marks Legacy
// (adminaccess.LegacyNames), so the count follows the registry, not a list
// kept here. Every non-test Go file under internal/ counts, except the
// registry package itself (internal/adminaccess declares the names, the
// narrowing rows and the action table; it gates nothing by a legacy name).
// One site is one of:
//
//   - a reference to the registry constant of a legacy name
//     (adminaccess.RuntimePlugins, adminaccess.ConfigurationGovernance): a
//     route gate (central(...)), an in-application check, a section's
//     `required_permission`, a Holds call;
//   - a string literal whose value is a legacy name, so the ratchet cannot
//     be dodged with a literal where the literal test of R0.1a does not
//     look (admin_permission_literal_test.go reads gates and in-application
//     comparisons only). A comment is not a site.
//
// A site that wraps the name in a local constant or variable is counted at
// the one reference to the registry constant; the literal test already
// refuses a gate whose name ends at a string defined outside the registry.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

// legacyGateBaseline is the number of sites that name a legacy permission,
// pinned when R0.14a merged, after R0.3, R0.4a/b/c and R0.5 split their gates
// (each split gate still accepts the legacy name beside the narrow one). It
// may only go down. Lower it in the commit that removes a site.
//
// At R0.14a: 17 route gates in router.go (seven runtime.plugins gates of the
// platform routes, three configuration.governance gates of the governance
// routes, six of the /admin/gateway block and one of the operations status
// read), the kind check of the gateway governance handler, 20
// `required_permission` declarations of Configuration sections, the four
// runtime.plugins checks of the section legacy path, the worker egress
// permission, and the two entitlement classifications.
const legacyGateBaseline = 45

func TestLegacyPermissionSitesOnlyShrink(t *testing.T) {
	t.Parallel()
	root := repoRootFrom(t)
	sites := legacySites(t, root, adminaccess.LegacyNames())
	if len(sites) == 0 {
		t.Fatal("found no site that names a legacy permission; the count is broken")
	}
	if err := ratchet(sites, legacyGateBaseline); err != nil {
		t.Error(err)
	}
}

// TestLegacyRatchetFixture proves the ratchet: a synthetic tree with three
// sites passes at 3, fails when a fixture gate adds a legacy name (by the
// constant or by a literal), passes again when a site is removed and the
// checked-in count is lowered, and fails when the count is lowered below the
// sites or left above them.
func TestLegacyRatchetFixture(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	registryDir := filepath.Join(root, registryPackage)
	gatesDir := filepath.Join(root, "internal", "synthetic")
	for _, dir := range []string{registryDir, gatesDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}
	write(t, filepath.Join(registryDir, "permissions.go"), `package adminaccess

const (
	Legacy = "fixture.legacy"
	Narrow = "fixture.narrow"
)
`)
	imported := `import access "` + modulePrefix + registryPackage + `"`
	write(t, filepath.Join(gatesDir, "routes.go"), `package synthetic

`+imported+`

func central(_ ...string) func() { return nil }

// A comment naming "fixture.legacy" is no site.
var gates = []func(){
	central(access.Narrow, access.Legacy),
	central(access.Legacy),
}

var section = map[string]any{"required_permission": access.Legacy}
`)
	legacy := []string{"fixture.legacy"}
	count := func() []legacySite { return legacySites(t, root, legacy) }

	if err := ratchet(count(), 3); err != nil {
		t.Fatalf("three sites at a baseline of 3: %v", err)
	}

	write(t, filepath.Join(gatesDir, "added.go"), `package synthetic

`+imported+`

var added = central(access.Narrow, access.Legacy)
`)
	if err := ratchet(count(), 3); err == nil || !strings.Contains(err.Error(), "added.go") {
		t.Fatalf("a fixture gate that adds the legacy name passed, or the failure does not name it: %v", err)
	}
	write(t, filepath.Join(gatesDir, "added.go"), `package synthetic

var added = central("fixture.legacy")
`)
	if err := ratchet(count(), 3); err == nil {
		t.Fatal("a fixture gate that adds the legacy name as a literal passed")
	}

	if err := os.Remove(filepath.Join(gatesDir, "added.go")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(gatesDir, "routes.go"), `package synthetic

`+imported+`

func central(_ ...string) func() { return nil }

var gates = []func(){
	central(access.Narrow, access.Legacy),
	central(access.Narrow),
}

var section = map[string]any{"required_permission": access.Legacy}
`)
	if err := ratchet(count(), 2); err != nil {
		t.Fatalf("a site removed and the checked-in count lowered to 2: %v", err)
	}
	if err := ratchet(count(), 3); err == nil || !strings.Contains(err.Error(), "lower legacyGateBaseline") {
		t.Fatalf("a count left above the sites passed: %v", err)
	}
	if err := ratchet(count(), 1); err == nil {
		t.Fatal("a checked-in count below the sites passed")
	}
}

type legacySite struct {
	file string
	line int
	name string
}

func ratchet(sites []legacySite, baseline int) error {
	switch {
	case len(sites) > baseline:
		return fmt.Errorf("%d sites name a legacy permission, more than the baseline of %d. A legacy name "+
			"(runtime.plugins, configuration.governance) ends on the sunset date and no new gate may name it "+
			"(ADR-0059 decision 2): gate on the narrow permission instead.\n%s",
			len(sites), baseline, describeSites(sites))
	case len(sites) < baseline:
		return fmt.Errorf("%d sites name a legacy permission, fewer than the baseline of %d: "+
			"lower legacyGateBaseline to %d in internal/api/legacy_gate_ratchet_test.go.\n%s",
			len(sites), baseline, len(sites), describeSites(sites))
	}
	return nil
}

func describeSites(sites []legacySite) string {
	perFile := map[string][]string{}
	for _, site := range sites {
		perFile[site.file] = append(perFile[site.file], fmt.Sprintf("%d (%s)", site.line, site.name))
	}
	files := make([]string, 0, len(perFile))
	for file := range perFile {
		files = append(files, file)
	}
	sort.Strings(files)
	var b strings.Builder
	for _, file := range files {
		fmt.Fprintf(&b, "    %s: %d: %s\n", file, len(perFile[file]), strings.Join(perFile[file], ", "))
	}
	return b.String()
}

// legacySites reads every non-test Go file under root/internal outside the
// registry package and returns each site that names one of legacy.
func legacySites(t *testing.T, root string, legacy []string) []legacySite {
	t.Helper()
	registryDir := filepath.Join(root, registryPackage)
	constants := legacyConstants(t, registryDir, legacy)
	if len(constants) != len(legacy) {
		t.Fatalf("the registry declares constants %v for the legacy names %v; want one each", constants, legacy)
	}
	isLegacy := map[string]bool{}
	for _, name := range legacy {
		isLegacy[name] = true
	}
	registryImport := modulePrefix + registryPackage

	var sites []legacySite
	fileSet := token.NewFileSet()
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path == registryDir || entry.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		alias := importName(file, registryImport)
		relative, _ := filepath.Rel(root, path)
		ast.Inspect(file, func(node ast.Node) bool {
			var name string
			switch typed := node.(type) {
			case *ast.SelectorExpr:
				x, ok := typed.X.(*ast.Ident)
				if ok && alias != "" && x.Name == alias && constants[typed.Sel.Name] != "" {
					name = constants[typed.Sel.Name]
				}
			case *ast.BasicLit:
				if typed.Kind == token.STRING {
					if value, err := strconv.Unquote(typed.Value); err == nil && isLegacy[value] {
						name = value
					}
				}
			}
			if name != "" {
				sites = append(sites, legacySite{
					file: filepath.ToSlash(relative), line: fileSet.Position(node.Pos()).Line, name: name,
				})
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("read the service source: %v", err)
	}
	return sites
}

// legacyConstants maps each registry constant whose value is a legacy name to
// that name.
func legacyConstants(t *testing.T, registryDir string, legacy []string) map[string]string {
	t.Helper()
	isLegacy := map[string]bool{}
	for _, name := range legacy {
		isLegacy[name] = true
	}
	entries, err := os.ReadDir(registryDir)
	if err != nil {
		t.Fatalf("read %s: %v", registryDir, err)
	}
	constants := map[string]string{}
	fileSet := token.NewFileSet()
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, filepath.Join(registryDir, entry.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
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
				for i, ident := range value.Names {
					literal, ok := value.Values[i].(*ast.BasicLit)
					if !ok || literal.Kind != token.STRING {
						continue
					}
					if text, err := strconv.Unquote(literal.Value); err == nil && isLegacy[text] {
						constants[ident.Name] = text
					}
				}
			}
		}
	}
	return constants
}

// importName is the name file refers to path by, or "" when it does not
// import it.
func importName(file *ast.File, path string) string {
	for _, spec := range file.Imports {
		imported, err := strconv.Unquote(spec.Path.Value)
		if err != nil || imported != path {
			continue
		}
		if spec.Name != nil {
			return spec.Name.Name
		}
		return path[strings.LastIndex(path, "/")+1:]
	}
	return ""
}
