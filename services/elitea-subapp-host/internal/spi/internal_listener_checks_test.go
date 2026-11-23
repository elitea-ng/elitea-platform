package spi_test

// The checks of the internal-listener surface test (ADR-0059 R1.8). The same
// file is in services/elitea-main/internal/transport and
// services/elitea-scheduler/cmd/elitea-scheduler, each module with its own
// allowlist; keep the three copies alike.
//
// The allowlist is reviewed text, one entry a line, fields separated by "|":
//
//	grpc | <listener> | /<service>/<method> | <writes> | <decision> | <caller>
//	http | <file>:<func> | <mux pattern>   | <writes> | <decision> | <caller>
//	site | <file>:<func> | <call>
//
// writes:   read (changes nothing), write (changes run, content, project or
//           metering data, but no provider, policy, identity or role data),
//           admin (can change provider, policy, identity or role data).
// decision: main-route (runs elitea-main's public HTTP route in process:
//           Main's authentication and permission checks decide for the
//           caller's own principal), claim (an execution claim and fence
//           Main issued, bound to the peer), workload (the caller's workload
//           identity on its mTLS certificate), none (no credential: probes
//           and metrics).
// A site is a place in the source that builds a listener (grpc.NewServer,
// http.Server, http.NewServeMux) or registers a gRPC service
// (Register<Service>Server, RegisterService).

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/grpc"
)

type surfaceEntry struct {
	kind     string
	listener string
	name     string
	writes   string
	decision string
	caller   string
	line     int
}

func (e surfaceEntry) key() string { return e.kind + " " + e.listener + " " + e.name }

var (
	surfaceWrites    = []string{"read", "write", "admin"}
	surfaceDecisions = []string{"main-route", "claim", "workload", "none"}
)

func loadAllowlist(t *testing.T, file string) []surfaceEntry {
	t.Helper()
	handle, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Close() }()
	var entries []surfaceEntry
	seen := map[string]int{}
	scanner := bufio.NewScanner(handle)
	for number := 1; scanner.Scan(); number++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Split(text, "|")
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}
		entry := surfaceEntry{kind: fields[0], line: number}
		switch {
		case entry.kind == "site" && len(fields) == 3:
			entry.listener, entry.name = fields[1], fields[2]
		case (entry.kind == "grpc" || entry.kind == "http") && len(fields) == 6:
			entry.listener, entry.name, entry.writes, entry.decision, entry.caller = fields[1], fields[2], fields[3], fields[4], fields[5]
			if !slices.Contains(surfaceWrites, entry.writes) || !slices.Contains(surfaceDecisions, entry.decision) || entry.caller == "" {
				t.Fatalf("%s:%d: writes must be one of %v, decision one of %v, and the caller named: %q", file, number, surfaceWrites, surfaceDecisions, text)
			}
		default:
			t.Fatalf("%s:%d: not an allowlist entry: %q", file, number, text)
		}
		if previous, duplicate := seen[entry.key()]; duplicate {
			t.Fatalf("%s:%d: repeats line %d", file, number, previous)
		}
		seen[entry.key()] = number
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return entries
}

// ruleProblem is the rule every classified entry obeys: a method that can
// change administration data runs Main's own route, where the decision is
// made for the caller's principal; a write is never undecided.
func ruleProblem(entry surfaceEntry) string {
	switch {
	case entry.writes == "admin" && entry.decision != "main-route":
		return fmt.Sprintf("allowlist line %d: %s can change administration data without an authorization decision in elitea-main (decision %s; it must be main-route)", entry.line, entry.name, entry.decision)
	case entry.writes != "read" && entry.decision == "none":
		return fmt.Sprintf("allowlist line %d: %s writes with no decision", entry.line, entry.name)
	}
	return ""
}

// checkServed compares what the composed gRPC listeners serve with the
// grpc entries of the allowlist, both ways.
func checkServed(served map[string]map[string]grpc.ServiceInfo, list []surfaceEntry) []string {
	var problems []string
	listed := map[string]bool{}
	for _, entry := range list {
		if entry.kind != "grpc" {
			continue
		}
		listed[entry.key()] = true
		if problem := ruleProblem(entry); problem != "" {
			problems = append(problems, problem)
		}
	}
	found := map[string]bool{}
	for listener, services := range served {
		for service, info := range services {
			for _, method := range info.Methods {
				entry := surfaceEntry{kind: "grpc", listener: listener, name: "/" + service + "/" + method.Name}
				found[entry.key()] = true
				if !listed[entry.key()] {
					problems = append(problems, fmt.Sprintf("the %s listener serves %s, which is not on the allowlist: review what it writes, what decides it and who calls it, and list it", listener, entry.name))
				}
			}
		}
	}
	for _, entry := range list {
		if entry.kind == "grpc" && !found[entry.key()] {
			problems = append(problems, fmt.Sprintf("allowlist line %d: the %s listener does not serve %s (remove the line, or compose the service in the test)", entry.line, entry.listener, entry.name))
		}
	}
	slices.Sort(problems)
	return problems
}

// sourceScan is what the module's source builds and registers.
type sourceScan struct {
	sites  map[string]string // "file:func call" -> position
	routes map[string]string // "file:func pattern" -> position
}

func skippedDir(name string) bool {
	return name == "testdata" || name == "gen" || name == "vendor" || name == "node_modules" ||
		strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") ||
		// Test-helper packages (llmproxytest, providertest, spitest, ...)
		// serve fakes to tests only.
		strings.HasSuffix(name, "test")
}

var registerServer = regexp.MustCompile(`^Register[A-Za-z0-9]+Server$`)

// scanSource reads every non-test Go file under root.
func scanSource(t *testing.T, root string) sourceScan {
	t.Helper()
	scan := sourceScan{sites: map[string]string{}, routes: map[string]string{}}
	fileSet := token.NewFileSet()
	err := filepath.WalkDir(root, func(file string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if file != root && skippedDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(fileSet, file, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, file)
		if err != nil {
			return err
		}
		scanFile(fileSet, filepath.ToSlash(relative), parsed, scan)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return scan
}

func scanFile(fileSet *token.FileSet, relative string, file *ast.File, scan sourceScan) {
	imports := map[string]string{}
	for _, spec := range file.Imports {
		importPath, _ := strconv.Unquote(spec.Path.Value)
		name := path.Base(importPath)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		imports[name] = importPath
	}
	isPackage := func(expr ast.Expr, importPath string) bool {
		ident, ok := expr.(*ast.Ident)
		return ok && imports[ident.Name] == importPath
	}
	visit := func(owner string, node ast.Node) {
		site := relative + ":" + owner
		muxes := map[string]bool{}
		ast.Inspect(node, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				for i, value := range n.Rhs {
					if call, ok := value.(*ast.CallExpr); ok && i < len(n.Lhs) {
						if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "NewServeMux" && isPackage(selector.X, "net/http") {
							if ident, ok := n.Lhs[i].(*ast.Ident); ok {
								muxes[ident.Name] = true
							}
						}
					}
				}
			case *ast.CompositeLit:
				if selector, ok := n.Type.(*ast.SelectorExpr); ok && selector.Sel.Name == "Server" && isPackage(selector.X, "net/http") {
					scan.sites[site+" http.Server"] = fileSet.Position(n.Pos()).String()
				}
			case *ast.CallExpr:
				selector, ok := n.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				name := selector.Sel.Name
				switch {
				case name == "NewServer" && isPackage(selector.X, "google.golang.org/grpc"):
					scan.sites[site+" grpc.NewServer"] = fileSet.Position(n.Pos()).String()
				case name == "NewServeMux" && isPackage(selector.X, "net/http"):
					scan.sites[site+" http.NewServeMux"] = fileSet.Position(n.Pos()).String()
				case name == "RegisterService":
					scan.sites[site+" RegisterService"] = fileSet.Position(n.Pos()).String()
				case registerServer.MatchString(name):
					if ident, ok := selector.X.(*ast.Ident); ok && imports[ident.Name] != "" {
						scan.sites[site+" "+name] = fileSet.Position(n.Pos()).String()
					}
				case name == "Handle" || name == "HandleFunc":
					ident, ok := selector.X.(*ast.Ident)
					if !ok || !muxes[ident.Name] || len(n.Args) == 0 {
						return true
					}
					pattern := "<dynamic>"
					if literal, ok := n.Args[0].(*ast.BasicLit); ok && literal.Kind == token.STRING {
						pattern, _ = strconv.Unquote(literal.Value)
					}
					scan.routes[site+" "+pattern] = fileSet.Position(n.Pos()).String()
				}
			}
			return true
		})
	}
	for _, declaration := range file.Decls {
		switch declaration := declaration.(type) {
		case *ast.FuncDecl:
			owner := declaration.Name.Name
			if declaration.Recv != nil && len(declaration.Recv.List) == 1 {
				receiver := declaration.Recv.List[0].Type
				if star, ok := receiver.(*ast.StarExpr); ok {
					receiver = star.X
				}
				if index, ok := receiver.(*ast.IndexExpr); ok {
					receiver = index.X
				}
				if ident, ok := receiver.(*ast.Ident); ok {
					owner = ident.Name + "." + owner
				}
			}
			visit(owner, declaration)
		case *ast.GenDecl:
			visit("package", declaration)
		}
	}
}

// checkSource compares the scanned sites and mux routes with the site and
// http entries of the allowlist, both ways.
func checkSource(scan sourceScan, list []surfaceEntry) []string {
	var problems []string
	sites, routes := map[string]bool{}, map[string]bool{}
	for _, entry := range list {
		switch entry.kind {
		case "site":
			sites[entry.listener+" "+entry.name] = true
		case "http":
			routes[entry.listener+" "+entry.name] = true
			if problem := ruleProblem(entry); problem != "" {
				problems = append(problems, problem)
			}
		}
	}
	for key, position := range scan.sites {
		if !sites[key] {
			problems = append(problems, fmt.Sprintf("%s: %s builds a listener or registers a gRPC service and is not on the allowlist: review it and list it", position, key))
		}
	}
	for key, position := range scan.routes {
		if !routes[key] {
			problems = append(problems, fmt.Sprintf("%s: the internal HTTP route %s is not on the allowlist: review what it writes and list it", position, key))
		}
	}
	for _, entry := range list {
		key := entry.listener + " " + entry.name
		if entry.kind == "site" && scan.sites[key] == "" {
			problems = append(problems, fmt.Sprintf("allowlist line %d: no %s at %s (remove the line)", entry.line, entry.name, entry.listener))
		}
		if entry.kind == "http" && scan.routes[key] == "" {
			problems = append(problems, fmt.Sprintf("allowlist line %d: no route %s at %s (remove the line)", entry.line, entry.name, entry.listener))
		}
	}
	slices.Sort(problems)
	return problems
}

// scanAdministrationMode reads the non-test Go files of one package
// directory and flags every reference to a PermissionModeAdministration
// constant and every "administration" literal passed to a call: an internal
// handler resolving that mode would pick up the caller's elevation.
func scanAdministrationMode(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var problems []string
	fileSet := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fileSet, file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Ident:
				if n.Name == "PermissionModeAdministration" {
					problems = append(problems, fmt.Sprintf("%s: an internal handler names the administration permission mode", fileSet.Position(n.Pos())))
				}
			case *ast.CallExpr:
				for _, argument := range n.Args {
					if literal, ok := argument.(*ast.BasicLit); ok && literal.Kind == token.STRING {
						if value, _ := strconv.Unquote(literal.Value); value == "administration" {
							problems = append(problems, fmt.Sprintf("%s: an internal handler passes the administration permission mode", fileSet.Position(literal.Pos())))
						}
					}
				}
			}
			return true
		})
	}
	return problems
}

const (
	architectureHeading = "## Internal listeners and administration data"
	architectureBegin   = "<!-- internal-admin-writes:begin -->"
	architectureEnd     = "<!-- internal-admin-writes:end -->"
)

// checkArchitecture compares the architecture page's table of internal
// methods that change administration data with the allowlist's admin
// entries, for one service.
func checkArchitecture(t *testing.T, file, service string, list []surfaceEntry) []string {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "\n"+architectureHeading+"\n") {
		return []string{fmt.Sprintf("%s has no %q section", file, architectureHeading)}
	}
	_, rest, ok := strings.Cut(text, architectureBegin)
	table, _, closed := strings.Cut(rest, architectureEnd)
	if !ok || !closed {
		return []string{fmt.Sprintf("%s has no %s ... %s table", file, architectureBegin, architectureEnd)}
	}
	documented := map[string]bool{}
	for _, row := range strings.Split(table, "\n") {
		row = strings.TrimSpace(row)
		if !strings.HasPrefix(row, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(row, "|"), "|")
		if len(cells) != 4 {
			return []string{fmt.Sprintf("%s: a row of the internal administration writes needs 4 cells: %q", file, row)}
		}
		for i := range cells {
			cells[i] = strings.Trim(strings.TrimSpace(cells[i]), "`")
		}
		if cells[0] != service {
			continue
		}
		documented[cells[1]+" "+cells[2]+" "+cells[3]] = true
	}
	var problems []string
	expected := map[string]bool{}
	for _, entry := range list {
		if entry.kind == "site" || entry.writes != "admin" {
			continue
		}
		key := entry.listener + " " + entry.name + " " + entry.caller
		expected[key] = true
		if !documented[key] {
			problems = append(problems, fmt.Sprintf("%s does not list %s on the %s listener with the caller %q (allowlist line %d)", file, entry.name, entry.listener, entry.caller, entry.line))
		}
	}
	for key := range documented {
		if !expected[key] {
			problems = append(problems, fmt.Sprintf("%s lists %q for %s, which the allowlist does not mark admin", file, key, service))
		}
	}
	slices.Sort(problems)
	return problems
}
