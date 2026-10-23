package repos

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
)

func TestCurrentToolkitTypesRepositoryUsesAuthorizedReadOnlyTenantAndIndependentFilters(t *testing.T) {
	tests := []struct {
		name              string
		filterMCP         bool
		filterApplication bool
	}{
		{name: "exclude both"},
		{name: "MCP only", filterMCP: true},
		{name: "application only", filterApplication: true},
		{name: "include both", filterMCP: true, filterApplication: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			queries := &currentToolkitTypesQueriesStub{rows: []string{"ado_repos", "github"}}
			projects := &currentToolkitTypesProjectStore{}
			repository := newCurrentToolkitTypesRepositoryForTest(t, projects, queries)

			rows, err := repository.ListCurrentToolkitTypes(
				context.Background(),
				7,
				test.filterMCP,
				test.filterApplication,
			)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(rows, []string{"ado_repos", "github"}) {
				t.Fatalf("rows=%v", rows)
			}
			wantParams := sqlcgen.ListCurrentToolkitTypesParams{
				FilterMcp:         test.filterMCP,
				FilterApplication: test.filterApplication,
			}
			if queries.calls != 1 || queries.params != wantParams {
				t.Fatalf("query calls=%d params=%+v want=%+v", queries.calls, queries.params, wantParams)
			}
			if !reflect.DeepEqual(projects.projectIDs, []int64{7}) ||
				len(projects.options) != 1 ||
				projects.options[0].IsoLevel != pgx.ReadCommitted ||
				projects.options[0].AccessMode != pgx.ReadOnly {
				t.Fatalf("project transaction ids=%v options=%#v", projects.projectIDs, projects.options)
			}
		})
	}
}

func TestCurrentToolkitTypesRepositoryRejectsInvalidRequestsBeforeTenantRouting(t *testing.T) {
	queries := &currentToolkitTypesQueriesStub{}
	projects := &currentToolkitTypesProjectStore{}
	repository := newCurrentToolkitTypesRepositoryForTest(t, projects, queries)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name      string
		ctx       context.Context
		projectID int32
		want      error
	}{
		{name: "nil context", projectID: 7, want: ErrInvalidCurrentToolkitTypesRequest},
		{name: "zero project", ctx: context.Background(), want: ErrInvalidCurrentToolkitTypesRequest},
		{name: "negative project", ctx: context.Background(), projectID: -1, want: ErrInvalidCurrentToolkitTypesRequest},
		{name: "canceled context", ctx: canceled, projectID: 7, want: context.Canceled},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := repository.ListCurrentToolkitTypes(test.ctx, test.projectID, false, false)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v, want %v", err, test.want)
			}
		})
	}
	if len(projects.projectIDs) != 0 || queries.calls != 0 {
		t.Fatalf("invalid request reached tenant store: projects=%v calls=%d", projects.projectIDs, queries.calls)
	}
}

func TestCurrentToolkitTypesRepositoryPreservesQueryFailureIdentityWithoutSensitiveRows(t *testing.T) {
	databaseFailure := errors.New("database unavailable")
	queries := &currentToolkitTypesQueriesStub{err: databaseFailure}
	repository := newCurrentToolkitTypesRepositoryForTest(
		t,
		&currentToolkitTypesProjectStore{},
		queries,
	)

	rows, err := repository.ListCurrentToolkitTypes(context.Background(), 7, true, false)
	if rows != nil || !errors.Is(err, databaseFailure) ||
		!strings.Contains(err.Error(), "list current toolkit types") {
		t.Fatalf("rows=%v error=%v", rows, err)
	}
}

func TestNewCurrentToolkitTypesRepositoryRejectsMissingDependencies(t *testing.T) {
	if _, err := NewCurrentToolkitTypesRepository(nil); err == nil {
		t.Fatal("nil database pool was accepted")
	}
	if _, err := newCurrentToolkitTypesRepository(
		nil,
		func(sqlExecutor) (currentToolkitTypesQueries, error) {
			return &currentToolkitTypesQueriesStub{}, nil
		},
	); err == nil {
		t.Fatal("nil project store was accepted")
	}
	if _, err := newCurrentToolkitTypesRepository(
		&currentToolkitTypesProjectStore{},
		nil,
	); err == nil {
		t.Fatal("nil query factory was accepted")
	}
}

func newCurrentToolkitTypesRepositoryForTest(
	t *testing.T,
	projects projectStore,
	queries currentToolkitTypesQueries,
) *CurrentToolkitTypesRepository {
	t.Helper()
	repository, err := newCurrentToolkitTypesRepository(
		projects,
		func(sqlExecutor) (currentToolkitTypesQueries, error) {
			return queries, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

type currentToolkitTypesProjectStore struct {
	projectIDs []int64
	options    []pgx.TxOptions
}

func (store *currentToolkitTypesProjectStore) WithinProjectTx(
	_ context.Context,
	projectID int64,
	options pgx.TxOptions,
	function func(sqlExecutor) error,
) error {
	store.projectIDs = append(store.projectIDs, projectID)
	store.options = append(store.options, options)
	return function(nil)
}

type currentToolkitTypesQueriesStub struct {
	rows   []string
	err    error
	params sqlcgen.ListCurrentToolkitTypesParams
	calls  int
}

func (stub *currentToolkitTypesQueriesStub) ListCurrentToolkitTypes(
	_ context.Context,
	params sqlcgen.ListCurrentToolkitTypesParams,
) ([]string, error) {
	stub.calls++
	stub.params = params
	return stub.rows, stub.err
}
