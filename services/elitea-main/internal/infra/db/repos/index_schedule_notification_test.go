package repos

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	indexscheduleapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexschedule"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
)

func TestCurrentIndexScheduleNotificationRepositoryPersistsCurrentShape(t *testing.T) {
	queries := &currentIndexScheduleNotificationQueriesStub{rows: 1}
	repository, err := newCurrentIndexScheduleNotificationRepository(queries)
	if err != nil {
		t.Fatal(err)
	}
	effect := validScheduleFailureEffect()
	if err := repository.Persist(context.Background(), effect); err != nil {
		t.Fatal(err)
	}
	if queries.calls != 1 ||
		queries.arg.NotificationUuid != scheduleFailureUUID(effect.EffectID) ||
		queries.arg.ProjectID != 7 ||
		queries.arg.UserID != 11 {
		t.Fatalf("calls=%d arg=%+v", queries.calls, queries.arg)
	}
	var metadata map[string]any
	if err := json.Unmarshal(queries.arg.Meta, &metadata); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"id":         nil,
		"index_name": "docs",
		"state":      "failed",
		"error":      "dependency unavailable",
		"reindex":    false,
		"indexed":    float64(0),
		"updated":    float64(0),
		"toolkit_id": float64(19),
		"initiator":  "schedule",
		"message":    "Index [docs]() is failed.",
	}
	if !reflect.DeepEqual(metadata, want) {
		t.Fatalf("metadata=%#v want=%#v", metadata, want)
	}
}

func TestCurrentIndexScheduleNotificationRepositoryAcceptsIdempotentReplay(t *testing.T) {
	queries := &currentIndexScheduleNotificationQueriesStub{rows: 0}
	repository, err := newCurrentIndexScheduleNotificationRepository(queries)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Persist(
		context.Background(),
		validScheduleFailureEffect(),
	); err != nil {
		t.Fatalf("idempotent conflict must succeed: %v", err)
	}
}

func TestScheduleFailureUUIDMatchesCurrentNotificationUUIDv4Contract(t *testing.T) {
	first := scheduleFailureUUID("stable-notification-effect")
	second := scheduleFailureUUID("stable-notification-effect")
	if first != second {
		t.Fatalf("notification UUID changed across replay: first=%q second=%q", first, second)
	}
	if len(first) != 36 || first[14] != '4' {
		t.Fatalf("notification UUID is not UUIDv4-shaped: %q", first)
	}
	switch first[19] {
	case '8', '9', 'a', 'b':
	default:
		t.Fatalf("notification UUID has an invalid RFC 4122 variant: %q", first)
	}
}

func TestCurrentIndexScheduleNotificationRepositoryMapsDependencyFailure(t *testing.T) {
	for _, test := range []struct {
		name    string
		rows    int64
		err     error
		wantErr error
	}{
		{
			name:    "database",
			err:     errors.New("database unavailable"),
			wantErr: indexscheduleapp.ErrScheduleDependency,
		},
		{
			name:    "impossible row count",
			rows:    2,
			wantErr: indexscheduleapp.ErrScheduleDependency,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, err := newCurrentIndexScheduleNotificationRepository(
				&currentIndexScheduleNotificationQueriesStub{
					rows: test.rows,
					err:  test.err,
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			err = repository.Persist(
				context.Background(),
				validScheduleFailureEffect(),
			)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error=%v want=%v", err, test.wantErr)
			}
		})
	}
}

type currentIndexScheduleNotificationQueriesStub struct {
	calls int
	arg   sqlcgen.InsertCurrentIndexScheduleNotificationParams
	rows  int64
	err   error
}

func (stub *currentIndexScheduleNotificationQueriesStub) InsertCurrentIndexScheduleNotification(
	_ context.Context,
	arg sqlcgen.InsertCurrentIndexScheduleNotificationParams,
) (int64, error) {
	stub.calls++
	stub.arg = arg
	return stub.rows, stub.err
}

func validScheduleFailureEffect() indexscheduleapp.FailureEffect {
	return indexscheduleapp.FailureEffect{
		EffectID:    "index.schedule.scan.v1:7:19:docs:11:2026-07-30T00:00:00Z",
		ProjectID:   7,
		UserID:      11,
		ToolkitID:   19,
		IndexMetaID: "docs",
		SafeReason:  "dependency unavailable",
		OccurredAt:  time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC),
	}
}
