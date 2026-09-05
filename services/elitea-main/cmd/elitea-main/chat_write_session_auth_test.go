package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestChatWriteRoutesAcceptABrowserSession reads main.go's own AuthConfig for
// the agent-start composition and asserts it carries a session credential.
//
// That handler serves START, REGENERATE and CONTINUE (production_router.go),
// i.e. every write path a chat conversation has, and the product's UI
// authenticates with a session cookie and nothing else — no bearer, no
// forwarded identity. Composed without SessionSecret the route answered
// `401 missing authorization header` to the browser while every server-side
// hop (worker, PAT-driven smoke) succeeded, so the whole backend looked
// healthy and chat simply did not work (#291).
//
// It is an AST assertion for the same reason
// TestProductionRouterConfigSetsRouteGatingRepositories is one: every route
// test composes its OWN AuthConfig, so all of them keep passing while main's
// real literal is missing the field. Nothing else in the build reads what
// production actually wires.
func TestChatWriteRoutesAcceptABrowserSession(t *testing.T) {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	config := authConfigForCall(t, file, "NewCurrentApplicationStartRoute")
	if !authConfigHasField(config, "SessionSecret") {
		t.Fatal("the agent-start AuthConfig has no SessionSecret: the chat UI " +
			"cannot start, regenerate or continue a turn — it authenticates " +
			"with a session cookie and nothing else (#291)")
	}

	// The configuration reads the chat page makes on every load — the model
	// catalogue among them — share one `currentAuth`. Without a session the
	// model picker rendered EMPTY, so no model could be chosen and the turn was
	// then rejected for not naming one (#292): a chat that cannot run, with
	// every configuration row present and correct.
	//
	// `currentAuth` is no longer an inline literal. It is the apiGroupAuth
	// composition, because the inline one was built from formGraph and formGraph
	// is nil on every OIDC-only deployment (gap G2). So the assertion moves with
	// it: the session credential is now pinned by apiGroupAuthConfig's own
	// tests, in BOTH branches —
	// TestAPIGroupAuthConfigKeepsTheProductionCredentials for the Form shape and
	// TestAPIGroupAuthConfigDoesNotReuseTheProductionValidator for the OIDC one.
	// What this test still owns is the WIRE: that the configuration routes read
	// that composition and not a second, private one.
	if !assignsIdentifier(file, "currentAuth", "apiGroupAuth") {
		t.Fatal("the shared configuration AuthConfig is no longer the " +
			"apiGroupAuth composition: an inline literal here loses the " +
			"session secret the model picker needs (#292), and loses it " +
			"silently on OIDC-only deployments (gap G2)")
	}

	// The project switcher is another browser-only caller. In the standalone
	// composition both Form auth and OIDC are enabled, so the Form branch owns
	// the route and must retain the OIDC browser-session credential. Without it
	// GET /projects/project/default/{publicProjectId} answers 401 while the
	// neighbouring /social/author and permission reads succeed.
	projectList := authConfigForCall(t, file, "NewCurrentProjectListRoute")
	if !authConfigHasField(projectList, "SessionSecret") {
		t.Fatal("the project-list AuthConfig has no SessionSecret: the browser " +
			"project switcher cannot list any project in the combined Form/OIDC stack")
	}

	// The runtime routes the web app calls directly (#93 Surface A): the index
	// list, an index run and its cancel, and the chat stop button. Composed for
	// forwarded identity alone they answered 401 to the product's own UI while
	// working for the worker — Surface A's REST path could not be exercised
	// from a browser at all.
	runtime := authConfigVariable(t, file, "browserRuntimeAuth")
	if !authConfigHasField(runtime, "SessionSecret") {
		t.Fatal("the browser runtime AuthConfig has no SessionSecret: the index " +
			"list, run and cancel are unreachable from the web app (#93 Surface A)")
	}
	for _, field := range []string{"PrincipalValidator", "ForwardedIdentityVerifier"} {
		if !authConfigHasField(runtime, field) {
			t.Fatalf("the browser runtime AuthConfig lost %s — the worker and the "+
				"forward-auth edge authenticate with it", field)
		}
	}

	// The peer verifier must survive alongside it. Accepting a session is
	// additive; dropping forwarded identity would break the worker and the
	// forward-auth edge, which are the callers that work today.
	for _, field := range []string{"PrincipalValidator", "ForwardedIdentityVerifier"} {
		if !authConfigHasField(config, field) {
			t.Fatalf("the agent-start AuthConfig lost %s — server-side callers "+
				"authenticate with it", field)
		}
	}
}

// authConfigVariable finds `name := apimw.AuthConfig{...}`.
func authConfigVariable(t *testing.T, file *ast.File, name string) *ast.CompositeLit {
	t.Helper()
	var found *ast.CompositeLit
	ast.Inspect(file, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		identifier, ok := assign.Lhs[0].(*ast.Ident)
		if !ok || identifier.Name != name {
			return true
		}
		if literal, ok := assign.Rhs[0].(*ast.CompositeLit); ok && selectorName(literal.Type) == "AuthConfig" {
			found = literal
			return false
		}
		return true
	})
	if found == nil {
		t.Fatalf("no apimw.AuthConfig assigned to %s in main.go", name)
	}
	return found
}

// authConfigForCall finds `apimw.AuthConfig{...}` passed to the named function.
func authConfigForCall(t *testing.T, file *ast.File, callee string) *ast.CompositeLit {
	t.Helper()
	var found *ast.CompositeLit
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel == nil || selector.Sel.Name != callee {
			return true
		}
		for _, argument := range call.Args {
			literal, ok := argument.(*ast.CompositeLit)
			if !ok {
				continue
			}
			if selectorName(literal.Type) == "AuthConfig" {
				found = literal
				return false
			}
		}
		return true
	})
	if found == nil {
		t.Fatalf("no apimw.AuthConfig literal passed to %s in main.go", callee)
	}
	return found
}

func authConfigHasField(literal *ast.CompositeLit, name string) bool {
	for _, element := range literal.Elts {
		keyValue, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := keyValue.Key.(*ast.Ident); ok && key.Name == name {
			return true
		}
	}
	return false
}

func selectorName(expr ast.Expr) string {
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok || selector.Sel == nil {
		return ""
	}
	return selector.Sel.Name
}
