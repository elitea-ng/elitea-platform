package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

func TestInternalInstructionPatchBacksUpEveryVersionScopedAttachment(t *testing.T) {
	pool := newInternalApplicationsPool(t)
	applicationID, versionID := seedInternalApplicationVersion(t, pool)
	executor := newPostgresInternalApplicationExecutor(pool)
	original := "First rule.\nSecond rule."

	result, err := executor.Execute(context.Background(), 1, 73, internalPatchInstructions, map[string]any{
		"application_id":               applicationID,
		"version_id":                   versionID,
		"expected_instructions_sha256": instructionsSHA256(original),
		"old_text":                     "Second rule.",
		"replacement":                  "Updated second rule.",
	})
	if err != nil {
		t.Fatalf("patch instructions: %v", err)
	}
	if result.status != 201 {
		t.Fatalf("patch status = %d, body = %s", result.status, result.body)
	}
	var response map[string]any
	if err := json.Unmarshal(result.body, &response); err != nil {
		t.Fatalf("decode patch response: %v", err)
	}
	backup := response["mcp_backup_version"].(map[string]any)
	backupID := int64(backup["id"].(float64))
	if !strings.HasPrefix(backup["name"].(string), fmt.Sprintf("mcp-backup-%d-", versionID)) {
		t.Fatalf("backup = %v", backup)
	}

	var sourceInstructions, backupInstructions string
	if err := pool.QueryRow(context.Background(), `
		SELECT instructions FROM p_1.application_versions WHERE id = $1`, versionID).Scan(&sourceInstructions); err != nil {
		t.Fatalf("read patched source: %v", err)
	}
	if err := pool.QueryRow(context.Background(), `
		SELECT instructions FROM p_1.application_versions WHERE id = $1`, backupID).Scan(&backupInstructions); err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if sourceInstructions != "First rule.\nUpdated second rule." || backupInstructions != original {
		t.Fatalf("source=%q backup=%q", sourceInstructions, backupInstructions)
	}
	for table, foreignKey := range map[string]string{
		"application_variables":               "application_version_id",
		"application_version_tag_association": "version_id",
		"entity_tool_mapping":                 "entity_version_id",
		"entity_skill_mapping":                "entity_version_id",
	} {
		var count int
		query := fmt.Sprintf("SELECT count(*) FROM p_1.%s WHERE %s = $1", table, foreignKey)
		if err := pool.QueryRow(context.Background(), query, backupID).Scan(&count); err != nil {
			t.Fatalf("count backup %s: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("backup %s rows = %d, want 1", table, count)
		}
	}

	var backupCountBefore int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM p_1.application_versions
		WHERE application_id = $1 AND name LIKE 'mcp-backup-%'`, applicationID).Scan(&backupCountBefore); err != nil {
		t.Fatalf("count backups: %v", err)
	}
	stale, err := executor.Execute(context.Background(), 1, 73, internalPatchInstructions, map[string]any{
		"application_id":               applicationID,
		"version_id":                   versionID,
		"expected_instructions_sha256": instructionsSHA256(original),
		"replace_all":                  true,
		"replacement":                  "A stale rewrite.",
	})
	if err != nil {
		t.Fatalf("stale patch: %v", err)
	}
	if stale.status != 409 {
		t.Fatalf("stale status = %d, body = %s", stale.status, stale.body)
	}
	var backupCountAfter int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM p_1.application_versions
		WHERE application_id = $1 AND name LIKE 'mcp-backup-%'`, applicationID).Scan(&backupCountAfter); err != nil {
		t.Fatalf("count backups after conflict: %v", err)
	}
	if backupCountAfter != backupCountBefore {
		t.Fatalf("conflict created a backup: before=%d after=%d", backupCountBefore, backupCountAfter)
	}
}

func TestInternalSettingsUpdateIsAtomicAndCannotBypassInstructionPatch(t *testing.T) {
	pool := newInternalApplicationsPool(t)
	applicationID, versionID := seedInternalApplicationVersion(t, pool)
	executor := newPostgresInternalApplicationExecutor(pool)

	result, err := executor.Execute(context.Background(), 1, 91, internalUpdateVersion, map[string]any{
		"application_id":  applicationID,
		"version_id":      versionID,
		"welcome_message": "Ready.",
		"notes":           "Changed through internal MCP.",
		"pipeline_settings": map[string]any{
			"nodes": []any{map[string]any{"id": "one"}},
		},
		"variables": []any{map[string]any{"name": "new", "value": "value"}},
		"tags":      []any{map[string]any{"name": "new-tag", "data": map[string]any{"color": "blue"}}},
	})
	if err != nil {
		t.Fatalf("update settings: %v", err)
	}
	if result.status != 201 {
		t.Fatalf("update status = %d, body = %s", result.status, result.body)
	}
	var response map[string]any
	if err := json.Unmarshal(result.body, &response); err != nil {
		t.Fatalf("decode update response: %v", err)
	}
	settings := response["pipeline_settings"].(map[string]any)
	if settings["trigger"] != "manual" {
		t.Fatalf("pipeline trigger was lost: %v", settings)
	}
	if response["author_id"] != "91" || response["welcome_message"] != "Ready." {
		t.Fatalf("updated response = %v", response)
	}

	var backupCount int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM p_1.application_versions
		WHERE application_id = $1 AND name LIKE 'mcp-backup-%'`, applicationID).Scan(&backupCount); err != nil {
		t.Fatalf("count backups: %v", err)
	}
	rejected, err := executor.Execute(context.Background(), 1, 91, internalUpdateVersion, map[string]any{
		"application_id": applicationID,
		"version_id":     versionID,
		"instructions":   "Bypass the hash guard.",
	})
	if err != nil {
		t.Fatalf("reject direct instructions: %v", err)
	}
	if rejected.status != 409 {
		t.Fatalf("direct instruction status = %d, body = %s", rejected.status, rejected.body)
	}
	var backupCountAfter int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM p_1.application_versions
		WHERE application_id = $1 AND name LIKE 'mcp-backup-%'`, applicationID).Scan(&backupCountAfter); err != nil {
		t.Fatalf("count backups after rejection: %v", err)
	}
	if backupCountAfter != backupCount {
		t.Fatalf("rejected instruction edit created a backup: before=%d after=%d", backupCount, backupCountAfter)
	}
}

func TestInternalGetApplicationResolvesDefaultThenBaseVersion(t *testing.T) {
	pool := newInternalApplicationsPool(t)
	applicationID, baseVersionID := seedInternalApplicationVersion(t, pool)
	ctx := context.Background()
	var defaultVersionID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO p_1.application_versions
			(application_id, name, status, author_id, agent_type, instructions)
		VALUES ($1, 'selected', 'draft', 7, 'react', 'Selected instructions.')
		RETURNING id`, applicationID).Scan(&defaultVersionID); err != nil {
		t.Fatalf("seed selected version: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE p_1.applications
		SET meta = jsonb_build_object('default_version_id', $2::bigint)
		WHERE id = $1`, applicationID, defaultVersionID); err != nil {
		t.Fatalf("set default version: %v", err)
	}

	executor := newPostgresInternalApplicationExecutor(pool)
	result, err := executor.Execute(ctx, 1, 73, internalGetApplication, map[string]any{
		"application_id": applicationID,
	})
	if err != nil || result.status != 200 {
		t.Fatalf("get default application: status=%d error=%v body=%s", result.status, err, result.body)
	}
	var response map[string]any
	if err := json.Unmarshal(result.body, &response); err != nil {
		t.Fatalf("decode default application: %v", err)
	}
	details := response["version_details"].(map[string]any)
	if details["id"] != fmt.Sprintf("%d", defaultVersionID) ||
		details["instructions_sha256"] != instructionsSHA256("Selected instructions.") {
		t.Fatalf("default version details = %v", details)
	}

	if _, err := pool.Exec(ctx, `UPDATE p_1.applications SET meta = '{}'::jsonb WHERE id = $1`, applicationID); err != nil {
		t.Fatalf("clear default version: %v", err)
	}
	result, err = executor.Execute(ctx, 1, 73, internalGetApplication, map[string]any{
		"application_id": applicationID,
	})
	if err != nil || result.status != 200 {
		t.Fatalf("get base fallback: status=%d error=%v body=%s", result.status, err, result.body)
	}
	if err := json.Unmarshal(result.body, &response); err != nil {
		t.Fatalf("decode base fallback: %v", err)
	}
	details = response["version_details"].(map[string]any)
	if details["id"] != fmt.Sprintf("%d", baseVersionID) {
		t.Fatalf("base fallback details = %v", details)
	}
}

func seedInternalApplicationVersion(t *testing.T, pool *pgxpool.Pool) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	var applicationID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO p_1.applications (name, description, owner_id, meta)
		VALUES ('Internal builder fixture', 'fixture', 1, '{}'::jsonb) RETURNING id`).Scan(&applicationID); err != nil {
		t.Fatalf("seed application: %v", err)
	}
	var versionID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO p_1.application_versions
			(application_id, name, status, author_id, agent_type, instructions, pipeline_settings)
		VALUES ($1, 'base', 'draft', 7, 'pipeline', $2, '{"trigger":"manual"}'::jsonb)
		RETURNING id`, applicationID, "First rule.\nSecond rule.").Scan(&versionID); err != nil {
		t.Fatalf("seed version: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO p_1.application_variables (application_version_id, name, value)
		VALUES ($1, 'old', 'value')`, versionID); err != nil {
		t.Fatalf("seed variable: %v", err)
	}
	var tagID int64
	if err := pool.QueryRow(ctx, `INSERT INTO p_1.tags (name, data) VALUES ('old-tag', '{}') RETURNING id`).Scan(&tagID); err != nil {
		t.Fatalf("seed tag: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO p_1.application_version_tag_association (version_id, tag_id) VALUES ($1, $2)`, versionID, tagID); err != nil {
		t.Fatalf("seed tag mapping: %v", err)
	}
	var toolID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO p_1.elitea_tools (name, type, owner_id, author_id, settings)
		VALUES ('fixture-tool', 'openapi', 1, 7, '{}'::jsonb) RETURNING id`).Scan(&toolID); err != nil {
		t.Fatalf("seed toolkit: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO p_1.entity_tool_mapping
			(entity_version_id, entity_id, entity_type, tool_id, selected_tools)
		VALUES ($1, $2, 'agent', $3, '["one"]'::jsonb)`, versionID, applicationID, toolID); err != nil {
		t.Fatalf("seed tool mapping: %v", err)
	}
	var skillID, skillVersionID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO p_1.skills (name, description, owner_id, author_id)
		VALUES ('fixture-skill', 'fixture', 1, 7) RETURNING id`).Scan(&skillID); err != nil {
		t.Fatalf("seed skill: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO p_1.skill_versions (skill_id, name, instructions, author_id)
		VALUES ($1, 'base', 'Use skill.', 7) RETURNING id`, skillID).Scan(&skillVersionID); err != nil {
		t.Fatalf("seed skill version: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO p_1.entity_skill_mapping
			(entity_version_id, entity_type, skill_id, skill_version_id)
		VALUES ($1, 'agent', $2, $3)`, versionID, skillID, skillVersionID); err != nil {
		t.Fatalf("seed skill mapping: %v", err)
	}
	return applicationID, versionID
}

func newInternalApplicationsPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := os.Getenv(environment)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", environment, err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open PostgreSQL admin pool: %v", err)
	}
	databaseName := fmt.Sprintf("elitea_internal_apps_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated database: %v", err)
	}
	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)")
		adminPool.Close()
		t.Fatalf("open isolated database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated database: %v", err)
		}
		adminPool.Close()
	})
	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	if err := migrate.New(pool, platformmigrations.Files).ApplyTenant(ctx, 1); err != nil {
		t.Fatalf("run tenant migrations: %v", err)
	}
	return pool
}
