package nativeauth

// TestEverySuspensionWriteRevokesNativeSessions — the deactivation hook's
// guard (ADR-0025 WP3, coordinator decision 10).
//
// Six write sites set auth_core__user.suspended (five in the SCIM directory,
// one in the admin user surface), and before ADR-0025 none of them revoked
// anything. A hook called from six places is only as good as the SEVENTH place,
// so this test reads the source: every non-test Go file under internal/ and
// cmd/ is parsed, every SQL string literal that assigns `suspended` on
// auth_core__user is found, and the function that holds it must call
// nativeauth.RevokeUserSessions — directly, or through a same-package helper
// that does — or be listed in hookAllowlist with a reason.
//
// It also refuses to pass VACUOUSLY: it must find the known sites, so a change
// that hid them from the matcher (a different spelling, a constant) fails here
// instead of reading as "no site needs the hook".

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var suspensionWrite = regexp.MustCompile(`(?is)(\bUPDATE\s+(public\.)?auth_core__user\b[^;]*?\bsuspended\s*=)|` +
	`(\bINSERT\s+INTO\s+(public\.)?auth_core__user\s*\([^)]*\bsuspended\b)`)

// hookAllowlist names a "file:function" whose suspension write needs no hook,
// with the reason. Empty today: every site that can SET suspended=true calls
// the hook. A provisioning path that only ever writes suspended=false belongs
// here, with that stated.
var hookAllowlist = map[string]string{}

// knownSites must all be found, so the matcher cannot silently go blind.
var knownSites = []string{
	"internal/scimdirectory/directory.go:Create",
	"internal/scimdirectory/directory.go:Replace",
	"internal/scimdirectory/directory.go:SetActive",
	"internal/scimdirectory/directory.go:ApplyUserChanges",
	"internal/api/v2/admin/users.go:UserSuspend",
}

func TestEverySuspensionWriteRevokesNativeSessions(t *testing.T) {
	root := filepath.Join("..", "..")
	found := map[string]bool{}
	var missing []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") ||
				strings.HasSuffix(path, ".gen.go") {
				return nil
			}
			fileSet := token.NewFileSet()
			file, err := parser.ParseFile(fileSet, path, nil, 0)
			if err != nil {
				return err
			}
			hooks := hookFunctions(file)
			relative, _ := filepath.Rel(root, path)
			relative = filepath.ToSlash(relative)
			for _, declaration := range file.Decls {
				function, ok := declaration.(*ast.FuncDecl)
				if !ok || function.Body == nil || !writesSuspension(function.Body) {
					continue
				}
				site := relative + ":" + function.Name.Name
				found[site] = true
				if _, allowed := hookAllowlist[site]; allowed {
					continue
				}
				if !callsAny(function.Body, hooks) {
					missing = append(missing, site)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("these functions write auth_core__user.suspended without calling nativeauth.RevokeUserSessions "+
			"(ADR-0025 WP3): %s\nCall the hook in the same transaction, or add the function to hookAllowlist "+
			"with the reason it can never suspend.", strings.Join(missing, ", "))
	}
	for _, site := range knownSites {
		if !found[site] {
			t.Fatalf("the guard did not find the known suspension write %s; the matcher went blind", site)
		}
	}
}

// writesSuspension reports a string literal in body that assigns suspended.
func writesSuspension(body *ast.BlockStmt) bool {
	matched := false
	ast.Inspect(body, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(literal.Value)
		if err == nil && suspensionWrite.MatchString(value) {
			matched = true
		}
		return true
	})
	return matched
}

// hookFunctions is RevokeUserSessions plus every function in this file that
// calls it (one level of indirection, e.g. scimdirectory.revokeOnDeactivation).
func hookFunctions(file *ast.File) map[string]bool {
	hooks := map[string]bool{"RevokeUserSessions": true}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Body != nil && callsAny(function.Body, map[string]bool{"RevokeUserSessions": true}) {
			hooks[function.Name.Name] = true
		}
	}
	return hooks
}

func callsAny(body *ast.BlockStmt, names map[string]bool) bool {
	called := false
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			called = called || names[fun.Name]
		case *ast.SelectorExpr:
			called = called || names[fun.Sel.Name]
		}
		return true
	})
	return called
}
