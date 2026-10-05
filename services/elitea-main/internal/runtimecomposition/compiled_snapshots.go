package runtimecomposition

import (
	"context"
	"errors"
	"fmt"

	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

func validateCompiledSnapshotDependencies(config Config, dependencies Dependencies) error {
	if config.RustCompiledSnapshots == nil {
		if dependencies.CompiledSnapshotStatePool != nil {
			return errors.New("compiled snapshot state pool requires explicit enablement")
		}
		return nil
	}
	if dependencies.CompiledSnapshotStatePool == nil || dependencies.ObjectStore == nil {
		return errors.New("compiled snapshots require their original agentstate pool and existing object store")
	}
	for _, business := range []*pgxpool.Pool{dependencies.AdmissionPool, dependencies.ControlPool, dependencies.OutputPool, dependencies.ReplayPool, dependencies.TerminalEffectsPool, dependencies.ContentPool} {
		if business == dependencies.CompiledSnapshotStatePool {
			return errors.New("compiled snapshot agentstate must not substitute a business runtime pool")
		}
	}
	return nil
}
func configureCompiledSnapshotAuthority(ctx context.Context, config Config, dependencies Dependencies) (*repos.CompiledSnapshotsRepository, *domain.RustSnapshotProfiles, error) {
	if err := validateCompiledSnapshotDependencies(config, dependencies); err != nil {
		return nil, nil, err
	}
	if config.RustCompiledSnapshots == nil {
		return nil, nil, nil
	}
	if err := migrate.New(dependencies.CompiledSnapshotStatePool, platformmigrations.Files).CheckHead(ctx, migrate.ScopeAgentState, "agentstate"); err != nil {
		return nil, nil, fmt.Errorf("compiled snapshot agentstate migration head is not applied: %w", err)
	}
	profiles, err := loadCompiledSnapshotProfiles(*config.RustCompiledSnapshots)
	if err != nil {
		return nil, nil, err
	}
	index, err := repos.NewCompiledSnapshotsRepository(dependencies.CompiledSnapshotStatePool, config.RustCompiledSnapshots.Quota)
	if err != nil {
		return nil, nil, err
	}
	return index, profiles, nil
}

// compiledSnapshotMaintenance extends the existing replay janitor's synchronous
// pass. It starts no loop or goroutine and owns no database/storage resources.
type compiledSnapshotMaintenance struct {
	replay executionReplayProgressPruner
	store  *storage.SandboxBundleStore
	index  domain.RustSnapshotEvictor
	owner  string
}

func (p *compiledSnapshotMaintenance) PruneExpiredReplayProgress(ctx context.Context) (int64, error) {
	count, replayErr := p.replay.PruneExpiredReplayProgress(ctx)
	cacheErr := p.store.SweepCompiledSnapshots(ctx, p.index, p.owner, 32)
	return count, errors.Join(replayErr, cacheErr)
}
func attachCompiledSnapshotMaintenance(janitor *executionReplayRetentionJanitor, store *storage.SandboxBundleStore, index domain.RustSnapshotEvictor, owner string) error {
	if janitor == nil || janitor.pruner == nil || store == nil || index == nil || owner == "" || len(owner) > 128 {
		return errors.New("compiled snapshot maintenance dependencies are incomplete")
	}
	janitor.pruner = &compiledSnapshotMaintenance{replay: janitor.pruner, store: store, index: index, owner: owner}
	return nil
}
