package repos

// Take back up to turn k, then the rest (ADR-0034 §4.6, shared/0176) against
// a REAL database: a release that left turns not taken keeps them for the
// return grace, the owner's desktop takes them back from LOCAL, a retry is
// idempotent, the refusals name their reason, and the sweeper returns an
// expired rest take back to LOCAL, never to CLOUD_IDLE.

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacestore"
)

func turns(n int) *int { return &n }

func TestPostgresWorkspaceHandoffRestTakeBack(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	h := newHandoffHarness(t, pool)
	const owner int64 = 5401
	fixture := seedLocalWorkThread(t, pool, owner, "local_work")
	thread := fixture.thread
	seedHandoffRow(t, pool, thread, owner, workspacestore.StateReclaiming, 3, false, "")

	for _, bad := range []int{-1, workspacehandoff.MaxTurnsNotTaken + 1} {
		if _, err := h.service.Release(ctx, workspacehandoff.ReleaseRequest{
			Thread: thread, ActorUserID: owner, Epoch: 3, Outcome: "applied", TurnsNotTaken: turns(bad),
		}); !errors.Is(err, workspacehandoff.ErrInvalid) {
			t.Fatalf("turns_not_taken %d = %v, want ErrInvalid", bad, err)
		}
	}
	released, err := h.service.Release(ctx, workspacehandoff.ReleaseRequest{
		Thread: thread, ActorUserID: owner, Epoch: 3, Outcome: "applied", TurnsNotTaken: turns(2),
	})
	if err != nil || released.State != workspacestore.StateLocal || released.Epoch != 4 || !released.RestPending ||
		!released.Released || released.DeleteAfter == nil {
		t.Fatalf("release up to turn k = %+v, %v; want LOCAL epoch 4, rest pending, a grace", released, err)
	}
	grace := *released.DeleteAfter
	if view, err := h.service.State(ctx, thread); err != nil || !view.RestPending {
		t.Fatalf("state after a partial release = %+v, %v; want rest_pending", view, err)
	}
	// The database clock decides the grace, as ReclaimRest does: an app clock
	// a month ahead still sees the rest pending, and the take back works.
	h.service.WithClock(func() time.Time { return time.Now().Add(30 * 24 * time.Hour) })
	if view, err := h.service.State(ctx, thread); err != nil || !view.RestPending {
		t.Fatalf("state with the app clock ahead = %+v, %v; want rest_pending by the database clock", view, err)
	}
	h.service.WithClock(time.Now)

	request := workspacehandoff.ReclaimRequest{Thread: thread, ActorUserID: owner, Epoch: 4, DeviceID: "device-2", Rest: true}
	rest, err := h.service.Reclaim(ctx, request)
	if err != nil || rest.State != workspacestore.StateReclaiming || rest.Epoch != 5 || !rest.RestPending ||
		rest.DeleteAfter == nil || !rest.DeleteAfter.Equal(grace) || rest.DeviceID != "device-2" {
		t.Fatalf("rest take back = %+v, %v; want RECLAIMING epoch 5, rest pending, the same grace", rest, err)
	}
	if again, err := h.service.Reclaim(ctx, request); err != nil || again.Epoch != 5 || again.State != workspacestore.StateReclaiming {
		t.Fatalf("a retried rest take back = %+v, %v; want the same RECLAIMING epoch 5", again, err)
	}
	if view, err := h.service.State(ctx, thread); err != nil || !view.RestPending {
		t.Fatalf("state during the rest take back = %+v, %v; want rest_pending", view, err)
	}
	if _, err := h.service.Reclaim(ctx, workspacehandoff.ReclaimRequest{Thread: thread, ActorUserID: owner, Epoch: 3, Rest: true}); !errors.Is(err, workspacehandoff.ErrStaleEpoch) {
		t.Fatalf("a rest take back at an old epoch = %v, want ErrStaleEpoch", err)
	}

	final, err := h.service.Release(ctx, workspacehandoff.ReleaseRequest{
		Thread: thread, ActorUserID: owner, Epoch: 5, Outcome: "applied", TurnsNotTaken: turns(0),
	})
	if err != nil || final.State != workspacestore.StateLocal || final.Epoch != 6 || final.RestPending ||
		final.DeleteAfter == nil || !final.DeleteAfter.Equal(grace) {
		t.Fatalf("release of the rest = %+v, %v; want LOCAL epoch 6, nothing pending, the first grace kept", final, err)
	}
	if view, err := h.service.State(ctx, thread); err != nil || view.RestPending {
		t.Fatalf("state after taking everything = %+v, %v; want no rest_pending", view, err)
	}
	if _, err := h.service.Reclaim(ctx, workspacehandoff.ReclaimRequest{Thread: thread, ActorUserID: owner, Epoch: 6, Rest: true}); !errors.Is(err, workspacehandoff.ErrStaleEpoch) {
		t.Fatalf("a rest take back with nothing left = %v, want ErrStaleEpoch", err)
	}
	if _, err := h.service.Reclaim(ctx, workspacehandoff.ReclaimRequest{Thread: thread, ActorUserID: owner + 1, Epoch: 6, Rest: true}); !errors.Is(err, workspacehandoff.ErrNotFound) {
		t.Fatalf("another user's rest take back = %v, want ErrNotFound", err)
	}

	actions := strings.Join(h.recorder.actions(), "\n")
	for _, want := range []string{"handoff.release: outcome applied, applied none, epoch 4, turns not taken 2",
		"handoff.reclaim: rest, from LOCAL, epoch 5", "turns not taken 0"} {
		if !strings.Contains(actions, want) {
			t.Errorf("audit lacks %q:\n%s", want, actions)
		}
	}
}

func TestPostgresWorkspaceHandoffRestTakeBackRefusals(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	h := newHandoffHarness(t, pool)
	const owner int64 = 5411
	releasedWith := func(t *testing.T, notTaken *int) workspacestore.Thread {
		t.Helper()
		fixture := seedLocalWorkThread(t, pool, owner, "local_work")
		seedHandoffRow(t, pool, fixture.thread, owner, workspacestore.StateReclaiming, 3, false, "")
		if _, err := h.service.Release(ctx, workspacehandoff.ReleaseRequest{
			Thread: fixture.thread, ActorUserID: owner, Epoch: 3, Outcome: "applied", TurnsNotTaken: notTaken,
		}); err != nil {
			t.Fatal(err)
		}
		return fixture.thread
	}
	rest := func(thread workspacestore.Thread) error {
		_, err := h.service.Reclaim(ctx, workspacehandoff.ReclaimRequest{Thread: thread, ActorUserID: owner, Epoch: 4, Rest: true})
		return err
	}

	t.Run("a release that did not say leaves nothing to take", func(t *testing.T) {
		if err := rest(releasedWith(t, nil)); !errors.Is(err, workspacehandoff.ErrStaleEpoch) {
			t.Fatalf("err = %v, want ErrStaleEpoch", err)
		}
	})
	t.Run("the return grace is over", func(t *testing.T) {
		thread := releasedWith(t, turns(3))
		if _, err := pool.Exec(ctx, `UPDATE elitea_runtime.workspace_handoffs SET delete_after = clock_timestamp() - interval '1 second'
WHERE conversation_uuid = $1::uuid`, thread.ConversationUUID); err != nil {
			t.Fatal(err)
		}
		if err := rest(thread); !errors.Is(err, workspacehandoff.ErrErased) {
			t.Fatalf("err = %v, want ErrErased", err)
		}
		if view, _ := h.service.State(ctx, thread); view.RestPending {
			t.Fatal("state answers rest_pending past the grace")
		}
	})
	t.Run("the released copy was erased", func(t *testing.T) {
		thread := releasedWith(t, turns(3))
		if _, err := h.service.Erase(ctx, workspacehandoff.EraseRequest{Thread: thread, ActorUserID: owner}); err != nil {
			t.Fatal(err)
		}
		if err := rest(thread); !errors.Is(err, workspacehandoff.ErrErased) {
			t.Fatalf("err = %v, want ErrErased", err)
		}
	})
	t.Run("a local turn is not committed", func(t *testing.T) {
		thread := releasedWith(t, turns(1))
		if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.local_turn_executions (execution_id, project_id, actor_id, token_id, native_client_id,
    conversation_uuid, question_id, response_message_id, target_participant_id, memories_used, expires_at)
VALUES ('bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', 1, $1::text, '91', 'ai.elitea.desktop', $2::uuid, gen_random_uuid(),
        gen_random_uuid(), 1, 0, clock_timestamp() + interval '1 hour')`, strconv.FormatInt(owner, 10), thread.ConversationUUID); err != nil {
			t.Fatal(err)
		}
		if err := rest(thread); !errors.Is(err, workspacehandoff.ErrLocalTurnInProgress) {
			t.Fatalf("err = %v, want ErrLocalTurnInProgress", err)
		}
	})
}

func TestPostgresWorkspaceHandoffSweeperExpiresARestTakeBackToLocal(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	h := newHandoffHarness(t, pool)
	const owner int64 = 5421
	restTakeBack := func(t *testing.T, stale bool) workspacestore.Thread {
		t.Helper()
		fixture := seedLocalWorkThread(t, pool, owner, "local_work")
		seedHandoffRow(t, pool, fixture.thread, owner, workspacestore.StateReclaiming, 3, false, "")
		if _, err := h.service.Release(ctx, workspacehandoff.ReleaseRequest{
			Thread: fixture.thread, ActorUserID: owner, Epoch: 3, Outcome: "applied", TurnsNotTaken: turns(2),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := h.service.Reclaim(ctx, workspacehandoff.ReclaimRequest{
			Thread: fixture.thread, ActorUserID: owner, Epoch: 4, Rest: true,
		}); err != nil {
			t.Fatal(err)
		}
		if !stale {
			return fixture.thread
		}
		// Older than the 7-day abandonment: a rest take back must never be
		// abandoned to CLOUD_IDLE.
		if _, err := pool.Exec(ctx, `UPDATE elitea_runtime.workspace_handoffs SET state_changed_at = clock_timestamp() - interval '8 days'
WHERE conversation_uuid = $1::uuid`, fixture.thread.ConversationUUID); err != nil {
			t.Fatal(err)
		}
		return fixture.thread
	}
	expired := restTakeBack(t, true)
	if _, err := pool.Exec(ctx, `UPDATE elitea_runtime.workspace_handoffs SET delete_after = clock_timestamp() - interval '1 second'
WHERE conversation_uuid = $1::uuid`, expired.ConversationUUID); err != nil {
		t.Fatal(err)
	}
	// Abandoned (RECLAIMING well past RestReclaimAbandonedAfter) within the
	// grace: back to LOCAL with the rest still pending, never CLOUD_IDLE.
	abandoned := restTakeBack(t, true)
	inGrace := restTakeBack(t, false)

	report, err := h.service.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if report.RestExpired != 1 || report.RestAbandoned != 1 || report.AbandonedReclaim != 0 {
		t.Fatalf("report = %+v, want one rest expiry, one rest abandonment and no reclaim abandonment", report)
	}
	back, err := h.service.State(ctx, abandoned)
	if err != nil || back.Handoff.State != workspacestore.StateLocal || !back.Handoff.Released ||
		!back.Handoff.RestPending || back.Handoff.Epoch != 6 || !back.RestPending || back.Handoff.DeleteAfter == nil {
		t.Fatalf("abandoned rest take back = %+v, %v; want LOCAL released epoch 6, the rest still pending", back.Handoff, err)
	}
	// The desktop that gave up cannot release the RECLAIMING it lost; its
	// release is refused, not read as a retried success.
	if _, err := h.service.Release(ctx, workspacehandoff.ReleaseRequest{
		Thread: abandoned, ActorUserID: owner, Epoch: 5, Outcome: "applied", TurnsNotTaken: turns(0),
	}); !errors.Is(err, workspacehandoff.ErrStaleEpoch) {
		t.Fatalf("release of an abandoned rest take back = %v, want ErrStaleEpoch", err)
	}
	// The rest may be asked for again within the grace.
	if again, err := h.service.Reclaim(ctx, workspacehandoff.ReclaimRequest{
		Thread: abandoned, ActorUserID: owner, Epoch: 6, Rest: true,
	}); err != nil || again.State != workspacestore.StateReclaiming || again.Epoch != 7 {
		t.Fatalf("rest take back after the abandonment = %+v, %v; want RECLAIMING epoch 7", again, err)
	}
	if !strings.Contains(strings.Join(h.recorder.actions(), "\n"), "rest take back abandoned") {
		t.Fatalf("the abandonment is not audited: %v", h.recorder.actions())
	}
	view, err := h.service.State(ctx, expired)
	if err != nil || view.Handoff.State != workspacestore.StateLocal || !view.Handoff.Released ||
		view.Handoff.RestPending || view.Handoff.Epoch != 6 || view.RestPending {
		t.Fatalf("expired rest take back = %+v, %v; want LOCAL released epoch 6, nothing pending", view.Handoff, err)
	}
	if kept, _ := h.service.State(ctx, inGrace); kept.Handoff.State != workspacestore.StateReclaiming || !kept.RestPending {
		t.Fatalf("a fresh rest take back = %+v; want it still RECLAIMING", kept.Handoff)
	}
	// The next pass (the erasure lease is a minute) erases the expired copy.
	h.service.WithClock(func() time.Time { return time.Now().Add(2 * time.Minute) })
	if _, err := h.service.Sweep(ctx); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if erased, _ := h.service.State(ctx, expired); erased.Handoff.DeletedAt == nil {
		t.Fatalf("expired rest take back not erased: %+v", erased.Handoff)
	}
	if kept, _ := h.service.State(ctx, inGrace); kept.Handoff.State != workspacestore.StateReclaiming || kept.Handoff.DeletedAt != nil {
		t.Fatalf("the erasure touched a RECLAIMING row: %+v", kept.Handoff)
	}
}
