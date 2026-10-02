package runtimecomposition

import (
	"context"
	"errors"
	"fmt"
	"time"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	toolkitexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

var errInvalidCurrentToolkitAdapterRow = errors.New("current toolkit adapter row is invalid")

type currentToolkitStore interface {
	Get(context.Context, int32, int32) (repos.CurrentToolkit, error)
	GetMCPVisible(context.Context, int32, int32, int32) (repos.CurrentToolkit, error)
}

// CurrentToolkitReaderAdapter projects the provider-neutral current toolkit
// row into the index admission contract. Authorization and tenant routing
// remain owned by CurrentToolkitsRepository.
type CurrentToolkitReaderAdapter struct {
	repository currentToolkitStore
	names      CurrentToolkitNameDeriver
}

func NewCurrentToolkitReaderAdapter(
	repository *repos.CurrentToolkitsRepository,
	names CurrentToolkitNameDeriver,
) (*CurrentToolkitReaderAdapter, error) {
	if repository == nil {
		return nil, errors.New("current toolkit repository and name deriver are required")
	}
	return newCurrentToolkitReaderAdapter(repository, names)
}

func newCurrentToolkitReaderAdapter(
	repository currentToolkitStore,
	names CurrentToolkitNameDeriver,
) (*CurrentToolkitReaderAdapter, error) {
	if repository == nil || names == nil {
		return nil, errors.New("current toolkit repository and name deriver are required")
	}
	return &CurrentToolkitReaderAdapter{repository: repository, names: names}, nil
}

func (r *CurrentToolkitReaderAdapter) GetCurrentToolkit(
	ctx context.Context,
	projectID int32,
	userID int32,
	toolkitID int32,
) (indexingapp.CurrentToolkitSnapshot, bool, error) {
	toolkit, found, err := readCurrentToolkit(ctx, r.repository, projectID, toolkitID)
	if err != nil || !found {
		return indexingapp.CurrentToolkitSnapshot{}, found, err
	}
	settings, ok := toolkit.Settings.(map[string]any)
	if !ok || settings == nil {
		return indexingapp.CurrentToolkitSnapshot{}, false, errInvalidCurrentToolkitAdapterRow
	}

	name, err := r.names.DeriveCurrentToolkitName(ctx, CurrentToolkitNameInput{
		ProjectID:   projectID,
		UserID:      userID,
		ToolkitType: toolkit.Type,
		StoredName:  toolkit.Name,
		Settings:    settings,
	})
	if err != nil {
		return indexingapp.CurrentToolkitSnapshot{}, false, fmt.Errorf("derive current toolkit name: %w", err)
	}
	return indexingapp.CurrentToolkitSnapshot{
		ID:       toolkit.ID,
		Type:     toolkit.Type,
		Name:     name,
		Settings: settings,
	}, true, nil
}

// GetCurrentMCPToolkit projects the same generated-query row into the direct
// toolkit admission contract. Keeping it on this adapter ensures index and MCP
// execution share project transaction routing and schema-aware name derivation
// instead of growing a second SQL path in the HTTP layer.
func (r *CurrentToolkitReaderAdapter) GetCurrentMCPToolkit(
	ctx context.Context,
	projectID int32,
	userID int32,
	toolkitID int32,
) (toolkitexecutionapp.CurrentMCPToolkitSnapshot, bool, error) {
	toolkit, found, err := readCurrentMCPToolkit(
		ctx, r.repository, projectID, userID, toolkitID,
	)
	if err != nil || !found {
		return toolkitexecutionapp.CurrentMCPToolkitSnapshot{}, found, err
	}
	settings, settingsOK := toolkit.Settings.(map[string]any)
	meta, metaOK := toolkit.Meta.(map[string]any)
	if !settingsOK || settings == nil || !metaOK || meta == nil {
		return toolkitexecutionapp.CurrentMCPToolkitSnapshot{}, false, errInvalidCurrentToolkitAdapterRow
	}

	name, err := r.names.DeriveCurrentToolkitName(ctx, CurrentToolkitNameInput{
		ProjectID:   projectID,
		UserID:      userID,
		ToolkitType: toolkit.Type,
		StoredName:  toolkit.Name,
		Settings:    settings,
	})
	if err != nil {
		return toolkitexecutionapp.CurrentMCPToolkitSnapshot{}, false, fmt.Errorf("derive current toolkit name: %w", err)
	}
	return toolkitexecutionapp.CurrentMCPToolkitSnapshot{
		ID: toolkit.ID, Type: toolkit.Type, Name: name, Settings: settings, Meta: meta,
	}, true, nil
}

func readCurrentMCPToolkit(
	ctx context.Context,
	repository currentToolkitStore,
	projectID int32,
	userID int32,
	toolkitID int32,
) (repos.CurrentToolkit, bool, error) {
	toolkit, err := repository.GetMCPVisible(ctx, projectID, userID, toolkitID)
	if errors.Is(err, repos.ErrCurrentToolkitNotFound) {
		return repos.CurrentToolkit{}, false, nil
	}
	if err != nil {
		return repos.CurrentToolkit{}, false, fmt.Errorf("read current MCP toolkit: %w", err)
	}
	return toolkit, true, nil
}

func readCurrentToolkit(
	ctx context.Context,
	repository currentToolkitStore,
	projectID int32,
	toolkitID int32,
) (repos.CurrentToolkit, bool, error) {
	toolkit, err := repository.Get(ctx, projectID, toolkitID)
	if errors.Is(err, repos.ErrCurrentToolkitNotFound) {
		return repos.CurrentToolkit{}, false, nil
	}
	if err != nil {
		return repos.CurrentToolkit{}, false, fmt.Errorf("read current toolkit: %w", err)
	}
	return toolkit, true, nil
}

type CurrentToolkitNameInput struct {
	ProjectID   int32
	UserID      int32
	ToolkitType string
	StoredName  *string
	Settings    map[string]any
}

// CurrentToolkitNameDeriver owns exact schema-aware toolkit naming. Its
// implementation must use the current built-in registry used by
// ToolDetails.set_toolkit_name (not the actor-visible dynamic schema overlay),
// including toolkit_name and max_toolkit_length annotations. A row-only
// fallback is not a valid implementation.
type CurrentToolkitNameDeriver interface {
	DeriveCurrentToolkitName(context.Context, CurrentToolkitNameInput) (string, error)
}

// CurrentNestedToolkitReaderAdapter projects the same provider-neutral row
// into the current recursive toolkit shape. Construction requires the exact
// schema-aware name deriver; there is deliberately no stored-name fallback.
type CurrentNestedToolkitReaderAdapter struct {
	repository currentToolkitStore
	names      CurrentToolkitNameDeriver
}

func NewCurrentNestedToolkitReaderAdapter(
	repository *repos.CurrentToolkitsRepository,
	names CurrentToolkitNameDeriver,
) (*CurrentNestedToolkitReaderAdapter, error) {
	if repository == nil {
		return nil, errors.New("current toolkit repository and name deriver are required")
	}
	return newCurrentNestedToolkitReaderAdapter(repository, names)
}

func newCurrentNestedToolkitReaderAdapter(
	repository currentToolkitStore,
	names CurrentToolkitNameDeriver,
) (*CurrentNestedToolkitReaderAdapter, error) {
	if repository == nil || names == nil {
		return nil, errors.New("current toolkit repository and name deriver are required")
	}
	return &CurrentNestedToolkitReaderAdapter{repository: repository, names: names}, nil
}

func (r *CurrentNestedToolkitReaderAdapter) GetCurrentNestedToolkit(
	ctx context.Context,
	projectID int32,
	userID int32,
	toolkitID int32,
) (configurationapp.CurrentNestedToolkit, bool, error) {
	toolkit, found, err := readCurrentToolkit(ctx, r.repository, projectID, toolkitID)
	if err != nil || !found {
		return configurationapp.CurrentNestedToolkit{}, found, err
	}
	settings, ok := toolkit.Settings.(map[string]any)
	if !ok || settings == nil {
		return configurationapp.CurrentNestedToolkit{}, false, errInvalidCurrentToolkitAdapterRow
	}
	toolkitName, err := r.names.DeriveCurrentToolkitName(ctx, CurrentToolkitNameInput{
		ProjectID:   projectID,
		UserID:      userID,
		ToolkitType: toolkit.Type,
		StoredName:  toolkit.Name,
		Settings:    settings,
	})
	if err != nil {
		return configurationapp.CurrentNestedToolkit{}, false, fmt.Errorf("derive current toolkit name: %w", err)
	}

	authorID := toolkit.AuthorID
	createdAt := currentPythonTimestampISOFormat(toolkit.CreatedAt)
	return configurationapp.CurrentNestedToolkit{
		ID:          toolkit.ID,
		ToolkitName: toolkitName,
		Type:        toolkit.Type,
		Settings:    settings,
		AuthorID:    &authorID,
		CreatedAt:   &createdAt,
	}, true, nil
}

// elitea_tools.created_at is a PostgreSQL timestamp without time zone. Python
// datetime.isoformat() emits no zone and includes six fractional digits only
// when its microsecond component is non-zero.
func currentPythonTimestampISOFormat(value time.Time) string {
	formatted := value.Format("2006-01-02T15:04:05")
	microseconds := value.Nanosecond() / int(time.Microsecond)
	if microseconds == 0 {
		return formatted
	}
	return fmt.Sprintf("%s.%06d", formatted, microseconds)
}

type currentModelCatalog interface {
	Get(context.Context, configurationapp.CurrentModelCatalogQuery) (configurationapp.CurrentModelCatalogResponse, error)
}

// CurrentModelVisibilityAdapter applies the current project plus shared public
// visibility rule through Configurations' model catalog. Model credentials and
// provider selection never cross this boundary.
type CurrentModelVisibilityAdapter struct {
	catalog         currentModelCatalog
	publicProjectID int32
}

func NewCurrentModelVisibilityAdapter(
	catalog *configurationapp.CurrentModelCatalogService,
	publicProjectID int32,
) (*CurrentModelVisibilityAdapter, error) {
	if catalog == nil {
		return nil, errors.New("current model catalog and public project are required")
	}
	return newCurrentModelVisibilityAdapter(catalog, publicProjectID)
}

func newCurrentModelVisibilityAdapter(
	catalog currentModelCatalog,
	publicProjectID int32,
) (*CurrentModelVisibilityAdapter, error) {
	if catalog == nil || publicProjectID <= 0 {
		return nil, errors.New("current model catalog and public project are required")
	}
	return &CurrentModelVisibilityAdapter{catalog: catalog, publicProjectID: publicProjectID}, nil
}

func (a *CurrentModelVisibilityAdapter) IsCurrentModelVisible(
	ctx context.Context,
	projectID int32,
	section string,
	name string,
) (bool, error) {
	modelSection := configurationapp.CurrentModelSection(section)
	if !configurationapp.IsSupportedCurrentModelSection(modelSection) {
		// The current configurations_get_available_models contract returns an
		// empty collection for an unknown section.
		return false, nil
	}
	catalog, err := a.catalog.Get(ctx, configurationapp.CurrentModelCatalogQuery{
		Section:         modelSection,
		ProjectID:       projectID,
		PublicProjectID: a.publicProjectID,
		IncludeShared:   true,
	})
	if err != nil {
		return false, err
	}
	for _, item := range catalog.Items {
		if item.Name == name {
			return true, nil
		}
	}
	return false, nil
}

var _ indexingapp.CurrentToolkitReader = (*CurrentToolkitReaderAdapter)(nil)
var _ configurationapp.CurrentNestedToolkitReader = (*CurrentNestedToolkitReaderAdapter)(nil)
var _ configurationapp.CurrentModelVisibility = (*CurrentModelVisibilityAdapter)(nil)
