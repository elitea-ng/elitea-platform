package repos

// The handoff fences (ADR-0034 decision 7, contract §2.10) against a REAL
// database: the local turn's 409 thread_executor_is_cloud on start and
// commit, the Local work fence's SQL that opens only while the cloud owns the
// thread, and the regenerate check's SQL.

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/memories"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/localturn"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacestore"
)

func TestPostgresLocalTurnRefusedWhileTheCloudOwnsTheThread(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const owner int64 = 5301
	fixture := seedLocalWorkThread(t, pool, owner, "local_work")
	service, err := localturn.NewService(NewLocalTurnsRepo(pool), &localTurnPolicy{allowed: true}, NewMemoriesRepo(pool),
		&localTurnAudit{}, func() (string, error) { return strings.Repeat("e", 32), nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := localturn.StartRequest{
		ProjectID: 1, ActorUserID: owner, TokenID: "77", NativeClientID: "ai.elitea.desktop",
		ConversationUUID: fixture.thread.ConversationUUID,
		QuestionID:       "7f1c2d3e-4a5b-4c6d-8e7f-901234567890", UserInput: "Fix it",
	}
	started, err := service.Start(ctx, start)
	if err != nil {
		t.Fatalf("start while LOCAL: %v", err)
	}
	for _, state := range []workspacestore.State{
		workspacestore.StateHandingOff, workspacestore.StateCloudIdle, workspacestore.StateCloudRunning, workspacestore.StateReclaiming,
	} {
		running := ""
		if state == workspacestore.StateCloudRunning {
			running = "exec"
		}
		seedHandoffRow(t, pool, fixture.thread, owner, state, 2, false, running)
		retried := start
		retried.QuestionID = "8f1c2d3e-4a5b-4c6d-8e7f-901234567890"
		if _, err := service.Start(ctx, retried); !errors.Is(err, localturn.ErrThreadExecutorIsCloud) {
			t.Fatalf("start in %s = %v, want ErrThreadExecutorIsCloud", state, err)
		}
		if _, err := service.Commit(ctx, localturn.CommitRequest{
			ProjectID: 1, ActorUserID: owner, ExecutionID: started.ExecutionID,
			Credential:  localturn.Credential{TokenID: "77", NativeClientID: "ai.elitea.desktop"},
			UserMessage: "Fix it", AssistantMessage: "done",
		}); !errors.Is(err, localturn.ErrThreadExecutorIsCloud) {
			t.Fatalf("commit in %s = %v, want ErrThreadExecutorIsCloud", state, err)
		}
	}
	seedHandoffRow(t, pool, fixture.thread, owner, workspacestore.StateLocal, 4, false, "")
	if _, err := service.Commit(ctx, localturn.CommitRequest{
		ProjectID: 1, ActorUserID: owner, ExecutionID: started.ExecutionID,
		Credential:  localturn.Credential{TokenID: "77", NativeClientID: "ai.elitea.desktop"},
		UserMessage: "Fix it", AssistantMessage: "done",
	}); err != nil {
		t.Fatalf("commit after the thread came back = %v", err)
	}
}

func TestPostgresLocalWorkFenceAndRegenerationCheckSQL(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const owner int64 = 5311
	web := seedLocalWorkThread(t, pool, owner, "elitea")
	local := seedLocalWorkThread(t, pool, owner, "local_work")

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL search_path TO p_1, public`); err != nil {
		t.Fatal(err)
	}
	executor := pgxExecutor{queryer: tx}
	fence := func(thread workspacestore.Thread, project int64) (bool, bool) {
		t.Helper()
		id, _ := currentPGUUID(thread.ConversationUUID)
		localWork, cloud, err := executor.LocalWorkThreadFence(ctx, project, id)
		if err != nil {
			t.Fatal(err)
		}
		return localWork, cloud
	}
	setRow := func(state workspacestore.State, running string, pending bool) {
		t.Helper()
		var runningValue *string
		if running != "" {
			runningValue = &running
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.workspace_handoffs (project_id, conversation_uuid, handoff_id, state, epoch, owner_user_id,
    running_execution_id, reclaim_pending)
VALUES (1, $1::uuid, gen_random_uuid(), $2, 2, $3, $4, $5)
ON CONFLICT (project_id, conversation_uuid) DO UPDATE
SET state = EXCLUDED.state, running_execution_id = EXCLUDED.running_execution_id, reclaim_pending = EXCLUDED.reclaim_pending`,
			local.thread.ConversationUUID, string(state), owner, runningValue, pending); err != nil {
			t.Fatal(err)
		}
	}
	if localWork, cloud := fence(web.thread, 1); localWork || cloud {
		t.Fatalf("a web conversation = %t/%t", localWork, cloud)
	}
	if localWork, cloud := fence(local.thread, 1); !localWork || cloud {
		t.Fatalf("a Local work thread with no handoff = %t/%t, want shut", localWork, cloud)
	}
	for _, tc := range []struct {
		state   workspacestore.State
		running string
		pending bool
		open    bool
	}{
		{workspacestore.StateHandingOff, "", false, false},
		{workspacestore.StateCloudIdle, "", false, true},
		{workspacestore.StateCloudRunning, "exec", false, true},
		{workspacestore.StateCloudRunning, "exec", true, false},
		{workspacestore.StateReclaiming, "", false, false},
		{workspacestore.StateLocal, "", false, false},
	} {
		setRow(tc.state, tc.running, tc.pending)
		if localWork, cloud := fence(local.thread, 1); !localWork || cloud != tc.open {
			t.Fatalf("%s (pending %t): fence open = %t, want %t", tc.state, tc.pending, cloud, tc.open)
		}
	}
	// Another project's row with the same conversation id opens nothing.
	setRow(workspacestore.StateCloudIdle, "", false)
	if _, cloud := fence(local.thread, 2); cloud {
		t.Fatalf("the fence read a handoff row of another project")
	}

	// The regenerate check: a desktop answer, then a cloud answer (newest).
	var dummy int64
	if err := tx.QueryRow(ctx, `
SELECT p.id FROM chat_participants p JOIN chat_participant_mapping m ON m.participant_id = p.id
WHERE m.conversation_id = $1 AND p.entity_name = 'dummy'`, local.conversationID).Scan(&dummy); err != nil {
		t.Fatal(err)
	}
	insert := func(uuid, executedBy, offset string) {
		t.Helper()
		if _, err := tx.Exec(ctx, `
INSERT INTO chat_message_group (uuid, author_participant_id, conversation_id, meta, created_at)
VALUES ($1::uuid, $2, $3, jsonb_build_object('executed_by', $4::text), now() + $5::interval)`,
			uuid, dummy, local.conversationID, executedBy, offset); err != nil {
			t.Fatal(err)
		}
	}
	const desktopAnswer, cloudAnswer = "a1000000-0000-4000-8000-000000000001", "a2000000-0000-4000-8000-000000000002"
	insert(desktopAnswer, "desktop", "0 seconds")
	insert(cloudAnswer, "cloud_executor", "1 second")
	check := func(uuid string) (string, bool) {
		t.Helper()
		id, _ := currentPGUUID(uuid)
		executedBy, newest, err := executor.CloudRegenerationCheck(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return executedBy, newest
	}
	if executedBy, newest := check(desktopAnswer); executedBy != "desktop" || newest {
		t.Fatalf("desktop answer = %s/%t", executedBy, newest)
	}
	if executedBy, newest := check(cloudAnswer); executedBy != "cloud_executor" || !newest {
		t.Fatalf("cloud answer = %s/%t", executedBy, newest)
	}
}

// The first cloud turn after a handoff recalls a memory saved during the last
// desktop turn (ADR-0034 decision 8): the handoff event messages are authored
// by the system participant, so they do not move the recall boundary.
func TestPostgresCloudTurnAfterHandoffRecallsTheLastDesktopTurnsMemory(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const owner int64 = 5321
	fixture := seedLocalWorkThread(t, pool, owner, "local_work")
	memoriesRepo := NewMemoriesRepo(pool)
	// Eight older memories that overlap the input fill every ranked slot, so
	// the memory saved during the desktop turn is recalled only through the
	// next-turn reservation, which the recall boundary decides.
	long := strings.Repeat("restaurant dinner booking preference detail ", 7)
	for i := 0; i < 8; i++ {
		created, err := memoriesRepo.Create(ctx, "1", strconv.FormatInt(owner, 10), memories.MemoryEntry{
			Content: strconv.Itoa(i) + " " + long, Enabled: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
UPDATE p_1.personal_memory_entries SET created_at = now() - make_interval(mins => $2) WHERE id = $1::bigint`,
			created.ID, 120-i); err != nil {
			t.Fatal(err)
		}
	}
	service, err := localturn.NewService(NewLocalTurnsRepo(pool), &localTurnPolicy{allowed: true}, memoriesRepo,
		&localTurnAudit{}, func() (string, error) { return strings.Repeat("f", 32), nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	const input = "Book a restaurant for dinner, same preference as before."
	turn, err := service.Start(ctx, localturn.StartRequest{
		ProjectID: 1, ActorUserID: owner, TokenID: "79", ConversationUUID: fixture.thread.ConversationUUID,
		QuestionID: "41111111-2222-4333-8444-555555555555", UserInput: input,
	})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := memoriesRepo.Create(ctx, "1", strconv.FormatInt(owner, 10), memories.MemoryEntry{
		Content: "Prefers dark mode in every editor.", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Commit(ctx, localturn.CommitRequest{
		ProjectID: 1, ActorUserID: owner, Credential: localturn.Credential{TokenID: "79"}, ExecutionID: turn.ExecutionID,
		UserMessage: input, AssistantMessage: "Booked.",
	}); err != nil {
		t.Fatal(err)
	}
	// The send's event messages land in the conversation.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"handoff.complete", "handoff.release"} {
		if err := appendHandoffEvent(ctx, tx, fixture.thread, event, "an event", 2); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	recall, err := memoriesRepo.ResolveCurrentMemoryRecall(ctx, 1, owner, input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recall.Text, "Prefers dark mode in every editor.") || !containsRecallID(recall.IDs, fresh.ID) {
		t.Fatalf("the first cloud turn's recall = %+v, want the memory saved during the last desktop turn", recall)
	}
}
