// overhead_wiring_test.go — issue #17, the guard on the guard.
//
// X-Elapsed-Ms feeds the BFF.9d overhead gate. #610 made it include the
// per-request credential resolution: the handler attaches an overhead.Meter to
// the BifrostContext before dispatch, account.GetKeysForProvider marks it from
// inside the router call, and the handler stamps meter.Overhead(preDispatch)
// after the router returns.
//
// Every part of that is enforced by a behaviour test EXCEPT the composition:
// overhead_header_test.go drives a router stub that marks the meter, and it
// would keep passing if a future handler stamped the header from a plain
// time.Since. httpio.go says so in a doc comment —
//
//	Compute the argument with overhead.Meter.Overhead. Do NOT compute it with
//	time.Since at the call site.
//
// — and a comment is not a gate. This file is the gate. It reads the package
// source and asserts the two facts a behaviour test cannot see:
//
//  1. every setElapsedHeader call is fed by a *.Overhead(...) call, so the
//     stamped value can carry the mark;
//  2. every function that stamps the header also attaches a Meter, so there is
//     a mark to carry.
//
// A handler that satisfies both and still measures nothing is possible in
// principle. It is not possible by accident, which is the failure this repeats.
package llmproxy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestElapsedHeaderIsAlwaysFedByAMeter parses this package and checks both
// facts above.
func TestElapsedHeaderIsAlwaysFedByAMeter(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob the package directory: %v", err)
	}

	stamps := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			// The definition of setElapsedHeader itself is not a call site.
			if fn.Name.Name == "setElapsedHeader" {
				continue
			}
			calls, attaches := 0, false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				ce, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch callee := ce.Fun.(type) {
				case *ast.Ident:
					if callee.Name != "setElapsedHeader" {
						return true
					}
					calls++
					stamps++
					// setElapsedHeader(w, overhead): the VALUE is the second
					// argument. Reading the first would test the writer.
					if len(ce.Args) < 2 {
						t.Errorf("%s: %s calls setElapsedHeader with %d argument(s), want the writer and the overhead",
							name, fn.Name.Name, len(ce.Args))
						return true
					}
					arg, ok := ce.Args[1].(*ast.CallExpr)
					if !ok {
						t.Errorf("%s: %s stamps X-Elapsed-Ms from %T, not from a Meter. "+
							"The value must come from overhead.Meter.Overhead, or the header stops "+
							"reporting credential resolution and understates the gateway hop (issue #17)",
							name, fn.Name.Name, ce.Args[1])
						return true
					}
					sel, ok := arg.Fun.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "Overhead" {
						t.Errorf("%s: %s stamps X-Elapsed-Ms from a call that is not *.Overhead(...). "+
							"time.Since at the call site cannot see the credential resolution that runs "+
							"inside the router call (issue #17)", name, fn.Name.Name)
					}
				case *ast.SelectorExpr:
					if id, ok := callee.X.(*ast.Ident); ok && id.Name == "overhead" && callee.Sel.Name == "Attach" {
						attaches = true
					}
				}
				return true
			})
			if calls > 0 && !attaches {
				t.Errorf("%s: %s stamps X-Elapsed-Ms but never calls overhead.Attach, so no Meter reaches "+
					"the BifrostContext and the header reports the pre-dispatch time alone — the exact "+
					"understatement issue #17 fixed. Attach the Meter in this function before dispatch",
					name, fn.Name.Name)
			}
		}
	}

	// The floor. Without it, deleting every stamp would leave this gate green
	// and reporting a clean package.
	if stamps == 0 {
		t.Fatal("no setElapsedHeader call remains in this package: nothing stamps X-Elapsed-Ms, " +
			"so the BFF.9d overhead gate reads a header that is never sent")
	}
}
