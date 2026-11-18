package repos

import (
	"context"
	"fmt"
	"log/slog"

	dg "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/delegationgrant"
)

// DelegationMigrationReport counts what one pass of the startup migration did.
type DelegationMigrationReport struct {
	Projects int
	// Done are the projects the marker said had been migrated already.
	Done int
	// Migrated are the projects migrated by this pass, Failed the ones that
	// were not (the next start tries them again).
	Migrated, Failed         int
	Edges, Created, Notified int
}

// MigrateAll runs the one-time migration for every project that has not had it.
// A project is migrated in one transaction with its marker, so a failure leaves
// it as it was and the next start tries it again. It never stops the service:
// the caller logs the report.
func (e *DelegationEdges) MigrateAll(ctx context.Context) (DelegationMigrationReport, error) {
	var report DelegationMigrationReport
	// No session lock is held across the pass: each project is written under its
	// own transaction lock and finds the marker a replica that got there first
	// wrote, so a second replica repeats nothing and no connection is pinned.
	projects, err := e.projectIDs(ctx)
	if err != nil {
		return report, err
	}
	report.Projects = len(projects)
	for _, projectID := range projects {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		result, err := e.Run(ctx, projectID, DelegationEdgeOptions{
			Origin: dg.OriginMigration, Legacy: true, Notify: true, Marker: DelegationEdgesMigrationName,
		})
		switch {
		case err != nil:
			report.Failed++
			slog.WarnContext(ctx, "delegation migration: the project was not migrated and is tried again at the next start",
				"project_id", projectID, "error", err)
		case result.Skipped:
			report.Done++
		default:
			report.Migrated++
			report.Edges += result.Edges
			report.Created += result.Created
			report.Notified += result.Notified
		}
	}
	return report, nil
}

// ScanAll runs the migration again for every project, ignoring the marker:
// the administrator's "scan now" for a restored database, a project provisioned
// after the upgrade, or an edge written by a path the save sync does not see.
// It writes only what is missing (a pair with an active grant, or a legacy
// grant that was decided, is left alone), so it is safe to repeat.
func (e *DelegationEdges) ScanAll(ctx context.Context) (DelegationMigrationReport, error) {
	var report DelegationMigrationReport
	projects, err := e.projectIDs(ctx)
	if err != nil {
		return report, err
	}
	report.Projects = len(projects)
	for _, projectID := range projects {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		result, err := e.Run(ctx, projectID, DelegationEdgeOptions{Origin: dg.OriginMigration, Legacy: true, Notify: true})
		if err != nil {
			report.Failed++
			slog.WarnContext(ctx, "delegation scan: the project was not scanned", "project_id", projectID, "error", err)
			continue
		}
		report.Migrated++
		report.Edges += result.Edges
		report.Created += result.Created
		report.Notified += result.Notified
	}
	return report, nil
}

// SyncQuietly gives the edges a save created their pending grants, as the person
// who saved. It runs after the drift check of a project (delegationdrift.Trigger)
// and covers what the attach dialog never sees: a pipeline's YAML Agent nodes, an
// import, a version save that carries tools. It never fails the write that armed
// it: a failure is logged and the administrator's scan makes it good.
func (e *DelegationEdges) SyncQuietly(ctx context.Context, projectID, actorID int64) {
	result, err := e.Run(ctx, projectID, DelegationEdgeOptions{Origin: dg.OriginAttach, Actor: actorID})
	switch {
	case err != nil:
		slog.WarnContext(ctx, "delegation sync: the project's edges were not synced", "project_id", projectID, "error", err)
	case result.Created > 0:
		slog.InfoContext(ctx, "delegation sync: new edges have pending grants", "project_id", projectID, "created", result.Created)
	}
}

// projectIDs lists the projects that have the delegation tables.
func (e *DelegationEdges) projectIDs(ctx context.Context) ([]int64, error) {
	rows, err := e.grants.pool.Query(ctx, `
SELECT p.id FROM centry.project AS p
WHERE to_regclass(format('%I.delegation_migration_runs', 'p_' || p.id)) IS NOT NULL
ORDER BY p.id`)
	if undefinedTable(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("delegation migration: list the projects: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
