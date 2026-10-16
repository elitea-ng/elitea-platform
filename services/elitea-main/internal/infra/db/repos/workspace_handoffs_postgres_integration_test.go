package repos

// The handoff state machine (ADR-0034 decision 7) against a REAL database
// (ELITEA_TEST_DATABASE_URL): every transition and its stale-epoch refusal,
// the send fences, exactly one winner among concurrent sends, the
// system-authored event messages, erase cancelling a running turn, and the
// sweeper's timed transitions and erasure.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacestore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacestore/workspacestoretest"
)

type fixedPolicy struct {
	mu     sync.Mutex
	policy workspacehandoff.Policy
}

func (f *fixedPolicy) Effective(context.Context, int64) (workspacehandoff.Policy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.policy, nil
}

func (f *fixedPolicy) set(change func(*workspacehandoff.Policy)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(&f.policy)
}

func enabledPolicy() workspacehandoff.Policy {
	return workspacehandoff.Policy{
		Enabled: true, Shell: true, Network: true, NetworkAllowlist: []string{},
		Limits: defaultWorkspaceLimits(), IdleRetention: 30 * 24 * time.Hour, ReturnGrace: 7 * 24 * time.Hour,
	}
}

type recordingCanceller struct {
	mu        sync.Mutex
	cancelled []string
}

func (c *recordingCanceller) CancelCloudTurn(_ context.Context, _, _ int64, responseMessageID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancelled = append(c.cancelled, responseMessageID)
	return nil
}

type handoffHarness struct {
	pool      *pgxpool.Pool
	store     *workspacestoretest.Store
	objects   *workspacestore.Service
	service   *workspacehandoff.Service
	policy    *fixedPolicy
	canceller *recordingCanceller
	recorder  *recordingAudit
}

func newHandoffHarness(t *testing.T, pool *pgxpool.Pool) handoffHarness {
	t.Helper()
	store := workspacestoretest.New(true)
	objects, _ := newWorkspaceStoreService(t, pool, store, defaultWorkspaceLimits())
	repo, err := NewWorkspaceHandoffsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	policy := &fixedPolicy{policy: enabledPolicy()}
	canceller := &recordingCanceller{}
	recorder := &recordingAudit{}
	service, err := workspacehandoff.NewService(repo, policy, objects, canceller, recorder, nil)
	if err != nil {
		t.Fatal(err)
	}
	return handoffHarness{pool: pool, store: store, objects: objects, service: service, policy: policy,
		canceller: canceller, recorder: recorder}
}

func beginRequest(thread workspacestore.Thread, owner int64) workspacehandoff.BeginRequest {
	return workspacehandoff.BeginRequest{
		Thread: thread, ActorUserID: owner, TokenID: "91", DeviceID: "device-1", HistoryMode: "none",
		Stats: workspacehandoff.Stats{Files: 42, Bytes: 1234, UnpackedBytes: 5555, Excluded: []string{"./.env", "id_rsa"}},
		Note:  "Fixed the parser; tests pass.",
	}
}

// publishSnapshot uploads and publishes base + handoff/1 for a HANDING_OFF thread.
func (h handoffHarness) publishSnapshot(t *testing.T, thread workspacestore.Thread, owner, epoch int64, seed string) string {
	t.Helper()
	content := []byte("snapshot " + seed)
	sha := uploadSegment(t, h.store, thread, content, time.Now())
	snapshot := sha1Of("snapshot " + seed)
	if _, err := h.objects.Publish(context.Background(), desktopPublish(thread, owner, epoch, sha, int64(len(content)),
		workspacestore.RefUpdate{Name: workspacestore.RefBase, New: sha1Of("base " + seed)},
		workspacestore.RefUpdate{Name: workspacestore.RefHandoff1, New: snapshot})); err != nil {
		t.Fatalf("publish snapshot: %v", err)
	}
	return snapshot
}

func eventMessages(t *testing.T, pool *pgxpool.Pool, conversationID int64) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT p.entity_name, t.content, g.meta ->> 'workspace_event'
FROM p_1.chat_message_group g
JOIN p_1.chat_participants p ON p.id = g.author_participant_id
JOIN p_1.chat_message_items i ON i.message_group_id = g.id
JOIN p_1.chat_messages_text t ON t.id = i.id
WHERE g.conversation_id = $1
ORDER BY g.created_at, g.id`, conversationID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var author, content string
		var event *string
		if err := rows.Scan(&author, &content, &event); err != nil {
			t.Fatal(err)
		}
		if author != "system" || event == nil {
			t.Fatalf("an event message is authored by %q (event %v); only the system participant may author it", author, event)
		}
		out = append(out, *event+": "+content)
	}
	return out
}

func TestPostgresWorkspaceHandoffLifecycle(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const owner, stranger int64 = 5201, 5202
	h := newHandoffHarness(t, pool)
	fixture := seedLocalWorkThread(t, pool, owner, "local_work")
	thread := fixture.thread

	// No row is LOCAL.
	if view, err := h.service.State(ctx, thread); err != nil || view.Present {
		t.Fatalf("state with no row = %+v, %v", view, err)
	}

	// The send fences.
	h.policy.set(func(p *workspacehandoff.Policy) { p.Enabled = false })
	if _, err := h.service.Begin(ctx, beginRequest(thread, owner)); !errors.Is(err, workspacehandoff.ErrDisabled) {
		t.Fatalf("begin with the policy off = %v", err)
	}
	h.policy.set(func(p *workspacehandoff.Policy) { p.Enabled = true })
	full := beginRequest(thread, owner)
	full.HistoryMode = "full"
	if _, err := h.service.Begin(ctx, full); !errors.Is(err, workspacehandoff.ErrHistoryModeNotAllowed) {
		t.Fatalf("begin with history full = %v", err)
	}
	over := beginRequest(thread, owner)
	over.Stats.Files = 20001
	if _, err := h.service.Begin(ctx, over); !errors.Is(err, workspacehandoff.ErrOverLimits) {
		t.Fatalf("begin over the file cap = %v", err)
	}
	if _, err := h.service.Begin(ctx, beginRequest(thread, stranger)); !errors.Is(err, workspacehandoff.ErrNotFound) {
		t.Fatalf("a stranger's begin = %v, want ErrNotFound", err)
	}
	web := seedLocalWorkThread(t, pool, owner, "elitea")
	if _, err := h.service.Begin(ctx, beginRequest(web.thread, owner)); !errors.Is(err, workspacehandoff.ErrNotLocalWork) {
		t.Fatalf("begin on a web conversation = %v", err)
	}
	// An uncommitted local turn blocks the send.
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.local_turn_executions (execution_id, project_id, actor_id, token_id, native_client_id,
    conversation_uuid, question_id, response_message_id, target_participant_id, memories_used, expires_at)
VALUES ('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 1, $1::text, '91', 'ai.elitea.desktop', $2::uuid, gen_random_uuid(),
        gen_random_uuid(), 1, 0, clock_timestamp() + interval '1 hour')`, strconv.FormatInt(owner, 10), thread.ConversationUUID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Begin(ctx, beginRequest(thread, owner)); !errors.Is(err, workspacehandoff.ErrLocalTurnInProgress) {
		t.Fatalf("begin with an uncommitted local turn = %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE elitea_runtime.local_turn_executions SET committed_at = clock_timestamp(), commit_digest = sha256('x'::bytea)
WHERE execution_id = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'`); err != nil {
		t.Fatal(err)
	}
	h.policy.set(func(p *workspacehandoff.Policy) { p.Limits.MaxProjectWorkspaceBytes = 1000 })
	if _, err := h.service.Begin(ctx, beginRequest(thread, owner)); !errors.Is(err, workspacehandoff.ErrQuotaExceeded) {
		t.Fatalf("begin over the project ceiling = %v", err)
	}
	h.policy.set(func(p *workspacehandoff.Policy) { p.Limits = defaultWorkspaceLimits() })

	// LOCAL → HANDING_OFF (epoch 1).
	began, err := h.service.Begin(ctx, beginRequest(thread, owner))
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if began.Handoff.State != workspacestore.StateHandingOff || began.Handoff.Epoch != 1 ||
		len(began.AllowedRefs) != 2 || began.Handoff.ExpiresAt == nil {
		t.Fatalf("begin = %+v", began)
	}
	if _, err := h.service.Begin(ctx, beginRequest(thread, owner)); !errors.Is(err, workspacehandoff.ErrInProgress) {
		t.Fatalf("a second begin = %v, want ErrInProgress", err)
	}

	// HANDING_OFF → CLOUD_IDLE needs the refs.
	complete := workspacehandoff.CompleteRequest{
		ProjectID: 1, HandoffID: began.Handoff.HandoffID, ActorUserID: owner, Epoch: 1,
		HandoffRef: workspacestore.RefHandoff1, SHA: sha1Of("nothing"),
	}
	if _, err := h.service.Complete(ctx, complete); !errors.Is(err, workspacehandoff.ErrRefsMissing) {
		t.Fatalf("complete before publish = %v", err)
	}
	complete.SHA = h.publishSnapshot(t, thread, owner, 1, "one")
	stale := complete
	stale.Epoch = 2
	if _, err := h.service.Complete(ctx, stale); !errors.Is(err, workspacehandoff.ErrStaleEpoch) {
		t.Fatalf("complete with a stale epoch = %v", err)
	}
	foreign := complete
	foreign.ActorUserID = stranger
	if _, err := h.service.Complete(ctx, foreign); !errors.Is(err, workspacehandoff.ErrNotFound) {
		t.Fatalf("a stranger's complete = %v", err)
	}
	idle, err := h.service.Complete(ctx, complete)
	if err != nil || idle.State != workspacestore.StateCloudIdle || idle.Epoch != 2 {
		t.Fatalf("complete = %+v, %v", idle, err)
	}

	// CLOUD_IDLE → CLOUD_RUNNING → CLOUD_IDLE keeps the epoch.
	const response = "7d5a8c0e-3f4b-4a2b-9c1d-0e2f3a4b5c6d"
	running, policy, err := h.service.StartCloudTurn(ctx, thread, 2, "exec-1", response)
	if err != nil || running.State != workspacestore.StateCloudRunning || running.Epoch != 2 || !policy.Enabled {
		t.Fatalf("start cloud turn = %+v, %v", running, err)
	}
	if _, _, err := h.service.StartCloudTurn(ctx, thread, 2, "exec-2", response); !errors.Is(err, workspacehandoff.ErrInProgress) {
		t.Fatalf("a second cloud turn = %v", err)
	}
	if _, err := h.service.FinishCloudTurn(ctx, thread, 2, "exec-2"); !errors.Is(err, workspacehandoff.ErrStaleEpoch) {
		t.Fatalf("finishing another turn = %v", err)
	}
	if back, err := h.service.FinishCloudTurn(ctx, thread, 2, "exec-1"); err != nil || back.State != workspacestore.StateCloudIdle || back.Epoch != 2 {
		t.Fatalf("finish = %+v, %v", back, err)
	}

	// Take back during a turn: pending, then RECLAIMING at turn end.
	if _, _, err := h.service.StartCloudTurn(ctx, thread, 2, "exec-3", response); err != nil {
		t.Fatal(err)
	}
	pending, err := h.service.Reclaim(ctx, workspacehandoff.ReclaimRequest{Thread: thread, ActorUserID: owner, Epoch: 2, DeviceID: "device-1"})
	if err != nil || !pending.ReclaimPending || pending.State != workspacestore.StateCloudRunning || pending.Epoch != 2 {
		t.Fatalf("reclaim during a turn = %+v, %v", pending, err)
	}
	if _, _, err := h.service.StartCloudTurn(ctx, thread, 2, "exec-4", response); err == nil {
		t.Fatalf("a cloud turn started while a take back is pending")
	}
	reclaiming, err := h.service.FinishCloudTurn(ctx, thread, 2, "exec-3")
	if err != nil || reclaiming.State != workspacestore.StateReclaiming || reclaiming.Epoch != 3 || reclaiming.ReclaimPending {
		t.Fatalf("turn end with a pending take back = %+v, %v", reclaiming, err)
	}
	// A retried take back answers the same row; a stale one is refused.
	if again, err := h.service.Reclaim(ctx, workspacehandoff.ReclaimRequest{Thread: thread, ActorUserID: owner, Epoch: 2}); err != nil || again.Epoch != 3 {
		t.Fatalf("retried reclaim = %+v, %v", again, err)
	}
	if _, err := h.service.Reclaim(ctx, workspacehandoff.ReclaimRequest{Thread: thread, ActorUserID: owner, Epoch: 1}); !errors.Is(err, workspacehandoff.ErrStaleEpoch) {
		t.Fatalf("stale reclaim = %v", err)
	}

	// RECLAIMING → LOCAL, with the return grace.
	released, err := h.service.Release(ctx, workspacehandoff.ReleaseRequest{Thread: thread, ActorUserID: owner, Epoch: 3, Outcome: "applied"})
	if err != nil || released.State != workspacestore.StateLocal || released.Epoch != 4 || released.DeleteAfter == nil ||
		released.DeleteAfter.Before(time.Now().Add(6*24*time.Hour)) {
		t.Fatalf("release = %+v, %v", released, err)
	}
	if _, err := h.service.Release(ctx, workspacehandoff.ReleaseRequest{Thread: thread, ActorUserID: owner, Epoch: 3, Outcome: "applied"}); err != nil {
		t.Fatalf("a retried release = %v", err)
	}

	// A second send of the same thread: a new handoff, refs reset.
	again, err := h.service.Begin(ctx, beginRequest(thread, owner))
	if err != nil || again.Handoff.Epoch != 5 || again.Handoff.HandoffID == began.Handoff.HandoffID {
		t.Fatalf("second begin = %+v, %v", again, err)
	}
	var refs int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.workspace_refs WHERE conversation_uuid = $1::uuid`, thread.ConversationUUID).Scan(&refs)
	if refs != 0 {
		t.Fatalf("a new send kept %d refs", refs)
	}

	messages := eventMessages(t, pool, fixture.conversationID)
	if len(messages) != 2 || !strings.HasPrefix(messages[0], "handoff.complete: Work moved to the cloud executor (42 files, 2 left on the laptop).") ||
		!strings.Contains(messages[0], "Fixed the parser") || messages[1] != "handoff.release: Work returned to the desktop." {
		t.Fatalf("event messages = %q", messages)
	}
	actions := strings.Join(h.recorder.actions(), "\n")
	for _, want := range []string{"handoff.begin", "handoff.complete", "handoff.reclaim", "handoff.release"} {
		if !strings.Contains(actions, want) {
			t.Fatalf("audit lacks %s:\n%s", want, actions)
		}
	}
}

// Two devices racing on a send: exactly one winner. Run with -race.
func TestPostgresWorkspaceHandoffConcurrentSendsOneWinner(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const owner int64 = 5211
	h := newHandoffHarness(t, pool)
	thread := seedLocalWorkThread(t, pool, owner, "local_work").thread
	for round := 0; round < 3; round++ {
		var (
			wg          sync.WaitGroup
			mu          sync.Mutex
			wins, loses int
			others      []error
		)
		start := make(chan struct{})
		for device := 0; device < 6; device++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := h.service.Begin(ctx, beginRequest(thread, owner))
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					wins++
				case errors.Is(err, workspacehandoff.ErrInProgress):
					loses++
				default:
					others = append(others, err)
				}
			}()
		}
		close(start)
		wg.Wait()
		if wins != 1 || loses != 5 || len(others) != 0 {
			t.Fatalf("round %d: wins %d, refused %d, others %v; want exactly one winner", round, wins, loses, others)
		}
		// Back to LOCAL for the next round (a desktop cancel), objects erased.
		view, _ := h.service.State(ctx, thread)
		if _, err := h.service.Erase(ctx, workspacehandoff.EraseRequest{Thread: thread, ActorUserID: owner}); err != nil {
			t.Fatal(err)
		}
		_ = view
	}
}

func TestPostgresWorkspaceHandoffEraseCancelsRunningTurn(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const owner, admin, stranger int64 = 5221, 5222, 5223
	h := newHandoffHarness(t, pool)
	fixture := seedLocalWorkThread(t, pool, owner, "local_work")
	thread := fixture.thread
	began, err := h.service.Begin(ctx, beginRequest(thread, owner))
	if err != nil {
		t.Fatal(err)
	}
	sha := h.publishSnapshot(t, thread, owner, 1, "erase")
	if _, err := h.service.Complete(ctx, workspacehandoff.CompleteRequest{ProjectID: 1, HandoffID: began.Handoff.HandoffID,
		ActorUserID: owner, Epoch: 1, HandoffRef: workspacestore.RefHandoff1, SHA: sha}); err != nil {
		t.Fatal(err)
	}
	const response = "8d5a8c0e-3f4b-4a2b-9c1d-0e2f3a4b5c6d"
	if _, _, err := h.service.StartCloudTurn(ctx, thread, 2, "exec-9", response); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Erase(ctx, workspacehandoff.EraseRequest{Thread: thread, ActorUserID: stranger}); !errors.Is(err, workspacehandoff.ErrNotFound) {
		t.Fatalf("a stranger's erase = %v", err)
	}
	erased, err := h.service.Erase(ctx, workspacehandoff.EraseRequest{Thread: thread, ActorUserID: admin, Manager: true})
	if err != nil || erased.State != workspacestore.StateLocal || erased.Epoch != 3 || erased.DeleteAfter == nil {
		t.Fatalf("erase = %+v, %v", erased, err)
	}
	if len(h.canceller.cancelled) != 1 || h.canceller.cancelled[0] != response {
		t.Fatalf("cancelled = %v, want the running turn", h.canceller.cancelled)
	}
	// The turn's late publish is fenced by the epoch.
	late := []byte("late cloud bundle")
	lateSHA := uploadSegment(t, h.store, thread, late, time.Now())
	request := desktopPublish(thread, owner, 2, lateSHA, int64(len(late)), workspacestore.RefUpdate{Name: workspacestore.RefCloud, New: sha1Of("late")})
	request.Caller = workspacestore.Caller{Writer: workspacestore.WriterCloudExecutor, ExecutionID: "exec-9"}
	if _, err := h.objects.Publish(ctx, request); !errors.Is(err, workspacestore.ErrStaleEpoch) {
		t.Fatalf("a publish after the erase = %v", err)
	}
	messages := eventMessages(t, pool, fixture.conversationID)
	if len(messages) != 2 || !strings.HasPrefix(messages[1], "handoff.erase: The cloud copy of this thread was erased.") {
		t.Fatalf("event messages = %q", messages)
	}

	// The erase deletes the objects at once and marks the row after an empty
	// listing; the sweeper then has nothing left to do.
	h.service.WithClock(func() time.Time { return time.Now().Add(2 * time.Minute) })
	report, err := h.service.Sweep(ctx)
	if err != nil || report.ObjectsErased != 0 {
		t.Fatalf("sweep = %+v, %v; want nothing left", report, err)
	}
	// Only the late turn's upload, written after the erasure, is left: it has
	// no segment row, so the store's orphan sweep deletes it after 24 hours.
	if h.store.Count() != 1 {
		t.Fatalf("%d objects left after the erasure, want only the late upload", h.store.Count())
	}
	view, err := h.service.State(ctx, thread)
	if err != nil || view.Handoff.DeletedAt == nil || view.DeleteAfter != nil {
		t.Fatalf("state after erasure = %+v, %v", view, err)
	}
}

func TestPostgresWorkspaceHandoffSweeperTimedTransitions(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const owner int64 = 5231
	h := newHandoffHarness(t, pool)

	// HANDING_OFF past 15 minutes → LOCAL.
	expiring := seedLocalWorkThread(t, pool, owner, "local_work")
	if _, err := h.service.Begin(ctx, beginRequest(expiring.thread, owner)); err != nil {
		t.Fatal(err)
	}
	// A take back pending past the 60-second grace → turn cancelled, RECLAIMING.
	graced := seedLocalWorkThread(t, pool, owner, "local_work")
	seedHandoffRow(t, pool, graced.thread, owner, workspacestore.StateCloudRunning, 2, false, "exec-g")
	if _, err := pool.Exec(ctx, `UPDATE elitea_runtime.workspace_handoffs
SET reclaim_pending = TRUE, reclaim_requested_at = clock_timestamp() - interval '2 minutes',
    running_response_message_id = '9d5a8c0e-3f4b-4a2b-9c1d-0e2f3a4b5c6d'
WHERE conversation_uuid = $1::uuid`, graced.thread.ConversationUUID); err != nil {
		t.Fatal(err)
	}
	// RECLAIMING for 8 days → CLOUD_IDLE.
	abandoned := seedLocalWorkThread(t, pool, owner, "local_work")
	seedHandoffRow(t, pool, abandoned.thread, owner, workspacestore.StateReclaiming, 3, false, "")
	// CLOUD_IDLE for 31 days → erased; for 2 days → kept.
	idle := seedLocalWorkThread(t, pool, owner, "local_work")
	seedHandoffRow(t, pool, idle.thread, owner, workspacestore.StateCloudIdle, 2, false, "")
	recent := seedLocalWorkThread(t, pool, owner, "local_work")
	seedHandoffRow(t, pool, recent.thread, owner, workspacestore.StateCloudIdle, 2, false, "")
	// CLOUD_IDLE for 28 days → the owner is warned (erased in 2 days).
	expiringIdle := seedLocalWorkThread(t, pool, owner, "local_work")
	seedHandoffRow(t, pool, expiringIdle.thread, owner, workspacestore.StateCloudIdle, 2, false, "")
	// A thread whose conversation was deleted → erased.
	orphan := seedLocalWorkThread(t, pool, owner, "local_work")
	seedHandoffRow(t, pool, orphan.thread, owner, workspacestore.StateCloudIdle, 2, false, "")
	for _, statement := range []struct {
		sql  string
		uuid string
	}{
		{`UPDATE elitea_runtime.workspace_handoffs SET state_changed_at = clock_timestamp() - interval '8 days' WHERE conversation_uuid = $1::uuid`, abandoned.thread.ConversationUUID},
		{`UPDATE elitea_runtime.workspace_handoffs SET state_changed_at = clock_timestamp() - interval '31 days' WHERE conversation_uuid = $1::uuid`, idle.thread.ConversationUUID},
		{`UPDATE elitea_runtime.workspace_handoffs SET state_changed_at = clock_timestamp() - interval '2 days' WHERE conversation_uuid = $1::uuid`, recent.thread.ConversationUUID},
		{`UPDATE elitea_runtime.workspace_handoffs SET state_changed_at = clock_timestamp() - interval '28 days' WHERE conversation_uuid = $1::uuid`, expiringIdle.thread.ConversationUUID},
		{`DELETE FROM p_1.chat_participant_mapping WHERE conversation_id = (SELECT id FROM p_1.chat_conversations WHERE uuid = $1::uuid)`, orphan.thread.ConversationUUID},
		{`DELETE FROM p_1.chat_conversations WHERE uuid = $1::uuid`, orphan.thread.ConversationUUID},
	} {
		if _, err := pool.Exec(ctx, statement.sql, statement.uuid); err != nil {
			t.Fatalf("%s: %v", statement.sql, err)
		}
	}

	h.service.WithClock(func() time.Time { return time.Now().Add(16 * time.Minute) })
	report, err := h.service.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if report.Expired != 1 || report.ForcedReclaims != 1 || report.AbandonedReclaim != 1 || report.IdleErased != 1 || report.OrphansErased != 1 ||
		report.IdleWarned != 1 {
		t.Fatalf("sweep report = %+v", report)
	}
	// The warning: one notification for the owner, with the thread and the
	// erasure date, and a message the notification list renders.
	var meta map[string]any
	if err := pool.QueryRow(ctx, `
SELECT meta FROM centry.notifications
WHERE event_type = 'workspace_idle_expiring' AND user_id = $1 AND meta->>'conversation_uuid' = $2`,
		owner, expiringIdle.thread.ConversationUUID).Scan(&meta); err != nil {
		t.Fatalf("idle warning notification: %v", err)
	}
	if meta["conversation_id"] != expiringIdle.thread.ConversationUUID || meta["delete_after"] == nil ||
		!strings.Contains(fmt.Sprint(meta["message"]), "erased on") {
		t.Fatalf("idle warning meta = %v", meta)
	}
	state := func(thread workspacestore.Thread) workspacehandoff.Handoff {
		view, err := h.service.State(ctx, thread)
		if err != nil {
			t.Fatal(err)
		}
		return view.Handoff
	}
	for _, tc := range []struct {
		name   string
		thread workspacestore.Thread
		state  workspacestore.State
		epoch  int64
	}{
		{"expired send", expiring.thread, workspacestore.StateLocal, 2},
		{"reclaim grace", graced.thread, workspacestore.StateReclaiming, 3},
		{"abandoned take back", abandoned.thread, workspacestore.StateCloudIdle, 4},
		{"idle retention", idle.thread, workspacestore.StateLocal, 3},
		{"recent idle", recent.thread, workspacestore.StateCloudIdle, 2},
		{"idle, warned", expiringIdle.thread, workspacestore.StateCloudIdle, 2},
		{"deleted conversation", orphan.thread, workspacestore.StateLocal, 3},
	} {
		got := state(tc.thread)
		if got.State != tc.state || got.Epoch != tc.epoch {
			t.Fatalf("%s: state %s epoch %d, want %s epoch %d", tc.name, got.State, got.Epoch, tc.state, tc.epoch)
		}
	}
	if len(h.canceller.cancelled) != 1 || h.canceller.cancelled[0] != "9d5a8c0e-3f4b-4a2b-9c1d-0e2f3a4b5c6d" {
		t.Fatalf("cancelled = %v", h.canceller.cancelled)
	}
	if messages := eventMessages(t, pool, idle.conversationID); len(messages) != 1 || !strings.HasPrefix(messages[0], "handoff.erase:") {
		t.Fatalf("idle erase messages = %q", messages)
	}
	actions := strings.Join(h.recorder.actions(), "\n")
	if !strings.Contains(actions, "handoff.expired") || !strings.Contains(actions, "handoff.erase") {
		t.Fatalf("audit = %s", actions)
	}
	// A second sweep changes nothing more (each transition happened once).
	again, err := h.service.Sweep(ctx)
	if err != nil || again.Expired+again.ForcedReclaims+again.AbandonedReclaim+again.IdleErased+again.OrphansErased+again.IdleWarned != 0 {
		t.Fatalf("second sweep = %+v, %v", again, err)
	}
}

// TestPostgresWorkspaceHandoffTransitionTable drives every request-driven
// transition from every state, at the row's epoch and at a stale one.
func TestPostgresWorkspaceHandoffTransitionTable(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const owner int64 = 5241
	const response = "6d5a8c0e-3f4b-4a2b-9c1d-0e2f3a4b5c6d"
	h := newHandoffHarness(t, pool)
	type result struct {
		state workspacestore.State
		epoch int64
		err   error
	}
	operations := map[string]func(thread workspacestore.Thread, epoch int64) (workspacehandoff.Handoff, error){
		"reclaim": func(thread workspacestore.Thread, epoch int64) (workspacehandoff.Handoff, error) {
			return h.service.Reclaim(ctx, workspacehandoff.ReclaimRequest{Thread: thread, ActorUserID: owner, Epoch: epoch})
		},
		"release": func(thread workspacestore.Thread, epoch int64) (workspacehandoff.Handoff, error) {
			return h.service.Release(ctx, workspacehandoff.ReleaseRequest{Thread: thread, ActorUserID: owner, Epoch: epoch, Outcome: "worktree"})
		},
		"start": func(thread workspacestore.Thread, epoch int64) (workspacehandoff.Handoff, error) {
			handoff, _, err := h.service.StartCloudTurn(ctx, thread, epoch, "other", response)
			return handoff, err
		},
		"finish": func(thread workspacestore.Thread, epoch int64) (workspacehandoff.Handoff, error) {
			return h.service.FinishCloudTurn(ctx, thread, epoch, "running")
		},
	}
	stale := result{err: workspacehandoff.ErrStaleEpoch}
	for _, tc := range []struct {
		state   workspacestore.State
		epoch   int64
		running string
		want    map[string]result // at the row's epoch
	}{
		{workspacestore.StateHandingOff, 1, "", map[string]result{
			"reclaim": stale, "release": stale, "start": stale, "finish": stale}},
		{workspacestore.StateCloudIdle, 2, "", map[string]result{
			"reclaim": {state: workspacestore.StateReclaiming, epoch: 3},
			"release": stale,
			"start":   {state: workspacestore.StateCloudRunning, epoch: 2},
			"finish":  stale}},
		{workspacestore.StateCloudRunning, 2, "running", map[string]result{
			"reclaim": {state: workspacestore.StateCloudRunning, epoch: 2},
			"release": stale,
			"start":   {err: workspacehandoff.ErrInProgress},
			"finish":  {state: workspacestore.StateCloudIdle, epoch: 2}}},
		{workspacestore.StateReclaiming, 3, "", map[string]result{
			"reclaim": stale, "release": {state: workspacestore.StateLocal, epoch: 4}, "start": stale, "finish": stale}},
		{workspacestore.StateLocal, 4, "", map[string]result{
			"reclaim": stale, "release": stale, "start": stale, "finish": stale}},
	} {
		for name, operation := range operations {
			for _, epochCase := range []string{"current", "stale"} {
				thread := seedLocalWorkThread(t, pool, owner, "local_work").thread
				seedHandoffRow(t, pool, thread, owner, tc.state, tc.epoch, false, tc.running)
				epoch := tc.epoch
				want := tc.want[name]
				if epochCase == "stale" {
					epoch = tc.epoch + 5
					want = stale
				}
				got, err := operation(thread, epoch)
				label := string(tc.state) + " " + name + " at the " + epochCase + " epoch"
				if want.err != nil {
					if !errors.Is(err, want.err) {
						t.Fatalf("%s: err = %v, want %v", label, err, want.err)
					}
					// A refusal changes nothing.
					view, _ := h.service.State(ctx, thread)
					if view.Handoff.State != tc.state || view.Handoff.Epoch != tc.epoch {
						t.Fatalf("%s: a refusal moved the row to %s/%d", label, view.Handoff.State, view.Handoff.Epoch)
					}
					continue
				}
				if err != nil || got.State != want.state || got.Epoch != want.epoch {
					t.Fatalf("%s = %s/%d, %v; want %s/%d", label, got.State, got.Epoch, err, want.state, want.epoch)
				}
			}
		}
		// Erase ends every handoff state; a LOCAL row stays at its epoch.
		thread := seedLocalWorkThread(t, pool, owner, "local_work").thread
		seedHandoffRow(t, pool, thread, owner, tc.state, tc.epoch, false, tc.running)
		erased, err := h.service.Erase(ctx, workspacehandoff.EraseRequest{Thread: thread, ActorUserID: owner})
		wantEpoch := tc.epoch + 1
		if tc.state == workspacestore.StateLocal {
			wantEpoch = tc.epoch
		}
		if err != nil || erased.State != workspacestore.StateLocal || erased.Epoch != wantEpoch {
			t.Fatalf("%s erase = %s/%d, %v", tc.state, erased.State, erased.Epoch, err)
		}
	}
}

// The desktop cancels a send (DELETE in HANDING_OFF): the thread is LOCAL at
// once, its objects go, and a new send is accepted right away. When the store
// cannot delete, the row waits for the sweeper, a send is refused until the
// listing is empty, and the sweeper finishes the erasure.
func TestPostgresWorkspaceHandoffCancelSend(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const owner int64 = 5251
	h := newHandoffHarness(t, pool)
	thread := seedLocalWorkThread(t, pool, owner, "local_work").thread
	if _, err := h.service.Begin(ctx, beginRequest(thread, owner)); err != nil {
		t.Fatal(err)
	}
	h.publishSnapshot(t, thread, owner, 1, "cancel")
	cancelled, err := h.service.Erase(ctx, workspacehandoff.EraseRequest{Thread: thread, ActorUserID: owner})
	if err != nil || cancelled.State != workspacestore.StateLocal || h.store.Count() != 0 {
		t.Fatalf("cancel = %+v, %v, %d objects left", cancelled, err, h.store.Count())
	}
	again, err := h.service.Begin(ctx, beginRequest(thread, owner))
	if err != nil || again.Handoff.State != workspacestore.StateHandingOff {
		t.Fatalf("a send right after a cancel = %+v, %v", again, err)
	}
	h.publishSnapshot(t, thread, owner, again.Handoff.Epoch, "cancel two")
	h.store.FailDelete = true
	if _, err := h.service.Erase(ctx, workspacehandoff.EraseRequest{Thread: thread, ActorUserID: owner}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Begin(ctx, beginRequest(thread, owner)); !errors.Is(err, workspacehandoff.ErrInProgress) {
		t.Fatalf("a send while the erasure is unfinished = %v, want ErrInProgress", err)
	}
	h.store.FailDelete = false
	h.service.WithClock(func() time.Time { return time.Now().Add(2 * time.Minute) })
	if report, err := h.service.Sweep(ctx); err != nil || report.ObjectsErased != 1 || h.store.Count() != 0 {
		t.Fatalf("sweep = %+v, %v, %d objects", report, err, h.store.Count())
	}
	h.service.WithClock(time.Now)
	if _, err := h.service.Begin(ctx, beginRequest(thread, owner)); err != nil {
		t.Fatalf("a send after the sweeper finished = %v", err)
	}
}

// User offboarding (ADR-0034 decision 12): a thread whose owner is suspended
// (an administrator's suspension or a SCIM deactivation) or deleted is erased
// by the sweeper, and a running cloud turn is stopped. An active owner's
// thread is kept.
func TestPostgresWorkspaceHandoffSweeperErasesOnUserOffboarding(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	withWorkspaceUsersTable(t, pool)
	h := newHandoffHarness(t, pool)
	const active, suspended, deleted int64 = 5291, 5292, 5293
	kept := seedLocalWorkThread(t, pool, active, "local_work")
	seedHandoffRow(t, pool, kept.thread, active, workspacestore.StateCloudIdle, 2, false, "")
	running := seedLocalWorkThread(t, pool, suspended, "local_work")
	seedHandoffRow(t, pool, running.thread, suspended, workspacestore.StateCloudRunning, 2, false, "exec-off")
	if _, err := pool.Exec(ctx, `UPDATE elitea_runtime.workspace_handoffs
SET running_response_message_id = '7e5a8c0e-3f4b-4a2b-9c1d-0e2f3a4b5c6d' WHERE conversation_uuid = $1::uuid`,
		running.thread.ConversationUUID); err != nil {
		t.Fatal(err)
	}
	gone := seedLocalWorkThread(t, pool, deleted, "local_work")
	seedHandoffRow(t, pool, gone.thread, deleted, workspacestore.StateCloudIdle, 2, false, "")
	if _, err := pool.Exec(ctx, `UPDATE public.auth_core__user SET suspended = true WHERE id = $1`, suspended); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM public.auth_core__user WHERE id = $1`, deleted); err != nil {
		t.Fatal(err)
	}
	report, err := h.service.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if report.OrphansErased != 2 {
		t.Fatalf("sweep report = %+v, want the two offboarded owners' threads erased", report)
	}
	// The objects go at a later sweep (the erasure claim is a one-minute
	// lease on updated_at).
	h.service.WithClock(func() time.Time { return time.Now().Add(2 * time.Minute) })
	if _, err := h.service.Sweep(ctx); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	for _, tc := range []struct {
		name   string
		thread workspacestore.Thread
		state  workspacestore.State
	}{
		{"active owner", kept.thread, workspacestore.StateCloudIdle},
		{"suspended owner", running.thread, workspacestore.StateLocal},
		{"deleted owner", gone.thread, workspacestore.StateLocal},
	} {
		view, err := h.service.State(ctx, tc.thread)
		if err != nil || view.Handoff.State != tc.state {
			t.Fatalf("%s: %+v, %v; want %s", tc.name, view.Handoff, err, tc.state)
		}
		if tc.state == workspacestore.StateLocal && view.Handoff.DeletedAt == nil {
			t.Fatalf("%s: not erased: %+v", tc.name, view.Handoff)
		}
	}
	if len(h.canceller.cancelled) != 1 || h.canceller.cancelled[0] != "7e5a8c0e-3f4b-4a2b-9c1d-0e2f3a4b5c6d" {
		t.Fatalf("cancelled = %v, want the suspended owner's running turn", h.canceller.cancelled)
	}
}

// A release that loses the race to an erase is not reported as a release:
// the row came to LOCAL by the erase (released = FALSE), so the release and
// its retries answer ErrErased (409 handoff_erased). A real release's retry
// still succeeds.
func TestPostgresWorkspaceHandoffReleaseAfterEraseIsRefused(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	h := newHandoffHarness(t, pool)
	const owner int64 = 5295
	erasedFirst := seedLocalWorkThread(t, pool, owner, "local_work")
	seedHandoffRow(t, pool, erasedFirst.thread, owner, workspacestore.StateReclaiming, 3, false, "")
	if _, err := h.service.Erase(ctx, workspacehandoff.EraseRequest{Thread: erasedFirst.thread, ActorUserID: owner}); err != nil {
		t.Fatal(err)
	}
	for _, epoch := range []int64{3, 2} {
		if _, err := h.service.Release(ctx, workspacehandoff.ReleaseRequest{
			Thread: erasedFirst.thread, ActorUserID: owner, Epoch: epoch, Outcome: "applied",
		}); !errors.Is(err, workspacehandoff.ErrErased) {
			t.Fatalf("release at epoch %d after an erase = %v, want ErrErased", epoch, err)
		}
	}

	releasedFirst := seedLocalWorkThread(t, pool, owner, "local_work")
	seedHandoffRow(t, pool, releasedFirst.thread, owner, workspacestore.StateReclaiming, 3, false, "")
	request := workspacehandoff.ReleaseRequest{Thread: releasedFirst.thread, ActorUserID: owner, Epoch: 3, Outcome: "applied"}
	if _, err := h.service.Release(ctx, request); err != nil {
		t.Fatal(err)
	}
	if again, err := h.service.Release(ctx, request); err != nil || again.State != workspacestore.StateLocal {
		t.Fatalf("a retried release = %+v, %v", again, err)
	}
	// Erasing the released copy within its grace keeps it a release.
	if _, err := h.service.Erase(ctx, workspacehandoff.EraseRequest{Thread: releasedFirst.thread, ActorUserID: owner}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Release(ctx, request); err != nil {
		t.Fatalf("a retried release after erasing the released copy = %v", err)
	}
}
