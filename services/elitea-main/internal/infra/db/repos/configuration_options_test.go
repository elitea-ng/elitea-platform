package repos

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
)

func TestCurrentConfigurationsRepositoryListsOptionCandidatesAcrossAuthorizedScopes(t *testing.T) {
	projectLabel := "Project credential"
	publicLabel := "Public model"
	queries := &currentConfigurationQueriesStub{
		optionRowsByCall: [][]sqlcgen.ListCurrentConfigurationOptionCandidatesRow{
			{
				{
					EliteaTitle: "project_github",
					Label:       &projectLabel,
					Type:        "github",
					Section:     "credentials",
					Shared:      false,
					ProjectID:   7,
				},
				{
					EliteaTitle: "project_openai",
					Type:        "openai",
					Section:     "llm",
					Shared:      true,
					ProjectID:   7,
				},
			},
			{
				{
					EliteaTitle: "public_openai",
					Label:       &publicLabel,
					Type:        "openai",
					Section:     "llm",
					Shared:      true,
					ProjectID:   1,
				},
			},
		},
	}
	store := &currentConfigurationProjectStore{}
	repository := newCurrentConfigurationsRepositoryForTest(t, store, queries)

	items, err := repository.ListCurrentConfigurationOptionCandidates(
		context.Background(),
		configurationapp.CurrentConfigurationOptionCandidatesQuery{
			ProjectID:       7,
			PublicProjectID: 1,
			IncludeShared:   true,
			Types:           []string{"github"},
			Sections:        []string{"llm"},
			MaxRows:         4,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []configurationapp.CurrentConfigurationOption{
		{
			EliteaTitle: "project_github",
			Label:       &projectLabel,
			Type:        "github",
			Section:     "credentials",
			Shared:      false,
			ProjectID:   7,
		},
		{
			EliteaTitle: "project_openai",
			Type:        "openai",
			Section:     "llm",
			Shared:      true,
			ProjectID:   7,
		},
		{
			EliteaTitle: "public_openai",
			Label:       &publicLabel,
			Type:        "openai",
			Section:     "llm",
			Shared:      true,
			ProjectID:   1,
		},
	}
	if !reflect.DeepEqual(items, want) {
		t.Fatalf("items=%#v want=%#v", items, want)
	}
	if !reflect.DeepEqual(store.projectIDs, []int64{7, 1}) {
		t.Fatalf("tenant routing=%v", store.projectIDs)
	}
	for _, options := range store.options {
		if options.IsoLevel != pgx.ReadCommitted || options.AccessMode != pgx.ReadOnly {
			t.Fatalf("transaction options=%#v", options)
		}
	}
	wantParams := []sqlcgen.ListCurrentConfigurationOptionCandidatesParams{
		{
			ProjectID:  7,
			Types:      []string{"github"},
			Sections:   []string{"llm"},
			SharedOnly: false,
			LimitRows:  4,
		},
		{
			ProjectID:  1,
			Types:      []string{"github"},
			Sections:   []string{"llm"},
			SharedOnly: true,
			LimitRows:  2,
		},
	}
	if !reflect.DeepEqual(queries.optionParams, wantParams) {
		t.Fatalf("query params=%#v want=%#v", queries.optionParams, wantParams)
	}
}

func TestCurrentConfigurationsRepositoryDoesNotDuplicatePublicOptionScope(t *testing.T) {
	queries := &currentConfigurationQueriesStub{
		optionRowsByCall: [][]sqlcgen.ListCurrentConfigurationOptionCandidatesRow{{
			{
				EliteaTitle: "public_unshared",
				Type:        "github",
				Section:     "credentials",
				Shared:      false,
				ProjectID:   1,
			},
		}},
	}
	store := &currentConfigurationProjectStore{}
	repository := newCurrentConfigurationsRepositoryForTest(t, store, queries)

	items, err := repository.ListCurrentConfigurationOptionCandidates(
		context.Background(),
		configurationapp.CurrentConfigurationOptionCandidatesQuery{
			ProjectID:       1,
			PublicProjectID: 1,
			IncludeShared:   true,
			Types:           []string{"github"},
			MaxRows:         10,
		},
	)
	if err != nil || len(items) != 1 || items[0].Shared {
		t.Fatalf("items=%#v err=%v", items, err)
	}
	if !reflect.DeepEqual(store.projectIDs, []int64{1}) ||
		len(queries.optionParams) != 1 ||
		queries.optionParams[0].SharedOnly {
		t.Fatalf("routing=%v params=%#v", store.projectIDs, queries.optionParams)
	}
}

func TestCurrentConfigurationsRepositoryStopsOptionPrefetchAtGlobalSentinel(t *testing.T) {
	queries := &currentConfigurationQueriesStub{
		optionRowsByCall: [][]sqlcgen.ListCurrentConfigurationOptionCandidatesRow{{
			{
				EliteaTitle: "first",
				Type:        "github",
				Section:     "credentials",
				ProjectID:   7,
			},
			{
				EliteaTitle: "second",
				Type:        "github",
				Section:     "credentials",
				ProjectID:   7,
			},
		}},
	}
	store := &currentConfigurationProjectStore{}
	repository := newCurrentConfigurationsRepositoryForTest(t, store, queries)

	items, err := repository.ListCurrentConfigurationOptionCandidates(
		context.Background(),
		configurationapp.CurrentConfigurationOptionCandidatesQuery{
			ProjectID:       7,
			PublicProjectID: 1,
			IncludeShared:   true,
			Types:           []string{"github"},
			MaxRows:         2,
		},
	)
	if err != nil || len(items) != 2 {
		t.Fatalf("items=%#v err=%v", items, err)
	}
	if !reflect.DeepEqual(store.projectIDs, []int64{7}) || len(queries.optionParams) != 1 {
		t.Fatalf("sentinel issued extra query: routing=%v params=%#v", store.projectIDs, queries.optionParams)
	}
}

func TestCurrentConfigurationsRepositoryRejectsInvalidOptionCandidateQueries(t *testing.T) {
	tooManySelectors := make([]string, configurationapp.MaxCurrentConfigurationOptionCandidates+1)
	for index := range tooManySelectors {
		tooManySelectors[index] = "github"
	}
	valid := configurationapp.CurrentConfigurationOptionCandidatesQuery{
		ProjectID:       7,
		PublicProjectID: 1,
		Types:           []string{"github"},
		MaxRows:         10,
	}
	tests := []struct {
		name  string
		ctx   context.Context
		query configurationapp.CurrentConfigurationOptionCandidatesQuery
	}{
		{name: "nil context", query: valid},
		{name: "project", ctx: context.Background(), query: withCurrentOptionProjectID(valid, 0)},
		{name: "public project", ctx: context.Background(), query: withCurrentOptionPublicProjectID(valid, 0)},
		{name: "zero limit", ctx: context.Background(), query: withCurrentOptionMaxRows(valid, 0)},
		{
			name: "limit above sentinel",
			ctx:  context.Background(),
			query: withCurrentOptionMaxRows(
				valid,
				configurationapp.MaxCurrentConfigurationOptionCandidates+2,
			),
		},
		{name: "no selectors", ctx: context.Background(), query: withCurrentOptionTypes(valid, nil)},
		{name: "empty selector", ctx: context.Background(), query: withCurrentOptionTypes(valid, []string{""})},
		{
			name:  "long selector",
			ctx:   context.Background(),
			query: withCurrentOptionTypes(valid, []string{strings.Repeat("x", configurationapp.MaxCurrentConfigurationTypeLength+1)}),
		},
		{name: "too many selectors", ctx: context.Background(), query: withCurrentOptionTypes(valid, tooManySelectors)},
	}

	store := &currentConfigurationProjectStore{}
	repository := newCurrentConfigurationsRepositoryForTest(t, store, &currentConfigurationQueriesStub{})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := repository.ListCurrentConfigurationOptionCandidates(test.ctx, test.query); !errors.Is(
				err,
				configurationapp.ErrInvalidCurrentConfigurationOptionsRequest,
			) {
				t.Fatalf("error=%v", err)
			}
		})
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repository.ListCurrentConfigurationOptionCandidates(canceled, valid); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error=%v", err)
	}
	if len(store.projectIDs) != 0 {
		t.Fatalf("invalid input reached tenant routing: %v", store.projectIDs)
	}
}

func TestCurrentConfigurationsRepositoryFailsClosedOnOptionScopeViolations(t *testing.T) {
	query := configurationapp.CurrentConfigurationOptionCandidatesQuery{
		ProjectID:       7,
		PublicProjectID: 1,
		IncludeShared:   true,
		Types:           []string{"github"},
		MaxRows:         10,
	}
	queryFailure := errors.New("query failure")

	tests := []struct {
		name    string
		queries *currentConfigurationQueriesStub
		want    error
	}{
		{
			name:    "query failure",
			queries: &currentConfigurationQueriesStub{optionErr: queryFailure},
			want:    queryFailure,
		},
		{
			name: "project row from another tenant",
			queries: &currentConfigurationQueriesStub{
				optionRowsByCall: [][]sqlcgen.ListCurrentConfigurationOptionCandidatesRow{{
					{EliteaTitle: "wrong", Type: "github", Section: "credentials", ProjectID: 8},
				}},
			},
		},
		{
			name: "public row is not shared",
			queries: &currentConfigurationQueriesStub{
				optionRowsByCall: [][]sqlcgen.ListCurrentConfigurationOptionCandidatesRow{
					nil,
					{{EliteaTitle: "private", Type: "github", Section: "credentials", ProjectID: 1}},
				},
			},
		},
		{
			name: "query exceeds supplied bound",
			queries: &currentConfigurationQueriesStub{
				optionRowsByCall: [][]sqlcgen.ListCurrentConfigurationOptionCandidatesRow{{
					{EliteaTitle: "first", Type: "github", Section: "credentials", ProjectID: 7},
					{EliteaTitle: "second", Type: "github", Section: "credentials", ProjectID: 7},
				}},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testQuery := query
			if test.name == "query exceeds supplied bound" {
				testQuery.MaxRows = 1
			}
			repository := newCurrentConfigurationsRepositoryForTest(
				t,
				&currentConfigurationProjectStore{},
				test.queries,
			)
			_, err := repository.ListCurrentConfigurationOptionCandidates(context.Background(), testQuery)
			if err == nil {
				t.Fatal("expected failure")
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("error=%v want identity=%v", err, test.want)
			}
		})
	}
}

func withCurrentOptionProjectID(
	query configurationapp.CurrentConfigurationOptionCandidatesQuery,
	projectID int32,
) configurationapp.CurrentConfigurationOptionCandidatesQuery {
	query.ProjectID = projectID
	return query
}

func withCurrentOptionPublicProjectID(
	query configurationapp.CurrentConfigurationOptionCandidatesQuery,
	projectID int32,
) configurationapp.CurrentConfigurationOptionCandidatesQuery {
	query.PublicProjectID = projectID
	return query
}

func withCurrentOptionMaxRows(
	query configurationapp.CurrentConfigurationOptionCandidatesQuery,
	maxRows int,
) configurationapp.CurrentConfigurationOptionCandidatesQuery {
	query.MaxRows = maxRows
	return query
}

func withCurrentOptionTypes(
	query configurationapp.CurrentConfigurationOptionCandidatesQuery,
	types []string,
) configurationapp.CurrentConfigurationOptionCandidatesQuery {
	query.Types = types
	return query
}
