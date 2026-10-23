package repos

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	indexscheduleapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexschedule"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
)

func TestCurrentIndexSchedulePatchRepositoryLocksAndMutatesOnlyTargetSchedule(t *testing.T) {
	t.Parallel()

	queries := &scriptedCurrentIndexSchedulePatchQueries{
		rowResults: []scriptedRow{{values: []any{
			[]byte(`{"pgvector_configuration":{"private":true},"untouched":17}`),
			[]byte(`{"root":"keep","large_id":9007199254740993,"indexes_meta":{"docs":{"title":"Docs","schedules":{"5":{"enabled":false}}},"wiki":{"marker":"keep"}}}`),
		}}},
		updateRows: []int64{1},
	}
	projects := &currentScheduleProjectStore{scriptedExecutor: &scriptedExecutor{}}
	repository, err := newCurrentIndexSchedulePatchRepository(
		projects,
		fixedCurrentIndexSchedulePatchQueries(queries),
	)
	if err != nil {
		t.Fatal(err)
	}

	result, err := repository.Patch(context.Background(), indexscheduleapp.Mutation{
		ProjectID: 7, ActorUserID: 11, ToolkitID: 19, IndexMetaID: "docs",
		RequestedUserID: -1,
		Schedule: indexscheduleapp.Schedule{
			Cron: "0 3 * * *", Enabled: true,
			Credentials: &indexscheduleapp.Credentials{
				Private: boolPointer(true), EliteaTitle: "personal-github",
			},
			CreatedBy: 11, Timezone: "Europe/Kyiv",
			LastRun: "2026-07-27T09:34:56.123456+00:00",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if projects.projectID != 7 ||
		projects.options.IsoLevel != pgx.ReadCommitted ||
		projects.options.AccessMode != pgx.ReadWrite {
		t.Fatalf("project=%d options=%+v", projects.projectID, projects.options)
	}
	if result.EffectiveUserID != 11 {
		t.Fatalf("effective user=%d", result.EffectiveUserID)
	}
	if !reflect.DeepEqual(queries.lockToolkitIDs, []int32{19}) {
		t.Fatalf("lock toolkit IDs=%+v", queries.lockToolkitIDs)
	}
	if len(queries.updateParams) != 1 ||
		queries.updateParams[0].ToolkitID != 19 {
		t.Fatalf("update=%+v", queries.updateParams)
	}

	persisted, err := decodeCurrentScheduleObject(queries.updateParams[0].Meta)
	if err != nil {
		t.Fatal(err)
	}
	if persisted["root"] != "keep" {
		t.Fatalf("unrelated metadata was changed: %#v", persisted)
	}
	if persisted["large_id"] != json.Number("9007199254740993") {
		t.Fatalf("metadata number was rounded: %#v", persisted["large_id"])
	}
	indexes := persisted["indexes_meta"].(map[string]any)
	if !reflect.DeepEqual(indexes["wiki"], map[string]any{"marker": "keep"}) {
		t.Fatalf("unrelated index was changed: %#v", indexes["wiki"])
	}
	docs := indexes["docs"].(map[string]any)
	if docs["title"] != "Docs" {
		t.Fatalf("target index fields were changed: %#v", docs)
	}
	schedules := docs["schedules"].(map[string]any)
	if _, present := schedules["5"]; !present {
		t.Fatalf("existing schedule was removed: %#v", schedules)
	}
	added := schedules["11"].(map[string]any)
	if added["cron"] != "0 3 * * *" ||
		added["enabled"] != true ||
		added["created_by"] != json.Number("11") ||
		added["timezone"] != "Europe/Kyiv" {
		t.Fatalf("added schedule=%#v", added)
	}
	if !reflect.DeepEqual(result.IndexesMeta, indexes) {
		t.Fatalf("raw indexes_meta result=%#v persisted=%#v", result.IndexesMeta, indexes)
	}
}

func TestCurrentIndexSchedulePatchRepositoryPreservesTeamAndCredentialRules(t *testing.T) {
	t.Parallel()

	schedule := indexscheduleapp.Schedule{
		Cron: "0 3 * * *", CreatedBy: 11, Timezone: "UTC",
		LastRun: "2026-07-27T09:34:56+00:00",
	}
	for _, test := range []struct {
		name        string
		privateTool bool
		credentials *indexscheduleapp.Credentials
		wantUser    int64
	}{
		{
			name: "public toolkit keeps project schedule",
			credentials: &indexscheduleapp.Credentials{
				Private: boolPointer(false), EliteaTitle: "shared-github",
			},
			wantUser: -1,
		},
		{
			name:        "private toolkit maps project sentinel to actor",
			privateTool: true,
			credentials: &indexscheduleapp.Credentials{
				Private: boolPointer(true), EliteaTitle: "personal-github",
			},
			wantUser: 11,
		},
		{
			name: "public toolkit preserves private creator credential",
			credentials: &indexscheduleapp.Credentials{
				Private: boolPointer(true), EliteaTitle: "personal-github",
			},
			wantUser: -1,
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			settings, err := json.Marshal(map[string]any{
				"pgvector_configuration": map[string]any{"private": test.privateTool},
			})
			if err != nil {
				t.Fatal(err)
			}
			queries := &scriptedCurrentIndexSchedulePatchQueries{
				rowResults: []scriptedRow{{values: []any{
					settings,
					[]byte(`{"indexes_meta":{"docs":{"schedules":{}}}}`),
				}}},
				updateRows: []int64{1},
			}
			repository, err := newCurrentIndexSchedulePatchRepository(
				&currentScheduleProjectStore{
					scriptedExecutor: &scriptedExecutor{},
				},
				fixedCurrentIndexSchedulePatchQueries(queries),
			)
			if err != nil {
				t.Fatal(err)
			}
			mutationSchedule := schedule
			mutationSchedule.Credentials = test.credentials
			result, err := repository.Patch(context.Background(), indexscheduleapp.Mutation{
				ProjectID: 7, ActorUserID: 11, ToolkitID: 19, IndexMetaID: "docs",
				RequestedUserID: -1, Schedule: mutationSchedule,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(queries.updateParams) != 1 {
				t.Fatalf("updates=%d", len(queries.updateParams))
			}
			if result.EffectiveUserID != test.wantUser {
				t.Fatalf("effective user=%d want=%d", result.EffectiveUserID, test.wantUser)
			}
			persisted, err := decodeCurrentScheduleObject(
				queries.updateParams[0].Meta,
			)
			if err != nil {
				t.Fatal(err)
			}
			indexes := persisted["indexes_meta"].(map[string]any)
			saved := indexes["docs"].(map[string]any)["schedules"].(map[string]any)[strconv.FormatInt(test.wantUser, 10)].(map[string]any)
			credentials := saved["credentials"].(map[string]any)
			if credentials["private"] != *test.credentials.Private ||
				credentials["elitea_title"] != test.credentials.EliteaTitle {
				t.Fatalf("credentials drifted: %#v", credentials)
			}
		})
	}
}

func TestCurrentIndexSchedulePatchRepositoryBoundsAndRedactsFailures(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		row      scriptedRow
		want     error
		mutation indexscheduleapp.Mutation
	}{
		{
			name: "missing toolkit",
			row:  scriptedRow{err: pgx.ErrNoRows},
			want: indexscheduleapp.ErrToolkitNotFound,
		},
		{
			name: "invalid settings",
			row: scriptedRow{values: []any{
				[]byte(`{"pgvector_configuration":{"private":"yes"}}`),
				[]byte(`{"indexes_meta":{}}`),
			}},
			want: indexscheduleapp.ErrInvalidToolkit,
		},
		{
			name: "null indexes meta preserves current failure",
			row: scriptedRow{values: []any{
				[]byte(`{"pgvector_configuration":{"private":false}}`),
				[]byte(`{"indexes_meta":null}`),
			}},
			want: indexscheduleapp.ErrInvalidToolkit,
		},
		{
			name: "database detail is redacted",
			row:  scriptedRow{err: errors.New("password=should-not-leak")},
			want: indexscheduleapp.ErrScheduleUnavailable,
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			queries := &scriptedCurrentIndexSchedulePatchQueries{
				rowResults: []scriptedRow{test.row},
			}
			repository, err := newCurrentIndexSchedulePatchRepository(
				&currentScheduleProjectStore{
					scriptedExecutor: &scriptedExecutor{},
				},
				fixedCurrentIndexSchedulePatchQueries(queries),
			)
			if err != nil {
				t.Fatal(err)
			}
			_, err = repository.Patch(context.Background(), indexscheduleapp.Mutation{
				ProjectID: 7, ActorUserID: 11, ToolkitID: 19, IndexMetaID: "docs",
				RequestedUserID: -1,
				Schedule: indexscheduleapp.Schedule{
					Cron: "0 3 * * *", CreatedBy: 11, Timezone: "UTC",
					LastRun: "2026-07-27T09:34:56+00:00",
				},
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v want=%v", err, test.want)
			}
			if strings.Contains(err.Error(), "password") {
				t.Fatalf("unsafe error=%v", err)
			}
		})
	}
}

type currentScheduleProjectStore struct {
	projectID int64
	options   pgx.TxOptions
	*scriptedExecutor
}

func (store *currentScheduleProjectStore) WithinProjectTx(
	ctx context.Context,
	projectID int64,
	options pgx.TxOptions,
	fn func(sqlExecutor) error,
) error {
	store.projectID = projectID
	store.options = options
	return fn(store.scriptedExecutor)
}

type scriptedCurrentIndexSchedulePatchQueries struct {
	rowResults     []scriptedRow
	updateRows     []int64
	updateErrors   []error
	lockToolkitIDs []int32
	updateParams   []sqlcgen.UpdateCurrentIndexScheduleToolkitMetaParams
}

func (queries *scriptedCurrentIndexSchedulePatchQueries) LockCurrentIndexScheduleToolkit(
	_ context.Context,
	toolkitID int32,
) (sqlcgen.LockCurrentIndexScheduleToolkitRow, error) {
	queries.lockToolkitIDs = append(queries.lockToolkitIDs, toolkitID)
	if len(queries.rowResults) == 0 {
		return sqlcgen.LockCurrentIndexScheduleToolkitRow{},
			errors.New("unexpected lock query")
	}
	result := queries.rowResults[0]
	queries.rowResults = queries.rowResults[1:]
	if result.err != nil {
		return sqlcgen.LockCurrentIndexScheduleToolkitRow{}, result.err
	}
	if len(result.values) != 2 {
		return sqlcgen.LockCurrentIndexScheduleToolkitRow{},
			errors.New("invalid scripted lock result")
	}
	settings, settingsOK := result.values[0].([]byte)
	meta, metaOK := result.values[1].([]byte)
	if !settingsOK || !metaOK {
		return sqlcgen.LockCurrentIndexScheduleToolkitRow{},
			errors.New("invalid scripted lock types")
	}
	return sqlcgen.LockCurrentIndexScheduleToolkitRow{
		Settings: settings,
		Meta:     meta,
	}, nil
}

func (queries *scriptedCurrentIndexSchedulePatchQueries) UpdateCurrentIndexScheduleToolkitMeta(
	_ context.Context,
	params sqlcgen.UpdateCurrentIndexScheduleToolkitMetaParams,
) (int64, error) {
	queries.updateParams = append(queries.updateParams, params)
	if len(queries.updateErrors) > 0 {
		err := queries.updateErrors[0]
		queries.updateErrors = queries.updateErrors[1:]
		if err != nil {
			return 0, err
		}
	}
	if len(queries.updateRows) == 0 {
		return 0, errors.New("unexpected schedule update")
	}
	updated := queries.updateRows[0]
	queries.updateRows = queries.updateRows[1:]
	return updated, nil
}

func fixedCurrentIndexSchedulePatchQueries(
	queries currentIndexSchedulePatchQueries,
) currentIndexSchedulePatchQueryFactory {
	return func(sqlExecutor) (currentIndexSchedulePatchQueries, error) {
		return queries, nil
	}
}

func boolPointer(value bool) *bool {
	return &value
}

// The registry runtime never reads the retired pgvector_configuration: a
// toolkit saved before the retirement with a private one keeps the requested
// schedule bucket instead of being resolved to the actor.
func TestRegistryScheduleEffectiveUserIgnoresTheRetiredPgvectorConfiguration(t *testing.T) {
	for name, settings := range map[string]map[string]any{
		"private reference":   {"pgvector_configuration": map[string]any{"private": true}},
		"malformed reference": {"pgvector_configuration": map[string]any{"private": "yes"}},
		"no reference":        {},
	} {
		got, err := currentScheduleEffectiveUser(settings, -1, 42, true)
		if err != nil || got != -1 {
			t.Fatalf("%s: user=%d err=%v, want the requested -1", name, got, err)
		}
	}
}
