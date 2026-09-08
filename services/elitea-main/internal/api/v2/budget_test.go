package v2_test

// ADR-0012's facade budget, made a gate.
//
// THE NUMBER WAS SET BEFORE THE WORK AND NEVER CHECKED. ADR-0012 measured the
// runner generalisation by what a SECOND provider facade costs: ≤8 files and
// ≤250 net non-test lines outside values.yaml. internal/api/v2/inventory's own
// header has quoted that budget since the facade landed, and nothing enforced
// it — a comment claiming a limit is the shape a limit takes when it has
// already stopped being true.
//
// WHAT IT MEASURES AND WHY. Net lines, not raw: blank lines and comments are
// excluded, because the point of the budget is how much per-provider MECHANISM
// a new facade carries, and this codebase deliberately writes long comments.
// A facade that grows past the budget has not failed a style rule — it has
// found something the shared packages (providerhost/{facade,proxy,routes,spi,
// material}) do not carry yet, and the fix belongs there.
//
// DEEPWIKI IS A RECORDED EXCEPTION, ratcheting. It is the FIRST facade: it
// predates the shared packages, it owns the wiki-specific field names and its
// own mTLS proxy type, and holding it to a budget written for the second would
// mean deleting behaviour to satisfy a number. What the exception is not is a
// pass — the recorded figures below are its CURRENT size, so the test fails
// the moment DeepWiki grows, and lowering them when it shrinks is the ratchet.

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type budget struct {
	files int
	lines int
	// why explains a recorded exception; empty for a facade held to ADR-0012.
	why string
}

var facadeBudgets = map[string]budget{
	// ADR-0012's budget, as written.
	"inventory": {files: 8, lines: 250},
	// The first facade's recorded size. It was 912 before the rewrite
	// mechanics moved to providerhost/material (ADR-0023 H4c I2), and 579
	// before the wiki chat gained server-side history; lower this again when
	// it shrinks again.
	//
	// THE HISTORY FEATURE RAISED THIS DELIBERATELY, AND THE SHARED HALF WENT
	// DOWN FIRST. Recording a wiki chat needs four things, and three of them
	// are nobody's in particular: reading the SPI poll envelope, deciding
	// whether a poll was terminal, turning a `result` into text, and teeing a
	// hop so a facade can watch the payload it is proxying. The frozen SPI
	// contract says it "belongs to no provider", Inventory polls that same
	// route, and the browser had already made this exact extraction and
	// recorded why (entities/provider-run, ADR-0023 decision 4). All of it is
	// now internal/providerhost/run — 117 of the 276 net lines the feature
	// added.
	//
	// The 159 that stayed cannot follow without inventing a framework for one
	// caller: the two headers this drawer sends, the map from a tool name to
	// which of the two agents ran, the parameter DeepWiki's tools carry a
	// question in, and the wiring to a store only this feature has. The ninth
	// file is that feature (history.go); folding it into invoke.go to hold a
	// file count would be the "deleting behaviour to satisfy a number" this
	// gate's own header warns against.
	// THE FOLDER SOURCE ADDED ONE LINE, AND THAT IS THE WHOLE FACADE COST.
	// A wiki may now be generated from an artifact folder instead of a
	// repository toolkit. Everything that feature needs of a facade — reading
	// the source out of the body, elitea-main's own bucket and object-key
	// rules, refusing a body that names two sources, and deriving the
	// repository the provider reads — is in providerhost/material
	// (artifact.go, ReferenceRewriter.folderInto), because none of it is
	// DeepWiki's: any provider that indexes a directory can name one. What is
	// DeepWiki's is the decision that its `Wikis` toolkit may, and a wiki
	// query may not. That decision is the line.
	"deepwiki": {files: 9, lines: 739,
		why: "the first facade, which predates providerhost/* and owns its own mTLS proxy " +
			"type, plus the wiki chat's transcript recorder (its shared half is in providerhost/run)"},
}

func TestAFacadeStaysInsideTheADR0012Budget(t *testing.T) {
	for provider, allowed := range facadeBudgets {
		files, lines := measure(t, provider)
		if files > allowed.files {
			t.Errorf("%s facade is %d files, over the budget of %d (%s)",
				provider, files, allowed.files, reason(allowed))
		}
		if lines > allowed.lines {
			t.Errorf("%s facade is %d net lines, over the budget of %d (%s). "+
				"What it grew is probably shared: put it in internal/providerhost.",
				provider, lines, allowed.lines, reason(allowed))
		}
		t.Logf("%s: %d files, %d net lines (budget %d/%d)",
			provider, files, lines, allowed.files, allowed.lines)
	}
}

// TestARecordedExceptionIsStillTheRatchet fails when a recorded facade shrinks
// far below its recorded figure, because a stale exception stops gating.
func TestARecordedExceptionIsStillTheRatchet(t *testing.T) {
	for provider, allowed := range facadeBudgets {
		if allowed.why == "" {
			continue
		}
		_, lines := measure(t, provider)
		if slack := allowed.lines - lines; slack > 40 {
			t.Errorf("%s is recorded at %d net lines but is now %d — %d lines of "+
				"slack. Lower the recorded figure so the ratchet keeps gating.",
				provider, allowed.lines, lines, slack)
		}
	}
}

func reason(allowed budget) string {
	if allowed.why == "" {
		return "ADR-0012"
	}
	return "recorded exception: " + allowed.why
}

// measure counts non-test Go files and their net non-blank, non-comment lines.
func measure(t *testing.T, provider string) (files, lines int) {
	t.Helper()
	entries, err := os.ReadDir(provider)
	if err != nil {
		t.Fatalf("read %s facade: %v", provider, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files++
		lines += netLines(t, filepath.Join(provider, name))
	}
	if files == 0 {
		t.Fatalf("no %s facade files found; the budget gate is measuring nothing", provider)
	}
	return files, lines
}

// netLines counts lines that are neither blank nor comment.
//
// The block-comment state is tracked rather than matched line by line: a
// `/* … */` spanning ten lines is ten comment lines, and counting only the
// opener would let a facade hide code inside one.
func netLines(t *testing.T, path string) int {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = file.Close() }()

	count := 0
	inBlock := false
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if inBlock {
			if index := strings.Index(line, "*/"); index >= 0 {
				inBlock = false
				line = strings.TrimSpace(line[index+2:])
			} else {
				continue
			}
		}
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		if strings.HasPrefix(line, "/*") {
			if !strings.Contains(line, "*/") {
				inBlock = true
			}
			continue
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return count
}
