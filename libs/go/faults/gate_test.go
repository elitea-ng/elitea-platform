package faults

// The FU-299 gates. They are ordinary Go tests in this module, so the
// existing CI runs them (scripts/go/workspace-run.sh tests every go.work
// module) with no workflow change (C5).

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var (
	codeGrammar = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
	// C7: no placeholder, format verb, id, path or URL in what a person reads.
	bannedText = []*regexp.Regexp{
		regexp.MustCompile(`[{%]`),
		regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-`),                  // uuid
		regexp.MustCompile(`\b[0-9a-f]{12,}\b`),                         // hex id
		regexp.MustCompile(`://`),                                       // URL
		regexp.MustCompile(`(^|\s)(/|\./|~/)[A-Za-z0-9_.-]+/`),          // unix path
		regexp.MustCompile(`\.(go|rs|json|yaml|yml|toml|sql|ts|tsx)\b`), // source file
	}
	audiences  = map[string]bool{"operator": true, "user": true, "both": true}
	retryables = map[string]bool{"yes": true, "no": true, "n/a": true}
)

func TestRegistryIsWellFormed(t *testing.T) {
	entries := Registry()
	if len(entries) == 0 {
		t.Fatal("the registry is empty")
	}
	exempt := readList(t, "multi-sentence-exemptions.txt")
	for code := range exempt {
		if entry, ok := byCode[code]; !ok || entry.RuntimeKind == "" {
			t.Errorf("multi-sentence-exemptions.txt lists %q, which is not an imported runtime failure; delete the line", code)
		}
	}
	for _, entry := range entries {
		if !exempt[entry.Code] && !oneSentence(entry.Sentence) {
			t.Errorf("%s: %s: the sentence is more than one sentence; say it in one", entry.File, entry.Code)
		}
		where := entry.File + ": " + entry.Code
		if !codeGrammar.MatchString(entry.Code) {
			t.Errorf("%s: the code is not area.reason in lower snake case", where)
		}
		for field, text := range map[string]string{"sentence": entry.Sentence, "action": entry.Action} {
			switch {
			case strings.TrimSpace(text) == "":
				t.Errorf("%s: no %s", where, field)
			case len(text) > 256:
				t.Errorf("%s: the %s is %d bytes, more than 256", where, field, len(text))
			case !strings.HasSuffix(text, "."):
				t.Errorf("%s: the %s does not end with a period", where, field)
			}
			for _, banned := range bannedText {
				if banned.MatchString(text) {
					t.Errorf("%s: the %s holds %q, which a person must not read (no placeholder, verb, id, path or URL)", where, field, banned.FindString(text))
				}
			}
		}
		if !audiences[entry.Audience] {
			t.Errorf("%s: audience %q is not operator, user or both", where, entry.Audience)
		}
		if !retryables[entry.Retryable] {
			t.Errorf("%s: retryable %q is not yes, no or n/a", where, entry.Retryable)
		}
		if strings.TrimSpace(entry.Owner) == "" {
			t.Errorf("%s: no owner", where)
		}
	}
}

func TestGeneratedCodesAreCurrent(t *testing.T) {
	raw, err := os.ReadFile("codes_gen.go")
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != GeneratedCodesSource() {
		t.Fatal("codes_gen.go is stale: run `go generate ./libs/go/faults` and commit the result")
	}
}

func TestGuideIsCurrent(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repositoryRoot(t), "docs", "errors", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != RenderGuide() {
		t.Fatal("docs/errors/README.md is stale: run `go generate ./libs/go/faults` and commit the result")
	}
}

// A code exists only as a generated constant: faults.Code("…") anywhere in
// the repository's Go code is refused, so a new code cannot ship without a
// registry entry (and so without its sentence).
func TestNoCodeIsMadeByHand(t *testing.T) {
	root := repositoryRoot(t)
	for _, file := range sourceFiles(t, root, ".go") {
		if strings.HasSuffix(file, "codes_gen.go") || strings.Contains(file, "/libs/go/faults/") && strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			continue // generated or build-tagged oddities are not this gate's concern
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				return true
			}
			if !isCodeConversion(call.Fun, parsed.Name.Name) {
				return true
			}
			if _, literal := call.Args[0].(*ast.BasicLit); literal {
				rel, _ := filepath.Rel(root, file)
				t.Errorf("%s: a faults.Code is made from a string literal; add the code to libs/go/faults/registry and use its generated constant", rel)
			}
			return true
		})
	}
}

func isCodeConversion(fun ast.Expr, pkg string) bool {
	switch f := fun.(type) {
	case *ast.SelectorExpr:
		ident, ok := f.X.(*ast.Ident)
		return ok && ident.Name == "faults" && f.Sel.Name == "Code"
	case *ast.Ident:
		return pkg == "faults" && f.Name == "Code"
	}
	return false
}

// Every registered code is raised somewhere, so the guide documents no dead
// codes. A code counts as used when Go names its constant, Rust names its
// constant (codes::UPPER_SNAKE), or it is a runtime failure pinned by the
// Worker's parity test (runtime_kind).
func TestEveryRegisteredCodeIsUsed(t *testing.T) {
	root := repositoryRoot(t)
	var corpus strings.Builder
	for _, file := range append(sourceFiles(t, root, ".go"), sourceFiles(t, root, ".rs")...) {
		if strings.HasSuffix(file, "codes_gen.go") || strings.Contains(file, "/libs/go/faults/registry/") {
			continue
		}
		raw, err := os.ReadFile(file)
		if err == nil {
			corpus.Write(raw)
			corpus.WriteByte('\n')
		}
	}
	text := corpus.String()
	for _, entry := range Registry() {
		if entry.RuntimeKind != "" {
			continue
		}
		goName := ConstName(entry.Code)
		rustName := strings.ToUpper(strings.NewReplacer(".", "_").Replace(entry.Code))
		if !strings.Contains(text, "faults."+goName) && !regexp.MustCompile(`\b`+goName+`\b`).MatchString(text) && !strings.Contains(text, "codes::"+rustName) {
			t.Errorf("%s (%s) is registered but nothing raises it; remove it or use it", entry.Code, entry.File)
		}
	}
}

// Old-style codes (Rust `fn code(&self) -> &'static str` and `as_str`
// literals, Go "reason" literals in error bodies) are frozen in
// legacy-codes.txt. A new one fails: register it instead. The sweep deletes
// lines as it migrates codes; the file can only shrink.
func TestLegacyCodesOnlyShrink(t *testing.T) {
	root := repositoryRoot(t)
	found := legacyCodes(t, root)
	registered := map[string]bool{}
	for _, entry := range Registry() {
		registered[entry.Code] = true
	}
	baseline := readBaseline(t)
	if os.Getenv("FAULTS_WRITE_LEGACY_BASELINE") == "1" {
		writeBaseline(t, found)
		t.Skip("legacy-codes.txt rewritten from the tree; review the diff: it must not grow")
	}
	for code, where := range found {
		if !baseline[code] && !registered[code] {
			t.Errorf("%s: %q is a new unregistered code; add it to libs/go/faults/registry and use the generated constant", where, code)
		}
	}
	for code := range baseline {
		if _, ok := found[code]; !ok {
			t.Errorf("legacy-codes.txt lists %q, which no source raises any more; delete the line", code)
		}
	}
}

var (
	rustCodeFn   = regexp.MustCompile(`fn (code|as_str)\(&?self\) -> &'static str \{`)
	stringLit    = regexp.MustCompile(`"([a-z][a-z0-9_]*(?:\.[a-z][a-z0-9_]*)*)"`)
	goReasonLit  = regexp.MustCompile(`"reason":\s*"([a-z][a-z0-9_]*)"`)
	legacyCodeOK = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9_]+)*$`)
)

func legacyCodes(t *testing.T, root string) map[string]string {
	t.Helper()
	found := map[string]string{}
	for _, file := range sourceFiles(t, root, ".rs") {
		if strings.Contains(file, "_tests.rs") || strings.Contains(file, "/tests/") || strings.Contains(file, "/vendor/") || strings.Contains(file, "/target/") {
			continue
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		text := string(raw)
		for _, loc := range rustCodeFn.FindAllStringIndex(text, -1) {
			body := braceBody(text, loc[1]-1)
			for _, m := range stringLit.FindAllStringSubmatch(body, -1) {
				if legacyCodeOK.MatchString(m[1]) && strings.ContainsAny(m[1], "._") {
					rel, _ := filepath.Rel(root, file)
					found[m[1]] = rel
				}
			}
		}
	}
	for _, file := range sourceFiles(t, filepath.Join(root, "services", "elitea-main", "internal", "api"), ".go") {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for _, m := range goReasonLit.FindAllStringSubmatch(string(raw), -1) {
			rel, _ := filepath.Rel(root, file)
			found[m[1]] = rel
		}
	}
	return found
}

// braceBody is the text between the brace at open and its match.
func braceBody(text string, open int) string {
	depth := 0
	for i := open; i < len(text); i++ {
		switch text[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return text[open : i+1]
			}
		}
	}
	return text[open:]
}

// oneSentence: one terminal period and no sentence boundary inside.
func oneSentence(text string) bool {
	inner := strings.TrimSuffix(text, ".")
	return !strings.Contains(inner, ". ") && !strings.Contains(inner, "? ") && !strings.Contains(inner, "! ")
}

func readBaseline(t *testing.T) map[string]bool {
	t.Helper()
	return readList(t, "legacy-codes.txt")
}

func readList(t *testing.T, name string) map[string]bool {
	t.Helper()
	baseline := map[string]bool{}
	file, err := os.Open(name)
	if err != nil {
		if os.IsNotExist(err) {
			return baseline
		}
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			baseline[line] = true
		}
	}
	return baseline
}

func writeBaseline(t *testing.T, found map[string]string) {
	t.Helper()
	codes := make([]string, 0, len(found))
	for code := range found {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	content := "# Old-style codes frozen by FU-299 (TestLegacyCodesOnlyShrink). This file only shrinks:\n# register a code in libs/go/faults/registry instead of adding a line.\n" + strings.Join(codes, "\n") + "\n"
	if err := os.WriteFile("legacy-codes.txt", []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.work is in no parent directory")
		}
		dir = parent
	}
}

func sourceFiles(t *testing.T, root, ext string) []string {
	t.Helper()
	var files []string
	skip := map[string]bool{"node_modules": true, "target": true, "vendor": true, ".git": true, "dist": true, ".worktrees": true}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if skip[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ext) {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
