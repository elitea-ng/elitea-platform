package repos

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	indexscheduleapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexschedule"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
)

func TestCurrentIndexScheduleDeleteRepositoryRemovesOnlyTarget(t *testing.T) {
	queries := &currentIndexScheduleDeleteQueriesStub{
		meta: []byte(
			`{"root":"keep","large_id":9007199254740993,"indexes_meta":{"docs":{"title":"Docs","schedules":{"-1":{"enabled":true},"11":{"enabled":false}}},"wiki":{"schedules":{"11":{"enabled":true}}}}}`,
		),
		updated: 1,
	}
	projects := &currentScheduleProjectStore{
		scriptedExecutor: &scriptedExecutor{},
	}
	repository, err := newCurrentIndexScheduleDeleteRepository(
		projects,
		func(sqlExecutor) (currentIndexScheduleDeleteQueries, error) {
			return queries, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := repository.Delete(
		context.Background(),
		indexscheduleapp.DeleteMutation{
			ProjectID: 7, ToolkitID: 19, IndexMetaID: "docs",
			TargetKey: "-1",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if projects.projectID != 7 ||
		projects.options.AccessMode != pgx.ReadWrite ||
		queries.lockToolkitID != 19 ||
		queries.update.ToolkitID != 19 {
		t.Fatalf(
			"project=%d lock=%d update=%+v",
			projects.projectID,
			queries.lockToolkitID,
			queries.update,
		)
	}
	persisted, err := decodeCurrentScheduleObject(
		queries.update.Meta,
	)
	if err != nil {
		t.Fatal(err)
	}
	if persisted["root"] != "keep" ||
		persisted["large_id"] != json.Number("9007199254740993") {
		t.Fatalf("unrelated metadata drifted: %#v", persisted)
	}
	indexes := persisted["indexes_meta"].(map[string]any)
	docs := indexes["docs"].(map[string]any)
	schedules := docs["schedules"].(map[string]any)
	if _, exists := schedules["-1"]; exists ||
		!reflect.DeepEqual(
			schedules["11"],
			map[string]any{"enabled": false},
		) ||
		!reflect.DeepEqual(indexes["wiki"], map[string]any{
			"schedules": map[string]any{
				"11": map[string]any{"enabled": true},
			},
		}) ||
		!reflect.DeepEqual(result.IndexesMeta, indexes) {
		t.Fatalf("indexes=%#v result=%#v", indexes, result.IndexesMeta)
	}
}

func TestCurrentIndexScheduleDeleteRepositoryDistinguishesNotFound(t *testing.T) {
	for _, test := range []struct {
		name string
		meta []byte
		err  error
		want error
	}{
		{
			name: "toolkit",
			err:  pgx.ErrNoRows,
			want: indexscheduleapp.ErrToolkitNotFound,
		},
		{
			name: "index",
			meta: []byte(`{"indexes_meta":{}}`),
			want: indexscheduleapp.ErrScheduleIndexNotFound,
		},
		{
			name: "user",
			meta: []byte(
				`{"indexes_meta":{"docs":{"schedules":{}}}}`,
			),
			want: indexscheduleapp.ErrScheduleUserNotFound,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			queries := &currentIndexScheduleDeleteQueriesStub{
				meta: test.meta,
				err:  test.err,
			}
			repository, err := newCurrentIndexScheduleDeleteRepository(
				&currentScheduleProjectStore{
					scriptedExecutor: &scriptedExecutor{},
				},
				func(sqlExecutor) (currentIndexScheduleDeleteQueries, error) {
					return queries, nil
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			_, err = repository.Delete(
				context.Background(),
				indexscheduleapp.DeleteMutation{
					ProjectID: 7, ToolkitID: 19,
					IndexMetaID: "docs", TargetKey: "11",
				},
			)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v want=%v", err, test.want)
			}
		})
	}
}

type currentIndexScheduleDeleteQueriesStub struct {
	lockToolkitID int32
	meta          []byte
	err           error
	update        sqlcgen.UpdateCurrentIndexScheduleToolkitMetaParams
	updated       int64
	updateErr     error
}

func (stub *currentIndexScheduleDeleteQueriesStub) LockCurrentIndexScheduleToolkitMeta(
	_ context.Context,
	toolkitID int32,
) ([]byte, error) {
	stub.lockToolkitID = toolkitID
	return append([]byte(nil), stub.meta...), stub.err
}

func (stub *currentIndexScheduleDeleteQueriesStub) UpdateCurrentIndexScheduleToolkitMeta(
	_ context.Context,
	arg sqlcgen.UpdateCurrentIndexScheduleToolkitMetaParams,
) (int64, error) {
	stub.update = arg
	return stub.updated, stub.updateErr
}
