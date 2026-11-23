package spi_test

// The checks of the internal-listener surface test (ADR-0059 R1.8, R1.9).
// The same file is in services/elitea-main/internal/transport,
// services/elitea-scheduler/cmd/elitea-scheduler,
// services/elitea-subapp-host/internal/spi and, outside go.work,
// services/elitea-llm-gateway/internal/grpcapi, each module with its own
// allowlist. TestTheListenerChecksCopiesAreAlike (elitea-main, ci-go) and its
// gateway twin (ci-gateway) fail when the copies differ beyond the package
// clause.
//
// The allowlist is reviewed text, one entry a line, fields separated by "|":
//
//	grpc   | <listener>    | /<service>/<method> | <writes> | <decision> | <caller>
//	http   | <file>:<func> | <mux pattern>       | <writes> | <decision> | <caller>
//	site   | <file>:<func> | <call>              | <count>
//	helper | <dir>         | <why the source scan skips it>
//	mode   | <file>        | <why it may name the administration mode>
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
//
// writes and decision are DECLARED by the reviewer of a line; the test cannot
// prove what a handler writes. What it proves: every served method, route,
// listener and registration is on the list (so each one was reviewed); a
// method declared admin is decided by Main's route and documented; and no
// internal handler, nor any package it reaches, names the administration
// permission mode outside the reviewed mode lines.
//
// A site is a place in the source that builds a listener (grpc.NewServer,
// http.Server, http.NewServeMux, net.Listen, tls.Listen, http.ListenAndServe,
// http.Serve and their TLS forms), registers on the default mux
// (http.Handle, http.HandleFunc) or registers a gRPC service
// (Register<Service>Server, RegisterService); count is how many times it
// occurs in that function. A route is a Handle or HandleFunc call in a file
// that names http.ServeMux, http.NewServeMux or the default mux.

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
	count    int
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
		case entry.kind == "site" && len(fields) == 4:
			entry.listener, entry.name = fields[1], fields[2]
			entry.count, err = strconv.Atoi(fields[3])
			if err != nil || entry.count < 1 {
				t.Fatalf("%s:%d: a site's count is a positive number: %q", file, number, text)
			}
		case (entry.kind == "helper" || entry.kind == "mode") && len(fields) == 3 && fields[2] != "":
			entry.listener, entry.caller = fields[1], fields[2]
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

// entriesOf is the allowlist's entries of one kind, keyed by listener (the
// directory of a helper line, the file of a mode line).
func entriesOf(list []surfaceEntry, kind string) map[string]surfaceEntry {
	out := map[string]surfaceEntry{}
	for _, entry := range list {
		if entry.kind == kind {
			out[entry.listener] = entry
		}
	}
	return out
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
	sites   map[string][]string // "file:func call" -> positions
	routes  map[string][]string // "file:func pattern" -> positions
	helpers map[string]bool     // helper directories the scan skipped
	imports map[string][]string // import path -> positions, of the scanned files
}

// alwaysSkipped are directories no build reads.
func alwaysSkipped(name string) bool {
	return name == "testdata" || name == "vendor" || name == "node_modules" ||
		strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

var registerServer = regexp.MustCompile(`^Register[A-Za-z0-9]+Server$`)

// listenerCalls are the package functions that open a listener or register
// on the default mux, by import path.
var listenerCalls = map[string][]string{
	"net":                    {"Listen", "ListenTCP", "ListenUnix", "FileListener"},
	"net/http":               {"ListenAndServe", "ListenAndServeTLS", "Serve", "ServeTLS", "Handle", "HandleFunc"},
	"crypto/tls":             {"Listen", "NewListener"},
	"google.golang.org/grpc": {"NewServer"},
}

// scanSource reads every non-test Go file under root, except the helper
// directories the allowlist names (test-helper packages that serve fakes to
// tests only, imported by no scanned file).
func scanSource(t *testing.T, root string, list []surfaceEntry) sourceScan {
	t.Helper()
	helpers := entriesOf(list, "helper")
	scan := sourceScan{sites: map[string][]string{}, routes: map[string][]string{}, helpers: map[string]bool{}, imports: map[string][]string{}}
	fileSet := token.NewFileSet()
	err := filepath.WalkDir(root, func(file string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, file)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			if file != root && alwaysSkipped(entry.Name()) {
				return filepath.SkipDir
			}
			if _, ok := helpers[relative]; ok {
				scan.helpers[relative] = true
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
		scanFile(fileSet, relative, parsed, scan)
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
		scan.imports[importPath] = append(scan.imports[importPath], fileSet.Position(spec.Pos()).String())
	}
	packageOf := func(expr ast.Expr) string {
		if ident, ok := expr.(*ast.Ident); ok {
			return imports[ident.Name]
		}
		return ""
	}
	// A file that names a ServeMux or the default mux registers routes with
	// every Handle and HandleFunc call it makes, whatever holds the mux (a
	// local, a parameter, a field, a package variable).
	hasMux := false
	ast.Inspect(file, func(n ast.Node) bool {
		if selector, ok := n.(*ast.SelectorExpr); ok && packageOf(selector.X) == "net/http" {
			switch selector.Sel.Name {
			case "ServeMux", "NewServeMux", "DefaultServeMux", "Handle", "HandleFunc":
				hasMux = true
			}
		}
		return true
	})
	visit := func(owner string, node ast.Node) {
		site := relative + ":" + owner
		record := func(into map[string][]string, key string, pos token.Pos) {
			into[key] = append(into[key], fileSet.Position(pos).String())
		}
		ast.Inspect(node, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CompositeLit:
				if selector, ok := n.Type.(*ast.SelectorExpr); ok && selector.Sel.Name == "Server" && packageOf(selector.X) == "net/http" {
					record(scan.sites, site+" http.Server", n.Pos())
				}
			case *ast.SelectorExpr:
				// A reference, called or passed as a value (listen := net.Listen).
				importPath := packageOf(n.X)
				if slices.Contains(listenerCalls[importPath], n.Sel.Name) {
					record(scan.sites, site+" "+path.Base(importPath)+"."+n.Sel.Name, n.Pos())
				}
				if importPath == "net/http" && n.Sel.Name == "NewServeMux" {
					record(scan.sites, site+" http.NewServeMux", n.Pos())
				}
				if importPath != "" && registerServer.MatchString(n.Sel.Name) {
					record(scan.sites, site+" "+n.Sel.Name, n.Pos())
				}
			case *ast.CallExpr:
				selector, ok := n.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				name := selector.Sel.Name
				switch {
				case name == "RegisterService":
					record(scan.sites, site+" RegisterService", n.Pos())
				case (name == "Handle" || name == "HandleFunc") && hasMux && len(n.Args) > 0:
					pattern := "<dynamic>"
					if literal, ok := n.Args[0].(*ast.BasicLit); ok && literal.Kind == token.STRING {
						pattern, _ = strconv.Unquote(literal.Value)
					}
					record(scan.routes, site+" "+pattern, n.Pos())
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

// checkSource compares the scanned sites (with their counts) and routes with
// the site and http entries of the allowlist, both ways, and the helper
// lines with the directories the scan skipped.
func checkSource(scan sourceScan, list []surfaceEntry) []string {
	var problems []string
	sites, routes := map[string]surfaceEntry{}, map[string]bool{}
	for _, entry := range list {
		switch entry.kind {
		case "site":
			sites[entry.listener+" "+entry.name] = entry
		case "http":
			routes[entry.listener+" "+entry.name] = true
			if problem := ruleProblem(entry); problem != "" {
				problems = append(problems, problem)
			}
		case "helper":
			if !scan.helpers[entry.listener] {
				problems = append(problems, fmt.Sprintf("allowlist line %d: no helper directory %s (remove the line)", entry.line, entry.listener))
			}
			// A helper serves tests only: a scanned (non-test, non-helper)
			// file that imports it would put it in a binary unreviewed.
			for importPath, positions := range scan.imports {
				if strings.HasSuffix(importPath, "/"+entry.listener) {
					problems = append(problems, fmt.Sprintf("%s: imports the helper directory %s, which the source scan skips as test-only (allowlist line %d)", strings.Join(positions, ", "), entry.listener, entry.line))
				}
			}
		}
	}
	for key, positions := range scan.sites {
		entry, ok := sites[key]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("%s: %s builds a listener or registers a gRPC service and is not on the allowlist: review it and list it", strings.Join(positions, ", "), key))
		case entry.count != len(positions):
			problems = append(problems, fmt.Sprintf("allowlist line %d: %s occurs %d times, not %d (%s): review the new one", entry.line, key, len(positions), entry.count, strings.Join(positions, ", ")))
		}
	}
	for key, positions := range scan.routes {
		if !routes[key] || len(positions) != 1 {
			problems = append(problems, fmt.Sprintf("%s: the internal HTTP route %s is not on the allowlist once: review what it writes and list it", strings.Join(positions, ", "), key))
		}
	}
	for _, entry := range list {
		key := entry.listener + " " + entry.name
		if entry.kind == "site" && len(scan.sites[key]) == 0 {
			problems = append(problems, fmt.Sprintf("allowlist line %d: no %s at %s (remove the line)", entry.line, entry.name, entry.listener))
		}
		if entry.kind == "http" && len(scan.routes[key]) == 0 {
			problems = append(problems, fmt.Sprintf("allowlist line %d: no route %s at %s (remove the line)", entry.line, entry.name, entry.listener))
		}
	}
	slices.Sort(problems)
	return problems
}

// administrationModeName is an identifier that names the administration
// permission mode: auth.PermissionModeAdministration,
// adminaccess.ModeAdministration, identity.InitialAdministrationMode, ...
func administrationModeName(name string) bool {
	return strings.Contains(name, "ModeAdministration") || strings.Contains(name, "AdministrationMode")
}

// scanAdministrationMode reads the non-test Go files of one package
// directory and flags every identifier that names the administration
// permission mode and every "administration" string literal: an internal
// handler resolving that mode would pick up the caller's elevation. A
// directory with no non-test Go file fails: a renamed package must not leave
// the scan reading nothing.
func scanAdministrationMode(t *testing.T, dir string) []string {
	t.Helper()
	hits, _ := administrationModeHits(t, dir)
	return hits
}

// administrationModeHits is scanAdministrationMode's findings, one per
// position, and the import paths of the package.
func administrationModeHits(t *testing.T, dir string) ([]string, []string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var problems, imports []string
	fileSet := token.NewFileSet()
	read := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		read++
		parsed, err := parser.ParseFile(fileSet, file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range parsed.Imports {
			importPath, _ := strconv.Unquote(spec.Path.Value)
			imports = append(imports, importPath)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Ident:
				if administrationModeName(n.Name) {
					problems = append(problems, fmt.Sprintf("%s: names the administration permission mode (%s)", fileSet.Position(n.Pos()), n.Name))
				}
			case *ast.BasicLit:
				if n.Kind == token.STRING {
					if value, _ := strconv.Unquote(n.Value); value == "administration" {
						problems = append(problems, fmt.Sprintf("%s: names the administration permission mode (a literal)", fileSet.Position(n.Pos())))
					}
				}
			}
			return true
		})
	}
	if read == 0 {
		t.Fatalf("%s has no non-test Go file to scan", dir)
	}
	return problems, imports
}

// scanAdministrationModeClosure scans the given package directories and
// every package of the module they import, directly or not. A finding in a
// file a mode line names (the constant's definition, the resolver, the
// public route middleware) is reviewed; a mode line that matches nothing
// fails. The starting packages themselves take no mode line.
func scanAdministrationModeClosure(t *testing.T, moduleRoot, modulePath string, starts []string, list []surfaceEntry) []string {
	t.Helper()
	reviewed := entriesOf(list, "mode")
	used := map[string]bool{}
	startSet := map[string]bool{}
	var queue, problems []string
	for _, start := range starts {
		start = filepath.Clean(start)
		startSet[start] = true
		queue = append(queue, start)
	}
	visited := map[string]bool{}
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		if visited[dir] {
			continue
		}
		visited[dir] = true
		hits, imports := administrationModeHits(t, dir)
		for _, hit := range hits {
			relative, err := filepath.Rel(moduleRoot, strings.SplitN(hit, ":", 2)[0])
			if err != nil {
				t.Fatal(err)
			}
			relative = filepath.ToSlash(relative)
			if _, ok := reviewed[relative]; ok && !startSet[dir] {
				used[relative] = true
				continue
			}
			problems = append(problems, hit)
		}
		for _, importPath := range imports {
			if rest, ok := strings.CutPrefix(importPath, modulePath+"/"); ok {
				queue = append(queue, filepath.Join(moduleRoot, filepath.FromSlash(rest)))
			}
		}
	}
	for file, entry := range reviewed {
		if !used[file] {
			problems = append(problems, fmt.Sprintf("allowlist line %d: %s does not name the administration mode in the scanned closure (remove the line)", entry.line, file))
		}
	}
	slices.Sort(problems)
	return problems
}

const (
	architectureHeading = "## Internal listeners and administration data"
	architectureBegin   = "<!-- internal-admin-writes:begin -->"
	architectureEnd     = "<!-- internal-admin-writes:end -->"
)

// checkArchitecture compares the architecture page's table of internal
// methods that change administration data with the allowlist's admin
// entries, for one service. The table must sit, once, in its section.
func checkArchitecture(t *testing.T, file, service string, list []surfaceEntry) []string {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Count(text, architectureBegin) != 1 || strings.Count(text, architectureEnd) != 1 {
		return []string{fmt.Sprintf("%s must hold exactly one %s ... %s table", file, architectureBegin, architectureEnd)}
	}
	_, section, ok := strings.Cut(text, "\n"+architectureHeading+"\n")
	if !ok {
		return []string{fmt.Sprintf("%s has no %q section", file, architectureHeading)}
	}
	if next := strings.Index(section, "\n## "); next >= 0 {
		section = section[:next]
	}
	_, rest, ok := strings.Cut(section, architectureBegin)
	table, _, closed := strings.Cut(rest, architectureEnd)
	if !ok || !closed {
		return []string{fmt.Sprintf("%s: the %s ... %s table is not inside the %q section", file, architectureBegin, architectureEnd, architectureHeading)}
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
		if (entry.kind != "grpc" && entry.kind != "http") || entry.writes != "admin" {
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
