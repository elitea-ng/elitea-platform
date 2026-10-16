package repos

import (
	"context"
	"testing"
	"time"
)

func TestCurrentNotificationEventRepositoryPostgresScopesByUserAndCursor(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := pool.Exec(ctx, `
INSERT INTO centry.notifications (
    uuid, is_seen, project_id, user_id, meta, event_type, created_at
, updated_at) VALUES
    ('00000000-0000-4000-8000-000000000001', FALSE, 7, 42,
     '{"message":"first for creator"}', 'index_data_changed', '2026-07-31 10:00:00', now() AT TIME ZONE 'UTC'),
    ('00000000-0000-4000-8000-000000000002', FALSE, 7, 99,
     '{"message":"other team member"}', 'index_data_changed', '2026-07-31 10:00:01', now() AT TIME ZONE 'UTC'),
    ('00000000-0000-4000-8000-000000000003', TRUE, 8, 42,
     '{"message":"second for creator"}', 'budget_threshold', '2026-07-31 10:00:02', now() AT TIME ZONE 'UTC')`); err != nil {
		t.Fatal(err)
	}

	repository, err := NewCurrentNotificationEventRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	highWater, err := repository.HighWater(ctx, 42)
	if err != nil || highWater != 3 {
		t.Fatalf("creator high water = (%d, %v), want (3, nil)", highWater, err)
	}
	events, err := repository.ListAfter(ctx, 42, 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Cursor != 3 || events[0].UserID != 42 ||
		events[0].ProjectID != 8 || events[0].EventType != "budget_threshold" {
		t.Fatalf("creator events after cursor = %+v", events)
	}
	otherEvents, err := repository.ListAfter(ctx, 99, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(otherEvents) != 1 || otherEvents[0].Cursor != 2 || otherEvents[0].UserID != 99 {
		t.Fatalf("other team member events = %+v", otherEvents)
	}
}
