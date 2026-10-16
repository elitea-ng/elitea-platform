package repos

import (
	"context"
	"errors"
	"testing"
	"time"

	notificationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/notifications"
)

func TestCurrentNotificationRepositoryPostgresPreservesGenericUserContract(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := pool.Exec(ctx, `
INSERT INTO centry.notifications (
    uuid, is_seen, project_id, user_id, meta, event_type, created_at
, updated_at) VALUES
    ('10000000-0000-4000-8000-000000000001', FALSE, 7, 42,
     '{"message":"Budget 100%_safe indexing completed"}', 'budget_threshold', '2026-07-31 10:00:00.123456', now() AT TIME ZONE 'UTC'),
    ('10000000-0000-4000-8000-000000000002', FALSE, 8, 42,
     '{"message":"Pipeline execution completed"}', 'pipeline_completed', '2026-07-31 10:00:01', now() AT TIME ZONE 'UTC'),
    ('10000000-0000-4000-8000-000000000003', FALSE, 7, 99,
     '{"message":"Other member activity"}', 'index_data_changed', '2026-07-31 10:00:02', now() AT TIME ZONE 'UTC')`); err != nil {
		t.Fatal(err)
	}

	repository, err := NewCurrentNotificationRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	filter := notificationapp.ListFilter{
		OnlyNew: true, SearchWords: []string{"Budget", "100%_safe"},
		SortBy: "created_at", SortOrder: "desc", Limit: 10,
	}
	count, err := repository.Count(ctx, 42, filter)
	if err != nil || count != 1 {
		t.Fatalf("filtered count=(%d,%v), want (1,nil)", count, err)
	}
	rows, err := repository.List(ctx, 42, filter)
	if err != nil || len(rows) != 1 || rows[0].EventType != "budget_threshold" ||
		rows[0].ProjectID != 7 || rows[0].UserID != 42 {
		t.Fatalf("filtered rows=%+v error=%v", rows, err)
	}

	if _, err := repository.Get(ctx, 99, int64(rows[0].ID)); !errors.Is(err, notificationapp.ErrNotificationNotFound) {
		t.Fatalf("cross-user get error=%v, want not found", err)
	}
	marked, err := repository.MarkSeen(ctx, 42, int64(rows[0].ID))
	if err != nil || !marked.IsSeen || marked.UpdatedAt == nil {
		t.Fatalf("marked notification=%+v error=%v", marked, err)
	}
	markedAgain, err := repository.MarkSeen(ctx, 42, int64(rows[0].ID))
	if err != nil || markedAgain.UpdatedAt == nil || !markedAgain.UpdatedAt.Equal(*marked.UpdatedAt) {
		t.Fatalf("idempotent mark changed timestamp: first=%+v second=%+v error=%v", marked, markedAgain, err)
	}

	updated, err := repository.BulkSetSeen(ctx, 42, nil, true, true)
	if err != nil || updated != 1 {
		t.Fatalf("mark-all result=(%d,%v), want (1,nil)", updated, err)
	}
	otherCount, err := repository.Count(ctx, 99, notificationapp.ListFilter{
		OnlyNew: true, SortBy: "created_at", SortOrder: "desc", Limit: 10,
	})
	if err != nil || otherCount != 1 {
		t.Fatalf("other user's unread count=(%d,%v), want (1,nil)", otherCount, err)
	}

	deleted, err := repository.BulkDelete(ctx, 42, []int64{int64(rows[0].ID), 3})
	if err != nil || deleted != 1 {
		t.Fatalf("user-scoped bulk delete=(%d,%v), want (1,nil)", deleted, err)
	}
	if err := repository.Delete(ctx, 42, 3); !errors.Is(err, notificationapp.ErrNotificationNotFound) {
		t.Fatalf("cross-user delete error=%v, want not found", err)
	}
}
