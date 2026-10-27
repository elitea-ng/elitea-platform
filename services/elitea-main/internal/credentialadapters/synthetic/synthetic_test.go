package synthetic_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialadapters"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialadapters/synthetic"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
)

// The sixth adapter registers beside the five built-ins with no core change.
func TestSyntheticAdapterRegistersBesideTheBuiltins(t *testing.T) {
	r, err := credentialadapters.NewRegistry(append(credentialadapters.Builtin(), synthetic.Descriptor())...)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Known("synthetic.widget.read") || r.Class("synthetic.widget.write") != credentialadapters.OperationWrite {
		t.Fatal("the synthetic operations are not registered")
	}
	if p, ok := r.ProviderFor("synthetic.widget.read"); !ok || p != synthetic.ProviderType {
		t.Fatalf("ProviderFor = %q, %v", p, ok)
	}
	if k := r.MaterialKeys(synthetic.ProviderType, credentialgrant.ClassOAuthDelegated); len(k) != 1 || k[0] != "access_token" {
		t.Fatalf("material keys = %v", k)
	}
	if !r.Known("jira.issue.read") {
		t.Fatal("the built-ins are lost")
	}
	// The built-in registry is unchanged: it does not know the synthetic one.
	builtin, err := credentialadapters.NewBuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if builtin.Known("synthetic.widget.read") {
		t.Fatal("the built-in registry knows the synthetic adapter")
	}
}

// The seam is the public API only: the synthetic package imports nothing
// but the descriptor package, the credential grant domain types and the
// standard library. A provider that needed anything more would need a core
// change, which issue 1120 forbids.
func TestSyntheticAdapterUsesThePublicAPIOnly(t *testing.T) {
	allowed := map[string]bool{
		"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialadapters":     true,
		"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant": true,
	}
	_, self, _, _ := runtime.Caller(0)
	dir := filepath.Dir(self)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files++
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range f.Imports {
			path, _ := strconv.Unquote(spec.Path.Value)
			if !allowed[path] && strings.Contains(path, ".") {
				t.Errorf("%s imports %s: a provider adapter may use only the public descriptor API", name, path)
			}
		}
	}
	if files == 0 {
		t.Fatal("no source file was checked")
	}
}
