package execution

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// TestEveryJobStateIsClassified fails when a JobState constant is added to
// model.go without being listed as terminal or non-terminal. A state nobody
// classified would silently count as terminal in the project-delete fence and
// the admission guard, so a new in-flight state could be deleted under.
// terminalJobStates is the complement of NonTerminalJobStates, held here so the
// production package exports only the list a caller acts on.
var terminalJobStates = []JobState{JobSucceeded, JobFailed, JobCancelled, JobQuarantined}

func TestEveryJobStateIsClassified(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "model.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	declared := map[JobState]string{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value := spec.(*ast.ValueSpec)
			ident, ok := value.Type.(*ast.Ident)
			if !ok || ident.Name != "JobState" {
				continue
			}
			for i, name := range value.Names {
				literal, ok := value.Values[i].(*ast.BasicLit)
				if !ok {
					t.Fatalf("%s is not a string literal", name.Name)
				}
				unquoted, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatal(err)
				}
				declared[JobState(unquoted)] = name.Name
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("found no JobState constants; the parse is stale")
	}

	classified := map[JobState]string{}
	for _, s := range NonTerminalJobStates() {
		classified[s] = "non-terminal"
	}
	for _, s := range terminalJobStates {
		if prior, dup := classified[s]; dup {
			t.Errorf("%s is both %s and terminal", s, prior)
		}
		classified[s] = "terminal"
	}
	for state, name := range declared {
		if _, ok := classified[state]; !ok {
			t.Errorf("JobState %s (%q) is not classified: add it to NonTerminalJobStates or terminalJobStates in this test", name, state)
		}
		if !state.Valid() {
			t.Errorf("JobState %s (%q) is not accepted by Valid()", name, state)
		}
	}
	for state := range classified {
		if _, ok := declared[state]; !ok {
			t.Errorf("%q is classified but no JobState constant declares it", state)
		}
	}
}
