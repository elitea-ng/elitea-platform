package hopmarker

// path_pin_test.go — the marker must SURVIVE the loop path, and its name must
// not drift between the two Go modules that depend on it (issue #164).
//
// Everything else about hop detection can be green while the mechanism is
// dead. The marker is stamped in one module and recognised in the same module,
// but the loop it exists to detect travels through configuration and code this
// module does not compile:
//
//	gateway → provider (= the platform's own /llm)
//	        → traefik edge   (deploy/traefik/*.yml)
//	        → elitea-main    (services/elitea-main/internal/llmproxy)
//	        → gateway
//
// Any hop on that path that DELETES the header disarms the guard completely
// and breaks no test in this module. The X-Elitea-* namespace is deleted at the
// browser edge by design (#326), so this one name lives on an explicit
// exception — and an exception nothing checks is an exception somebody tidies
// away.
//
// These gates read the real files. They FAIL rather than skip when a file is
// missing: a path that moved must be repaired here, because "found nothing, so
// nothing is wrong" is how every gate in this repository has stopped gating.
//
// The exception is safe to carve out. This header is not identity: it grants
// nothing, names no project, and the only thing a client achieves by sending
// it is the refusal of its own request (see the package comment).

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// eliteaMainProxyDir is elitea-main's /llm reverse proxy — the hop between the
// edge and the gateway. stripIdentityHeaders there deletes client-supplied
// identity by name.
const eliteaMainProxyDir = "services/elitea-main/internal/llmproxy"

// edgeStripGate is the gate that owns the browser edge's side of the
// exception. It must NAME this header, or the two modules have drifted and the
// edge is being held to a rule about a header nobody sets any more.
const edgeStripGate = "services/elitea-main/tests/deployedge/edge_identity_strip_test.go"

// edgeForwardGateFunc is the test inside edgeStripGate that enforces the
// exception. Pinning the NAME alone is not enough: a constant can survive the
// deletion of the only check that reads it, which leaves the header
// documented and unprotected.
const edgeForwardGateFunc = "func TestBrowserEdgesForwardTheHopMarker("

// headerRemovalLine matches the shapes a YAML header-removal entry takes:
//
//	X-Elitea-Llm-Hop: ""      (traefik customRequestHeaders — empty deletes)
//	X-Elitea-Llm-Hop: ''
//	X-Elitea-Llm-Hop:
//	- X-Elitea-Llm-Hop        (a chart's list of names to strip)
//
// A line that assigns a real VALUE is not a removal and does not match.
var headerRemovalLine = regexp.MustCompile(`^\s*-?\s*` + regexp.QuoteMeta(Header) + `\s*:?\s*(""|'')?\s*$`)

// repoRoot walks up to the directory holding go.work. This module is
// deliberately outside that workspace, but the file still marks the repository
// root and CI checks out the whole tree.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolve the working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.work is not in any parent directory, so the repository root is unknown")
		}
		dir = parent
	}
}

// TestHopMarker_NoDeployConfigurationStripsIt walks every YAML file under
// deploy/ and requires that none of them deletes the marker.
//
// It covers the whole directory, not a named list, because the edge is not one
// file: deploy/traefik/dynamic.yml and dynamic.e2e.yml front the browser,
// deploy/runtime/platform-edge-dynamic.yml fronts the runtime plane, and
// deploy/helm/elitea/values.yaml carries the chart's own strip list. A gate
// that named three of the four would go quiet the day a fourth appeared.
func TestHopMarker_NoDeployConfigurationStripsIt(t *testing.T) {
	root := repoRoot(t)
	deployDir := filepath.Join(root, "deploy")
	if _, err := os.Stat(deployDir); err != nil {
		t.Fatalf("deploy/ does not exist (%v). The deployment configuration moved and this gate stopped gating.", err)
	}

	scanned := 0
	err := filepath.WalkDir(deployDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if ext := filepath.Ext(path); ext != ".yml" && ext != ".yaml" {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		scanned++
		for i, line := range strings.Split(string(raw), "\n") {
			if !headerRemovalLine.MatchString(line) {
				continue
			}
			relative, _ := filepath.Rel(root, path)
			t.Errorf(
				"%s:%d deletes %s.\n"+
					"That disarms hop detection for every request through this hop: the gateway keeps "+
					"stamping the marker outbound and never sees it come back, so a circular route runs "+
					"unnoticed and only the amplification backstop is left — which does no hop detection "+
					"at all (issue #12).\n"+
					"This header is the ONE X-Elitea-* name the edge forwards on purpose. It is not "+
					"identity: it grants nothing and names no project, and the only thing a client "+
					"achieves by sending it is the refusal of its own request.",
				relative, i+1, Header,
			)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk deploy/: %v", err)
	}
	// The extraction floor. Zero files read produces zero findings, which
	// reads exactly like a pass.
	if scanned == 0 {
		t.Fatalf("no YAML file was read under %s, so this gate compared nothing", deployDir)
	}
}

// TestHopMarker_EliteaMainForwardsIt covers the hop between the edge and the
// gateway. elitea-main's proxy deletes client-supplied identity BY NAME
// (stripIdentityHeaders), and adding this header to that list is the single
// most natural-looking way to disarm the guard — every other X-Elitea-* name
// belongs there.
//
// The rule is therefore blunt on purpose: elitea-main's proxy must not name
// this header at all. It has no reason to. If a future change needs to mention
// it there, that change must come back here and say why.
func TestHopMarker_EliteaMainForwardsIt(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, eliteaMainProxyDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf(
			"%s cannot be read (%v). elitea-main's /llm proxy moved and this gate stopped gating. "+
				"Update eliteaMainProxyDir in this file.",
			eliteaMainProxyDir, err,
		)
	}

	scanned := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(dir, name))
		if readErr != nil {
			t.Fatalf("read %s/%s: %v", eliteaMainProxyDir, name, readErr)
		}
		scanned++
		if !strings.Contains(string(raw), Header) {
			continue
		}
		t.Errorf(
			"%s/%s names %s.\n"+
				"That file's job is DELETING headers before the request reaches the gateway. The hop "+
				"marker must pass through it untouched, or the gateway never sees its own marker come "+
				"back and detects no loop at all.\n"+
				"If naming it there is deliberate, change this gate deliberately too.",
			eliteaMainProxyDir, name, Header,
		)
	}
	if scanned == 0 {
		t.Fatalf("no non-test .go file was read under %s, so this gate compared nothing", eliteaMainProxyDir)
	}
}

// TestHopMarker_NamePinnedAcrossModules is the drift gate the issue asks for.
//
// elitea-main and elitea-llm-gateway are separate Go modules, so neither can
// import the other's constant. The edge exception is enforced from the
// elitea-main side, against a header name written out as a literal there. Rename
// the constant here and that gate keeps passing while it protects a name
// nothing sets — the guard is then silently disarmed with every test green.
//
// This is the check that fails instead.
func TestHopMarker_NamePinnedAcrossModules(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, edgeStripGate)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf(
			"%s cannot be read (%v). The edge gate moved and the exception that lets this marker cross "+
				"the browser edge is no longer checked from either side. Update edgeStripGate in this file.",
			edgeStripGate, err,
		)
	}
	// Both halves matter, and they fail differently.
	//
	//   - the NAME pins the constant: rename Header here and the edge gate is
	//     left protecting a header nothing sets.
	//   - the TEST FUNCTION pins the gate itself: keeping the literal in a
	//     const while deleting the check that uses it leaves the name present
	//     and the rule gone, which is what a first cut of this file missed.
	for _, want := range []struct {
		literal string
		why     string
	}{
		{
			literal: Header,
			why: "the two modules have drifted: either the constant here was renamed, or the " +
				"exception was removed there",
		},
		{
			literal: edgeForwardGateFunc,
			why: "the gate that enforces the exception is gone, so a browser edge may now delete " +
				"the header while the name still sits in a constant nothing checks",
		},
	} {
		if strings.Contains(string(raw), want.literal) {
			continue
		}
		t.Errorf(
			"%s does not contain %q.\n"+
				"That file is what stops a browser edge deleting this header, and %s.\n"+
				"Either way hop detection stays armed in the code and dies on the wire.",
			edgeStripGate, want.literal, want.why,
		)
	}
}

// TestHeaderRemovalLine pins the pattern the deploy/ walk uses. The walk above
// reports nothing when the pattern stops matching, and nothing reads exactly
// like a clean tree — so the pattern needs its own test, against the shapes a
// header removal actually takes in this repository's configuration.
func TestHeaderRemovalLine(t *testing.T) {
	for _, tc := range []struct {
		name    string
		line    string
		removal bool
	}{
		// deploy/traefik/dynamic.yml — customRequestHeaders, empty deletes.
		{"traefik empty double quotes", `          X-Elitea-Llm-Hop: ""`, true},
		{"traefik empty single quotes", `          X-Elitea-Llm-Hop: ''`, true},
		{"bare key, no value", `          X-Elitea-Llm-Hop:`, true},
		// deploy/helm/elitea/values.yaml — a list of names to strip.
		{"chart list entry", `      - X-Elitea-Llm-Hop`, true},
		{"chart list entry with colon", `      - X-Elitea-Llm-Hop:`, true},
		// Not removals.
		{"assigned a real value", `          X-Elitea-Llm-Hop: "v1=abc"`, false},
		{"another header entirely", `          X-Elitea-Project-Id: ""`, false},
		{"named inside a comment", `    # X-Elitea-Llm-Hop is forwarded on purpose`, false},
		{"a longer name that contains it", `          X-Elitea-Llm-Hop-Extra: ""`, false},
	} {
		if got := headerRemovalLine.MatchString(tc.line); got != tc.removal {
			t.Errorf("%s: MatchString(%q) = %v, want %v", tc.name, tc.line, got, tc.removal)
		}
	}
}
