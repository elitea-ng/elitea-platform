package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestProductionRouterConfigSetsRouteGatingRepositories reads main.go's own
// api.RouterConfig composite literal and asserts the fields that gate whole
// route groups are present.
//
// internal/api/router.go registers the /elitea_core application and version
// routes inside `if cfg.AppsRepo != nil`. A nil repository unregisters
// thirteen routes with no startup error and no failing gate — the whole
// /elitea_core/applications surface simply 404s in production (#115). Nothing
// else in the build catches that, because every router test composes its own
// RouterConfig rather than main's, so this test reads the real literal.
func TestProductionRouterConfigSetsRouteGatingRepositories(t *testing.T) {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	literal := routerConfigLiteral(t, file)
	present := map[string]bool{}
	for _, element := range literal.Elts {
		keyValue, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := keyValue.Key.(*ast.Ident); ok {
			present[key.Name] = true
		}
	}

	// PlatformMCPTools: without it the remote toolkit route answers 501 for
	// every platform MCP tool of a local turn (issue 1231).
	for _, field := range []string{"Pool", "AppsRepo", "PlatformMCPTools"} {
		if !present[field] {
			t.Errorf("api.RouterConfig in main.go does not set %s; "+
				"internal/api/router.go silently drops the route group it gates", field)
		}
	}
}

// routerConfigLiteral returns the api.RouterConfig{...} literal main.go passes
// to api.NewRouter.
func routerConfigLiteral(t *testing.T, file *ast.File) *ast.CompositeLit {
	t.Helper()
	var found *ast.CompositeLit
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "NewRouter" {
			return true
		}
		packageIdent, ok := selector.X.(*ast.Ident)
		if !ok || packageIdent.Name != "api" {
			return true
		}
		literal, ok := call.Args[0].(*ast.CompositeLit)
		if !ok {
			return true
		}
		found = literal
		return false
	})
	if found == nil {
		t.Fatal("main.go no longer calls api.NewRouter with an api.RouterConfig literal; " +
			"this gate must be updated to follow the new composition")
	}
	return found
}
