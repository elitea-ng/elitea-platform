package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// recordingResolverPackage holds the recording permission resolver of the
// administration surface helper (ADR-0059 R0.1b). It grants whatever its probe
// asks, so it must never be reachable from production wiring.
const recordingResolverPackage = "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/adminsurface"

const (
	routerPackage = "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api"
	selfCommand   = "github.com/elitea-ng/elitea-platform/services/elitea-main/cmd/elitea-main"
)

// TestBinaryDoesNotLinkTheRecordingResolver lists the non-test dependencies
// of every main package of this module, for the graph the images build (linux
// on both published architectures, cgo off; the Containerfile passes no build
// tags), and fails if one is the recording resolver's package. It lists the
// main packages, not ./..., because the helper package is itself part of
// ./... .
func TestBinaryDoesNotLinkTheRecordingResolver(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("go tool not found on PATH: %v", err)
	}
	for _, goarch := range []string{"amd64", "arm64"} {
		env := append(os.Environ(), "GOOS=linux", "GOARCH="+goarch, "CGO_ENABLED=0")
		mains := strings.Fields(goList(t, goTool, env,
			"-f", `{{if eq .Name "main"}}{{.ImportPath}}{{end}}`, "./..."))
		if !contains(mains, selfCommand) {
			t.Fatalf("linux/%s: go list ./... reported no main package %s; the scan did not cover the binary",
				goarch, selfCommand)
		}
		deps := goList(t, goTool, env, append([]string{"-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}"}, mains...)...)
		for _, pkg := range strings.Fields(deps) {
			if pkg == recordingResolverPackage || strings.HasPrefix(pkg, recordingResolverPackage+"/") {
				t.Errorf("linux/%s: a main package of this module links %s, the recording resolver", goarch, pkg)
			}
		}
	}
}

func goList(t *testing.T, goTool string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command(goTool, append([]string{"list"}, args...)...)
	cmd.Dir = filepath.Join("..", "..") // module root
	cmd.Env = env
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return string(out)
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// testOnlyRouterFields are the RouterConfig fields only internal/api/adminsurface
// sets, each with what setting it in production would do.
var testOnlyRouterFields = map[string]string{
	"PermissionResolver": "it replaces the resolver of every route",
	"MaintenanceState": "it replaces the maintenance window the platform_settings rows hold, " +
		"for the gate and the platform_settings read",
}

// TestNoProductionCodeOverridesThePermissionResolver fails on non-test code
// that sets RouterConfig.PermissionResolver, the test-only override of the
// router's resolver for every route. The link test above catches the recording
// resolver; this one catches any other resolver put in that slot, which would
// replace authorization on every route. It reads the source of the whole
// module: only this module can import internal/api. RouterConfig.MaintenanceState,
// the helper's seam for an open maintenance window, is held to the same rule.
func TestNoProductionCodeOverridesThePermissionResolver(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var sites []string
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "testdata", "vendor", "node_modules", ".git":
				return filepath.SkipDir
			}
			// The helper is the one place that sets it; the link test above
			// keeps the helper out of every binary.
			if filepath.ToSlash(path) == "../../internal/api/adminsurface" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		scanned++
		importsRouter := file.Name.Name == "api" && strings.HasSuffix(filepath.Dir(path), filepath.Join("internal", "api"))
		for _, spec := range file.Imports {
			if p, _ := strconv.Unquote(spec.Path.Value); p == routerPackage {
				importsRouter = true
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CompositeLit:
				if !isRouterConfig(n.Type) {
					return true
				}
				for _, elt := range n.Elts {
					if kv, ok := elt.(*ast.KeyValueExpr); ok {
						if field, ok := kv.Key.(*ast.Ident); ok && testOnlyRouterFields[field.Name] != "" {
							sites = append(sites, fset.Position(kv.Pos()).String()+" sets RouterConfig."+field.Name)
						}
					}
				}
			case *ast.AssignStmt:
				if !importsRouter {
					return true
				}
				for _, lhs := range n.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && testOnlyRouterFields[sel.Sel.Name] != "" {
						sites = append(sites, fset.Position(sel.Pos()).String()+" sets RouterConfig."+sel.Sel.Name)
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 100 {
		t.Fatalf("scanned %d non-test Go files under the module root; the walk did not cover the module", scanned)
	}
	for _, site := range sites {
		field := site[strings.LastIndex(site, ".")+1:]
		t.Errorf("%s outside a test; %s, and it exists only for internal/api/adminsurface",
			site, testOnlyRouterFields[field])
	}
}

func isRouterConfig(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name == "RouterConfig"
	case *ast.SelectorExpr:
		return e.Sel.Name == "RouterConfig"
	}
	return false
}

func isIdent(expr ast.Expr, name string) bool {
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == name
}
