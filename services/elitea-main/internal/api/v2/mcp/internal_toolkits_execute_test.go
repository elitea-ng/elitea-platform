package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	toolkitsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
)

type fakeInternalToolkitRepo struct {
	listRows    []map[string]any
	listTotal   int
	created     map[string]any
	updated     map[string]any
	projectID   string
	toolkitID   string
	page        int
	pageSize    int
	createBody  map[string]any
	updateBody  map[string]any
	listCalls   int
	createCalls int
	updateCalls int
}

func (repo *fakeInternalToolkitRepo) ListTypes(context.Context, string) ([]string, error) {
	return nil, nil
}

func (repo *fakeInternalToolkitRepo) AvailableTools(context.Context, string, string) ([]toolkitsapi.Tool, error) {
	return nil, nil
}

func (repo *fakeInternalToolkitRepo) DiscoverTools(context.Context, string, string) ([]toolkitsapi.Tool, error) {
	return nil, nil
}

func (repo *fakeInternalToolkitRepo) ValidateToolkit(context.Context, string, string) (bool, error) {
	return true, nil
}

func (repo *fakeInternalToolkitRepo) ForkToolkit(context.Context, string, map[string]any) (toolkitsapi.Tool, error) {
	return toolkitsapi.Tool{}, nil
}

func (repo *fakeInternalToolkitRepo) ListToolkits(
	_ context.Context,
	projectID string,
	page int,
	pageSize int,
) ([]map[string]any, int, error) {
	repo.listCalls++
	repo.projectID = projectID
	repo.page = page
	repo.pageSize = pageSize
	return repo.listRows, repo.listTotal, nil
}

func (repo *fakeInternalToolkitRepo) CreateToolkit(
	_ context.Context,
	projectID string,
	body map[string]any,
) (map[string]any, error) {
	repo.createCalls++
	repo.projectID = projectID
	repo.createBody = body
	return repo.created, nil
}

func (repo *fakeInternalToolkitRepo) GetToolkit(
	_ context.Context,
	projectID string,
	toolkitID string,
) (map[string]any, error) {
	repo.projectID = projectID
	repo.toolkitID = toolkitID
	return nil, nil
}

func (repo *fakeInternalToolkitRepo) UpdateToolkit(
	_ context.Context,
	projectID string,
	toolkitID string,
	body map[string]any,
) (map[string]any, error) {
	repo.updateCalls++
	repo.projectID = projectID
	repo.toolkitID = toolkitID
	repo.updateBody = body
	return repo.updated, nil
}

func (repo *fakeInternalToolkitRepo) DeleteToolkit(context.Context, string, string) error {
	return nil
}

func TestInternalToolkitListUsesOnlyBoundedMainPagination(t *testing.T) {
	repo := &fakeInternalToolkitRepo{
		listRows:  []map[string]any{{"id": "7", "name": "github"}},
		listTotal: 1,
	}
	executor := newHandlerInternalToolkitExecutor(toolkitsapi.NewHandlerWithRepo(repo))
	result, err := executor.Execute(context.Background(), 9, 41, internalListToolkits, map[string]any{
		"limit": json.Number("25"), "offset": json.Number("50"),
	})
	if err != nil {
		t.Fatalf("list toolkits: %v", err)
	}
	if result.status != http.StatusOK || repo.listCalls != 1 || repo.projectID != "9" ||
		repo.page != 3 || repo.pageSize != 25 {
		t.Fatalf("status=%d calls=%d project=%q page=%d size=%d body=%s",
			result.status, repo.listCalls, repo.projectID, repo.page, repo.pageSize, result.body)
	}
}

func TestInternalToolkitCreateUsesActorAndDropsControlArguments(t *testing.T) {
	repo := &fakeInternalToolkitRepo{created: map[string]any{
		"id": "17", "type": "openapi", "name": "orders",
	}}
	executor := newHandlerInternalToolkitExecutor(toolkitsapi.NewHandlerWithRepo(repo))
	result, err := executor.Execute(context.Background(), 9, 41, internalCreateToolkit, map[string]any{
		"project_id": json.Number("9"),
		"type":       "openapi",
		"name":       "orders",
		"settings":   map[string]any{"spec": "openapi: 3.1.0"},
		"_author_id": "999",
		"unexpected": "must-not-cross",
	})
	if err != nil {
		t.Fatalf("create toolkit: %v", err)
	}
	if result.status != http.StatusCreated || repo.createCalls != 1 || repo.projectID != "9" {
		t.Fatalf("status=%d calls=%d project=%q body=%s",
			result.status, repo.createCalls, repo.projectID, result.body)
	}
	if repo.createBody["_author_id"] != "41" || repo.createBody["type"] != "openapi" ||
		repo.createBody["name"] != "orders" {
		t.Fatalf("created body = %#v", repo.createBody)
	}
	for _, forbidden := range []string{"project_id", "unexpected"} {
		if _, crossed := repo.createBody[forbidden]; crossed {
			t.Fatalf("control argument %q crossed into create body: %#v", forbidden, repo.createBody)
		}
	}
}

func TestInternalToolkitUpdateIsPartialAndCannotReplaceThePathID(t *testing.T) {
	repo := &fakeInternalToolkitRepo{updated: map[string]any{
		"id": "17", "type": "openapi", "name": "renamed",
	}}
	executor := newHandlerInternalToolkitExecutor(toolkitsapi.NewHandlerWithRepo(repo))
	result, err := executor.Execute(context.Background(), 9, 41, internalUpdateToolkit, map[string]any{
		"toolkit_id": json.Number("17"),
		"name":       "renamed",
		"id":         json.Number("999"),
		"owner_id":   json.Number("999"),
	})
	if err != nil {
		t.Fatalf("update toolkit: %v", err)
	}
	if result.status != http.StatusOK || repo.updateCalls != 1 || repo.toolkitID != "17" ||
		repo.updateBody["name"] != "renamed" {
		t.Fatalf("status=%d calls=%d id=%q update=%#v body=%s",
			result.status, repo.updateCalls, repo.toolkitID, repo.updateBody, result.body)
	}
	for _, forbidden := range []string{"toolkit_id", "id", "owner_id"} {
		if _, crossed := repo.updateBody[forbidden]; crossed {
			t.Fatalf("identity argument %q crossed into update body: %#v", forbidden, repo.updateBody)
		}
	}
}

func TestInternalToolkitRelationPreservesAbsentVersusEmptySelection(t *testing.T) {
	_, withoutSelection, failure := internalToolkitRelationBody(map[string]any{
		"toolkit_id": json.Number("17"), "entity_id": json.Number("5"),
		"entity_version_id": json.Number("33"), "has_relation": true,
	})
	if failure != nil {
		t.Fatalf("relation without selection failed: %s", failure.body)
	}
	if _, present := withoutSelection["selected_tools"]; present {
		t.Fatalf("absent selection became present: %#v", withoutSelection)
	}

	_, emptySelection, failure := internalToolkitRelationBody(map[string]any{
		"toolkit_id": json.Number("17"), "entity_id": json.Number("5"),
		"entity_version_id": json.Number("33"), "has_relation": true,
		"selected_tools": []any{},
	})
	if failure != nil {
		t.Fatalf("relation with empty selection failed: %s", failure.body)
	}
	selected, present := emptySelection["selected_tools"].([]any)
	if !present || len(selected) != 0 {
		t.Fatalf("empty selection was not preserved: %#v", emptySelection)
	}
}

func TestInternalToolkitInputValidationStopsBeforeMutation(t *testing.T) {
	tests := []struct {
		name      string
		operation internalToolkitOperation
		arguments map[string]any
	}{
		{"list limit", internalListToolkits, map[string]any{"limit": json.Number("101")}},
		{"create type", internalCreateToolkit, map[string]any{"name": "orders"}},
		{"update body", internalUpdateToolkit, map[string]any{"toolkit_id": json.Number("17")}},
		{"relation type", internalUpdateToolRelation, map[string]any{
			"toolkit_id": json.Number("17"), "entity_id": json.Number("5"),
			"entity_version_id": json.Number("33"), "has_relation": true, "entity_type": "pipeline",
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := &fakeInternalToolkitRepo{}
			executor := newHandlerInternalToolkitExecutor(toolkitsapi.NewHandlerWithRepo(repo))
			result, err := executor.Execute(context.Background(), 9, 41, test.operation, test.arguments)
			if err != nil {
				t.Fatalf("validate input: %v", err)
			}
			if result.status != http.StatusBadRequest || repo.createCalls != 0 ||
				repo.updateCalls != 0 || repo.listCalls != 0 {
				t.Fatalf("status=%d create=%d update=%d list=%d body=%s",
					result.status, repo.createCalls, repo.updateCalls, repo.listCalls, result.body)
			}
		})
	}
}
