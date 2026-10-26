package repos

// The thread's pull-request credential (shared/0179, ADR-0034 decision 10
// amended 2026-10-26) against a REAL database: only the owner's row of a
// thread with a cloud copy takes it, every read carries it back, null clears
// it, and the column refuses anything but a configuration reference.

import (
	"context"
	"errors"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
)

func TestPostgresWorkspaceHandoffForgeCredential(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const owner, stranger int64 = 5301, 5302
	h := newHandoffHarness(t, pool)
	thread := seedLocalWorkThread(t, pool, owner, "local_work").thread
	repo, err := NewWorkspaceHandoffsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	credential := &workspacehandoff.ForgeCredential{EliteaTitle: "gh-team", Private: true}

	// No handoff row yet: nothing to set.
	if _, err := repo.SetForgeCredential(ctx, thread, owner, credential); !errors.Is(err, workspacehandoff.ErrNotFound) {
		t.Fatalf("set with no row = %v", err)
	}
	if _, err := h.service.Begin(ctx, beginRequest(thread, owner)); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SetForgeCredential(ctx, thread, stranger, credential); !errors.Is(err, workspacehandoff.ErrNotFound) {
		t.Fatalf("a stranger's set = %v", err)
	}
	updated, err := repo.SetForgeCredential(ctx, thread, owner, credential)
	if err != nil || updated.ForgeCredential == nil || *updated.ForgeCredential != *credential {
		t.Fatalf("set = %+v, %v", updated.ForgeCredential, err)
	}
	read, found, err := repo.Read(ctx, thread)
	if err != nil || !found || read.ForgeCredential == nil || read.ForgeCredential.EliteaTitle != "gh-team" || !read.ForgeCredential.Private {
		t.Fatalf("read = %+v, %v, %v", read.ForgeCredential, found, err)
	}
	cleared, err := repo.SetForgeCredential(ctx, thread, owner, nil)
	if err != nil || cleared.ForgeCredential != nil {
		t.Fatalf("clear = %+v, %v", cleared.ForgeCredential, err)
	}
	// The column holds a reference, never anything else (a token, say).
	for _, value := range []string{`{"elitea_title":"gh","private":true,"token":"ghp_x"}`,
		`{"elitea_title":"","private":false}`, `{"elitea_title":"gh"}`, `"gh"`} {
		if _, err := pool.Exec(ctx, `UPDATE elitea_runtime.workspace_handoffs SET forge_credential = $3::jsonb
WHERE project_id = $1 AND conversation_uuid = $2::uuid`, thread.ProjectID, thread.ConversationUUID, value); err == nil {
			t.Fatalf("the column took %s", value)
		}
	}
}
