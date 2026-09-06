package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestChatWriteRoutesAcceptABrowserSession reads main.go's own composition for
// the browser-facing routes and asserts each one takes a session credential.
//
// The agent-start handler serves START, REGENERATE and CONTINUE
// (production_router.go), i.e. every write path a chat conversation has, and
// the product's UI authenticates with a session cookie and nothing else — no
// bearer, no forwarded identity. Composed without SessionSecret the route
// answered `401 missing authorization header` to the browser while every
// server-side hop (worker, PAT-driven smoke) succeeded, so the whole backend
// looked healthy and chat simply did not work (#291).
//
// It is an AST assertion for the same reason
// TestProductionRouterConfigSetsRouteGatingRepositories is one: every route
// test composes its OWN AuthConfig, so all of them keep passing while main's
// real wiring is missing the field. Nothing else in the build reads what
// production actually wires.
//
// The assertions moved from "the literal carries SessionSecret" to "the call
// takes apiGroupAuth", because the literals are gone. apiGroupAuthConfig owns
// the fields now, and its own tests pin them in BOTH branches —
// TestAPIGroupAuthConfigKeepsTheProductionCredentials for the Form shape and
// TestAPIGroupAuthConfigDoesNotReuseTheProductionValidator for the OIDC one.
// What this test owns is the WIRE: that each route reads that composition and
// not a second, private one.
func TestChatWriteRoutesAcceptABrowserSession(t *testing.T) {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	for _, route := range []struct {
		constructor string
		why         string
	}{
		{
			constructor: "NewCurrentApplicationStartRoute",
			why: "the chat UI cannot start, regenerate or continue a turn — " +
				"it authenticates with a session cookie and nothing else (#291)",
		},
		{
			// The project switcher is another browser-only caller. In the
			// standalone composition both Form auth and OIDC are enabled, so
			// the Form branch owned the route and had to keep the browser
			// session credential. Without it GET
			// /projects/project/default/{publicProjectId} answered 401 while
			// the neighbouring permission reads succeeded.
			constructor: "NewCurrentProjectListRoute",
			why: "the browser project switcher cannot list any project in the " +
				"combined Form/OIDC stack",
		},
		{
			// The runtime routes the web app calls directly (#93 Surface A):
			// the index list, an index run and its cancel, and the chat stop
			// button. Composed for forwarded identity alone they answered 401
			// to the product's own UI while working for the worker.
			constructor: "NewCurrentIndexStartRoute",
			why: "the index list, run and cancel are unreachable from the web " +
				"app (#93 Surface A)",
		},
		{
			// The avatar in the user menu, on every page of the product.
			constructor: "NewCurrentAvatarRoute",
			why:         "the user menu renders no avatar and the page reports 401",
		},
		{
			// The author names the artifacts and prompts lists read.
			constructor: "NewCurrentAuthorsRoute",
			why:         "author names are unreadable from the browser",
		},
		{
			// The project header the SPA loads on entering a project.
			constructor: "NewCurrentProjectInfoRoute",
			why:         "the project header cannot load and the SPA re-authenticates",
		},
	} {
		if !callPassesIdentifier(file, route.constructor, "apiGroupAuth") {
			t.Fatalf("%s no longer takes the apiGroupAuth composition: %s",
				route.constructor, route.why)
		}
	}

	// The configuration reads the chat page makes on every load — the model
	// catalogue among them — share one `currentAuth`. Without a session the
	// model picker rendered EMPTY, so no model could be chosen and the turn was
	// then rejected for not naming one (#292): a chat that cannot run, with
	// every configuration row present and correct.
	if !assignsIdentifier(file, "currentAuth", "apiGroupAuth") {
		t.Fatal("the shared configuration AuthConfig is no longer the " +
			"apiGroupAuth composition: an inline literal here loses the " +
			"session secret the model picker needs (#292), and loses it " +
			"silently on OIDC-only deployments (gap G2)")
	}
}

// TestNoPrivateAuthConfigLiteralsInMain fails the build if ANY apimw.AuthConfig
// literal appears in cmd/elitea-main outside api_group_auth.go.
//
// Every per-route defect this package has recorded took the same shape: a
// second AuthConfig, composed from the same raw inputs as the group's, one
// field short. Sixteen of the twenty-one literals main.go once held left
// SessionSecret empty, so apimw.Auth's cookie branch was inert on them. A
// browser holding a VALID session got `401 missing authorization header`, the
// SPA read that as a lost session and opened a fresh OIDC authorize window, so
// a healthy backend looked like a login loop. #291 fixed one route, #93 fixed
// four more, #301 fixed chat_config, #314 and #370 fixed the principal
// validator on three others — each time by editing one literal, and each time
// leaving the rest.
//
// A per-field assertion cannot close this, because the next literal is one
// nobody has written yet. Only the ABSENCE of a second composition can. So the
// rule is structural: apiGroupAuthConfig is the one place an
// apimw.AuthConfig is built, and this test states it.
//
// Test files are exempt. They compose AuthConfigs deliberately, to drive a
// route with a credential set the test controls.
func TestNoPrivateAuthConfigLiteralsInMain(t *testing.T) {
	const compositionRoot = "api_group_auth.go"

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read cmd/elitea-main: %v", err)
	}

	fileSet := token.NewFileSet()
	// The composition root must still hold literals. Without this check the
	// test passes when apiGroupAuthConfig itself is deleted or emptied — the
	// "nothing found reads as correct" trap this repository keeps meeting.
	rootLiterals, scanned := 0, 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned++
		file, parseErr := parser.ParseFile(fileSet, name, nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}
		literals := countAuthConfigLiterals(file)
		if name == compositionRoot {
			rootLiterals += literals
			continue
		}
		if literals > 0 {
			t.Errorf("%s builds %d apimw.AuthConfig literal(s) of its own. "+
				"Take apiGroupAuth instead: a private composition drifts from "+
				"the group's by one field and takes a whole route away from "+
				"the browser (#291, #93, #301, #314, #370).", name, literals)
		}
	}
	if scanned < 2 {
		t.Fatalf("scanned %d non-test Go file(s) in cmd/elitea-main: the guard "+
			"would pass on an empty result, which is the failure mode it "+
			"exists to prevent", scanned)
	}
	if rootLiterals == 0 {
		t.Fatalf("%s builds no apimw.AuthConfig at all: the guard above is "+
			"then vacuously true", compositionRoot)
	}
}

func countAuthConfigLiterals(file *ast.File) int {
	count := 0
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.CompositeLit)
		if ok && selectorName(literal.Type) == "AuthConfig" {
			count++
		}
		return true
	})
	return count
}

func selectorName(expr ast.Expr) string {
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok || selector.Sel == nil {
		return ""
	}
	return selector.Sel.Name
}
