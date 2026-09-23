package repos

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCurrentToolkitsRepositoryLoadsProviderNeutralCurrentRow(t *testing.T) {
	createdAt := time.Date(2026, time.July, 22, 9, 10, 11, 0, time.UTC)
	updatedAt := createdAt.Add(time.Hour)
	name := "docs"
	description := "Documentation source"
	sharedOwnerID := int32(31)
	sharedID := int32(41)
	queries := &currentToolkitQueriesStub{row: sqlcgen.EliteaTool{
		ID:          19,
		CreatedAt:   pgtype.Timestamp{Time: createdAt, Valid: true},
		UpdatedAt:   pgtype.Timestamp{Time: updatedAt, Valid: true},
		Type:        "provider-neutral",
		Name:        &name,
		Description: &description,
		Settings: []byte(`{
			"nested":{"id":9007199254740993,"ratio":1.25,"enabled":true,"absent":null},
			"items":[1,"two"]
		}`),
		AuthorID:      29,
		SharedOwnerID: &sharedOwnerID,
		SharedID:      &sharedID,
		Meta:          []byte(`{"revision":9223372036854775807,"tags":["one"]}`),
	}}
	projects := &currentToolkitProjectStore{}
	repository := newCurrentToolkitsRepositoryForTest(t, projects, queries)

	toolkit, err := repository.Get(context.Background(), 7, 19)
	if err != nil {
		t.Fatal(err)
	}
	if toolkit.ID != 19 || !toolkit.CreatedAt.Equal(createdAt) || toolkit.UpdatedAt == nil ||
		!toolkit.UpdatedAt.Equal(updatedAt) || toolkit.Type != "provider-neutral" ||
		toolkit.Name == nil || *toolkit.Name != name || toolkit.Description == nil ||
		*toolkit.Description != description || toolkit.AuthorID != 29 ||
		toolkit.SharedOwnerID == nil || *toolkit.SharedOwnerID != sharedOwnerID ||
		toolkit.SharedID == nil || *toolkit.SharedID != sharedID {
		t.Fatalf("mapped toolkit=%#v", toolkit)
	}
	wantSettings := map[string]any{
		"nested": map[string]any{
			"id":      json.Number("9007199254740993"),
			"ratio":   json.Number("1.25"),
			"enabled": true,
			"absent":  nil,
		},
		"items": []any{json.Number("1"), "two"},
	}
	wantMeta := map[string]any{
		"revision": json.Number("9223372036854775807"),
		"tags":     []any{"one"},
	}
	if !reflect.DeepEqual(toolkit.Settings, wantSettings) || !reflect.DeepEqual(toolkit.Meta, wantMeta) {
		t.Fatalf("JSON mapping settings=%#v meta=%#v", toolkit.Settings, toolkit.Meta)
	}
	if !reflect.DeepEqual(projects.projectIDs, []int64{7}) || len(projects.options) != 1 ||
		projects.options[0].IsoLevel != pgx.ReadCommitted || projects.options[0].AccessMode != pgx.ReadOnly {
		t.Fatalf("project transaction ids=%v options=%#v", projects.projectIDs, projects.options)
	}
	if queries.calls != 1 || queries.toolkitID != 19 {
		t.Fatalf("query calls=%d toolkit_id=%d", queries.calls, queries.toolkitID)
	}
}

func TestCurrentToolkitsRepositoryPreservesJSONShapesAndNullableColumns(t *testing.T) {
	queries := &currentToolkitQueriesStub{row: currentToolkitRow(
		7,
		`["source",9007199254740993,false]`,
		`null`,
	)}
	repository := newCurrentToolkitsRepositoryForTest(t, &currentToolkitProjectStore{}, queries)

	toolkit, err := repository.Get(context.Background(), 3, 7)
	if err != nil {
		t.Fatal(err)
	}
	wantSettings := []any{"source", json.Number("9007199254740993"), false}
	if !reflect.DeepEqual(toolkit.Settings, wantSettings) || toolkit.Meta != nil ||
		toolkit.Name != nil || toolkit.Description != nil || toolkit.UpdatedAt != nil ||
		toolkit.SharedOwnerID != nil || toolkit.SharedID != nil {
		t.Fatalf("shape-preserving toolkit=%#v", toolkit)
	}
}

func TestCurrentToolkitsRepositoryReturnsStableNotFoundAndDatabaseErrors(t *testing.T) {
	projects := &currentToolkitProjectStore{}
	queries := &currentToolkitQueriesStub{err: pgx.ErrNoRows}
	repository := newCurrentToolkitsRepositoryForTest(t, projects, queries)

	_, err := repository.Get(context.Background(), 4, 8)
	if !errors.Is(err, ErrCurrentToolkitNotFound) || err.Error() != ErrCurrentToolkitNotFound.Error() {
		t.Fatalf("not-found error=%v", err)
	}

	databaseErr := errors.New("database unavailable")
	queries.err = databaseErr
	_, err = repository.Get(context.Background(), 4, 8)
	if !errors.Is(err, databaseErr) || !strings.Contains(err.Error(), "get current toolkit row") {
		t.Fatalf("database error=%v", err)
	}
}

func TestCurrentToolkitsRepositoryAppliesOptionalMCPFolderAccess(t *testing.T) {
	t.Run("absent social projection preserves project RBAC", func(t *testing.T) {
		queries := &currentToolkitQueriesStub{
			row:         currentToolkitRow(19, `{}`, `{}`),
			accessState: 0,
		}
		repository := newCurrentToolkitsRepositoryForTest(t, &currentToolkitProjectStore{}, queries)

		toolkit, err := repository.GetMCPVisible(context.Background(), 7, 11, 19)
		if err != nil || toolkit.ID != 19 {
			t.Fatalf("toolkit=%#v error=%v", toolkit, err)
		}
		if queries.stateCalls != 1 || queries.calls != 1 || queries.visibleCalls != 0 {
			t.Fatalf("state=%d ordinary=%d visible=%d",
				queries.stateCalls, queries.calls, queries.visibleCalls)
		}
	})

	t.Run("complete social projection applies actor filter", func(t *testing.T) {
		queries := &currentToolkitQueriesStub{
			accessState: 1,
			visibleRow:  currentToolkitRow(19, `{}`, `{}`),
		}
		repository := newCurrentToolkitsRepositoryForTest(t, &currentToolkitProjectStore{}, queries)

		toolkit, err := repository.GetMCPVisible(context.Background(), 7, 11, 19)
		if err != nil || toolkit.ID != 19 {
			t.Fatalf("toolkit=%#v error=%v", toolkit, err)
		}
		if queries.calls != 0 || queries.visibleCalls != 1 ||
			queries.visibleParams.ToolkitID != 19 || queries.visibleParams.ActorID != 11 {
			t.Fatalf("ordinary=%d visible=%d params=%+v",
				queries.calls, queries.visibleCalls, queries.visibleParams)
		}

		queries.visibleErr = pgx.ErrNoRows
		_, err = repository.GetMCPVisible(context.Background(), 7, 11, 19)
		if !errors.Is(err, ErrCurrentToolkitNotFound) {
			t.Fatalf("restricted toolkit error=%v", err)
		}
	})

	t.Run("partial social projection fails closed", func(t *testing.T) {
		queries := &currentToolkitQueriesStub{accessState: -1}
		repository := newCurrentToolkitsRepositoryForTest(t, &currentToolkitProjectStore{}, queries)

		_, err := repository.GetMCPVisible(context.Background(), 7, 11, 19)
		if !errors.Is(err, ErrCurrentFolderAccessPartial) {
			t.Fatalf("partial projection error=%v", err)
		}
		if queries.calls != 0 || queries.visibleCalls != 0 {
			t.Fatalf("partial projection read toolkit: ordinary=%d visible=%d",
				queries.calls, queries.visibleCalls)
		}
	})
}

func TestCurrentToolkitsRepositoryRejectsInvalidRequestsBeforeDatabase(t *testing.T) {
	projects := &currentToolkitProjectStore{}
	queries := &currentToolkitQueriesStub{row: currentToolkitRow(1, `{}`, `{}`)}
	repository := newCurrentToolkitsRepositoryForTest(t, projects, queries)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	requests := []struct {
		ctx       context.Context
		projectID int32
		toolkitID int32
		want      error
	}{
		{ctx: nil, projectID: 1, toolkitID: 1, want: ErrInvalidCurrentToolkitRequest},
		{ctx: context.Background(), projectID: 0, toolkitID: 1, want: ErrInvalidCurrentToolkitRequest},
		{ctx: context.Background(), projectID: 1, toolkitID: 0, want: ErrInvalidCurrentToolkitRequest},
		{ctx: canceled, projectID: 1, toolkitID: 1, want: context.Canceled},
	}
	if _, err := repository.GetMCPVisible(context.Background(), 1, 0, 1); !errors.Is(err, ErrInvalidCurrentToolkitRequest) {
		t.Fatalf("zero actor error=%v", err)
	}
	for _, request := range requests {
		_, err := repository.Get(request.ctx, request.projectID, request.toolkitID)
		if !errors.Is(err, request.want) {
			t.Fatalf("request=%#v error=%v", request, err)
		}
	}
	if len(projects.projectIDs) != 0 || queries.calls != 0 {
		t.Fatalf("invalid request reached database: projects=%v calls=%d", projects.projectIDs, queries.calls)
	}
}

func TestCurrentToolkitsRepositoryRejectsInvalidRowsWithoutLeakingJSON(t *testing.T) {
	valid := currentToolkitRow(13, `{}`, `{}`)
	tests := []struct {
		name string
		row  sqlcgen.EliteaTool
	}{
		{name: "mismatched id", row: func() sqlcgen.EliteaTool { row := valid; row.ID = 14; return row }()},
		{name: "missing creation time", row: func() sqlcgen.EliteaTool { row := valid; row.CreatedAt.Valid = false; return row }()},
		{name: "malformed settings", row: func() sqlcgen.EliteaTool {
			row := valid
			row.Settings = []byte(`{"credential":"private-settings-value"`)
			return row
		}()},
		{name: "trailing settings", row: func() sqlcgen.EliteaTool {
			row := valid
			row.Settings = []byte(`{} "private-trailing-value"`)
			return row
		}()},
		{name: "malformed metadata", row: func() sqlcgen.EliteaTool {
			row := valid
			row.Meta = []byte(`{"credential":"private-meta-value"`)
			return row
		}()},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			queries := &currentToolkitQueriesStub{row: test.row}
			repository := newCurrentToolkitsRepositoryForTest(t, &currentToolkitProjectStore{}, queries)
			_, err := repository.Get(context.Background(), 5, 13)
			if !errors.Is(err, ErrInvalidCurrentToolkitRow) {
				t.Fatalf("error=%v", err)
			}
			for _, secret := range []string{"private-settings-value", "private-trailing-value", "private-meta-value"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("JSON value leaked in error: %v", err)
				}
			}
		})
	}
}

func TestNewCurrentToolkitsRepositoryRejectsMissingDependencies(t *testing.T) {
	if _, err := NewCurrentToolkitsRepository(nil); err == nil {
		t.Fatal("nil database pool was accepted")
	}
	if _, err := newCurrentToolkitsRepository(nil, func(sqlExecutor) (currentToolkitQueries, error) {
		return &currentToolkitQueriesStub{}, nil
	}); err == nil {
		t.Fatal("nil project store was accepted")
	}
	if _, err := newCurrentToolkitsRepository(&currentToolkitProjectStore{}, nil); err == nil {
		t.Fatal("nil query factory was accepted")
	}
}

func newCurrentToolkitsRepositoryForTest(
	t *testing.T,
	projects projectStore,
	queries currentToolkitQueries,
) *CurrentToolkitsRepository {
	t.Helper()
	repository, err := newCurrentToolkitsRepository(projects, func(sqlExecutor) (currentToolkitQueries, error) {
		return queries, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func currentToolkitRow(id int32, settings, meta string) sqlcgen.EliteaTool {
	return sqlcgen.EliteaTool{
		ID:        id,
		CreatedAt: pgtype.Timestamp{Time: time.Date(2026, time.July, 22, 8, 0, 0, 0, time.UTC), Valid: true},
		Type:      "toolkit",
		Settings:  []byte(settings),
		AuthorID:  1,
		Meta:      []byte(meta),
	}
}

type currentToolkitProjectStore struct {
	projectIDs []int64
	options    []pgx.TxOptions
}

func (s *currentToolkitProjectStore) WithinProjectTx(
	_ context.Context,
	projectID int64,
	options pgx.TxOptions,
	fn func(sqlExecutor) error,
) error {
	s.projectIDs = append(s.projectIDs, projectID)
	s.options = append(s.options, options)
	return fn(nil)
}

type currentToolkitQueriesStub struct {
	row           sqlcgen.EliteaTool
	err           error
	toolkitID     int32
	calls         int
	accessState   int32
	accessErr     error
	stateCalls    int
	visibleRow    sqlcgen.EliteaTool
	visibleErr    error
	visibleParams sqlcgen.GetCurrentMCPToolkitVisibleToActorParams
	visibleCalls  int
}

func (s *currentToolkitQueriesStub) CurrentFolderAccessState(context.Context) (int32, error) {
	s.stateCalls++
	return s.accessState, s.accessErr
}

func (s *currentToolkitQueriesStub) GetCurrentMCPToolkitVisibleToActor(
	_ context.Context,
	params sqlcgen.GetCurrentMCPToolkitVisibleToActorParams,
) (sqlcgen.EliteaTool, error) {
	s.visibleCalls++
	s.visibleParams = params
	return s.visibleRow, s.visibleErr
}

func (s *currentToolkitQueriesStub) GetCurrentToolkit(_ context.Context, toolkitID int32) (sqlcgen.EliteaTool, error) {
	s.calls++
	s.toolkitID = toolkitID
	return s.row, s.err
}
