package runtimecomposition

import (
	"fmt"

	toolkitexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewCurrentDelegatedAuthToolkitSettings composes Main's OAuth proxy onto the
// same sqlc-backed toolkit reader and claim-mode settings resolver as native
// direct execution. The caller supplies the already-built settings resolver so
// this path cannot acquire a second configuration expander or vault.
func NewCurrentDelegatedAuthToolkitSettings(
	pool *pgxpool.Pool,
	settings toolkitexecutionapp.CurrentToolkitSettingsResolver,
) (*toolkitexecutionapp.CurrentDelegatedAuthToolkitSettings, error) {
	if pool == nil || settings == nil {
		return nil, fmt.Errorf("compose delegated authorization toolkit settings: dependencies are required")
	}
	builtIn, err := LoadPinnedCurrentToolkitSchemaSnapshot()
	if err != nil {
		return nil, fmt.Errorf("load delegated authorization toolkit schema snapshot: %w", err)
	}
	names, err := NewCurrentBuiltInToolkitNameDeriver(builtIn)
	if err != nil {
		return nil, fmt.Errorf("construct delegated authorization toolkit names: %w", err)
	}
	toolkits, err := repos.NewCurrentToolkitsRepository(pool)
	if err != nil {
		return nil, fmt.Errorf("construct delegated authorization toolkit repository: %w", err)
	}
	reader, err := NewCurrentToolkitReaderAdapter(toolkits, names)
	if err != nil {
		return nil, fmt.Errorf("construct delegated authorization toolkit reader: %w", err)
	}
	resolver, err := toolkitexecutionapp.NewCurrentDelegatedAuthToolkitSettings(reader, settings)
	if err != nil {
		return nil, fmt.Errorf("construct delegated authorization settings resolver: %w", err)
	}
	return resolver, nil
}
