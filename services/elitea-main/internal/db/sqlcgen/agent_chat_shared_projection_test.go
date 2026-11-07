package sqlcgen

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	sharedProjectionBeginMarker = "-- BEGIN shared application_version_details_json projection"
	sharedProjectionEndMarker   = "-- END shared application_version_details_json projection"

	// sharedProjectionMinimumLines keeps a vacuous pass impossible. Two empty
	// regions are trivially equal, so an edit that deletes the projection from
	// BOTH queries — or a marker pair that accidentally brackets nothing —
	// would otherwise read as "identical" and this gate would report success
	// while guarding nothing. The block is ~127 lines today.
	sharedProjectionMinimumLines = 100
)

// TestSharedApplicationVersionDetailsProjectionsAreIdentical pins the one
// contract that makes the duplication in agent_chat.sql safe.
//
// `application_version_details_json` is built twice, byte for byte: once in
// ResolveCurrentApplicationTurn for the agent a user is talking to, and once in
// ResolveCurrentApplicationVersionDetails for a nested (agent-as-tool) child.
// They are not two documents that merely resemble each other:
//
//   - ONE freeze consumes both — FreezeCurrentApplicationVersion
//     (internal/application/agentexecution/tools.go) resolves toolkit
//     references, drops blocked toolkits and normalizes the runtime profile
//     for the parent and the nested child alike.
//   - ONE decoder reads both on the worker side —
//     `OrdinaryNoToolProfile::from_nested_version` and
//     `FrozenToolSnapshot::from_version_details`
//     (services/elitea-worker-rust/src/agents/assembly.rs), whose response
//     type is `deny_unknown_fields`.
//
// So a key added to, renamed in, or dropped from one copy and not the other
// does not degrade gracefully: it either fails every nested assembly with a
// malformed-response error that names nothing, or it silently gives a child a
// different definition than the same agent gets as a parent. Neither shows up
// in a resolver unit test, because each query is individually valid.
//
// The two copies are extracted from the .sql source between named markers
// rather than by line number, so this fails loudly on drift instead of going
// stale the way the line reference in the query's own comment did.
func TestSharedApplicationVersionDetailsProjectionsAreIdentical(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "queries", "agent_chat.sql"))
	if err != nil {
		t.Fatalf("read agent_chat.sql: %v", err)
	}
	blocks := extractSharedProjectionBlocks(t, string(source))
	if len(blocks) != 2 {
		t.Fatalf(
			"expected exactly 2 %q blocks in agent_chat.sql, found %d — the marker pair is what keeps ResolveCurrentApplicationTurn and ResolveCurrentApplicationVersionDetails comparable",
			sharedProjectionBeginMarker,
			len(blocks),
		)
	}
	turnProjection, nestedProjection := blocks[0], blocks[1]

	if lines := len(strings.Split(strings.TrimSpace(turnProjection), "\n")); lines < sharedProjectionMinimumLines {
		t.Fatalf(
			"shared projection shrank to %d lines (< %d): equality below would pass vacuously, so check the markers still bracket the whole jsonb_build_object",
			lines,
			sharedProjectionMinimumLines,
		)
	}
	if turnProjection != nestedProjection {
		t.Fatalf(
			"ResolveCurrentApplicationTurn and ResolveCurrentApplicationVersionDetails no longer build the SAME application_version_details_json.\n"+
				"One freeze (FreezeCurrentApplicationVersion) and one deny_unknown_fields decoder read both, so the parent's definition and a nested child's must have one shape.\n"+
				"First divergence:\n%s",
			firstProjectionDifference(turnProjection, nestedProjection),
		)
	}

	// The markers are only meaningful if they really do sit inside those two
	// queries. sqlc preserves SQL comments in the generated constants, so this
	// also proves the shipped queries carry the projection this test compared.
	for name, query := range map[string]string{
		"resolveCurrentApplicationTurn":           resolveCurrentApplicationTurn,
		"resolveCurrentApplicationVersionDetails": resolveCurrentApplicationVersionDetails,
	} {
		if !strings.Contains(query, turnProjection) {
			t.Fatalf("generated %s does not carry the shared application_version_details_json projection", name)
		}
	}
}

// extractSharedProjectionBlocks returns the text strictly between each
// BEGIN/END marker pair. It is deliberately strict about pairing: an unbalanced
// or nested marker is a test failure, not a silently skipped block.
func extractSharedProjectionBlocks(t *testing.T, source string) []string {
	t.Helper()
	var blocks []string
	var current []string
	open := false
	for index, line := range strings.Split(source, "\n") {
		trimmed := strings.TrimSpace(line)
		switch trimmed {
		case sharedProjectionBeginMarker:
			if open {
				t.Fatalf("agent_chat.sql:%d: nested %q marker", index+1, sharedProjectionBeginMarker)
			}
			open = true
			current = nil
		case sharedProjectionEndMarker:
			if !open {
				t.Fatalf("agent_chat.sql:%d: %q without a matching begin", index+1, sharedProjectionEndMarker)
			}
			open = false
			blocks = append(blocks, strings.Join(current, "\n"))
		default:
			if open {
				current = append(current, line)
			}
		}
	}
	if open {
		t.Fatalf("agent_chat.sql: %q is never closed", sharedProjectionBeginMarker)
	}
	return blocks
}

func firstProjectionDifference(turn, nested string) string {
	turnLines := strings.Split(turn, "\n")
	nestedLines := strings.Split(nested, "\n")
	for index := 0; index < len(turnLines) && index < len(nestedLines); index++ {
		if turnLines[index] != nestedLines[index] {
			return "  block line " + strconv.Itoa(index+1) + "\n" +
				"  turn:   " + turnLines[index] + "\n" +
				"  nested: " + nestedLines[index]
		}
	}
	return "  block lengths differ: turn has " + strconv.Itoa(len(turnLines)) +
		" lines, nested has " + strconv.Itoa(len(nestedLines))
}

// TestReaderViewFactsAreIdenticalInBothResolvers pins the reader view facts
// (ADR-0050 decision 9, issue 1609) of the application and ad-hoc history
// projections: Main filters both resolvers' references with one rule, so a
// fact that drifts in one copy filters ad-hoc and application readers
// differently.
func TestReaderViewFactsAreIdenticalInBothResolvers(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "queries", "agent_chat.sql"))
	if err != nil {
		t.Fatalf("read agent_chat.sql: %v", err)
	}
	const begin, end = "-- BEGIN shared reader view facts", "-- END shared reader view facts"
	var blocks []string
	rest := string(source)
	for {
		start := strings.Index(rest, begin)
		if start < 0 {
			break
		}
		stop := strings.Index(rest[start:], end)
		if stop < 0 {
			t.Fatalf("%q is never closed", begin)
		}
		blocks = append(blocks, rest[start:start+stop])
		rest = rest[start+stop+len(end):]
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 reader view fact blocks, found %d", len(blocks))
	}
	if !strings.Contains(blocks[0], "AS answered_by_reader") || blocks[0] != blocks[1] {
		t.Fatalf("the reader view facts differ:\n%s", firstProjectionDifference(blocks[0], blocks[1]))
	}
	for name, query := range map[string]string{
		"resolveCurrentApplicationTurn": resolveCurrentApplicationTurn,
		"resolveCurrentAdhocTurn":       resolveCurrentAdhocTurn,
	} {
		if !strings.Contains(query, blocks[0]) {
			t.Fatalf("generated %s does not carry the reader view facts", name)
		}
	}
}
