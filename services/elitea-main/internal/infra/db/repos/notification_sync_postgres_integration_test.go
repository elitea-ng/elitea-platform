package repos

// The notification delta (ADR-0025 WP6) against the ledgered corpus: shared
// 0144's stamp and tombstone triggers and CurrentNotificationRepository.
// ListChanges.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/changesync"
)

func TestNotificationChangesArePerUserAndCarryMarkSeenAndDeletes(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// A row inserted without updated_at — the pylon writers' shape — still
	// gets a stamp, and an old one is backdated past the settle window.
	insert := func(user int, message string) int64 {
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO centry.notifications (uuid, is_seen, project_id, user_id, meta, event_type)
			VALUES (gen_random_uuid(), FALSE, 1, $1, jsonb_build_object('message', $2::text), 'test') RETURNING id`,
			user, message).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	mineOld := insert(42, "old")
	mineNew := insert(42, "new")
	theirs := insert(99, "theirs")
	backdate(t, pool, "centry.notifications", mineOld, time.Hour)

	repository, err := NewCurrentNotificationRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	first, err := repository.ListChanges(ctx, 42, "", 1)
	if err != nil {
		t.Fatalf("ListChanges: %v", err)
	}
	if len(first.Rows) != 1 || int64(first.Rows[0].ID) != mineOld || !first.HasMore {
		t.Fatalf("first page = %+v, want the old notification alone and has_more", first)
	}
	second, err := repository.ListChanges(ctx, 42, first.NextCursor, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Rows) != 1 || int64(second.Rows[0].ID) != mineNew {
		t.Fatalf("second page = %+v, want the new notification alone", second.Rows)
	}

	// Mark-seen surfaces as a change; a delete as a tombstone; another user's
	// delete never reaches this user.
	cursor := second.NextCursor
	backdate(t, pool, "centry.notifications", mineNew, time.Hour)
	if _, err := repository.MarkSeen(ctx, 42, mineOld); err != nil {
		t.Fatal(err)
	}
	if err := repository.Delete(ctx, 42, mineNew); err != nil {
		t.Fatal(err)
	}
	if err := repository.Delete(ctx, 99, theirs); err != nil {
		t.Fatal(err)
	}
	third, err := repository.ListChanges(ctx, 42, cursor, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Rows) != 1 || int64(third.Rows[0].ID) != mineOld || !third.Rows[0].IsSeen {
		t.Fatalf("after mark-seen the delta rows = %+v, want the old notification, seen", third.Rows)
	}
	if len(third.Tombstones) != 1 || third.Tombstones[0].ID != mineNew || third.Tombstones[0].Reason != changesync.ReasonDeleted {
		t.Fatalf("tombstones = %+v, want exactly user 42's deleted notification", third.Tombstones)
	}

	theirsPage, err := repository.ListChanges(ctx, 99, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(theirsPage.Rows) != 0 {
		t.Fatalf("user 99 sees rows %+v after deleting their only notification", theirsPage.Rows)
	}

	// Another user's cursor is refused; a back-dated one expires.
	if _, err := repository.ListChanges(ctx, 99, cursor, 10); !errors.Is(err, changesync.ErrInvalidCursor) {
		t.Fatalf("user 42's cursor presented by user 99: err = %v", err)
	}
	var now time.Time
	if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	expired := changesync.Encode(changesync.Cursor{
		Stream: changesync.StreamNotifications, Scope: strconv.Itoa(42),
		Tombs: changesync.Position{At: now.Add(-changesync.TombstoneRetention - time.Minute)},
	})
	if _, err := repository.ListChanges(ctx, 42, expired, 10); !errors.Is(err, changesync.ErrCursorExpired) {
		t.Fatalf("back-dated cursor: err = %v, want ErrCursorExpired", err)
	}
}
