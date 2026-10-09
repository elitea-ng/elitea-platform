package mcp

// SEC-11: the safe instruction patch applies the YAML expansion budget the
// Worker parses a stored pipeline under; a refused patch changes and backs up
// nothing.

import (
	"context"
	"strings"
	"testing"
)

func TestInternalInstructionPatchRefusesOverBudgetPipelineExpansion(t *testing.T) {
	pool := newInternalApplicationsPool(t)
	applicationID, versionID := seedInternalApplicationVersion(t, pool)
	executor := newPostgresInternalApplicationExecutor(pool)
	current := "First rule.\nSecond rule."
	bomb := "a: &x [" + strings.Repeat("0,", 1_000) + "0]\nb: [" + strings.Repeat("*x,", 2_000) + "*x]\n"

	result, err := executor.Execute(context.Background(), 1, 73, internalPatchInstructions, map[string]any{
		"application_id": applicationID, "version_id": versionID,
		"expected_instructions_sha256": instructionsSHA256(current),
		"replace_all":                  true, "replacement": bomb,
	})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if body := string(result.body); result.status != 400 ||
		!strings.Contains(body, "once anchors and aliases are expanded") || !strings.Contains(body, "before saving") {
		t.Fatalf("status=%d body=%.300s", result.status, body)
	}
	var stored string
	var backups int
	if err := pool.QueryRow(context.Background(), `SELECT instructions FROM p_1.application_versions WHERE id = $1`, versionID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM p_1.application_versions WHERE application_id = $1 AND name LIKE 'mcp-backup-%'`, applicationID).Scan(&backups); err != nil {
		t.Fatal(err)
	}
	if stored != current || backups != 0 {
		t.Fatalf("a refused patch changed the version or created a backup (backups=%d)", backups)
	}
}
