package api_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestPrincipalNameIsNotDerivedFromTokenName is a source-level gate, not a
// behavioural test. It follows TestNilGatedRouterFieldsAreWiredOrDeclared in
// router_nil_gate_test.go: the defect is a property of the source, so the test
// reads the source.
//
// The escalation it blocks:
//
// middleware.Project parses a project id out of auth.User.Name. A name of the
// form ":system:project:42:" asks for project 42
// (internal/api/middleware/project.go, projectIDFromUserName). The resolved
// project is what the /llm edge signs into X-Elitea-Project-Id. The gateway
// then spends that project's budget and decrypts that project's provider
// credentials.
//
// The token `name` column is caller-supplied free text of up to 768 bytes. Any
// user creates a token and chooses its name (internal/api/v2/auth/tokens.go).
// So if auth.User.Name were ever populated from a token name, a caller could
// name a token ":system:project:42:" and ask for project 42's money.
//
// Issue #459 added the membership check to that branch, so the name now buys
// only a project the caller belongs to. This gate keeps its value after that
// change, for two reasons. It keeps the name out of the authorization input in
// the first place, which is the rule spec-llm-project-scope §7 invariant 2
// states. And a membership check is one line that a later edit can remove,
// while this gate fails loudly when the dangerous value returns.
//
// The gate is written around the token name because that is the mechanism
// ADR-0018 rejected for carrying scope, and because spec-llm-project-scope §7
// invariant 2 makes it a standing rule. The second check below generalises it:
// ANY new writer of auth.User.Name must state why its value is not
// caller-controlled.
//
// Why a source gate and not a unit test:
//
// No behavioural test can observe "this assignment does not exist". A test of
// the validator asserts what the validator returns today. Adding one field to a
// struct literal a year from now breaks the invariant and fails no assertion,
// because nothing asserts on a field nobody sets.
func TestPrincipalNameIsNotDerivedFromTokenName(t *testing.T) {
	t.Parallel()

	root := repoRootFrom(t)

	// The credential query is checked first: it is the supply of the dangerous
	// value, and while it carries no name column the escalation has no source.
	queryFindings, err := scanCredentialPrincipalQuery(root)
	if err != nil {
		t.Fatalf("%v\n"+
			"  This gate's premise no longer holds. Do not delete the gate without replacing the\n"+
			"  guarantee: the credential validator must not read the caller-supplied token name.", err)
	}
	for _, finding := range queryFindings {
		t.Errorf("%s\n%s", finding.location, finding.message)
	}

	writes, parsed, err := scanPrincipalNameWrites(root)
	if err != nil {
		t.Fatalf("scan %s: %v", root, err)
	}
	if parsed == 0 {
		t.Fatal("parsed no non-test Go files; this gate's premise no longer holds. " +
			"Do not delete it without replacing the guarantee.")
	}

	seenFiles := map[string]bool{}
	for _, item := range writes {
		seenFiles[item.file] = true
		if _, allowed := principalNameWriters[item.file]; allowed && !item.fromTokenName {
			continue
		}
		t.Errorf("%s\n%s", item.location(), item.message())
	}

	var stale []string
	for file := range principalNameWriters {
		if !seenFiles[file] {
			stale = append(stale, file)
		}
	}
	sort.Strings(stale)
	for _, file := range stale {
		t.Errorf("principalNameWriters lists %s, but that file no longer assigns auth.User.Name.\n"+
			"  Delete the entry: a stale allowlist reads as a live claim, and it hides the next real writer.", file)
	}
}

// principalNameWriters lists every non-test file that may assign auth.User.Name,
// with the reason its value is safe.
//
// This is not a dumping ground. An entry asserts "this value is not
// caller-controlled, and it is not a token name". A new writer must prove the
// same before it is added, because auth.User.Name is an authorization input at
// the /llm edge and not a display string.
//
// It is EMPTY, and that is the end state ADR-0018 describes. The one entry it
// ever held was internal/infra/authsvc/rpc.go, the pylon Redis RPC validator
// that populated auth.User.Name from an RPC response. #383 deleted that file
// and RouterConfig.AuthClient with it, so no non-test file in this service
// assigns a principal name any more. An addition here now needs the full
// proof above, with no precedent to point at.
var principalNameWriters = map[string]string{}

// tokenNameSource matches an expression that reads a name off a token. It is
// deliberately narrow: it fires on `tokenRow.Name`, `token.Name`, `pat.Name`,
// `principal.TokenName` and `nameFromToken`, and it does not fire on the
// hundreds of unrelated Name fields in this service.
var tokenNameSource = regexp.MustCompile(`(?i)\b(token|pat)[a-z0-9_]*\.[a-z0-9_]*name\b|\btoken[a-z0-9_]*name\b|\bname[a-z0-9_]*token\b`)

// gateFinding is one source location the gate refuses.
type gateFinding struct {
	location string
	message  string
}

// nameWrite is one assignment into a Name field that the gate tracks.
type nameWrite struct {
	file string
	line int
	// expression is the assigned value, as source text.
	expression string
	// target is the assigned field, as source text.
	target string
	// fromTokenName reports whether the value reads a name off a token. Such a
	// write is refused even in an allowlisted file.
	fromTokenName bool
}

func (w nameWrite) location() string {
	return fmt.Sprintf("%s:%d", w.file, w.line)
}

func (w nameWrite) message() string {
	if w.fromTokenName {
		return fmt.Sprintf("  assigns a TOKEN NAME into a principal name: `%s = %s`.\n"+
			"  The token `name` column is caller-supplied free text of up to 768 bytes\n"+
			"  (internal/api/v2/auth/tokens.go). middleware.Project parses \":system:project:<id>:\"\n"+
			"  out of auth.User.Name and runs no membership check, so this is a self-service\n"+
			"  escalation: a caller names a token \":system:project:42:\" and spends project 42's\n"+
			"  budget and provider credentials. Bind the token to a project instead — that is what\n"+
			"  ADR-0018 and elitea_identity.token_project_binding exist for.\n"+
			"  (spec-llm-project-scope §7 invariant 2)", w.target, w.expression)
	}
	return fmt.Sprintf("  assigns auth.User.Name (`Name: %s`), and this file is not listed in\n"+
		"  principalNameWriters in this test.\n"+
		"  auth.User.Name is an AUTHORIZATION INPUT at the /llm edge, not a display string:\n"+
		"  middleware.Project reads a project id out of it and runs no membership check.\n"+
		"  Prove the value is not caller-controlled and is not a token name, then add the file\n"+
		"  here with that reason. (spec-llm-project-scope §7 invariant 2)", w.expression)
}

// scanCredentialPrincipalQuery closes the escalation at its source.
//
// GetActivePATPrincipalByUUID is the single query the credential validator runs
// for every request (internal/infra/authsvc/local_validator.go). It selects the
// token id, the owner id, the owner email and the project binding — and no name
// column. While that holds, the validator CANNOT populate auth.User.Name from a
// token name, because it never reads one. Adding `token.name` to that SELECT is
// the first move of the escalation, and it looks harmless in review.
func scanCredentialPrincipalQuery(root string) ([]gateFinding, error) {
	const queryName = "GetActivePATPrincipalByUUID"

	sqlPath := filepath.Join(root, "internal/db/queries/auth_pat.sql")
	sql, err := os.ReadFile(sqlPath) //nolint:gosec // fixed, test-local path
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", sqlPath, err)
	}
	statement, ok := sqlStatement(string(sql), queryName)
	if !ok {
		return nil, fmt.Errorf("no `-- name: %s` statement in %s", queryName, sqlPath)
	}

	generatedPath := filepath.Join(root, "internal/db/sqlcgen/auth_pat.sql.go")
	generated, err := os.ReadFile(generatedPath) //nolint:gosec // fixed, test-local path
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", generatedPath, err)
	}
	row, ok := goStructBody(string(generated), queryName+"Row")
	if !ok {
		return nil, fmt.Errorf("no `type %sRow struct` in %s", queryName, generatedPath)
	}

	var findings []gateFinding
	if regexp.MustCompile(`(?i)\bname\b`).MatchString(statement) {
		findings = append(findings, gateFinding{
			location: "internal/db/queries/auth_pat.sql",
			message: fmt.Sprintf("  the %s query now selects a name column.\n"+
				"  That query builds auth.User for EVERY authenticated request, and the token `name`\n"+
				"  is caller-supplied free text. middleware.Project reads a project id out of\n"+
				"  auth.User.Name (\":system:project:<id>:\") with no membership check, so a caller\n"+
				"  could name a token \":system:project:42:\" and spend project 42's budget and\n"+
				"  provider credentials. Use the stored binding instead (ADR-0018).\n"+
				"  Statement:\n%s", queryName, strings.TrimSpace(statement)),
		})
	}
	if regexp.MustCompile(`(?m)^\s*Name\s`).MatchString(row) {
		findings = append(findings, gateFinding{
			location: "internal/db/sqlcgen/auth_pat.sql.go",
			message: fmt.Sprintf("  %sRow now carries a Name field, so the credential validator can read a\n"+
				"  token name. See the reasoning above: auth.User.Name is a project-scope input at\n"+
				"  the /llm edge, not a label.", queryName),
		})
	}
	return findings, nil
}

// scanPrincipalNameWrites reads every non-test Go file under root and returns
// each assignment into a principal name, plus the number of files parsed.
//
// Two shapes are collected:
//
//  1. `auth.User{… Name: <expr> …}` — every write into the field, whatever the
//     value. The caller decides which are allowed.
//  2. `<x>.Name = <expr>` where the value reads a name off a token. The
//     receiver's type is not resolvable without type checking, so the value
//     carries the signal: a token name in a principal name is wrong wherever it
//     lands.
//
// Shape 1 catches the indirect mistake, where the value is renamed on the way —
// `principal.Label`, `row.Title`, a helper — and no pattern could recognise it.
// Shape 2 catches the direct mistake outside a composite literal.
func scanPrincipalNameWrites(root string) ([]nameWrite, int, error) {
	var writes []nameWrite
	parsed := 0

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case "testdata", "vendor", ".git", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		parsed++

		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			relative = path
		}
		relative = filepath.ToSlash(relative)
		inAuthPackage := file.Name.Name == "auth"

		ast.Inspect(file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.CompositeLit:
				if !isAuthUserType(typed.Type, inAuthPackage) {
					return true
				}
				for _, element := range typed.Elts {
					pair, ok := element.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if key, ok := pair.Key.(*ast.Ident); !ok || key.Name != "Name" {
						continue
					}
					value := types.ExprString(pair.Value)
					writes = append(writes, nameWrite{
						file:          relative,
						line:          fset.Position(pair.Pos()).Line,
						expression:    value,
						target:        "auth.User.Name",
						fromTokenName: tokenNameSource.MatchString(value),
					})
				}
			case *ast.AssignStmt:
				for index, target := range typed.Lhs {
					selector, ok := target.(*ast.SelectorExpr)
					if !ok || selector.Sel.Name != "Name" || index >= len(typed.Rhs) {
						continue
					}
					value := types.ExprString(typed.Rhs[index])
					if !tokenNameSource.MatchString(value) {
						continue
					}
					writes = append(writes, nameWrite{
						file:          relative,
						line:          fset.Position(target.Pos()).Line,
						expression:    value,
						target:        types.ExprString(target),
						fromTokenName: true,
					})
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return writes, parsed, nil
}

// isAuthUserType reports whether a composite-literal type is auth.User, or User
// inside package auth.
func isAuthUserType(expr ast.Expr, inAuthPackage bool) bool {
	switch typed := expr.(type) {
	case *ast.SelectorExpr:
		pkg, ok := typed.X.(*ast.Ident)
		return ok && pkg.Name == "auth" && typed.Sel.Name == "User"
	case *ast.Ident:
		return inAuthPackage && typed.Name == "User"
	}
	return false
}

// sqlStatement returns the body of the named sqlc statement, up to the next
// `-- name:` marker.
func sqlStatement(source, name string) (string, bool) {
	marker := "-- name: " + name + " "
	start := strings.Index(source, marker)
	if start < 0 {
		return "", false
	}
	rest := source[start+len(marker):]
	if end := strings.Index(rest, "-- name: "); end >= 0 {
		rest = rest[:end]
	}
	return rest, true
}

// goStructBody returns the field block of the named struct declaration.
func goStructBody(source, name string) (string, bool) {
	marker := "type " + name + " struct {"
	start := strings.Index(source, marker)
	if start < 0 {
		return "", false
	}
	rest := source[start+len(marker):]
	end := strings.Index(rest, "}")
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}

// TestPrincipalNameGateCatchesTheEscalation mutation-checks the gate above.
//
// A gate that reports nothing is indistinguishable from a gate that cannot see.
// This test writes the escalation into a fixture tree, in each of the four
// shapes it can take, and requires the scanners to report every one. It also
// requires a clean tree to be reported clean, so the gate is not simply
// answering "yes" to everything.
func TestPrincipalNameGateCatchesTheEscalation(t *testing.T) {
	t.Parallel()

	t.Run("clean source is clean", func(t *testing.T) {
		t.Parallel()
		root := writeGateFixture(t, cleanQuery, cleanRow, cleanValidator)

		findings, err := scanCredentialPrincipalQuery(root)
		if err != nil {
			t.Fatalf("scan query: %v", err)
		}
		if len(findings) != 0 {
			t.Errorf("clean query reported %d findings: %+v", len(findings), findings)
		}

		writes, parsed, err := scanPrincipalNameWrites(root)
		if err != nil {
			t.Fatalf("scan writes: %v", err)
		}
		if parsed == 0 {
			t.Fatal("parsed no files from the fixture")
		}
		if len(writes) != 0 {
			t.Errorf("clean source reported %d principal-name writes: %+v", len(writes), writes)
		}
	})

	t.Run("query that selects the token name", func(t *testing.T) {
		t.Parallel()
		root := writeGateFixture(t, poisonedQuery, cleanRow, cleanValidator)

		findings, err := scanCredentialPrincipalQuery(root)
		if err != nil {
			t.Fatalf("scan query: %v", err)
		}
		if len(findings) != 1 {
			t.Fatalf("findings = %d, want 1; a query that selects token.name must be refused: %+v",
				len(findings), findings)
		}
	})

	t.Run("generated row that carries the token name", func(t *testing.T) {
		t.Parallel()
		root := writeGateFixture(t, cleanQuery, poisonedRow, cleanValidator)

		findings, err := scanCredentialPrincipalQuery(root)
		if err != nil {
			t.Fatalf("scan query: %v", err)
		}
		if len(findings) != 1 {
			t.Fatalf("findings = %d, want 1; a row that carries Name must be refused: %+v",
				len(findings), findings)
		}
	})

	t.Run("literal that names the principal from the token", func(t *testing.T) {
		t.Parallel()
		root := writeGateFixture(t, cleanQuery, cleanRow, literalFromTokenName)

		writes, _, err := scanPrincipalNameWrites(root)
		if err != nil {
			t.Fatalf("scan writes: %v", err)
		}
		if len(writes) != 1 || !writes[0].fromTokenName {
			t.Fatalf("writes = %+v, want one write flagged as a token name", writes)
		}
		if !strings.Contains(writes[0].message(), "self-service") {
			t.Errorf("failure message does not state the escalation: %s", writes[0].message())
		}
	})

	t.Run("assignment that names the principal from the token", func(t *testing.T) {
		t.Parallel()
		root := writeGateFixture(t, cleanQuery, cleanRow, assignmentFromTokenName)

		writes, _, err := scanPrincipalNameWrites(root)
		if err != nil {
			t.Fatalf("scan writes: %v", err)
		}
		if len(writes) != 1 || !writes[0].fromTokenName {
			t.Fatalf("writes = %+v, want one write flagged as a token name", writes)
		}
	})

	// The renamed value is the case no pattern can recognise, and it is why the
	// gate also reports every write into the field.
	t.Run("literal that renames the value on the way", func(t *testing.T) {
		t.Parallel()
		root := writeGateFixture(t, cleanQuery, cleanRow, literalFromRenamedValue)

		writes, _, err := scanPrincipalNameWrites(root)
		if err != nil {
			t.Fatalf("scan writes: %v", err)
		}
		if len(writes) != 1 {
			t.Fatalf("writes = %+v, want one write reported for review", writes)
		}
		if writes[0].fromTokenName {
			t.Error("a renamed value must not be reported as a token name; it is reported as a new writer")
		}
		if !strings.Contains(writes[0].message(), "principalNameWriters") {
			t.Errorf("failure message does not ask for a written reason: %s", writes[0].message())
		}
	})
}

const (
	cleanQuery = `-- name: GetActivePATPrincipalByUUID :one
SELECT
    token.id AS token_id,
    owner.id AS user_id,
    COALESCE(owner.email, '')::text AS email,
    binding.project_id
FROM public.auth_core__token AS token;
`
	poisonedQuery = `-- name: GetActivePATPrincipalByUUID :one
SELECT
    token.id AS token_id,
    token.name AS token_name,
    owner.id AS user_id
FROM public.auth_core__token AS token;
`
	cleanRow = `package sqlcgen

type GetActivePATPrincipalByUUIDRow struct {
	TokenID   int32
	UserID    int32
	Email     string
	ProjectID *int32
}
`
	poisonedRow = `package sqlcgen

type GetActivePATPrincipalByUUIDRow struct {
	TokenID   int32
	Name      string
	UserID    int32
}
`
	cleanValidator = `package authsvc

func build(row Row) auth.User {
	return auth.User{ID: row.UserID, Email: row.Email}
}
`
	literalFromTokenName = `package authsvc

func build(tokenRow Row) auth.User {
	return auth.User{ID: tokenRow.UserID, Name: tokenRow.Name}
}
`
	assignmentFromTokenName = `package authsvc

func build(pat Row) auth.User {
	var user auth.User
	user.Name = pat.Name
	return user
}
`
	literalFromRenamedValue = `package authsvc

func build(principal Row) auth.User {
	return auth.User{ID: principal.UserID, Name: principal.Label}
}
`
)

// writeGateFixture lays out the three files the scanners read, under a
// throwaway root.
func writeGateFixture(t *testing.T, query, row, validator string) string {
	t.Helper()

	root := t.TempDir()
	files := map[string]string{
		"internal/db/queries/auth_pat.sql":     query,
		"internal/db/sqlcgen/auth_pat.sql.go":  row,
		"internal/infra/authsvc/validator.go":  validator,
		"internal/infra/authsvc/unrelated.go":  unrelatedNameWrites,
		"internal/db/sqlcgen/auth_pat_test.go": literalFromTokenName, // a _test.go file is not scanned
	}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return root
}

// unrelatedNameWrites is the noise floor. None of these is a principal name, so
// none may be reported. Without this file the gate could pass its own mutation
// check while flagging every Name field in the service.
const unrelatedNameWrites = `package authsvc

func unrelated(row Row, bucket Bucket, token Token) {
	item := Item{Name: row.Title}
	bucket.Name = row.Label
	other := Cookie{Name: "elitea_session"}
	label := Label{Name: token.Label}
	_, _, _, _ = item, bucket, other, label
}
`
