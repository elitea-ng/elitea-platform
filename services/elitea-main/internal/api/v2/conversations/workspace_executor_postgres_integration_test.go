package conversations

import (
	"context"
	"testing"

	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

// The conversation details' `workspace_executor` (ADR-0034): `local` on a
// database without the handoff table (the read never fails on it), then the
// handoff row's state mapped to local / cloud / returning.
func TestPostgresConversationWorkspaceExecutor(t *testing.T) {
	pool := newListFiltersPool(t)
	ctx := context.Background()
	handler := (&Handler{}).WithPool(pool)
	const conversation = "0b6f9a52-1c2d-4e5f-8a9b-0c1d2e3f4a5b"
	if got := handler.workspaceExecutor(ctx, "1", conversation); got != WorkspaceExecutorLocal {
		t.Fatalf("without the handoff table = %q", got)
	}
	if _, err := pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS elitea_runtime`); err != nil {
		t.Fatal(err)
	}
	sql, err := platformmigrations.Files.ReadFile("shared/0167_workspace_handoffs.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("apply 0167: %v", err)
	}
	if got := handler.workspaceExecutor(ctx, "1", conversation); got != WorkspaceExecutorLocal {
		t.Fatalf("with no handoff row = %q", got)
	}
	for state, want := range map[string]string{
		"HANDING_OFF": "local", "CLOUD_IDLE": "cloud", "RECLAIMING": "returning", "LOCAL": "local",
	} {
		if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.workspace_handoffs (project_id, conversation_uuid, handoff_id, state, epoch, owner_user_id)
VALUES (1, $1::uuid, gen_random_uuid(), $2, 1, 1)
ON CONFLICT (project_id, conversation_uuid) DO UPDATE SET state = EXCLUDED.state`, conversation, state); err != nil {
			t.Fatal(err)
		}
		if got := handler.workspaceExecutor(ctx, "1", conversation); got != want {
			t.Fatalf("%s = %q, want %q", state, got, want)
		}
		if got := handler.workspaceExecutor(ctx, "2", conversation); got != WorkspaceExecutorLocal {
			t.Fatalf("another project's row leaked: %q", got)
		}
	}
	if WorkspaceExecutorOf("CLOUD_RUNNING") != WorkspaceExecutorCloud {
		t.Fatalf("CLOUD_RUNNING is not cloud")
	}
}
