package repos

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
)

func TestCurrentConfigurationsRepositoryFindByEliteaTitleIsProviderNeutral(t *testing.T) {
	tests := []struct {
		name              string
		configurationType string
		data              string
	}{
		{name: "github", configurationType: "github", data: `{"token":"{{secret.github}}"}`},
		{name: "openapi", configurationType: "openapi", data: `{"spec":"https://api.invalid/openapi.json"}`},
		{name: "custom", configurationType: "company_custom", data: `{"large":9007199254740993,"nested":{"enabled":true},"items":[1,"x"]}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			projects := &currentConfigurationProjectStore{}
			queries := &currentConfigurationQueriesStub{findRow: sqlcgen.FindCurrentConfigurationByEliteaTitleRow{
				ConfigurationUuid: "11111111-1111-1111-1111-111111111111",
				ProjectID:         73,
				Type:              test.configurationType,
				Data:              []byte(test.data),
				Shared:            true,
			}}
			repository := newCurrentConfigurationsRepositoryForTest(t, projects, queries)

			configuration, found, err := repository.FindByEliteaTitle(context.Background(), 73, "exact-title", true)
			if err != nil {
				t.Fatal(err)
			}
			if !found || configuration.ProjectID != 73 || configuration.Type != test.configurationType ||
				configuration.UUID != "11111111-1111-1111-1111-111111111111" {
				t.Fatalf("configuration=%#v found=%t", configuration, found)
			}
			if queries.findParams != (sqlcgen.FindCurrentConfigurationByEliteaTitleParams{
				ProjectID: 73, EliteaTitle: "exact-title", SharedOnly: true,
			}) {
				t.Fatalf("query params=%#v", queries.findParams)
			}
			if !reflect.DeepEqual(projects.projectIDs, []int64{73}) || len(projects.options) != 1 ||
				projects.options[0].AccessMode != pgx.ReadOnly {
				t.Fatalf("project transaction IDs=%v options=%#v", projects.projectIDs, projects.options)
			}

			if test.name == "custom" {
				if configuration.Data["large"] != json.Number("9007199254740993") {
					t.Fatalf("large provider number lost precision: %#v", configuration.Data["large"])
				}
				items := configuration.Data["items"].([]any)
				if items[0] != json.Number("1") || items[1] != "x" {
					t.Fatalf("provider array changed: %#v", items)
				}
			}
		})
	}
}

func TestCurrentConfigurationsRepositoryFindByEliteaTitleEnforcesScopeAndAbsence(t *testing.T) {
	tests := []struct {
		name       string
		projectID  int32
		title      string
		sharedOnly bool
		row        sqlcgen.FindCurrentConfigurationByEliteaTitleRow
		queryErr   error
		wantFound  bool
		wantErr    bool
	}{
		{
			name: "not found", projectID: 7, title: "missing", queryErr: pgx.ErrNoRows,
		},
		{
			name: "wrong project row", projectID: 7, title: "credential", wantErr: true,
			row: sqlcgen.FindCurrentConfigurationByEliteaTitleRow{
				ConfigurationUuid: "11111111-1111-1111-1111-111111111111", ProjectID: 8,
				Type: "github", Data: []byte(`{}`), Shared: true,
			},
		},
		{
			name: "non-shared row from shared query", projectID: 7, title: "credential", sharedOnly: true, wantErr: true,
			row: sqlcgen.FindCurrentConfigurationByEliteaTitleRow{
				ConfigurationUuid: "11111111-1111-1111-1111-111111111111", ProjectID: 7,
				Type: "github", Data: []byte(`{}`), Shared: false,
			},
		},
		{
			name: "malformed provider data", projectID: 7, title: "credential", wantErr: true,
			row: sqlcgen.FindCurrentConfigurationByEliteaTitleRow{
				ConfigurationUuid: "11111111-1111-1111-1111-111111111111", ProjectID: 7,
				Type: "github", Data: []byte(`{"token":"raw-sensitive-token"`), Shared: true,
			},
		},
		{
			name: "valid exact row", projectID: 7, title: "credential", wantFound: true,
			row: sqlcgen.FindCurrentConfigurationByEliteaTitleRow{
				ConfigurationUuid: "11111111-1111-1111-1111-111111111111", ProjectID: 7,
				Type: "github", Data: []byte(`{}`), Shared: false,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			queries := &currentConfigurationQueriesStub{findRow: test.row, findErr: test.queryErr}
			repository := newCurrentConfigurationsRepositoryForTest(t, &currentConfigurationProjectStore{}, queries)
			configuration, found, err := repository.FindByEliteaTitle(
				context.Background(), test.projectID, test.title, test.sharedOnly,
			)
			if (err != nil) != test.wantErr || found != test.wantFound {
				t.Fatalf("configuration=%#v found=%t err=%v", configuration, found, err)
			}
			if !found && !reflect.DeepEqual(configuration, configurationapp.CurrentExpansionConfiguration{}) {
				t.Fatalf("absent/error result leaked a partial configuration: %#v", configuration)
			}
			if err != nil && strings.Contains(err.Error(), "raw-sensitive-token") {
				t.Fatalf("error leaked provider data: %v", err)
			}
		})
	}
}

func TestCurrentConfigurationsRepositoryFindByEliteaTitleValidatesBeforeDatabase(t *testing.T) {
	queries := &currentConfigurationQueriesStub{}
	repository := newCurrentConfigurationsRepositoryForTest(t, &currentConfigurationProjectStore{}, queries)

	for _, test := range []struct {
		name      string
		ctx       context.Context
		projectID int32
		title     string
		want      error
	}{
		{name: "nil context", projectID: 7, title: "title", want: configurationapp.ErrInvalidCurrentExpansion},
		{name: "invalid project", ctx: context.Background(), projectID: 0, title: "title", want: configurationapp.ErrInvalidCurrentExpansion},
		{name: "empty title", ctx: context.Background(), projectID: 7, want: configurationapp.ErrInvalidCurrentExpansion},
		{name: "long title", ctx: context.Background(), projectID: 7, title: strings.Repeat("x", configurationapp.MaxCurrentExpansionIdentifierLength+1), want: configurationapp.ErrInvalidCurrentExpansion},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := repository.FindByEliteaTitle(test.ctx, test.projectID, test.title, false)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v want=%v", err, test.want)
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := repository.FindByEliteaTitle(ctx, 7, "title", false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error=%v", err)
	}
}

func TestCurrentExpansionScopeUsesInjectedPublicAndCurrentPersonalResolution(t *testing.T) {
	queries := &currentExpansionScopeQueriesStub{projectID: 73}
	repository, err := newCurrentExpansionScopeRepository(queries, 41)
	if err != nil {
		t.Fatal(err)
	}

	publicProjectID, err := repository.PublicProjectID(context.Background())
	if err != nil || publicProjectID != 41 {
		t.Fatalf("public project=%d err=%v", publicProjectID, err)
	}
	personalProjectID, err := repository.PersonalProjectID(context.Background(), 100)
	if err != nil || personalProjectID != 73 {
		t.Fatalf("personal project=%d err=%v", personalProjectID, err)
	}
	if !reflect.DeepEqual(queries.userIDs, []int32{100}) {
		t.Fatalf("personal lookup user IDs=%v", queries.userIDs)
	}
}

func TestCurrentExpansionScopeAbsenceFailureAndCancellation(t *testing.T) {
	tests := []struct {
		name      string
		ctx       context.Context
		userID    int32
		projectID int32
		queryErr  error
		want      error
	}{
		{name: "missing", ctx: context.Background(), userID: 100, queryErr: pgx.ErrNoRows, want: ErrCurrentPersonalProjectNotFound},
		{name: "invalid user", ctx: context.Background(), userID: 0, want: configurationapp.ErrInvalidCurrentExpansion},
		{name: "invalid row", ctx: context.Background(), userID: 100, projectID: 0, want: errors.New("invalid row")},
		{name: "query cancellation", ctx: context.Background(), userID: 100, queryErr: context.DeadlineExceeded, want: context.DeadlineExceeded},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			queries := &currentExpansionScopeQueriesStub{projectID: test.projectID, err: test.queryErr}
			repository, err := newCurrentExpansionScopeRepository(queries, 41)
			if err != nil {
				t.Fatal(err)
			}
			projectID, err := repository.PersonalProjectID(test.ctx, test.userID)
			if projectID != 0 {
				t.Fatalf("failure returned project ID %d", projectID)
			}
			if test.name == "invalid row" {
				if err == nil || strings.Contains(err.Error(), "100") {
					t.Fatalf("invalid-row error=%v", err)
				}
				return
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v want=%v", err, test.want)
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repository, err := newCurrentExpansionScopeRepository(&currentExpansionScopeQueriesStub{}, 41)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.PublicProjectID(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("public cancellation error=%v", err)
	}
	if _, err := repository.PersonalProjectID(ctx, 100); !errors.Is(err, context.Canceled) {
		t.Fatalf("personal cancellation error=%v", err)
	}
}

type currentExpansionScopeQueriesStub struct {
	projectID int32
	err       error
	userIDs   []int32
}

func (s *currentExpansionScopeQueriesStub) ResolveCurrentPersonalProjectID(_ context.Context, userID int32) (int32, error) {
	s.userIDs = append(s.userIDs, userID)
	return s.projectID, s.err
}
