package configurations

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestCurrentCRUDListPreservesCurrentAndPublicSharedSemantics(t *testing.T) {
	repository := &currentConfigurationRepositoryStub{
		counts: []int64{2, 1},
		lists: [][]CurrentConfiguration{
			{{ID: 1, ProjectID: 7, Shared: true}},
			{{ID: 2, ProjectID: 1, Shared: true}},
		},
	}
	service, err := NewCurrentCRUDService(repository)
	if err != nil {
		t.Fatal(err)
	}

	result, err := service.List(context.Background(), CurrentConfigurationListRequest{
		IDs:             []int32{1, 2, 1},
		ProjectID:       7,
		PublicProjectID: 1,
		Types:           []string{"github", "pgvector"},
		Sections:        []string{"credentials", "vectorstorage"},
		Offset:          4,
		Limit:           25,
		IncludeShared:   true,
		SharedOffset:    3,
		SharedLimit:     10,
		Query:           "team",
		SortBy:          "elitea_title",
		SortOrder:       "ASC",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Offset != 0 || result.Total != 2 || result.Shared == nil || result.Shared.Offset != 0 || result.Shared.Total != 1 {
		t.Fatalf("unexpected page result: %#v", result)
	}
	if len(repository.countFilters) != 2 || len(repository.listFilters) != 2 {
		t.Fatalf("repository calls: count=%d list=%d", len(repository.countFilters), len(repository.listFilters))
	}

	for _, filter := range append(repository.countFilters, repository.listFilters...) {
		if !reflect.DeepEqual(filter.IDs, []int32{1, 2}) {
			t.Fatalf("IDs not forwarded: %v", filter.IDs)
		}
	}
	current := repository.listFilters[0]
	if current.ProjectID != 7 || current.SharedOnly || current.LabelQuery != "team" || current.Offset != 0 {
		t.Fatalf("current filter = %#v", current)
	}
	shared := repository.listFilters[1]
	if shared.ProjectID != 1 || !shared.SharedOnly || shared.LabelQuery != "" || shared.Offset != 0 {
		t.Fatalf("shared filter = %#v", shared)
	}
	if current.SortOrder != "asc" || shared.SortOrder != "asc" {
		t.Fatalf("sort order was not normalized: current=%q shared=%q", current.SortOrder, shared.SortOrder)
	}
	if !reflect.DeepEqual(current.Types, []string{"github", "pgvector"}) || !reflect.DeepEqual(shared.Sections, []string{"credentials", "vectorstorage"}) {
		t.Fatalf("repeated filters changed: current=%#v shared=%#v", current, shared)
	}
}

func TestCurrentCRUDListRequestsSharedOnlyWhenCurrentAllowsIt(t *testing.T) {
	tests := []struct {
		name            string
		request         CurrentConfigurationListRequest
		wantCountCalls  int
		wantShared      bool
		wantEmptyOffset int
	}{
		{
			name: "not requested",
			request: CurrentConfigurationListRequest{
				ProjectID: 7, PublicProjectID: 1, Offset: 99,
			},
			wantCountCalls: 1, wantEmptyOffset: 99,
		},
		{
			name: "already public project",
			request: CurrentConfigurationListRequest{
				ProjectID: 1, PublicProjectID: 1, IncludeShared: true,
			},
			wantCountCalls: 1,
		},
		{
			name: "separate public project",
			request: CurrentConfigurationListRequest{
				ProjectID: 7, PublicProjectID: 1, IncludeShared: true,
			},
			wantCountCalls: 2, wantShared: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := &currentConfigurationRepositoryStub{counts: make([]int64, test.wantCountCalls)}
			service, err := NewCurrentCRUDService(repository)
			if err != nil {
				t.Fatal(err)
			}
			result, err := service.List(context.Background(), test.request)
			if err != nil {
				t.Fatal(err)
			}
			if len(repository.countFilters) != test.wantCountCalls || (result.Shared != nil) != test.wantShared {
				t.Fatalf("calls=%d shared=%v", len(repository.countFilters), result.Shared != nil)
			}
			if result.Offset != test.wantEmptyOffset {
				t.Fatalf("empty offset=%d, want %d", result.Offset, test.wantEmptyOffset)
			}
			if result.Items == nil {
				t.Fatal("empty items must encode as [] rather than null")
			}
		})
	}
}

func TestNormalizeCurrentConfigurationListRequestBoundsAndFallbacks(t *testing.T) {
	request, err := normalizeCurrentConfigurationListRequest(CurrentConfigurationListRequest{
		ProjectID:       7,
		PublicProjectID: 1,
		Offset:          -1,
		Limit:           MaxCurrentConfigurationListLimit + 1,
		SharedOffset:    -2,
		SharedLimit:     0,
		SortBy:          "not_a_column",
		SortOrder:       "anything_other_than_asc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.Offset != 0 || request.SharedOffset != 0 || request.Limit != MaxCurrentConfigurationListLimit || request.SharedLimit != DefaultCurrentConfigurationListLimit {
		t.Fatalf("pagination was not bounded: %#v", request)
	}
	if request.SortBy != "created_at" || request.SortOrder != "desc" {
		t.Fatalf("sort fallback differs from current behavior: %#v", request)
	}

	tooMany := make([]string, MaxCurrentConfigurationFilterValues+1)
	_, err = normalizeCurrentConfigurationListRequest(CurrentConfigurationListRequest{ProjectID: 7, Types: tooMany})
	if !errors.Is(err, ErrInvalidCurrentConfigurationRequest) {
		t.Fatalf("too many filters error = %v", err)
	}
}

func TestCurrentCRUDDelegatesRowOperationsWithoutOwningDeferredBehavior(t *testing.T) {
	repository := &currentConfigurationRepositoryStub{}
	service, err := NewCurrentCRUDService(repository)
	if err != nil {
		t.Fatal(err)
	}

	data := map[string]any{
		"token": "{{secret.github_token}}",
		"nested": map[string]any{
			"models": []any{map[string]any{"name": "first"}},
		},
	}
	created, err := service.Create(context.Background(), CurrentConfigurationCreate{
		ProjectID: 7, EliteaTitle: "github", Data: data, Meta: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != 11 || repository.createInput.EliteaTitle != "github" {
		t.Fatalf("create result=%#v input=%#v", created, repository.createInput)
	}
	data["token"] = "changed after call"
	if repository.createInput.Data["token"] != "{{secret.github_token}}" {
		t.Fatal("create input map aliases caller state")
	}
	data["nested"].(map[string]any)["models"].([]any)[0].(map[string]any)["name"] = "changed after call"
	storedNested := repository.createInput.Data["nested"].(map[string]any)
	storedModels := storedNested["models"].([]any)
	if storedModels[0].(map[string]any)["name"] != "first" {
		t.Fatal("create input nested JSON aliases caller state")
	}

	got, err := service.Get(context.Background(), 7, 11)
	if err != nil || got.ID != 11 {
		t.Fatalf("get result=%#v err=%v", got, err)
	}
	updated, err := service.Replace(context.Background(), CurrentConfigurationReplace{
		ProjectID: 7, ConfigurationID: 11, EliteaTitle: "github", Data: map[string]any{}, Meta: map[string]any{},
	})
	if err != nil || updated.ID != 11 {
		t.Fatalf("replace result=%#v err=%v", updated, err)
	}
	if err := service.Delete(context.Background(), 7, 11); err != nil {
		t.Fatal(err)
	}
	if repository.getProjectID != 7 || repository.getConfigurationID != 11 || repository.deleteProjectID != 7 || repository.deleteConfigurationID != 11 {
		t.Fatalf("row identity was not preserved: %#v", repository)
	}
}

type currentConfigurationRepositoryStub struct {
	counts []int64
	lists  [][]CurrentConfiguration

	countFilters []CurrentConfigurationListFilter
	listFilters  []CurrentConfigurationListFilter

	getProjectID          int32
	getConfigurationID    int32
	createInput           CurrentConfigurationCreate
	replaceInput          CurrentConfigurationReplace
	deleteProjectID       int32
	deleteConfigurationID int32
}

func (s *currentConfigurationRepositoryStub) Count(_ context.Context, filter CurrentConfigurationListFilter) (int64, error) {
	s.countFilters = append(s.countFilters, filter)
	index := len(s.countFilters) - 1
	if index < len(s.counts) {
		return s.counts[index], nil
	}
	return 0, nil
}

func (s *currentConfigurationRepositoryStub) List(_ context.Context, filter CurrentConfigurationListFilter) ([]CurrentConfiguration, error) {
	s.listFilters = append(s.listFilters, filter)
	index := len(s.listFilters) - 1
	if index < len(s.lists) {
		return s.lists[index], nil
	}
	return nil, nil
}

func (s *currentConfigurationRepositoryStub) Get(_ context.Context, projectID, configurationID int32) (CurrentConfiguration, error) {
	s.getProjectID = projectID
	s.getConfigurationID = configurationID
	return CurrentConfiguration{ID: configurationID, ProjectID: projectID}, nil
}

func (s *currentConfigurationRepositoryStub) Create(_ context.Context, input CurrentConfigurationCreate) (CurrentConfiguration, error) {
	s.createInput = input
	return CurrentConfiguration{ID: 11, ProjectID: input.ProjectID, EliteaTitle: input.EliteaTitle}, nil
}

func (s *currentConfigurationRepositoryStub) Replace(_ context.Context, input CurrentConfigurationReplace) (CurrentConfiguration, error) {
	s.replaceInput = input
	return CurrentConfiguration{ID: input.ConfigurationID, ProjectID: input.ProjectID, EliteaTitle: input.EliteaTitle}, nil
}

func (s *currentConfigurationRepositoryStub) Delete(_ context.Context, projectID, configurationID int32) error {
	s.deleteProjectID = projectID
	s.deleteConfigurationID = configurationID
	return nil
}
