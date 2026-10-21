package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestWorkloadIdentityReachesTheAccount is the wiring gate for the keyless
// platform credential mode. TestMainWiring checks CALLS; the flag reaches the
// Account through a struct literal, which a call gate cannot see. Without the
// two fields the Account refuses every keyless row while /governance/status
// reports workload_identity_enabled: true — two signals that disagree, which is
// the shape of the "built but not wired" defect this package keeps meeting.
func TestWorkloadIdentityReachesTheAccount(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	found := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Config" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "account" {
			return true
		}
		for _, elt := range lit.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				if key, ok := kv.Key.(*ast.Ident); ok {
					found[key.Name] = true
				}
			}
		}
		return true
	})
	for _, field := range []string{"WorkloadIdentityEnabled", "IdentityTokens", "PublicProjectID"} {
		if !found[field] {
			t.Errorf("account.Config in main.go does not set %s — keyless platform credentials are refused "+
				"for every request although the operator armed them", field)
		}
	}
}
