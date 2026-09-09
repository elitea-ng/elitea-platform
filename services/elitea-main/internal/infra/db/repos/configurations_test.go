package repos

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCurrentConfigurationsRepositoryFindsExactEmbeddingModelWithoutExpansion(t *testing.T) {
	saved := []byte(`{"name":"text-embedding-3-small","ai_credentials":{"elitea_title":"credential-current","private":true}}`)
	queries := &currentConfigurationQueriesStub{embeddingRows: []sqlcgen.FindCurrentEmbeddingConfigurationsRow{{
		ConfigurationUuid: "00000000-0000-0000-0000-000000000101",
		ProjectID:         7,
		Type:              "embedding_model",
		Section:           "embedding",
		Data:              saved,
	}}}
	store := &currentConfigurationProjectStore{}
	repository := newCurrentConfigurationsRepositoryForTest(t, store, queries)

	configuration, found, err := repository.FindCurrentEmbeddingConfiguration(
		context.Background(),
		7,
		"text-embedding-3-small",
		false,
	)
	if err != nil || !found {
		t.Fatalf("configuration=%#v found=%t err=%v", configuration, found, err)
	}
	if configuration.UUID != "00000000-0000-0000-0000-000000000101" ||
		configuration.ProjectID != 7 ||
		!reflect.DeepEqual([]byte(configuration.Data), saved) {
		t.Fatalf("configuration=%#v", configuration)
	}
	if queries.embeddingParams != (sqlcgen.FindCurrentEmbeddingConfigurationsParams{
		ProjectID: 7,
		ModelName: "text-embedding-3-small",
	}) {
		t.Fatalf("params=%#v", queries.embeddingParams)
	}
	if !reflect.DeepEqual(store.projectIDs, []int64{7}) {
		t.Fatalf("project transaction routing=%v", store.projectIDs)
	}

	saved[2] = 'X'
	if configuration.Data[2] == 'X' {
		t.Fatal("embedding configuration aliases database scan memory")
	}
}

func TestCurrentConfigurationsRepositoryRejectsAmbiguousOrOutOfScopeEmbeddingModels(t *testing.T) {
	valid := sqlcgen.FindCurrentEmbeddingConfigurationsRow{
		ConfigurationUuid: "00000000-0000-0000-0000-000000000101",
		ProjectID:         1,
		Type:              "embedding_model",
		Section:           "embedding",
		Data:              []byte(`{"name":"embedding","ai_credentials":{"elitea_title":"credential","private":false}}`),
		Shared:            true,
	}
	t.Run("duplicate", func(t *testing.T) {
		queries := &currentConfigurationQueriesStub{embeddingRows: []sqlcgen.FindCurrentEmbeddingConfigurationsRow{valid, valid}}
		repository := newCurrentConfigurationsRepositoryForTest(t, &currentConfigurationProjectStore{}, queries)
		if _, _, err := repository.FindCurrentEmbeddingConfiguration(
			context.Background(), 1, "embedding", true,
		); !errors.Is(err, indexingapp.ErrCurrentEmbeddingBindingAmbiguous) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("not shared", func(t *testing.T) {
		notShared := valid
		notShared.Shared = false
		queries := &currentConfigurationQueriesStub{embeddingRows: []sqlcgen.FindCurrentEmbeddingConfigurationsRow{notShared}}
		repository := newCurrentConfigurationsRepositoryForTest(t, &currentConfigurationProjectStore{}, queries)
		if _, _, err := repository.FindCurrentEmbeddingConfiguration(
			context.Background(), 1, "embedding", true,
		); err == nil || !strings.Contains(err.Error(), "authorized scope") {
			t.Fatalf("error=%v", err)
		}
	})
}

func TestCurrentConfigurationsRepositoryRoutesCurrentAndSharedLists(t *testing.T) {
	pinnedCurrentRow := sqlcgen.ListCurrentConfigurationsRow(currentConfigurationRow(7, 11))
	pinnedCurrentRow.IsPinned = true
	queries := &currentConfigurationQueriesStub{
		currentCount: 2,
		sharedCount:  1,
		currentRows: []sqlcgen.ListCurrentConfigurationsRow{
			pinnedCurrentRow,
		},
		sharedRows: []sqlcgen.ListCurrentSharedConfigurationsRow{
			sqlcgen.ListCurrentSharedConfigurationsRow(currentConfigurationRow(1, 12)),
		},
	}
	store := &currentConfigurationProjectStore{}
	repository := newCurrentConfigurationsRepositoryForTest(t, store, queries)

	currentFilter := configurationapp.CurrentConfigurationListFilter{
		IDs: []int32{11, 12}, ProjectID: 7, Types: []string{"github"}, Sections: []string{"credentials"},
		Offset: 3, Limit: 25, LabelQuery: "team", SortBy: "elitea_title", SortOrder: "asc",
	}
	total, err := repository.Count(context.Background(), currentFilter)
	if err != nil || total != 2 {
		t.Fatalf("current count=%d err=%v", total, err)
	}
	items, err := repository.List(context.Background(), currentFilter)
	if err != nil || len(items) != 1 || items[0].ProjectID != 7 || !items[0].IsPinned {
		t.Fatalf("current items=%#v err=%v", items, err)
	}
	if queries.countCurrentParams.LabelQuery != "team" || queries.listCurrentParams.OffsetRows != 3 || queries.listCurrentParams.LimitRows != 25 {
		t.Fatalf("current params: count=%#v list=%#v", queries.countCurrentParams, queries.listCurrentParams)
	}
	if queries.listCurrentCalls != 1 || queries.listSharedCalls != 0 {
		t.Fatalf("current query calls: current=%d shared=%d", queries.listCurrentCalls, queries.listSharedCalls)
	}

	sharedFilter := currentFilter
	sharedFilter.ProjectID = 1
	sharedFilter.SharedOnly = true
	total, err = repository.Count(context.Background(), sharedFilter)
	if err != nil || total != 1 {
		t.Fatalf("shared count=%d err=%v", total, err)
	}
	items, err = repository.List(context.Background(), sharedFilter)
	if err != nil || len(items) != 1 || items[0].ProjectID != 1 || !items[0].Shared {
		t.Fatalf("shared items=%#v err=%v", items, err)
	}
	if queries.listCurrentCalls != 1 || queries.listSharedCalls != 1 {
		t.Fatalf("shared query calls: current=%d shared=%d", queries.listCurrentCalls, queries.listSharedCalls)
	}
	if !reflect.DeepEqual(queries.countSharedParams.Types, []string{"github"}) || !reflect.DeepEqual(queries.listSharedParams.Sections, []string{"credentials"}) {
		t.Fatalf("shared params: count=%#v list=%#v", queries.countSharedParams, queries.listSharedParams)
	}

	for _, ids := range [][]int32{queries.countCurrentParams.Ids, queries.listCurrentParams.Ids, queries.countSharedParams.Ids, queries.listSharedParams.Ids} {
		if !reflect.DeepEqual(ids, []int32{11, 12}) {
			t.Fatalf("IDs not forwarded: %v", ids)
		}
	}
	if !reflect.DeepEqual(store.projectIDs, []int64{7, 7, 1, 1}) {
		t.Fatalf("project transaction routing=%v", store.projectIDs)
	}
	for _, options := range store.options {
		if options.AccessMode != pgx.ReadOnly || options.IsoLevel != pgx.ReadCommitted {
			t.Fatalf("read transaction options=%#v", options)
		}
	}
}

func TestCurrentConfigurationsRepositoryListsBoundedTypesInAuthorizedTenant(t *testing.T) {
	queries := &currentConfigurationQueriesStub{typesRows: []string{"github", "gitlab"}}
	store := &currentConfigurationProjectStore{}
	repository := newCurrentConfigurationsRepositoryForTest(t, store, queries)

	rows, err := repository.ListDistinctTypes(context.Background(), configurationapp.CurrentConfigurationTypesFilter{
		ProjectID: 7,
		Section:   "credentials",
		MaxRows:   configurationapp.MaxCurrentConfigurationTypes + 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rows, []string{"github", "gitlab"}) {
		t.Fatalf("types=%v", rows)
	}
	wantParams := sqlcgen.ListCurrentConfigurationTypesParams{
		ProjectID: 7,
		Section:   "credentials",
		LimitRows: configurationapp.MaxCurrentConfigurationTypes + 1,
	}
	if queries.typesCalls != 1 || !reflect.DeepEqual(queries.typesParams, wantParams) {
		t.Fatalf("query calls=%d params=%#v", queries.typesCalls, queries.typesParams)
	}
	if !reflect.DeepEqual(store.projectIDs, []int64{7}) || len(store.options) != 1 ||
		store.options[0].IsoLevel != pgx.ReadCommitted || store.options[0].AccessMode != pgx.ReadOnly {
		t.Fatalf("project transaction ids=%v options=%#v", store.projectIDs, store.options)
	}
}

func TestCurrentConfigurationsRepositoryRejectsInvalidTypesFilterBeforeTenantRouting(t *testing.T) {
	queries := &currentConfigurationQueriesStub{}
	store := &currentConfigurationProjectStore{}
	repository := newCurrentConfigurationsRepositoryForTest(t, store, queries)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []configurationapp.CurrentConfigurationTypesFilter{
		{ProjectID: 7, MaxRows: 1},
		{ProjectID: 0, MaxRows: 1},
		{ProjectID: 7, MaxRows: 0},
		{ProjectID: 7, Section: strings.Repeat("x", configurationapp.MaxCurrentConfigurationTypeLength+1), MaxRows: 1},
		{ProjectID: 7, MaxRows: configurationapp.MaxCurrentConfigurationTypes + 2},
	}
	for index, filter := range tests {
		ctx := context.Background()
		if index == 0 {
			ctx = nil
		}
		if _, err := repository.ListDistinctTypes(ctx, filter); !errors.Is(err, configurationapp.ErrInvalidCurrentConfigurationTypesRequest) {
			t.Fatalf("filter %d error=%v", index, err)
		}
	}
	if _, err := repository.ListDistinctTypes(canceled, configurationapp.CurrentConfigurationTypesFilter{
		ProjectID: 7,
		MaxRows:   1,
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error=%v", err)
	}
	if len(store.projectIDs) != 0 || queries.typesCalls != 0 {
		t.Fatalf("invalid input reached tenant store: projects=%v calls=%d", store.projectIDs, queries.typesCalls)
	}
}

func TestCurrentConfigurationsRepositoryFailsClosedOnTypesQueryErrorsAndBounds(t *testing.T) {
	queryFailure := errors.New("database failure")
	queries := &currentConfigurationQueriesStub{typesErr: queryFailure}
	repository := newCurrentConfigurationsRepositoryForTest(t, &currentConfigurationProjectStore{}, queries)
	filter := configurationapp.CurrentConfigurationTypesFilter{ProjectID: 7, MaxRows: 1}
	if _, err := repository.ListDistinctTypes(context.Background(), filter); !errors.Is(err, queryFailure) {
		t.Fatalf("query error identity lost: %v", err)
	}

	queries = &currentConfigurationQueriesStub{typesRows: []string{"a", "b"}}
	repository = newCurrentConfigurationsRepositoryForTest(t, &currentConfigurationProjectStore{}, queries)
	if _, err := repository.ListDistinctTypes(context.Background(), filter); err == nil {
		t.Fatal("expected repository result-bound failure")
	}
}

func TestCurrentConfigurationsRepositoryMapsAllCurrentColumnsAndCopiesJSON(t *testing.T) {
	row := currentConfigurationRow(7, 11)
	row.IsPinned = true
	queries := &currentConfigurationQueriesStub{getRow: row}
	repository := newCurrentConfigurationsRepositoryForTest(t, &currentConfigurationProjectStore{}, queries)

	configuration, err := repository.Get(context.Background(), 7, 11)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.ID != row.ID || configuration.UUID != row.ConfigurationUuid || configuration.ProjectID != row.ProjectID ||
		configuration.Label == nil || *configuration.Label != *row.Label || configuration.EliteaTitle != row.EliteaTitle ||
		configuration.Type != row.Type || configuration.Section != row.Section || configuration.Shared != row.Shared ||
		configuration.StatusOK != row.StatusOk || configuration.StatusLogs == nil || *configuration.StatusLogs != *row.StatusLogs ||
		configuration.Source != row.Source || configuration.AuthorID == nil || *configuration.AuthorID != *row.AuthorID ||
		!configuration.IsPinned ||
		!configuration.CreatedAt.Equal(row.CreatedAt.Time) || configuration.UpdatedAt == nil || !configuration.UpdatedAt.Equal(row.UpdatedAt.Time) {
		t.Fatalf("mapped configuration=%#v", configuration)
	}
	if configuration.Options != nil {
		t.Fatalf("options enrichment must remain unset: %#v", configuration)
	}

	queries.getRow.Data[2] = 'X'
	nested := configuration.Data["nested"].(map[string]any)
	if nested["value"] != "unchanged" {
		t.Fatalf("mapped data aliases SQL bytes: %#v", configuration.Data)
	}
	storedBytes := string(queries.getRow.Data)
	configuration.Data["nested"].(map[string]any)["value"] = "caller mutation"
	if string(queries.getRow.Data) != storedBytes {
		t.Fatalf("mapped data mutated SQL row: %s", queries.getRow.Data)
	}
}

func TestCurrentConfigurationsRepositoryWritesExactMutableColumns(t *testing.T) {
	queries := &currentConfigurationQueriesStub{
		insertRow:  sqlcgen.InsertCurrentConfigurationRow(currentConfigurationRow(7, 21)),
		replaceRow: sqlcgen.ReplaceCurrentConfigurationRow(currentConfigurationRow(7, 21)),
		deleteID:   21,
	}
	store := &currentConfigurationProjectStore{}
	repository := newCurrentConfigurationsRepositoryForTest(t, store, queries)

	data := map[string]any{"token": "{{secret.github_token}}"}
	meta := map[string]any{"owner": "team"}
	label := "github-team"
	logs := "ok"
	authorID := int32(42)
	created, err := repository.Create(context.Background(), configurationapp.CurrentConfigurationCreate{
		UUID: "00000000-0000-0000-0000-000000000021", ProjectID: 7, Label: &label,
		EliteaTitle: "github-team", Type: "github", Section: "credentials", Data: data, Meta: meta,
		Shared: true, StatusOK: true, StatusLogs: &logs, Source: "user", AuthorID: &authorID,
	})
	if err != nil || created.ID != 21 {
		t.Fatalf("created=%#v err=%v", created, err)
	}
	if queries.insertParams.ProjectID != 7 || queries.insertParams.ConfigurationType != "github" || queries.insertParams.Section != "credentials" || !queries.insertParams.Shared || !queries.insertParams.StatusOk {
		t.Fatalf("insert params=%#v", queries.insertParams)
	}
	data["token"] = "changed"
	var storedData map[string]any
	if err := json.Unmarshal(queries.insertParams.Data, &storedData); err != nil || storedData["token"] != "{{secret.github_token}}" {
		t.Fatalf("stored data=%#v err=%v", storedData, err)
	}

	replaced, err := repository.Replace(context.Background(), configurationapp.CurrentConfigurationReplace{
		ProjectID: 7, ConfigurationID: 21, Label: &label, EliteaTitle: "github-team-2",
		Data: map[string]any{"token": "replacement"}, Meta: map[string]any{"owner": "new-team"},
		Shared: false, StatusOK: false, StatusLogs: &logs,
	})
	if err != nil || replaced.ID != 21 {
		t.Fatalf("replaced=%#v err=%v", replaced, err)
	}
	if queries.replaceParams.ProjectID != 7 || queries.replaceParams.ConfigurationID != 21 || queries.replaceParams.EliteaTitle != "github-team-2" {
		t.Fatalf("replace params=%#v", queries.replaceParams)
	}
	if err := repository.Delete(context.Background(), 7, 21); err != nil {
		t.Fatal(err)
	}
	if queries.deleteParams.ProjectID != 7 || queries.deleteParams.ConfigurationID != 21 {
		t.Fatalf("delete params=%#v", queries.deleteParams)
	}
	if !reflect.DeepEqual(store.projectIDs, []int64{7, 7, 7}) {
		t.Fatalf("write project routing=%v", store.projectIDs)
	}
	for _, options := range store.options {
		if options.AccessMode != pgx.ReadWrite || options.IsoLevel != pgx.ReadCommitted {
			t.Fatalf("write transaction options=%#v", options)
		}
	}
}

func TestCurrentConfigurationsRepositoryMapsMissingRowsAndBounds(t *testing.T) {
	tests := []struct {
		name string
		run  func(*CurrentConfigurationsRepository) error
	}{
		{name: "get", run: func(repository *CurrentConfigurationsRepository) error {
			_, err := repository.Get(context.Background(), 7, 99)
			return err
		}},
		{name: "replace", run: func(repository *CurrentConfigurationsRepository) error {
			_, err := repository.Replace(context.Background(), configurationapp.CurrentConfigurationReplace{ProjectID: 7, ConfigurationID: 99, Data: map[string]any{}, Meta: map[string]any{}})
			return err
		}},
		{name: "delete", run: func(repository *CurrentConfigurationsRepository) error {
			return repository.Delete(context.Background(), 7, 99)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			queries := &currentConfigurationQueriesStub{getErr: pgx.ErrNoRows, replaceErr: pgx.ErrNoRows, deleteErr: pgx.ErrNoRows}
			repository := newCurrentConfigurationsRepositoryForTest(t, &currentConfigurationProjectStore{}, queries)
			if err := test.run(repository); !errors.Is(err, configurationapp.ErrCurrentConfigurationNotFound) {
				t.Fatalf("error=%v", err)
			}
		})
	}

	conflict := &pgconn.PgError{Code: "23505", ConstraintName: "must-not-leak"}
	queries := &currentConfigurationQueriesStub{insertErr: conflict, replaceErr: conflict}
	repository := newCurrentConfigurationsRepositoryForTest(t, &currentConfigurationProjectStore{}, queries)
	_, err := repository.Create(context.Background(), configurationapp.CurrentConfigurationCreate{ProjectID: 7, Data: map[string]any{}, Meta: map[string]any{}})
	if !errors.Is(err, configurationapp.ErrCurrentConfigurationConflict) || err.Error() != configurationapp.ErrCurrentConfigurationConflict.Error() {
		t.Fatalf("create conflict error=%v", err)
	}
	_, err = repository.Replace(context.Background(), configurationapp.CurrentConfigurationReplace{ProjectID: 7, ConfigurationID: 99, Data: map[string]any{}, Meta: map[string]any{}})
	if !errors.Is(err, configurationapp.ErrCurrentConfigurationConflict) || err.Error() != configurationapp.ErrCurrentConfigurationConflict.Error() {
		t.Fatalf("replace conflict error=%v", err)
	}

	repository = newCurrentConfigurationsRepositoryForTest(t, &currentConfigurationProjectStore{}, &currentConfigurationQueriesStub{})
	_, err = repository.List(context.Background(), configurationapp.CurrentConfigurationListFilter{
		ProjectID: 7, Offset: int(int64(math.MaxInt32) + 1), Limit: 1,
	})
	if !errors.Is(err, configurationapp.ErrInvalidCurrentConfigurationRequest) {
		t.Fatalf("offset error=%v", err)
	}
	_, err = repository.List(context.Background(), configurationapp.CurrentConfigurationListFilter{ProjectID: 7, Limit: 0})
	if !errors.Is(err, configurationapp.ErrInvalidCurrentConfigurationRequest) {
		t.Fatalf("limit error=%v", err)
	}
}

func newCurrentConfigurationsRepositoryForTest(t *testing.T, projects projectStore, queries currentConfigurationQueries) *CurrentConfigurationsRepository {
	t.Helper()
	repository, err := newCurrentConfigurationsRepository(projects, func(sqlExecutor) (currentConfigurationQueries, error) {
		return queries, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func currentConfigurationRow(projectID, configurationID int32) sqlcgen.GetCurrentConfigurationRow {
	label := "team"
	statusLogs := "healthy"
	authorID := int32(42)
	createdAt := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	updatedAt := createdAt.Add(time.Minute)
	return sqlcgen.GetCurrentConfigurationRow{
		ID: configurationID, ConfigurationUuid: "00000000-0000-0000-0000-000000000011",
		ProjectID: projectID, Label: &label, EliteaTitle: "github-team", Type: "github", Section: "credentials",
		Data: []byte(`{"nested":{"value":"unchanged"}}`), Meta: []byte(`{"owner":"team"}`), Shared: true,
		StatusOk: true, StatusLogs: &statusLogs, Source: "user", AuthorID: &authorID,
		CreatedAt: pgtype.Timestamp{Time: createdAt, Valid: true}, UpdatedAt: pgtype.Timestamp{Time: updatedAt, Valid: true},
	}
}

type currentConfigurationProjectStore struct {
	projectIDs []int64
	options    []pgx.TxOptions
}

func (s *currentConfigurationProjectStore) WithinProjectTx(ctx context.Context, projectID int64, options pgx.TxOptions, fn func(sqlExecutor) error) error {
	s.projectIDs = append(s.projectIDs, projectID)
	s.options = append(s.options, options)
	return fn(nil)
}

type currentConfigurationQueriesStub struct {
	currentCount     int64
	sharedCount      int64
	typesRows        []string
	optionRowsByCall [][]sqlcgen.ListCurrentConfigurationOptionCandidatesRow
	currentRows      []sqlcgen.ListCurrentConfigurationsRow
	sharedRows       []sqlcgen.ListCurrentSharedConfigurationsRow
	findRow          sqlcgen.FindCurrentConfigurationByEliteaTitleRow
	embeddingRows    []sqlcgen.FindCurrentEmbeddingConfigurationsRow
	getRow           sqlcgen.GetCurrentConfigurationRow
	insertRow        sqlcgen.InsertCurrentConfigurationRow
	replaceRow       sqlcgen.ReplaceCurrentConfigurationRow
	deleteID         int32
	findErr          error
	embeddingErr     error
	getErr           error
	insertErr        error
	replaceErr       error
	deleteErr        error
	typesErr         error
	optionErr        error

	countCurrentParams sqlcgen.CountCurrentConfigurationsParams
	countSharedParams  sqlcgen.CountCurrentSharedConfigurationsParams
	listCurrentParams  sqlcgen.ListCurrentConfigurationsParams
	listSharedParams   sqlcgen.ListCurrentSharedConfigurationsParams
	findParams         sqlcgen.FindCurrentConfigurationByEliteaTitleParams
	embeddingParams    sqlcgen.FindCurrentEmbeddingConfigurationsParams
	insertParams       sqlcgen.InsertCurrentConfigurationParams
	replaceParams      sqlcgen.ReplaceCurrentConfigurationParams
	deleteParams       sqlcgen.DeleteCurrentConfigurationParams
	typesParams        sqlcgen.ListCurrentConfigurationTypesParams
	optionParams       []sqlcgen.ListCurrentConfigurationOptionCandidatesParams
	listCurrentCalls   int
	listSharedCalls    int
	typesCalls         int
}

func (s *currentConfigurationQueriesStub) FindCurrentConfigurationByEliteaTitle(
	_ context.Context,
	params sqlcgen.FindCurrentConfigurationByEliteaTitleParams,
) (sqlcgen.FindCurrentConfigurationByEliteaTitleRow, error) {
	s.findParams = params
	return s.findRow, s.findErr
}

func (s *currentConfigurationQueriesStub) FindCurrentEmbeddingConfigurations(
	_ context.Context,
	params sqlcgen.FindCurrentEmbeddingConfigurationsParams,
) ([]sqlcgen.FindCurrentEmbeddingConfigurationsRow, error) {
	s.embeddingParams = params
	return s.embeddingRows, s.embeddingErr
}

func (s *currentConfigurationQueriesStub) ListCurrentConfigurationTypes(
	_ context.Context,
	params sqlcgen.ListCurrentConfigurationTypesParams,
) ([]string, error) {
	s.typesCalls++
	s.typesParams = params
	return s.typesRows, s.typesErr
}

func (s *currentConfigurationQueriesStub) ListCurrentConfigurationOptionCandidates(
	_ context.Context,
	params sqlcgen.ListCurrentConfigurationOptionCandidatesParams,
) ([]sqlcgen.ListCurrentConfigurationOptionCandidatesRow, error) {
	call := len(s.optionParams)
	s.optionParams = append(s.optionParams, params)
	if s.optionErr != nil {
		return nil, s.optionErr
	}
	if call >= len(s.optionRowsByCall) {
		return nil, nil
	}
	return s.optionRowsByCall[call], nil
}

func (s *currentConfigurationQueriesStub) CountCurrentConfigurations(_ context.Context, params sqlcgen.CountCurrentConfigurationsParams) (int64, error) {
	s.countCurrentParams = params
	return s.currentCount, nil
}

func (s *currentConfigurationQueriesStub) CountCurrentSharedConfigurations(_ context.Context, params sqlcgen.CountCurrentSharedConfigurationsParams) (int64, error) {
	s.countSharedParams = params
	return s.sharedCount, nil
}

func (s *currentConfigurationQueriesStub) ListCurrentConfigurations(_ context.Context, params sqlcgen.ListCurrentConfigurationsParams) ([]sqlcgen.ListCurrentConfigurationsRow, error) {
	s.listCurrentCalls++
	s.listCurrentParams = params
	return s.currentRows, nil
}

func (s *currentConfigurationQueriesStub) ListCurrentSharedConfigurations(_ context.Context, params sqlcgen.ListCurrentSharedConfigurationsParams) ([]sqlcgen.ListCurrentSharedConfigurationsRow, error) {
	s.listSharedCalls++
	s.listSharedParams = params
	return s.sharedRows, nil
}

func (s *currentConfigurationQueriesStub) GetCurrentConfiguration(_ context.Context, _ sqlcgen.GetCurrentConfigurationParams) (sqlcgen.GetCurrentConfigurationRow, error) {
	return s.getRow, s.getErr
}

func (s *currentConfigurationQueriesStub) InsertCurrentConfiguration(_ context.Context, params sqlcgen.InsertCurrentConfigurationParams) (sqlcgen.InsertCurrentConfigurationRow, error) {
	s.insertParams = params
	return s.insertRow, s.insertErr
}

func (s *currentConfigurationQueriesStub) ReplaceCurrentConfiguration(_ context.Context, params sqlcgen.ReplaceCurrentConfigurationParams) (sqlcgen.ReplaceCurrentConfigurationRow, error) {
	s.replaceParams = params
	return s.replaceRow, s.replaceErr
}

func (s *currentConfigurationQueriesStub) DeleteCurrentConfiguration(_ context.Context, params sqlcgen.DeleteCurrentConfigurationParams) (int32, error) {
	s.deleteParams = params
	return s.deleteID, s.deleteErr
}
