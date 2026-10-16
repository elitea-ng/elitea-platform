package repos

// The cloud executor's application methods (the later gRPC adapter's
// targets): resolving a claim's execution to its thread, the claim-bound
// upload, the answer's cloud turn markers, and the executor audit events.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacestore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

func TestPostgresWorkspaceExecutorSurface(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const owner int64 = 5501
	h := newHandoffHarness(t, pool)
	fixture := seedLocalWorkThread(t, pool, owner, "local_work")
	thread := fixture.thread
	began, err := h.service.Begin(ctx, beginRequest(thread, owner))
	if err != nil {
		t.Fatal(err)
	}
	sha := h.publishSnapshot(t, thread, owner, 1, "exec")
	if _, err := h.service.Complete(ctx, workspacehandoff.CompleteRequest{ProjectID: 1, HandoffID: began.Handoff.HandoffID,
		ActorUserID: owner, Epoch: 1, HandoffRef: workspacestore.RefHandoff1, SHA: sha}); err != nil {
		t.Fatal(err)
	}
	// The cloud turn's answer group, as admission writes it.
	const response = "4d5a8c0e-3f4b-4a2b-9c1d-0e2f3a4b5c6d"
	var dummy int64
	if err := pool.QueryRow(ctx, `
SELECT p.id FROM p_1.chat_participants p JOIN p_1.chat_participant_mapping m ON m.participant_id = p.id
WHERE m.conversation_id = $1 AND p.entity_name = 'dummy'`, fixture.conversationID).Scan(&dummy); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO p_1.chat_message_group (uuid, author_participant_id, conversation_id, meta)
VALUES ($1::uuid, $2, $3, '{"is_streaming_marker": true}'::jsonb)`, response, dummy, fixture.conversationID); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := h.service.ResolveRunningTurn(ctx, "exec-x"); err != nil || ok {
		t.Fatalf("an execution that is no cloud turn resolved: %v %v", ok, err)
	}
	if _, _, err := h.service.StartCloudTurn(ctx, thread, 2, "exec-x", response); err != nil {
		t.Fatal(err)
	}
	resolved, ok, err := h.service.ResolveRunningTurn(ctx, "exec-x")
	if err != nil || !ok || resolved.ConversationUUID != thread.ConversationUUID || resolved.Epoch != 2 {
		t.Fatalf("resolve = %+v %v %v", resolved, ok, err)
	}
	meta := func() map[string]any {
		t.Helper()
		var raw map[string]any
		if err := pool.QueryRow(ctx, `SELECT meta FROM p_1.chat_message_group WHERE uuid = $1::uuid`, response).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	if got := meta(); got["executed_by"] != "cloud_executor" || got["workspace_epoch"] != float64(2) || got["is_streaming_marker"] != true {
		t.Fatalf("answer meta at start = %v", got)
	}

	// The claim-bound upload: stored only when the bytes match.
	executor := workspacestore.Caller{Writer: workspacestore.WriterCloudExecutor, ExecutionID: "exec-x"}
	content := []byte("cloud turn bundle")
	digest := digestOf(content)
	if _, err := h.objects.UploadSegment(ctx, thread, executor, 2, digest, int64(len(content)), strings.NewReader("tampered bundle!!")); !errors.Is(err, workspacestore.ErrChecksumMismatch) {
		t.Fatalf("a tampered upload = %v", err)
	}
	ref, _ := storage.NewObjectRef("1", storage.WorkspaceBucketName, workspacestore.ObjectKey(thread.ConversationUUID, digest))
	if h.store.Has(ref) {
		t.Fatalf("a tampered upload stayed stored")
	}
	other := workspacestore.Caller{Writer: workspacestore.WriterCloudExecutor, ExecutionID: "exec-y"}
	if _, err := h.objects.UploadSegment(ctx, thread, other, 2, digest, int64(len(content)), strings.NewReader(string(content))); !errors.Is(err, workspacestore.ErrStaleEpoch) {
		t.Fatalf("another turn's upload = %v", err)
	}
	if exists, err := h.objects.UploadSegment(ctx, thread, executor, 2, digest, int64(len(content)), strings.NewReader(string(content))); err != nil || exists || !h.store.Has(ref) {
		t.Fatalf("upload = %v %v", exists, err)
	}
	commit := sha1Of("cloud commit")
	request := desktopPublish(thread, owner, 2, digest, int64(len(content)), workspacestore.RefUpdate{Name: workspacestore.RefCloud, New: commit})
	request.Caller = executor
	if _, err := h.objects.Publish(ctx, request); err != nil {
		t.Fatal(err)
	}
	if exists, err := h.objects.UploadSegment(ctx, thread, executor, 2, digest, int64(len(content)), strings.NewReader(string(content))); err != nil || !exists {
		t.Fatalf("a re-upload of a published digest = %v %v, want exists", exists, err)
	}
	if got := meta(); got["workspace_commit"] != commit || got["executed_by"] != "cloud_executor" {
		t.Fatalf("answer meta after publish = %v", got)
	}

	// The executor's audit events: allowlisted attributes only, no path.
	if err := h.service.RecordExecutorEvent(ctx, thread, 2, "exec-x", workspacehandoff.EventScanBlocked,
		map[string]string{"findings": "2", "rules": "aws-access-key", "path": "./secrets.txt", "files": "./deploy/prod.env"}); err != nil {
		t.Fatal(err)
	}
	// A path smuggled into an allowed attribute is dropped by its shape.
	if err := h.service.RecordExecutorEvent(ctx, thread, 2, "exec-x", workspacehandoff.EventScanBlocked,
		map[string]string{"findings": "./leak/one.txt", "rules": "rule-a,./leak/two.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := h.service.RecordExecutorEvent(ctx, thread, 2, "exec-x", workspacehandoff.EventPullRequestOpened,
		map[string]string{"branch": "elitea/cloud-1", "number": "42", "url": "https://github.com/o/r/pull/42"}); err != nil {
		t.Fatal(err)
	}
	if err := h.service.RecordExecutorEvent(ctx, thread, 2, "exec-y", workspacehandoff.EventPullRequestOpened, nil); !errors.Is(err, workspacehandoff.ErrStaleEpoch) {
		t.Fatalf("another turn's event = %v", err)
	}
	if err := h.service.RecordExecutorEvent(ctx, thread, 2, "exec-x", "store.publish", nil); !errors.Is(err, workspacehandoff.ErrInvalid) {
		t.Fatalf("an unknown kind = %v", err)
	}
	actions := strings.Join(h.recorder.actions(), "\n")
	if !strings.Contains(actions, "handoff.scan_blocked: epoch 2, findings 2, rules aws-access-key") ||
		strings.Contains(actions, "secrets.txt") || strings.Contains(actions, "prod.env") || strings.Contains(actions, "leak") ||
		!strings.Contains(actions, "pull_request.opened: epoch 2, branch elitea/cloud-1, number 42") {
		t.Fatalf("audit = %s", actions)
	}
}
