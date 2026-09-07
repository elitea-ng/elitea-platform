package api

// EVERY reader of "the caller's personal project" must be able to ASK for one.
//
// THE DEFECT THIS CLOSES. `personalproject.Ensurer` is reached from a handful
// of places, and each one is opt-in: a `*DBPersonalProjectResolver` built
// without `.WithPersonalProjectEnsurer` is read-only, and a route behind it
// answers "no personal project" forever with nothing in any log. Two of them
// were built that way — both MCP surfaces — so an MCP client, which
// authenticates with a personal access token and never calls
// `/social/author`, could not get a personal project by any route it uses.
//
// WHY THIS READS SOURCE. The gap is invisible at the response: the route
// answers, the resolver resolves, and the only difference is that nothing was
// ever provisioned. It is the same class as issue #830 — the route works and
// the behaviour is missing — and the same class `dead code with no caller`
// records. Unit tests of the resolver pass either way, because they construct
// the resolver themselves.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestEveryPersonalProjectResolverInTheRouterCanProvision fails when a
// `NewDBPersonalProjectResolver(...)` in router.go is not given the ensurer.
//
// The rule is stated as "the ensurer appears within the same statement or in
// the ten lines that follow", because the two spellings in this file differ: a
// call site either chains `.WithPersonalProjectEnsurer(...)` or assigns the
// resolver and then re-assigns it under a nil test. Both are fine; building one
// and never mentioning the ensurer is not.
func TestEveryPersonalProjectResolverInTheRouterCanProvision(t *testing.T) {
	const file = "router.go"

	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, file, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	lines := strings.Split(readRouterSource(t), "\n")

	var constructions int
	ast.Inspect(parsed, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "NewDBPersonalProjectResolver" {
			return true
		}
		constructions++
		line := fileSet.Position(call.Pos()).Line
		// One line before, for a chained construction that starts on the
		// previous line, and ten after, for the assign-then-attach spelling.
		window := strings.Join(lines[maxInt(0, line-2):minInt(len(lines), line+10)], "\n")
		if !strings.Contains(window, "WithPersonalProjectEnsurer") {
			t.Errorf("%s:%d builds a personal-project resolver that can never ask for one. "+
				"A route behind it answers \"no personal project\" forever, and the response "+
				"looks the same either way. Give it .WithPersonalProjectEnsurer(personalProjects).",
				file, line)
		}
		return true
	})

	if constructions == 0 {
		t.Fatal("no NewDBPersonalProjectResolver call was found in router.go — this test " +
			"stopped checking anything, which is the failure mode it exists to prevent")
	}
}

// TestBothFederationPlanesAskForThePersonalProjectAtSignIn.
//
// The read paths are lazy: a new user whose first screen calls neither
// `/social/author` nor `/llm` is left with no personal project and parks on
// `/onboarding`. A login is the one moment every account passes through, so
// router.go attaches the ensurer to both federation planes.
func TestBothFederationPlanesAskForThePersonalProjectAtSignIn(t *testing.T) {
	source := readRouterSource(t)
	for _, wanted := range []string{
		"cfg.OIDCHandler.WithPersonalProjectEnsurer(personalProjects)",
		"cfg.SAMLHandler.WithPersonalProjectEnsurer(personalProjects)",
		"cfg.Auth.OIDCHandler.WithPersonalProjectEnsurer(personalProjects)",
		"cfg.Auth.SAMLHandler.WithPersonalProjectEnsurer(personalProjects)",
	} {
		if !strings.Contains(source, wanted) {
			t.Errorf("router.go no longer contains %q. A federation plane that does not ask "+
				"leaves a first-time user with no personal project.", wanted)
		}
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
