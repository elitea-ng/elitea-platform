package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestMainWiring_GatewayProxyAuthFields is the "wiring gate" for issue #86:
// PR #84 mounted GatewayProxy into api.RouterConfig without wiring
// AuthValidator/PrincipalValidator/SessionSecret (and the forwarded-identity
// verifier FormSessions has since replaced, #1359), so
// every /llm request returned 401 in any deployment where LLM_GATEWAY_URL is
// set. TestGatewayProxy_* in internal/api/router_gateway_test.go inject
// AuthValidator directly via a test helper and unit-test the router in
// isolation — they cannot catch the composition root forgetting to wire it.
// This is a source-level assertion (parses the api.RouterConfig{} literal in
// main.go) so it needs no live DB/auth stack and runs in milliseconds.
func TestMainWiring_GatewayProxyAuthFields(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var routerConfigLit *ast.CompositeLit
	ast.Inspect(f, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := cl.Type.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "api" && sel.Sel.Name == "RouterConfig" {
			routerConfigLit = cl
		}
		return true
	})
	if routerConfigLit == nil {
		t.Fatal("could not find api.RouterConfig{} composite literal in main.go")
	}

	keys := map[string]bool{}
	for _, elt := range routerConfigLit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		id, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		keys[id.Name] = true

		// The FormSessions field is only reachable through the
		// nested Auth (AuthDeps) struct, not a top-level RouterConfig field —
		// collect its keys too when the value is a composite literal.
		if id.Name == "Auth" {
			if nested, ok := kv.Value.(*ast.CompositeLit); ok {
				for _, nestedElt := range nested.Elts {
					nestedKV, ok := nestedElt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if nestedID, ok := nestedKV.Key.(*ast.Ident); ok {
						keys[nestedID.Name] = true
					}
				}
			}
		}
	}

	if !keys["GatewayProxy"] {
		t.Fatal("api.RouterConfig{} in main.go no longer sets GatewayProxy — this test needs updating")
	}

	required := []struct {
		field string
		why   string
	}{
		{"AuthValidator", "validateToken() returns \"authentication validator is not configured\" and every /llm request returns 401 (issue #86)"},
		{"PrincipalValidator", "principal validation is skipped for the gateway auth middleware, breaking downstream project resolution"},
		{"FormSessions", "the Form session cookie is never read, so a Form-login browser cannot reach /llm (issues #86, #1359)"},
		{"SessionSecret", "the session-cookie auth path is skipped for /llm requests when SessionSecret is empty (issue #86)"},
	}
	for _, r := range required {
		if !keys[r.field] {
			t.Errorf("WIRING GATE: api.RouterConfig{} sets GatewayProxy but not %s — %s. "+
				"Wire it in main() alongside GatewayProxy.", r.field, r.why)
		}
	}
}
