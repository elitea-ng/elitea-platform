package mcp

import (
	"context"
	"strings"
	"testing"
)

// TestInternalPromptEditsRefuseAnInstalledAgent pins ADR-0051 decision 3 on the
// two internal write paths that edit a version: the settings update and the
// instruction patch. The definition of an agent installed from the Marketplace
// is read-only, so each answers 409 and writes nothing, not even a backup.
func TestInternalPromptEditsRefuseAnInstalledAgent(t *testing.T) {
	pool := newInternalApplicationsPool(t)
	ctx := context.Background()
	applicationID, versionID := seedInternalApplicationVersion(t, pool)
	if _, err := pool.Exec(ctx, `
UPDATE p_1.application_versions SET meta = COALESCE(meta, '{}'::jsonb) || '{"origin_kind":"installed"}'::jsonb WHERE id = $1`, versionID); err != nil {
		t.Fatal(err)
	}
	executor := newPostgresInternalApplicationExecutor(pool)
	const stored = "First rule.\nSecond rule."

	patch, err := executor.Execute(ctx, 1, 73, internalPatchInstructions, map[string]any{
		"application_id": applicationID, "version_id": versionID,
		"expected_instructions_sha256": instructionsSHA256(stored),
		"replace_all":                  true, "replacement": "Changed rule.",
	})
	if err != nil {
		t.Fatal(err)
	}
	update, err := executor.Execute(ctx, 1, 73, internalUpdateVersion, map[string]any{
		"application_id": applicationID, "version_id": versionID, "instructions": "Changed rule.",
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, result := range map[string]internalApplicationExecution{"patch": patch, "update": update} {
		if result.status != 409 || !strings.Contains(string(result.body), "read-only") {
			t.Errorf("%s = %d %.200s, want 409 read-only", name, result.status, result.body)
		}
	}
	var instructions string
	var backups int
	if err := pool.QueryRow(ctx, `SELECT instructions FROM p_1.application_versions WHERE id = $1`, versionID).Scan(&instructions); err != nil || instructions != stored {
		t.Errorf("instructions after refused edits = %q (%v)", instructions, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM p_1.application_versions WHERE name LIKE 'mcp-backup-%'`).Scan(&backups); err != nil || backups != 0 {
		t.Errorf("a refused edit made %d backups (%v)", backups, err)
	}
}
