package runtimecomposition

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

type currentToolkitStoreStub struct {
	get       func(context.Context, int32, int32) (repos.CurrentToolkit, error)
	calls     int
	mcpCalls  int
	projectID int32
	actorID   int32
	toolkitID int32
}

func (s *currentToolkitStoreStub) Get(
	ctx context.Context,
	projectID int32,
	toolkitID int32,
) (repos.CurrentToolkit, error) {
	s.calls++
	s.projectID = projectID
	s.toolkitID = toolkitID
	return s.get(ctx, projectID, toolkitID)
}

func (s *currentToolkitStoreStub) GetMCPVisible(
	ctx context.Context,
	projectID int32,
	actorID int32,
	toolkitID int32,
) (repos.CurrentToolkit, error) {
	s.mcpCalls++
	s.projectID = projectID
	s.actorID = actorID
	s.toolkitID = toolkitID
	return s.get(ctx, projectID, toolkitID)
}

func TestCurrentToolkitReaderAdapterProjectsCurrentRow(t *testing.T) {
	name := "Team repo.v2 !"
	settings := map[string]any{
		"credential": map[string]any{"elitea_title": "github", "private": true},
		"large_id":   json.Number("9007199254740993"),
	}
	store := &currentToolkitStoreStub{get: func(context.Context, int32, int32) (repos.CurrentToolkit, error) {
		return repos.CurrentToolkit{
			ID:       19,
			Type:     "github",
			Name:     &name,
			Settings: settings,
		}, nil
	}}
	names := &currentToolkitNameDeriverStub{derive: func(_ context.Context, input CurrentToolkitNameInput) (string, error) {
		if input.ProjectID != 7 || input.UserID != 11 || input.ToolkitType != "github" || input.StoredName == nil || *input.StoredName != name {
			t.Fatalf("unexpected name input: %+v", input)
		}
		return "Team_repo_v2", nil
	}}
	reader, err := newCurrentToolkitReaderAdapter(store, names)
	if err != nil {
		t.Fatal(err)
	}

	snapshot, found, err := reader.GetCurrentToolkit(context.Background(), 7, 11, 19)
	if err != nil || !found {
		t.Fatalf("indexing snapshot found=%t err=%v", found, err)
	}
	if snapshot.ID != 19 || snapshot.Type != "github" || snapshot.Name != "Team_repo_v2" {
		t.Fatalf("unexpected indexing snapshot: %+v", snapshot)
	}
	if snapshot.Settings["large_id"] != json.Number("9007199254740993") {
		t.Fatalf("JSON number changed: %#v", snapshot.Settings["large_id"])
	}
	if store.calls != 1 || store.projectID != 7 || store.toolkitID != 19 {
		t.Fatalf("repository calls=%d project=%d toolkit=%d", store.calls, store.projectID, store.toolkitID)
	}
}

func TestCurrentToolkitReaderAdapterMapsNotFoundAndPreservesFailures(t *testing.T) {
	dependencyErr := errors.New("database unavailable")
	tests := []struct {
		name      string
		storedErr error
		found     bool
		wantErr   error
	}{
		{name: "not found", storedErr: repos.ErrCurrentToolkitNotFound, found: false},
		{name: "dependency", storedErr: dependencyErr, found: false, wantErr: dependencyErr},
		{name: "canceled", storedErr: context.Canceled, found: false, wantErr: context.Canceled},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &currentToolkitStoreStub{get: func(context.Context, int32, int32) (repos.CurrentToolkit, error) {
				return repos.CurrentToolkit{}, test.storedErr
			}}
			reader, err := newCurrentToolkitReaderAdapter(store, storedCurrentToolkitNameDeriver())
			if err != nil {
				t.Fatal(err)
			}
			_, found, err := reader.GetCurrentToolkit(context.Background(), 7, 11, 19)
			if found != test.found || !errors.Is(err, test.wantErr) {
				t.Fatalf("found=%t err=%v", found, err)
			}
		})
	}
}

func TestCurrentToolkitReaderAdapterRejectsNonObjectSettings(t *testing.T) {
	store := &currentToolkitStoreStub{get: func(context.Context, int32, int32) (repos.CurrentToolkit, error) {
		return repos.CurrentToolkit{ID: 19, Settings: []any{"not", "an", "object"}}, nil
	}}
	reader, err := newCurrentToolkitReaderAdapter(store, storedCurrentToolkitNameDeriver())
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := reader.GetCurrentToolkit(context.Background(), 7, 11, 19); found || !errors.Is(err, errInvalidCurrentToolkitAdapterRow) {
		t.Fatalf("indexing invalid row found=%t err=%v", found, err)
	}
}

func TestCurrentToolkitReaderAdapterRequiresRepository(t *testing.T) {
	if _, err := NewCurrentToolkitReaderAdapter(nil, storedCurrentToolkitNameDeriver()); err == nil {
		t.Fatal("expected missing repository error")
	}
	store := &currentToolkitStoreStub{get: func(context.Context, int32, int32) (repos.CurrentToolkit, error) {
		return repos.CurrentToolkit{}, nil
	}}
	if _, err := newCurrentToolkitReaderAdapter(store, nil); err == nil {
		t.Fatal("expected missing name deriver error")
	}
}

type currentToolkitNameDeriverStub struct {
	derive func(context.Context, CurrentToolkitNameInput) (string, error)
	calls  int
	input  CurrentToolkitNameInput
}

func storedCurrentToolkitNameDeriver() *currentToolkitNameDeriverStub {
	return &currentToolkitNameDeriverStub{derive: func(_ context.Context, input CurrentToolkitNameInput) (string, error) {
		if input.StoredName == nil {
			return "", nil
		}
		return *input.StoredName, nil
	}}
}

func (s *currentToolkitNameDeriverStub) DeriveCurrentToolkitName(
	ctx context.Context,
	input CurrentToolkitNameInput,
) (string, error) {
	s.calls++
	s.input = input
	return s.derive(ctx, input)
}

func TestCurrentNestedToolkitReaderRequiresAndUsesExactNameDeriver(t *testing.T) {
	storedName := "stored name must not be used"
	createdAt := time.Date(2026, time.July, 22, 9, 8, 7, 123456000, time.FixedZone("ignored", 3*60*60))
	settings := map[string]any{
		"repository": "EliteaAI/elitea",
		"large_id":   json.Number("9007199254740993"),
	}
	store := &currentToolkitStoreStub{get: func(context.Context, int32, int32) (repos.CurrentToolkit, error) {
		return repos.CurrentToolkit{
			ID:        19,
			CreatedAt: createdAt,
			Type:      "github",
			Name:      &storedName,
			Settings:  settings,
			AuthorID:  41,
		}, nil
	}}
	names := &currentToolkitNameDeriverStub{derive: func(_ context.Context, input CurrentToolkitNameInput) (string, error) {
		if input.ProjectID != 7 || input.UserID != 11 || input.ToolkitType != "github" ||
			input.StoredName == nil || *input.StoredName != storedName || input.Settings["repository"] != "EliteaAI/elitea" {
			t.Fatalf("unexpected name input: %+v", input)
		}
		return "EliteaAI_elitea", nil
	}}
	reader, err := newCurrentNestedToolkitReaderAdapter(store, names)
	if err != nil {
		t.Fatal(err)
	}

	nested, found, err := reader.GetCurrentNestedToolkit(context.Background(), 7, 11, 19)
	if err != nil || !found {
		t.Fatalf("found=%t err=%v", found, err)
	}
	if nested.ID != 19 || nested.Type != "github" || nested.ToolkitName != "EliteaAI_elitea" {
		t.Fatalf("unexpected nested toolkit: %+v", nested)
	}
	if nested.AuthorID == nil || *nested.AuthorID != 41 {
		t.Fatalf("nested author = %v", nested.AuthorID)
	}
	if nested.CreatedAt == nil || *nested.CreatedAt != "2026-07-22T09:08:07.123456" {
		t.Fatalf("nested created_at = %v", nested.CreatedAt)
	}
	if nested.Settings["large_id"] != json.Number("9007199254740993") || names.calls != 1 {
		t.Fatalf("nested settings=%v name calls=%d", nested.Settings, names.calls)
	}
}

func TestCurrentToolkitReaderProjectsMCPMetadataThroughGeneratedRepository(t *testing.T) {
	storedName := "stored name"
	store := &currentToolkitStoreStub{get: func(context.Context, int32, int32) (repos.CurrentToolkit, error) {
		return repos.CurrentToolkit{
			ID: 19, Type: "github", Name: &storedName,
			Settings: map[string]any{"selected_tools": []any{"get_issue"}},
			Meta:     map[string]any{"mcp_options": map[string]any{"available_by_mcp": true}},
		}, nil
	}}
	names := &currentToolkitNameDeriverStub{derive: func(_ context.Context, input CurrentToolkitNameInput) (string, error) {
		if input.ProjectID != 7 || input.UserID != 11 || input.ToolkitType != "github" ||
			input.StoredName == nil || *input.StoredName != storedName {
			t.Fatalf("unexpected name input: %+v", input)
		}
		return "Source Control", nil
	}}
	reader, err := newCurrentToolkitReaderAdapter(store, names)
	if err != nil {
		t.Fatal(err)
	}

	toolkit, found, err := reader.GetCurrentMCPToolkit(context.Background(), 7, 11, 19)
	if err != nil || !found {
		t.Fatalf("found=%t err=%v", found, err)
	}
	if toolkit.ID != 19 || toolkit.Type != "github" || toolkit.Name != "Source Control" ||
		toolkit.Settings["selected_tools"] == nil || toolkit.Meta["mcp_options"] == nil {
		t.Fatalf("unexpected MCP toolkit projection: %+v", toolkit)
	}
	if store.calls != 0 || store.mcpCalls != 1 || store.projectID != 7 ||
		store.actorID != 11 || store.toolkitID != 19 {
		t.Fatalf("MCP repository calls=%d ordinary=%d project=%d actor=%d toolkit=%d",
			store.mcpCalls, store.calls, store.projectID, store.actorID, store.toolkitID)
	}
}

func TestCurrentNestedToolkitReaderDoesNotFallbackWhenNameDerivationFails(t *testing.T) {
	storedName := "unsafe-fallback"
	dependencyErr := errors.New("effective schema unavailable")
	store := &currentToolkitStoreStub{get: func(context.Context, int32, int32) (repos.CurrentToolkit, error) {
		return repos.CurrentToolkit{
			ID: 19, Type: "github", Name: &storedName, Settings: map[string]any{},
		}, nil
	}}
	names := &currentToolkitNameDeriverStub{derive: func(context.Context, CurrentToolkitNameInput) (string, error) {
		return "", dependencyErr
	}}
	reader, err := newCurrentNestedToolkitReaderAdapter(store, names)
	if err != nil {
		t.Fatal(err)
	}
	_, found, err := reader.GetCurrentNestedToolkit(context.Background(), 7, 11, 19)
	if found || !errors.Is(err, dependencyErr) {
		t.Fatalf("found=%t err=%v", found, err)
	}
}

func TestCurrentNestedToolkitReaderMapsNotFoundBeforeNameDerivation(t *testing.T) {
	store := &currentToolkitStoreStub{get: func(context.Context, int32, int32) (repos.CurrentToolkit, error) {
		return repos.CurrentToolkit{}, repos.ErrCurrentToolkitNotFound
	}}
	names := &currentToolkitNameDeriverStub{derive: func(context.Context, CurrentToolkitNameInput) (string, error) {
		t.Fatal("not-found toolkit reached name derivation")
		return "", nil
	}}
	reader, err := newCurrentNestedToolkitReaderAdapter(store, names)
	if err != nil {
		t.Fatal(err)
	}
	_, found, err := reader.GetCurrentNestedToolkit(context.Background(), 7, 11, 19)
	if err != nil || found || names.calls != 0 {
		t.Fatalf("found=%t err=%v name calls=%d", found, err, names.calls)
	}
}

func TestCurrentNestedToolkitReaderRequiresNameDeriver(t *testing.T) {
	store := &currentToolkitStoreStub{get: func(context.Context, int32, int32) (repos.CurrentToolkit, error) {
		return repos.CurrentToolkit{}, nil
	}}
	names := &currentToolkitNameDeriverStub{derive: func(context.Context, CurrentToolkitNameInput) (string, error) {
		return "", nil
	}}
	if _, err := newCurrentNestedToolkitReaderAdapter(nil, names); err == nil {
		t.Fatal("expected missing repository error")
	}
	if _, err := newCurrentNestedToolkitReaderAdapter(store, nil); err == nil {
		t.Fatal("expected missing exact name deriver error")
	}
	if _, err := NewCurrentNestedToolkitReaderAdapter(nil, names); err == nil {
		t.Fatal("expected missing concrete repository error")
	}
}

func TestCurrentPythonTimestampISOFormatMatchesNaiveDatetime(t *testing.T) {
	tests := []struct {
		name  string
		value time.Time
		want  string
	}{
		{
			name:  "whole second",
			value: time.Date(2026, time.July, 22, 9, 8, 7, 0, time.UTC),
			want:  "2026-07-22T09:08:07",
		},
		{
			name:  "microseconds",
			value: time.Date(2026, time.July, 22, 9, 8, 7, 1000, time.UTC),
			want:  "2026-07-22T09:08:07.000001",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := currentPythonTimestampISOFormat(test.value); got != test.want {
				t.Fatalf("timestamp=%q want=%q", got, test.want)
			}
		})
	}
}

type currentModelCatalogStub struct {
	get   func(context.Context, configurationapp.CurrentModelCatalogQuery) (configurationapp.CurrentModelCatalogResponse, error)
	calls int
	query configurationapp.CurrentModelCatalogQuery
}

func (s *currentModelCatalogStub) Get(
	ctx context.Context,
	query configurationapp.CurrentModelCatalogQuery,
) (configurationapp.CurrentModelCatalogResponse, error) {
	s.calls++
	s.query = query
	return s.get(ctx, query)
}

func TestCurrentModelVisibilityUsesExactProjectAndSharedPublicCatalog(t *testing.T) {
	catalog := &currentModelCatalogStub{get: func(
		_ context.Context,
		query configurationapp.CurrentModelCatalogQuery,
	) (configurationapp.CurrentModelCatalogResponse, error) {
		if query.Section != configurationapp.CurrentModelSectionEmbedding || query.ProjectID != 7 ||
			query.PublicProjectID != 1 || !query.IncludeShared {
			t.Fatalf("unexpected catalog query: %+v", query)
		}
		return configurationapp.CurrentModelCatalogResponse{Items: []configurationapp.CurrentModelCatalogItem{
			{Name: "project-embedding", ProjectID: 7},
			{Name: "shared-embedding", ProjectID: 1, Shared: true},
		}}, nil
	}}
	visibility, err := newCurrentModelVisibilityAdapter(catalog, 1)
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"project-embedding", "shared-embedding"} {
		visible, err := visibility.IsCurrentModelVisible(context.Background(), 7, "embedding", name)
		if err != nil || !visible {
			t.Fatalf("model %q visible=%t err=%v", name, visible, err)
		}
	}
	visible, err := visibility.IsCurrentModelVisible(context.Background(), 7, "embedding", "SHARED-EMBEDDING")
	if err != nil || visible {
		t.Fatalf("case-changing lookup visible=%t err=%v", visible, err)
	}
	if catalog.calls != 3 {
		t.Fatalf("catalog calls = %d", catalog.calls)
	}
}

func TestCurrentModelVisibilityMatchesCurrentUnknownSectionContract(t *testing.T) {
	catalog := &currentModelCatalogStub{get: func(
		context.Context,
		configurationapp.CurrentModelCatalogQuery,
	) (configurationapp.CurrentModelCatalogResponse, error) {
		t.Fatal("unsupported section reached model catalog")
		return configurationapp.CurrentModelCatalogResponse{}, nil
	}}
	visibility, err := newCurrentModelVisibilityAdapter(catalog, 1)
	if err != nil {
		t.Fatal(err)
	}
	visible, err := visibility.IsCurrentModelVisible(context.Background(), 7, "github", "model")
	if err != nil || visible || catalog.calls != 0 {
		t.Fatalf("visible=%t err=%v calls=%d", visible, err, catalog.calls)
	}
}

func TestCurrentModelVisibilityPreservesDependencyFailure(t *testing.T) {
	dependencyErr := errors.New("catalog unavailable")
	catalog := &currentModelCatalogStub{get: func(
		context.Context,
		configurationapp.CurrentModelCatalogQuery,
	) (configurationapp.CurrentModelCatalogResponse, error) {
		return configurationapp.CurrentModelCatalogResponse{}, dependencyErr
	}}
	visibility, err := newCurrentModelVisibilityAdapter(catalog, 1)
	if err != nil {
		t.Fatal(err)
	}
	visible, err := visibility.IsCurrentModelVisible(context.Background(), 7, "llm", "model")
	if visible || !errors.Is(err, dependencyErr) {
		t.Fatalf("visible=%t err=%v", visible, err)
	}
}

func TestCurrentModelVisibilityRequiresCatalogAndPublicProject(t *testing.T) {
	catalog := &currentModelCatalogStub{get: func(
		context.Context,
		configurationapp.CurrentModelCatalogQuery,
	) (configurationapp.CurrentModelCatalogResponse, error) {
		return configurationapp.CurrentModelCatalogResponse{}, nil
	}}
	if _, err := newCurrentModelVisibilityAdapter(nil, 1); err == nil {
		t.Fatal("expected missing catalog error")
	}
	if _, err := newCurrentModelVisibilityAdapter(catalog, 0); err == nil {
		t.Fatal("expected invalid public project error")
	}
}
