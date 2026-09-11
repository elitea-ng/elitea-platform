package runtimecomposition

import (
	"context"
	"errors"
	"fmt"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpregistry"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/jackc/pgx/v5/pgxpool"
)

// newCurrentAgentVersionFreezer reuses the same schema-driven, provider-neutral
// Configurations graph as index admission. It freezes references but never
// places plaintext credentials in the durable agent input.
func newCurrentAgentVersionFreezer(
	pool *pgxpool.Pool,
	configurations *CurrentConfigurationsRuntime,
) (*agentexecutionapp.CurrentApplicationToolSnapshotService, error) {
	if pool == nil || configurations == nil || configurations.expander == nil ||
		configurations.models == nil || configurations.unsecreter == nil ||
		configurations.publicProjectID <= 0 {
		return nil, errors.New("current agent configuration dependencies are required")
	}
	settings, names, prebuilt, err := newCurrentToolkitSettingsGraph(pool, configurations)
	if err != nil {
		return nil, err
	}
	guardrailPolicies, err := platformconfig.NewGuardrailPolicyAdapter(pool)
	if err != nil {
		return nil, fmt.Errorf("construct current agent guardrail policy source: %w", err)
	}
	return agentexecutionapp.NewCurrentApplicationToolSnapshotService(
		currentAgentToolkitSettingsResolver{inner: settings, prebuilt: prebuilt},
		currentAgentToolkitNameAdapter{names: names},
		configurations.models,
		guardrailPolicies,
		configurations.publicProjectID,
	)
}

type currentAgentToolkitNameAdapter struct {
	names CurrentToolkitNameDeriver
}

func (adapter currentAgentToolkitNameAdapter) ResolveCurrentAgentToolkitName(
	ctx context.Context,
	request agentexecutionapp.CurrentAgentToolkitNameRequest,
) (string, error) {
	if adapter.names == nil {
		return "", errors.New("current agent toolkit name resolver is unavailable")
	}
	return adapter.names.DeriveCurrentToolkitName(
		ctx,
		CurrentToolkitNameInput{
			ProjectID: request.ProjectID, UserID: request.UserID,
			ToolkitType: request.ToolkitType, StoredName: request.StoredName,
			Settings: request.Settings,
		},
	)
}

var _ agentexecutionapp.CurrentAgentToolkitNameResolver = currentAgentToolkitNameAdapter{}

// newCurrentToolkitSettingsGraph composes the schema-driven toolkit settings
// resolver and the name deriver it is built from.
//
// It was extracted from newCurrentAgentVersionFreezer above so the toolkit
// WRITE path can run the identical graph rather than a second, separately
// maintained copy of it (NewToolkitSettingsValidator below).
// Index admission uses a different dynamic source. Agent execution admits
// enabled prebuilt MCP types and omits other unavailable dynamic types.
func newCurrentToolkitSettingsGraph(
	pool *pgxpool.Pool,
	configurations *CurrentConfigurationsRuntime,
) (*configurationapp.CurrentToolkitSettingsResolver, CurrentToolkitNameDeriver, *currentAgentPrebuiltMCP, error) {
	builtInSchemas, err := LoadPinnedCurrentToolkitSchemaSnapshot()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load current agent toolkit schema snapshot: %w", err)
	}
	prebuilt, err := newCurrentAgentPrebuiltMCP(mcpregistry.NewPrebuiltStore(pool))
	if err != nil {
		return nil, nil, nil, err
	}
	schemas, err := NewCurrentCompositeToolkitSchemaCatalog(builtInSchemas, prebuilt)
	if err != nil {
		return nil, nil, nil, err
	}
	names, err := NewCurrentBuiltInToolkitNameDeriver(builtInSchemas)
	if err != nil {
		return nil, nil, nil, err
	}
	toolkitRows, err := repos.NewCurrentToolkitsRepository(pool)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("construct current agent toolkit repository: %w", err)
	}
	nestedToolkits, err := NewCurrentNestedToolkitReaderAdapter(toolkitRows, names)
	if err != nil {
		return nil, nil, nil, err
	}
	modelVisibility, err := NewCurrentModelVisibilityAdapter(
		configurations.models,
		configurations.publicProjectID,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	settings, err := configurationapp.NewCurrentToolkitSettingsResolver(
		schemas,
		nestedToolkits,
		configurations.expander,
		modelVisibility,
		configurations.unsecreter,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	return settings, names, prebuilt, nil
}

// NewToolkitSettingsValidator composes the resolver the toolkit CREATE/UPDATE
// routes run before they persist a credential reference
// (internal/api/v2/toolkits/settings_validation.go).
//
// It is the SAME graph agent-version freezing uses, deliberately: a second
// composition would be a second answer to "which credentials can this caller
// see", and the two would drift. In particular it takes the agent variant of
// the dynamic-schema half, so a toolkit type absent from the pinned SDK
// snapshot — database, custom, datasource, every *_loader — reports "no schema"
// rather than a dependency failure, and the write path treats that as nothing
// to say instead of a refusal.
//
// The caller runs it in CurrentToolkitSettingsReferenceMode, which never
// redeems a secret: the vault is not read, and the returned settings are
// discarded.
func (runtime *CurrentConfigurationsRuntime) NewToolkitSettingsValidator(
	pool *pgxpool.Pool,
) (*configurationapp.CurrentToolkitSettingsResolver, error) {
	if pool == nil || runtime == nil || runtime.expander == nil ||
		runtime.models == nil || runtime.unsecreter == nil || runtime.publicProjectID <= 0 {
		return nil, errors.New("current toolkit settings validator dependencies are required")
	}
	settings, _, _, err := newCurrentToolkitSettingsGraph(pool, runtime)
	return settings, err
}
