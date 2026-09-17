package agentexecution

// The internal-tools catalogue exists in four places that no compiler links:
// the web form's authorable list, this package's forwarding map, the Rust
// runtime's skip-list, and the SQL admission gates. Each layer fails a drift
// differently and none fails loudly at build time: a name added to the web
// form alone makes the resolver return zero rows (every send answers 422); a
// name missing from the Go map refuses the start; a name missing from the
// Rust list refuses the WHOLE profile (its list is a skip-list, so absence is
// a hard refusal, not a skip). This test is the one place that reads all four
// and asserts the documented relationships, so the next catalogue change
// breaks HERE, with the relationship named, instead of in a browser.
//
// The relationships (see currentPlatformInternalTools's own comment):
//   - Go map  = web list − internal_mcp + ask_user (internal_mcp is dropped
//     by the freeze and materialized through the tools projection; ask_user
//     is runtime-authored, not form-authored).
//   - Rust: the web list is exactly PLATFORM_INTERNAL_TOOLS (what the native
//     runtime RECOGNIZES and SKIPS) plus the names it IMPLEMENTS
//     (SKILLS_BUILDER_TOOL_NAME / PROJECT_CONTEXT_BUILDER_TOOL_NAME, #940 A8).
//     A name must be in exactly one of the two: in neither, the runtime
//     refuses the whole profile; in both, it would warn that a tool it is
//     about to run is unavailable.
//   - Every SQL admission list = web list + ask_user, at every gate site.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
)

const expectedInternalToolsGateSites = 9

func repoRootForCatalogue(t *testing.T) string {
	t.Helper()
	// package dir: services/elitea-main/internal/application/agentexecution
	root, err := filepath.Abs(filepath.Join("..", "..", "..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.work")); err != nil {
		t.Fatalf("repo root %s does not look like the workspace root: %v", root, err)
	}
	return root
}

func readCatalogueFile(t *testing.T, root string, parts ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{root}, parts...)...)
	content, err := os.ReadFile(path) //nolint:gosec // fixed in-repository path
	if err != nil {
		t.Fatalf("the drift gate needs %s: %v", path, err)
	}
	return string(content)
}

func sortedNames(set map[string]bool) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func TestInternalToolsCatalogueAgreesAcrossWebGoRustAndSQL(t *testing.T) {
	t.Parallel()
	root := repoRootForCatalogue(t)

	// The web form's authorable list is the source the product exposes.
	webSource := readCatalogueFile(t, root, "apps", "elitea-web", "src", "features", "agents", "lib", "internalTools.ts")
	webSet := map[string]bool{}
	for _, match := range regexp.MustCompile(`name: '([a-z_]+)'`).FindAllStringSubmatch(webSource, -1) {
		webSet[match[1]] = true
	}
	if len(webSet) < 8 {
		t.Fatalf("parsed only %d names from internalTools.ts (%v) — the `name: '...'` pattern stopped matching, so this gate measured nothing", len(webSet), sortedNames(webSet))
	}

	// Go map = web − internal_mcp + ask_user.
	wantGo := map[string]bool{"ask_user": true}
	for name := range webSet {
		if name != "internal_mcp" {
			wantGo[name] = true
		}
	}
	for name := range currentPlatformInternalTools {
		if !wantGo[name] {
			t.Errorf("Go forwards %q, which the web list does not author (and it is not ask_user) — either add it to internalTools.ts or remove it here", name)
		}
	}
	for name := range wantGo {
		if !currentPlatformInternalTools[name] {
			t.Errorf("web authors %q but currentPlatformInternalTools does not forward it — every agent that toggles it is refused at start", name)
		}
	}

	// Rust: skipped ∪ implemented = web list, and the two sets are disjoint.
	rustSource := readCatalogueFile(t, root, "services", "elitea-worker-rust", "src", "agents", "internal_tools.rs")
	rustBlock := regexp.MustCompile(`(?s)PLATFORM_INTERNAL_TOOLS: &\[&str\] = &\[(.*?)\];`).FindStringSubmatch(rustSource)
	if rustBlock == nil {
		t.Fatal("PLATFORM_INTERNAL_TOOLS not found in internal_tools.rs — the pattern stopped matching, so this gate measured nothing")
	}
	skippedSet := map[string]bool{}
	for _, match := range regexp.MustCompile(`"([a-z_]+)"`).FindAllStringSubmatch(rustBlock[1], -1) {
		skippedSet[match[1]] = true
	}
	// The names the native runtime IMPLEMENTS, read from their own constants
	// rather than restated here — a constant renamed or a tool deleted makes
	// this gate fail with the relationship named, which is the whole point.
	implementedSet := map[string]bool{}
	for _, match := range regexp.MustCompile(
		`(?m)^pub\(crate\) const (?:SKILLS_BUILDER_TOOL_NAME|PROJECT_CONTEXT_BUILDER_TOOL_NAME): &str = "([a-z_]+)";`,
	).FindAllStringSubmatch(rustSource, -1) {
		implementedSet[match[1]] = true
	}
	if len(implementedSet) != 2 {
		t.Fatalf("parsed %d implemented builder names from internal_tools.rs (%v) — the constant pattern stopped matching, so this half of the gate measured nothing", len(implementedSet), sortedNames(implementedSet))
	}
	for name := range webSet {
		if !skippedSet[name] && !implementedSet[name] {
			t.Errorf("web authors %q but the native runtime neither implements nor skips it — it refuses the WHOLE profile instead", name)
		}
		if skippedSet[name] && implementedSet[name] {
			t.Errorf("the native runtime both implements and skips %q — the run would warn the tool is unavailable in the same turn it runs", name)
		}
	}
	for name := range skippedSet {
		if !webSet[name] {
			t.Errorf("Rust skips %q, which the web list does not author — stale entry", name)
		}
	}
	for name := range implementedSet {
		if !webSet[name] {
			t.Errorf("Rust implements %q, which the web list does not author — the toggle is unreachable", name)
		}
	}

	// Every SQL gate site = web + ask_user, and there are exactly as many
	// sites as the queries carry today: a partial edit that updates an admit
	// clause but not its paired invalid-participant clause is precisely the
	// drift this count catches.
	wantSQL := map[string]bool{"ask_user": true}
	for name := range webSet {
		wantSQL[name] = true
	}
	sqlSource := readCatalogueFile(t, root, "services", "elitea-main", "internal", "db", "queries", "agent_chat.sql")
	// Line comments go first: the gate sites carry explanatory comments whose
	// own parentheses would otherwise stop the list match early.
	sqlSource = regexp.MustCompile(`(?m)--[^\n]*`).ReplaceAllString(sqlSource, "")
	sites := regexp.MustCompile(`(?s)NOT IN \(([^)]*?'ask_user'[^)]*?)\)`).FindAllStringSubmatch(sqlSource, -1)
	if len(sites) != expectedInternalToolsGateSites {
		t.Fatalf("found %d internal-tools gate sites in agent_chat.sql, want %d — a site was added or removed; update the count AND make the new site carry the full catalogue", len(sites), expectedInternalToolsGateSites)
	}
	for index, site := range sites {
		siteSet := map[string]bool{}
		for _, match := range regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(site[1], -1) {
			siteSet[match[1]] = true
		}
		for name := range wantSQL {
			if !siteSet[name] {
				t.Errorf("SQL gate site %d lacks %q — the resolver answers zero rows for any version that toggles it (the 422 class this catalogue widening fixed)", index+1, name)
			}
		}
		for name := range siteSet {
			if !wantSQL[name] {
				t.Errorf("SQL gate site %d admits %q, which neither the web list nor ask_user names", index+1, name)
			}
		}
	}
}
