package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/pipelinelimits"
)

func limitYAML(nodes, size int) string {
	var b strings.Builder
	b.WriteString("entry_point: 1\nnodes:\n")
	for i := 1; i <= nodes; i++ {
		fmt.Fprintf(&b, "  - id: %d\n    type: state_modifier\n    transition: %d\n", i, i+1)
	}
	if size > 0 {
		b.WriteString("# LIMIT-MARKER-DO-NOT-ECHO ")
		if pad := size - b.Len(); pad > 0 {
			b.WriteString(strings.Repeat("x", pad))
		}
	}
	return b.String()
}

// The safe instruction patch writes new pipeline text, so it carries the same
// save-time bound as the REST writes; a refused patch changes and backs up
// nothing.
func TestInternalInstructionPatchRefusesOverBoundPipelineText(t *testing.T) {
	pool := newInternalApplicationsPool(t)
	applicationID, versionID := seedInternalApplicationVersion(t, pool)
	executor := newPostgresInternalApplicationExecutor(pool)
	current := "First rule.\nSecond rule."

	patch := func(replacement string) (int, string) {
		t.Helper()
		result, err := executor.Execute(context.Background(), 1, 73, internalPatchInstructions, map[string]any{
			"application_id": applicationID, "version_id": versionID,
			"expected_instructions_sha256": instructionsSHA256(current),
			"replace_all":                  true, "replacement": replacement,
		})
		if err != nil {
			t.Fatalf("patch: %v", err)
		}
		return result.status, string(result.body)
	}
	backups := func() int {
		var n int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM p_1.application_versions WHERE application_id = $1 AND name LIKE 'mcp-backup-%'`, applicationID).Scan(&n); err != nil {
			t.Fatalf("count backups: %v", err)
		}
		return n
	}

	for name, tc := range map[string]struct{ replacement, text string }{
		"bytes+1": {limitYAML(1, pipelinelimits.MaxInstructionsBytes+1), "512 KiB"},
		"nodes+1": {limitYAML(pipelinelimits.MaxNodes+1, 0), "128 nodes"},
	} {
		status, body := patch(tc.replacement)
		if status != 400 || !strings.Contains(body, tc.text) || !strings.Contains(body, "before saving") {
			t.Fatalf("%s: status=%d body=%.300s", name, status, body)
		}
		if strings.Contains(body, "LIMIT-MARKER") {
			t.Errorf("%s: the refusal echoes pipeline content", name)
		}
	}
	var stored string
	if err := pool.QueryRow(context.Background(), `SELECT instructions FROM p_1.application_versions WHERE id = $1`, versionID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != current || backups() != 0 {
		t.Fatalf("a refused patch changed the version or created a backup (backups=%d)", backups())
	}

	if status, body := patch(limitYAML(1, pipelinelimits.MaxInstructionsBytes)); status != 201 {
		t.Fatalf("exactly %d bytes: status=%d body=%.300s", pipelinelimits.MaxInstructionsBytes, status, body)
	}
}
