package api_test

// A route can ship with a permission gate that no migration grants. The route
// then answers 403 to every caller on a clean database. Every seeded database
// hides the fault, so continuous integration stays green.
//
// This has happened three times: #354 (`models.chat.messages.create`), then the
// audit it forced found six more (#359, closed by migration 0072). Chat was
// broken on a clean deployment in all three instances. This file makes a fourth
// instance fail in continuous integration, and name the permission.
//
// WHY NO OTHER TEST SEES IT.
//
//   - The end-to-end seed grants permissions per project. A journey that passes
//     on the seeded database proves nothing about a clean one.
//   - A legacy or dump-loaded database carries its own per-project rows. Those
//     rows suppress the central default-mode fallback, so the fault is invisible
//     there too.
//   - The migration tests assert the grants that exist. They cannot report a
//     grant that nobody wrote.
//
// So the defect is visible only on a clean database. No routine gate builds one.
//
// WHY THIS READS THE SOURCE. The property is "no migration in the corpus grants
// this string". That is a property of the source corpus, not of any behaviour a
// request can exercise. A behavioural test needs a live database seeded with the
// very grants under test, which is the assumption being checked.
//
// SCOPE: WHY NOT ONLY THE TWO ROUTER FILES.
//
// Issue #372 names `internal/api/router.go` and `internal/api/production_router.go`.
// Both are read here. Reading only those two files is not sufficient, and the
// history proves it:
//
//   - `models.chat.messages.create`, the #354 defect that started this class, is
//     gated in `internal/api/v2/agentexecution/route.go`. It is named in neither
//     router file. A gate that reads only the two routers MISSES the defect it
//     was written for.
//   - `production_router.go` contains no permission gate at all. Its `Current*`
//     handlers carry their gates inside their own packages.
//
// So this walks every non-test Go file under `internal/`. That is the same trap
// #367 records for the nil-gate test, which read `router.go` and missed 25 gates
// in `production_router.go`. The answer to "which file holds the gates" is
// "find them all", not "add the second file".

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// modulePrefix is this service's import path. An import that does not start with
// it leaves the module, so its constants are out of reach and irrelevant.
const modulePrefix = "github.com/EliteaAI/elitea-platform/services/elitea-main/"

// baseGates maps each middleware constructor in internal/api/middleware/rbac.go
// to the index of its FIRST permission argument. Every one of them takes the
// permissions as a trailing `required ...string`, so the arguments from that
// index onward are permissions.
var baseGates = map[string]int{
	"RequirePermissions":                   0,
	"RequireResolvedPermissions":           2,
	"RequireCentralPermissions":            2,
	"RequireResolvedPermissionsForProject": 3,
	// #830 added this one. A gate this map does not name is a gate this file
	// stops reading, and the route behind it goes back to being invisible on a
	// clean database — the exact failure the file exists to prevent.
	"RequireResolvedMembershipPermissions": 2,
}

// modeArgIndex maps a gate to the index of its permission-mode argument.
//
// The mode matters, and ignoring it makes the check wrong in both directions.
// `legacyrbac` resolves an `administration`-mode gate against the central
// administration roles, and a `default`-mode gate against the default ones. A
// permission granted only to `administration` roles does NOT satisfy a
// `default`-mode gate. Counting a grant from the wrong mode lets an unreachable
// gate pass.
//
// RequirePermissions has no mode: it reads the caller's already-resolved
// permission set. It is recorded under the empty mode.
var modeArgIndex = map[string]int{
	"RequireResolvedPermissions":           1,
	"RequireCentralPermissions":            1,
	"RequireResolvedPermissionsForProject": 1,
	"RequireResolvedMembershipPermissions": 1,
}

// grantModes are the role modes a migration can grant to.
var grantModes = []string{"default", "administration", "developer"}

// allowlistKey identifies one gated permission in one mode. A permission can be
// gated in two modes and be granted in only one, so the mode is part of the key.
type allowlistKey struct {
	mode       string
	permission string
}

func (k allowlistKey) String() string {
	if k.mode == "" {
		return fmt.Sprintf("%s (no resolved mode)", k.permission)
	}
	return fmt.Sprintf("%s in %s mode", k.permission, k.mode)
}

// allowlistedGates records every gated permission that no migration grants, and
// that the change adding the entry does NOT fix. Each entry needs a written
// reason.
//
// This is not a dumping ground. An entry asserts "someone looked at this route
// and recorded why it stays unreachable on a clean database". Adding one to
// silence the test, without meaning it, reintroduces exactly the invisibility
// this file guards against.
//
// THE ALLOWLIST IS EMPTY, AND THAT IS THE POINT.
//
// #372 added this gate and could not fix what it found: that issue asks for the
// gate only, and forbids a change to a route gate or to a migration. The first
// run reported 47 ungranted permissions. Six were granted by migration 0072 in
// #369. The other 41 were listed here as a backlog, because a permanently red
// gate gets switched off, and a gate that is switched off catches nothing.
//
// #386 cleared that backlog. Migrations 0074 to 0082 grant 40 of the 41, one
// migration per surface, each with the role split
// testdata/postgres/legacy-rbac-matrix.json gives it. The 41st,
// `configuration.governance`, needed a code change rather than a grant: its gate
// used `RequirePermissions`, which reads the never-populated
// `auth.User.Permissions`. router.go now gates it with
// `RequireCentralPermissions` in the `administration` mode, and
// shared/0082_admin_panel_permissions.sql grants it.
//
// KEEP IT EMPTY IF YOU CAN. Add an entry only for a route you have decided must
// stay unreachable on a clean database, and write the reason. Every entry is a
// route that answers 403 to every caller. The usual fix is a NEW migration in
// migrations/shared. Remove the entry in the same change as the grant. The
// stale-entry check below fails if you forget.
var allowlistedGates = map[allowlistKey]string{}

// gate is one call site that gates a route on a permission.
type gate struct {
	position    token.Position
	helper      string
	mode        string
	permissions []string
}

// TestEveryGatedPermissionHasAGrant is the gate this file exists for.
func TestEveryGatedPermissionHasAGrant(t *testing.T) {
	t.Parallel()

	root := repoRootFrom(t)
	gates := collectGates(t, root)
	granted := grantsByMode(t, root)

	// Premise guards. Each one fails loudly rather than passing quietly.
	// A check that finds nothing must never report success: that is how
	// check-playwright-image-tag stopped gating and nobody noticed.
	if len(gates) == 0 {
		t.Fatal("found no permission gate under internal/.\n" +
			"  Either the middleware constructors were renamed, or the gates were removed.\n" +
			"  Do not delete this test without replacing the guarantee.")
	}
	if len(granted) == 0 {
		t.Fatal("found no grant in migrations/shared.\n" +
			"  Either the INSERT shape changed, or the grants were removed.\n" +
			"  Do not delete this test without replacing the guarantee.")
	}

	// The three counts #372 asks for, reported by the check rather than by hand.
	gatedKeyCount := map[allowlistKey]bool{}
	for _, g := range gates {
		for _, permission := range g.permissions {
			gatedKeyCount[allowlistKey{mode: g.mode, permission: permission}] = true
		}
	}
	grantedCount := 0
	for _, permissions := range granted {
		grantedCount += len(permissions)
	}
	t.Logf("gate call sites: %d; gated permissions (permission+mode pairs): %d; "+
		"grants in migrations/shared (permission+mode pairs): %d; allowlisted: %d",
		len(gates), len(gatedKeyCount), grantedCount, len(allowlistedGates))

	used := map[allowlistKey]bool{}
	var failures []string

	for _, g := range gates {
		// A gate is satisfied when AT LEAST ONE of its permissions is granted.
		// middleware.hasIntersection accepts the request if the caller holds any
		// one of them, so a gate with a granted member is reachable.
		//
		// Every gate in the tree today names exactly one permission, so this is
		// the same as "the permission is granted". The form is kept because a
		// multi-permission gate added later would otherwise be reported as
		// broken while it works.
		satisfied := false
		var ungranted []allowlistKey
		for _, permission := range g.permissions {
			key := allowlistKey{mode: g.mode, permission: permission}
			switch {
			case granted[g.mode][permission]:
				satisfied = true
			case allowlistedGates[key] != "":
				used[key] = true
				satisfied = true
			default:
				ungranted = append(ungranted, key)
			}
		}
		if satisfied {
			continue
		}
		for _, key := range ungranted {
			failures = append(failures, fmt.Sprintf(
				"%s:%d gates a route on %q in %q mode, which no migration in migrations/shared grants.\n"+
					"    The gate helper is %s.\n"+
					"    On a clean database this route answers 403 to EVERY caller, including the operator.\n"+
					"    That reads as a broken page, not as a missing grant. It is the shape of #354 and #359.\n"+
					"    Fix it at the source: add the grant to a NEW migration in migrations/shared with the\n"+
					"    role split the legacy matrix gives it. Migrations are checksum-immutable, so never\n"+
					"    edit an applied one. If the gate is ungranted on purpose, add the permission to\n"+
					"    allowlistedGates in this file WITH a written reason and an issue reference.",
				relativeTo(root, g.position), g.position.Line, key.permission, g.mode, g.helper))
		}
	}

	sort.Strings(failures)
	for _, failure := range failures {
		t.Errorf("%s", failure)
	}

	checkAllowlistHygiene(t, gates, granted, used)
}

// checkAllowlistHygiene keeps the allowlist honest. A stale entry hides the next
// real regression, and an orphaned entry reads as a live claim about a route
// that no longer exists.
func checkAllowlistHygiene(
	t *testing.T,
	gates []gate,
	granted map[string]map[string]bool,
	used map[allowlistKey]bool,
) {
	t.Helper()

	gatedKeys := map[allowlistKey]bool{}
	for _, g := range gates {
		for _, permission := range g.permissions {
			gatedKeys[allowlistKey{mode: g.mode, permission: permission}] = true
		}
	}

	var stale, orphaned []string
	for key := range allowlistedGates {
		switch {
		case !gatedKeys[key]:
			orphaned = append(orphaned, key.String())
		case granted[key.mode][key.permission] && !used[key]:
			stale = append(stale, key.String())
		}
	}
	sort.Strings(stale)
	sort.Strings(orphaned)

	for _, entry := range stale {
		t.Errorf("allowlistedGates lists %s, but a migration now grants it.\n"+
			"  Remove the entry. A stale allowlist hides the next real regression for this permission.", entry)
	}
	for _, entry := range orphaned {
		t.Errorf("allowlistedGates lists %s, but no route gates on it in that mode.\n"+
			"  The gate was removed and the entry stayed behind, where it reads as a live claim\n"+
			"  about a route that no longer exists. Delete the entry.", entry)
	}
}

func relativeTo(root string, position token.Position) string {
	relative, err := filepath.Rel(root, position.Filename)
	if err != nil {
		return position.Filename
	}
	return relative
}

/* ── the gated side: every permission a route gates on ─────────────────── */

// constantDefinition records where a constant's value expression lives, so the
// resolver can follow a chain across packages.
type constantDefinition struct {
	value ast.Expr
	file  *ast.File
	dir   string
}

type sourceIndex struct {
	fileSet *token.FileSet
	root    string
	// constants is dir -> name -> definition.
	constants map[string]map[string]constantDefinition
	files     map[string][]*ast.File
	// imports is file -> alias -> package directory, for in-module imports.
	imports map[*ast.File]map[string]string
	// helperCache is dir -> helper name -> signature. Helper discovery reads
	// every file of the package, so it produces the same table for every
	// file in it. Without the cache the six-round search re-runs once per
	// file. That is quadratic in the size of the package. It returns the
	// same answer each time.
	helperCache map[string]map[string]helperSignature
}

// collectGates walks every non-test Go file under internal/ and returns one
// entry per gate call site.
func collectGates(t *testing.T, root string) []gate {
	t.Helper()

	index := &sourceIndex{
		fileSet:   token.NewFileSet(),
		root:      root,
		constants: map[string]map[string]constantDefinition{},
		files:     map[string][]*ast.File{},
		imports:   map[*ast.File]map[string]string{},

		helperCache: map[string]map[string]helperSignature{},
	}
	if err := index.load(filepath.Join(root, "internal")); err != nil {
		t.Fatalf("parse the service source: %v", err)
	}

	// Both files #372 names must be present. If a rename moves one, this test
	// must fail rather than silently check a smaller surface.
	for _, required := range []string{"internal/api/router.go", "internal/api/production_router.go"} {
		if _, err := os.Stat(filepath.Join(root, required)); err != nil {
			t.Fatalf("%s is missing: %v.\n"+
				"  This gate is specified against both router files. Update the list if a file moved.",
				required, err)
		}
	}

	var gates []gate
	var unreadable []string
	for dir, files := range index.files {
		for _, file := range files {
			found, unresolved := index.gatesIn(dir, file)
			gates = append(gates, found...)
			unreadable = append(unreadable, unresolved...)
		}
	}

	// A gate whose permission this test cannot read is a gate this test cannot
	// check. Report it instead of skipping it: a silent skip is how a gate stops
	// covering the thing it was written for.
	sort.Strings(unreadable)
	for _, entry := range unreadable {
		t.Errorf("cannot resolve the permission argument at %s.\n"+
			"  This gate is invisible to the check, so an ungranted permission there ships unnoticed.\n"+
			"  Give the argument a package-level string constant, or extend the resolver in this file.",
			entry)
	}

	sort.Slice(gates, func(i, j int) bool {
		if gates[i].position.Filename != gates[j].position.Filename {
			return gates[i].position.Filename < gates[j].position.Filename
		}
		return gates[i].position.Line < gates[j].position.Line
	})
	return gates
}

func (index *sourceIndex) load(dir string) error {
	return filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, parseErr := parser.ParseFile(index.fileSet, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		packageDir := filepath.Dir(path)
		index.files[packageDir] = append(index.files[packageDir], file)
		index.imports[file] = index.importDirs(file)
		if index.constants[packageDir] == nil {
			index.constants[packageDir] = map[string]constantDefinition{}
		}
		for _, declaration := range file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || (general.Tok != token.CONST && general.Tok != token.VAR) {
				continue
			}
			for _, spec := range general.Specs {
				values, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range values.Names {
					if i < len(values.Values) {
						index.constants[packageDir][name.Name] = constantDefinition{
							value: values.Values[i], file: file, dir: packageDir,
						}
					}
				}
			}
		}
		return nil
	})
}

func (index *sourceIndex) importDirs(file *ast.File) map[string]string {
	dirs := map[string]string{}
	for _, imported := range file.Imports {
		path, err := strconv.Unquote(imported.Path.Value)
		if err != nil || !strings.HasPrefix(path, modulePrefix) {
			continue
		}
		dir := filepath.Join(index.root, strings.TrimPrefix(path, modulePrefix))
		alias := filepath.Base(dir)
		if imported.Name != nil {
			alias = imported.Name.Name
		}
		dirs[alias] = dir
	}
	return dirs
}

// resolveString follows a string constant to its literal value.
//
// It must follow CHAINS. `CurrentModelCatalogPermission = CurrentConfigurationListPermission`
// and `CurrentAgentCancelMode = auth.PermissionModeDefault` are both one hop from
// a literal, and a resolver that reads only literals reports them as unreadable.
func (index *sourceIndex) resolveString(
	expression ast.Expr, file *ast.File, dir string, depth int,
) (string, bool) {
	if depth > 12 {
		return "", false
	}
	switch node := expression.(type) {
	case *ast.BasicLit:
		if node.Kind == token.STRING {
			value, err := strconv.Unquote(node.Value)
			return value, err == nil
		}
	case *ast.Ident:
		if definition, ok := index.constants[dir][node.Name]; ok {
			return index.resolveString(definition.value, definition.file, definition.dir, depth+1)
		}
	case *ast.SelectorExpr:
		qualifier, ok := node.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		packageDir, ok := index.imports[file][qualifier.Name]
		if !ok {
			return "", false
		}
		if definition, ok := index.constants[packageDir][node.Sel.Name]; ok {
			return index.resolveString(definition.value, definition.file, definition.dir, depth+1)
		}
	}
	return "", false
}

// helperSignature describes where a helper carries its permission argument.
//
// A base gate takes a trailing `required ...string`, so it is variadic. A
// derived helper takes ONE permission at a fixed index, and its other arguments
// are handlers and dependencies. Treating a derived helper as variadic reports
// its handler argument as an unreadable permission.
type helperSignature struct {
	permissionIndex int
	variadic        bool
	mode            string
	modeKnown       bool
}

// helpersIn returns the gate helpers of one package: the four base
// middleware names plus every function that forwards one of its own
// parameters into a known gate's permission position.
//
// The answer depends on the package, not on the file, so it is cached per
// directory.
//
// SCOPE LIMIT, STATED SO THE NEXT READER DOES NOT ASSUME MORE. This search
// is package-transitive, not module-transitive. A helper EXPORTED from one
// package and called from another is still invisible, and every gate behind
// it would be dropped in silence. No such helper exists in the tree today.
// If one appears, extend this search across the import graph.
func (index *sourceIndex) helpersIn(dir string) map[string]helperSignature {
	if cached, ok := index.helperCache[dir]; ok {
		return cached
	}
	helpers := map[string]helperSignature{}
	for name, position := range baseGates {
		helpers[name] = helperSignature{permissionIndex: position, variadic: true}
	}

	helperName := func(function ast.Expr) (string, bool) {
		switch node := function.(type) {
		case *ast.Ident:
			_, ok := helpers[node.Name]
			return node.Name, ok
		case *ast.SelectorExpr:
			_, ok := helpers[node.Sel.Name]
			return node.Sel.Name, ok
		}
		return "", false
	}

	// Find the NAMED HELPERS #372 asks for. router.go does not call the
	// middleware directly for most routes. It builds `central(permission)` and
	// `projectPermission(permission)` first, then calls those. The permission
	// string is at the helper's call site, not at the middleware's.
	//
	// A function becomes a helper when it forwards one of its own parameters
	// into a known gate's permission position. The loop repeats so a helper
	// built from a helper is also found.
	//
	// THE TABLE IS BUILT FROM THE WHOLE PACKAGE, NOT FROM THIS ONE FILE.
	//
	// This is the fix for a hole that hid a whole surface. Go resolves an
	// unqualified name across every file of its package, so a helper may be
	// DEFINED in one file and CALLED from another. The earlier version scanned
	// `file` alone, so such a helper was never in the table. Its call sites
	// did not look like gates. The check reported "ok" for permissions no
	// migration granted.
	//
	// It was not hypothetical. internal/api/v2/secrets defines `adminGate` in
	// admin.go and calls it six times from handler.go's Routes(). Those six
	// administration-mode gates were invisible. Every one of them named a
	// permission that no migration granted in that mode. The whole global
	// secret vault therefore answered 403 to every principal.
	for round := 0; round < 6; round++ {
		grew := false
		for _, sourceFile := range index.files[dir] {
			forEachFunction(sourceFile, func(name string, signature *ast.FuncType, body *ast.BlockStmt) {
				if name == "" || body == nil {
					return
				}
				if _, known := helpers[name]; known {
					return
				}
				parameters := parameterNames(signature)
				ast.Inspect(body, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						return true
					}
					called, ok := helperName(call.Fun)
					if !ok {
						return true
					}
					inner := helpers[called]
					for i := inner.permissionIndex; i < len(call.Args); i++ {
						if !inner.variadic && i > inner.permissionIndex {
							break
						}
						identifier, ok := call.Args[i].(*ast.Ident)
						if !ok {
							continue
						}
						at := indexOf(parameters, identifier.Name)
						if at < 0 {
							continue
						}
						derived := helperSignature{permissionIndex: at}
						// The derived helper fixes the mode, because the mode
						// is written at the middleware call inside its body.
						// Resolve it against the file that DEFINES the helper.
						if modeIndex, ok := modeArgIndex[called]; ok && modeIndex < len(call.Args) {
							if mode, ok := index.resolveString(call.Args[modeIndex], sourceFile, dir, 0); ok {
								derived.mode, derived.modeKnown = mode, true
							}
						} else if inner.modeKnown {
							derived.mode, derived.modeKnown = inner.mode, true
						}
						helpers[name] = derived
						grew = true
						return false
					}
					return true
				})
			})
		}
		if !grew {
			break
		}
	}
	index.helperCache[dir] = helpers
	return helpers
}

// gatesIn returns the gates in one file, plus the positions of any permission
// argument that cannot be resolved.
func (index *sourceIndex) gatesIn(dir string, file *ast.File) ([]gate, []string) {
	helpers := index.helpersIn(dir)

	helperName := func(function ast.Expr) (string, bool) {
		switch node := function.(type) {
		case *ast.Ident:
			_, ok := helpers[node.Name]
			return node.Name, ok
		case *ast.SelectorExpr:
			_, ok := helpers[node.Sel.Name]
			return node.Sel.Name, ok
		}
		return "", false
	}

	var gates []gate
	var unreadable []string
	// enclosing holds the parameter names of the functions around the current
	// node. A permission argument that names one of them is the helper's own
	// DEFINITION, not a gate. Counting the definition reports a permission that
	// no route uses, and hides nothing.
	var enclosing []string

	var walk func(node ast.Node) bool
	walk = func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.FuncDecl:
			saved := enclosing
			enclosing = append(append([]string{}, enclosing...), parameterNames(typed.Type)...)
			if typed.Body != nil {
				ast.Inspect(typed.Body, walk)
			}
			enclosing = saved
			return false
		case *ast.FuncLit:
			saved := enclosing
			enclosing = append(append([]string{}, enclosing...), parameterNames(typed.Type)...)
			ast.Inspect(typed.Body, walk)
			enclosing = saved
			return false
		case *ast.CallExpr:
			called, ok := helperName(typed.Fun)
			if !ok {
				return true
			}
			signature := helpers[called]
			found := gate{
				position: index.fileSet.Position(typed.Lparen),
				helper:   called,
				mode:     index.modeOf(called, signature, typed, file, dir),
			}
			var unresolved []string
			for i := signature.permissionIndex; i < len(typed.Args); i++ {
				if !signature.variadic && i > signature.permissionIndex {
					break
				}
				argument := typed.Args[i]
				if identifier, ok := argument.(*ast.Ident); ok &&
					indexOf(enclosing, identifier.Name) >= 0 {
					// This call is the helper's definition. Skip the whole site.
					return true
				}
				if value, ok := index.resolveString(argument, file, dir, 0); ok {
					found.permissions = append(found.permissions, value)
					continue
				}
				unresolved = append(unresolved, fmt.Sprintf("%s:%d (%s)",
					relativeTo(index.root, found.position), found.position.Line, called))
			}
			unreadable = append(unreadable, unresolved...)
			if len(found.permissions) > 0 {
				gates = append(gates, found)
			}
			return true
		}
		return true
	}
	ast.Inspect(file, walk)
	return gates, unreadable
}

// modeOf returns the permission mode the gate resolves in.
func (index *sourceIndex) modeOf(
	called string, signature helperSignature, call *ast.CallExpr, file *ast.File, dir string,
) string {
	if signature.modeKnown {
		return signature.mode
	}
	modeIndex, ok := modeArgIndex[called]
	if !ok {
		// RequirePermissions carries no mode.
		return ""
	}
	if modeIndex >= len(call.Args) {
		return ""
	}
	if mode, ok := index.resolveString(call.Args[modeIndex], file, dir, 0); ok {
		return mode
	}
	return ""
}

func forEachFunction(file *ast.File, visit func(string, *ast.FuncType, *ast.BlockStmt)) {
	ast.Inspect(file, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.FuncDecl:
			visit(typed.Name.Name, typed.Type, typed.Body)
		case *ast.AssignStmt:
			if len(typed.Lhs) == 1 && len(typed.Rhs) == 1 {
				if name, ok := typed.Lhs[0].(*ast.Ident); ok {
					if literal, ok := typed.Rhs[0].(*ast.FuncLit); ok {
						visit(name.Name, literal.Type, literal.Body)
					}
				}
			}
		}
		return true
	})
}

func parameterNames(signature *ast.FuncType) []string {
	var names []string
	if signature == nil || signature.Params == nil {
		return names
	}
	for _, field := range signature.Params.List {
		for _, name := range field.Names {
			names = append(names, name.Name)
		}
	}
	return names
}

func indexOf(values []string, wanted string) int {
	for i, value := range values {
		if value == wanted {
			return i
		}
	}
	return -1
}

/* ── the granted side: every permission the corpus grants ──────────────── */

// grantsByMode collects every permission the ledgered corpus grants, per role
// mode.
//
// It reads migrations/shared only. That directory is the ledgered corpus:
// migrations/embed.go embeds it, and internal/infra/db/migrate/manifest.go
// ledgers it in elitea_runtime.schema_migrations. It is what every deployment
// applies.
//
// internal/infra/db/migrations/001_initial.sql is NOT read. It is the dev
// bootstrap, applied only behind ELITEA_DEV_BOOTSTRAP_LEGACY_SCHEMA. A clean
// production database never runs it, so a grant that exists only there does not
// make a route reachable.
func grantsByMode(t *testing.T, root string) map[string]map[string]bool {
	t.Helper()

	dir := filepath.Join(root, "migrations", "shared")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations/shared: %v", err)
	}

	granted := map[string]map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		source := stripSQLComments(readFile(t, filepath.Join(dir, entry.Name())))
		// Comments are stripped FIRST. Several files name a permission in prose
		// at the exact point where they say they do NOT grant it. 0063 names
		// `models.chat.messages.create` that way, seven migrations before 0070
		// granted it. Counting prose would have hidden #354.
		for _, block := range insertBlocks(source) {
			for _, mode := range grantModes {
				if !strings.Contains(block, "mode = '"+mode+"'") {
					continue
				}
				if granted[mode] == nil {
					granted[mode] = map[string]bool{}
				}
				for _, permission := range permissionLiterals(block) {
					granted[mode][permission] = true
				}
			}
		}
	}
	return granted
}
