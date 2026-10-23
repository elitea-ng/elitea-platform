package repos

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5/pgtype"
)

type currentNotificationEventQueriesStub struct {
	highWater int64
	rows      []sqlcgen.ListCurrentNotificationEventsAfterRow
	arg       sqlcgen.ListCurrentNotificationEventsAfterParams
	err       error
}

func (stub *currentNotificationEventQueriesStub) CurrentNotificationHighWater(
	context.Context,
	int32,
) (int64, error) {
	return stub.highWater, stub.err
}

func (stub *currentNotificationEventQueriesStub) ListCurrentNotificationEventsAfter(
	_ context.Context,
	arg sqlcgen.ListCurrentNotificationEventsAfterParams,
) ([]sqlcgen.ListCurrentNotificationEventsAfterRow, error) {
	stub.arg = arg
	return append([]sqlcgen.ListCurrentNotificationEventsAfterRow(nil), stub.rows...), stub.err
}

func TestCurrentNotificationEventRepositoryProjectsCurrentPayload(t *testing.T) {
	createdAt := time.Date(2026, time.July, 31, 16, 30, 0, 0, time.UTC)
	updatedAt := createdAt.Add(time.Minute)
	queries := &currentNotificationEventQueriesStub{
		highWater: 11,
		rows: []sqlcgen.ListCurrentNotificationEventsAfterRow{{
			ID:        11,
			Uuid:      "81816ebd-64de-4a55-815a-2d37471abf2e",
			ProjectID: 8,
			UserID:    42,
			Meta:      []byte(`{"message":"Index ready"}`),
			EventType: "index_data_changed",
			CreatedAt: pgtype.Timestamp{Time: createdAt, Valid: true},
			UpdatedAt: pgtype.Timestamp{Time: updatedAt, Valid: true},
		}},
	}
	repository, err := newCurrentNotificationEventRepository(queries)
	if err != nil {
		t.Fatal(err)
	}
	if cursor, err := repository.HighWater(context.Background(), 42); err != nil || cursor != 11 {
		t.Fatalf("HighWater() = (%d, %v), want (11, nil)", cursor, err)
	}
	events, err := repository.ListAfter(context.Background(), 42, 10, 25)
	if err != nil {
		t.Fatal(err)
	}
	if queries.arg.UserID != 42 || queries.arg.AfterCursor != 10 || queries.arg.PageLimit != 25 {
		t.Fatalf("sqlc args = %+v", queries.arg)
	}
	if len(events) != 1 || events[0].Cursor != 11 || events[0].UserID != 42 ||
		events[0].ProjectID != 8 || events[0].UpdatedAt == nil ||
		!events[0].UpdatedAt.Equal(updatedAt) {
		t.Fatalf("events = %+v", events)
	}
}

func TestCurrentNotificationEventRepositoryRejectsInvalidBoundaries(t *testing.T) {
	repository, err := newCurrentNotificationEventRepository(&currentNotificationEventQueriesStub{})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		userID int64
		cursor int64
		limit  int32
	}{
		{name: "zero user", cursor: 0, limit: 1},
		{name: "negative cursor", userID: 1, cursor: -1, limit: 1},
		{name: "zero limit", userID: 1, cursor: 0},
		{name: "oversized batch", userID: 1, cursor: 0, limit: maxCurrentNotificationEventBatch + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := repository.ListAfter(
				context.Background(), test.userID, test.cursor, test.limit,
			)
			if !errors.Is(err, ErrInvalidCurrentNotificationEventRead) {
				t.Fatalf("error = %v, want invalid read", err)
			}
		})
	}
}

func TestCurrentNotificationEventRepositoryRejectsCrossUserRows(t *testing.T) {
	queries := &currentNotificationEventQueriesStub{
		rows: []sqlcgen.ListCurrentNotificationEventsAfterRow{{
			ID:        2,
			Uuid:      "81816ebd-64de-4a55-815a-2d37471abf2e",
			ProjectID: 8,
			UserID:    99,
			Meta:      []byte(`{}`),
			EventType: "index_data_changed",
			CreatedAt: pgtype.Timestamp{Time: time.Now(), Valid: true},
		}},
	}
	repository, err := newCurrentNotificationEventRepository(queries)
	if err != nil {
		t.Fatal(err)
	}
	_, err = repository.ListAfter(context.Background(), 42, 1, 10)
	if !errors.Is(err, ErrInvalidCurrentNotificationEventRead) {
		t.Fatalf("error = %v, want cross-user rejection", err)
	}
}
