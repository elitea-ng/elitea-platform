package api

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	v2runcaps "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/runcaps"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workerruncaps"
	dbrepos "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// overrideCapPermission gates the run cap override route (ADR-0041
// decision 3), resolved in the project: the platform administrators and the
// project admin hold it (shared 0212).
const overrideCapPermission = workerruncaps.OverridePermission

// runCapsServices are the agent save check and the override grant of the
// run caps (ADR-0041 decision 3).
type runCapsServices struct {
	checker  *workerruncaps.Checker
	override *v2runcaps.Handler
}

// newRunCapsServices reads the run_caps section through the per-replica
// cache the admin save drops (cache), else a cache of its own. No pool
// leaves the save check off and the override route answering 503, never a
// typed-nil store.
func newRunCapsServices(pool *pgxpool.Pool, cache any) runCapsServices {
	if pool == nil {
		return runCapsServices{override: v2runcaps.NewHandler(nil)}
	}
	settings, ok := cache.(workerruncaps.SettingsReader)
	if !ok || settings == nil {
		settings = platformconfig.NewCachedSection(func(ctx context.Context) (platformconfig.RunCaps, error) {
			return platformconfig.LoadRunCaps(ctx, pool)
		}, 0)
	}
	services := runCapsServices{override: v2runcaps.NewHandler(nil)}
	if tiers, err := dbrepos.NewWorkerEgressSources(pool); err == nil {
		if checker, err := workerruncaps.NewChecker(settings, tiers); err == nil {
			services.checker = checker
		}
	}
	if store, err := dbrepos.NewRunCapsRepository(pool); err == nil {
		if service, err := workerruncaps.NewOverrideService(store, settings); err == nil {
			services.override = v2runcaps.NewHandler(service)
		}
	}
	return services
}
