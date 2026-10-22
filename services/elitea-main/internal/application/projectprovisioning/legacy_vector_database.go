package projectprovisioning

// The legacy PgVector database of a deleted project (#1359).
//
// Until #1359 a project could own a database, project_<id>, and a login role,
// project_<id>_user, on the PgVector server: pylon provisioned them for every
// project, and this service did too for a while (the project_pgvector step).
// Nothing reads them any more, since every vector is in Qdrant behind
// elitea-vector, but they still hold the project's chunk text and embeddings.
// A delete that forgot them would leave a deleted tenant's content in
// PostgreSQL for ever, so the journal keeps one step that drops them:
// StepProjectPgvectorDrop, the name the step had before, so a row written by
// an older build that already finished it (done) is not run again.
//
// WHEN THE STEP IS ON A ROW'S LIST. When the deciding transaction found
// evidence of a legacy database (HadVectorStore: the project's own
// `elitea-pgvector` configuration row, or the database or role on the
// platform database's server), or when this replica can drop one (an admin URL
// is configured, WithLegacyVectorDatabases). A fresh install that never ran
// the PgVector store has neither, and its deletes never see the step.
//
// WHAT A RUN OF IT DOES.
//
//   - Admin URL configured: drop project_<id> and project_<id>_user (both
//     idempotent) and note what went. The drop is attempted whatever the
//     evidence says: a database can exist without its configuration row (a
//     provisioning run that died between the two). A failure keeps the row
//     open and backs off, like any step.
//   - No admin URL, and the operator declared the server retired
//     (WithLegacyVectorDatabasesRetired, ELITEA_LEGACY_PGVECTOR_RETIRED=true):
//     done, noted as such. The data went with the server.
//   - No admin URL, not retired, evidence recorded: the step FAILS, logged at
//     Error with the variable to set, and the row stays open (attempts, backoff,
//     last_error) until one of the two is configured. Completing it would be
//     claiming a deleted project's data is gone while it is not.
//
// Projects deleted before this step came back (their rows completed without
// it, or they had no row) are `elitea-main legacy-pgvector-orphans`'s.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/cleanupjournal"
)

const (
	// StepProjectPgvectorDrop drops the project's legacy PgVector database and
	// role. See this file's comment.
	StepProjectPgvectorDrop = "project_pgvector_drop"
	// LegacyPgvectorAdminURLEnv names the admin URL of the server that holds
	// the legacy project_<id> databases: a role that may drop them and their
	// roles (the old store's bootstrap connection).
	LegacyPgvectorAdminURLEnv = "ELITEA_LEGACY_PGVECTOR_ADMIN_URL"
	// LegacyPgvectorRetiredEnv is the operator's statement that the legacy
	// PgVector server is gone, with its databases: the step closes without a
	// drop.
	LegacyPgvectorRetiredEnv = "ELITEA_LEGACY_PGVECTOR_RETIRED"
	// NoteLegacyVectorDatabasesRetired is the note of a step closed under the
	// retired switch.
	NoteLegacyVectorDatabasesRetired = "skipped: the legacy PgVector server is retired (" + LegacyPgvectorRetiredEnv + ")"
	// legacyPgvectorTitle is the elitea_title of the configuration row the
	// old store wrote into a provisioned project's tenant.
	legacyPgvectorTitle = "elitea-pgvector"
)

// ErrLegacyVectorDatabaseNotDropped reports a delete that removed the project
// and has not dropped its legacy PgVector database and role yet: the drop
// failed, or no admin URL is configured to run it. The journal retries it.
var ErrLegacyVectorDatabaseNotDropped = errors.New("projectprovisioning: legacy PgVector database was not dropped")

// legacyDropTimeout bounds one drop inside the run's own bound: DROP DATABASE
// ... WITH (FORCE) waits for the sessions it terminates to go.
var legacyDropTimeout = time.Minute

// LegacyVectorDatabases drops a project's legacy PgVector database and role.
// infra/legacypgvector's Dropper satisfies it. The summary is the step's note.
type LegacyVectorDatabases interface {
	DropProjectDatabase(ctx context.Context, projectID int64) (summary string, err error)
}

// WithLegacyVectorDatabases supplies the dropper. A typed nil is absent.
func WithLegacyVectorDatabases(databases LegacyVectorDatabases) Option {
	return func(p *Provisioner) {
		if databases == nil || cleanupjournal.IsNilPointer(databases) {
			return
		}
		p.legacyVectorDatabases = databases
	}
}

// WithLegacyVectorDatabasesRetired records the operator's statement that the
// legacy PgVector server is gone (LegacyPgvectorRetiredEnv).
func WithLegacyVectorDatabasesRetired() Option {
	return func(p *Provisioner) { p.legacyVectorDatabasesRetired = true }
}

// legacyVectorStepOwed reports whether a run of the row takes the drop step.
func (p *Provisioner) legacyVectorStepOwed(cleanup *deletionCleanup) bool {
	return cleanup.HadVectorStore || p.legacyVectorDatabases != nil
}

// legacyVectorStep is the journal step. It is a step of the ROW (it depends on
// the row's evidence), so it is appended by Provisioner.cleanupSteps after the
// base list, whose indices stay stable.
func legacyVectorStep() cleanupStep {
	return cleanupStep{name: StepProjectPgvectorDrop, notDone: ErrLegacyVectorDatabaseNotDropped, run: cleanupLegacyVectorDatabase}
}

func cleanupLegacyVectorDatabase(ctx context.Context, p *Provisioner, projectID int64, cleanup *deletionCleanup) (string, error) {
	if p.legacyVectorDatabases != nil {
		dropCtx, cancel := context.WithTimeout(ctx, legacyDropTimeout)
		defer cancel()
		summary, err := p.legacyVectorDatabases.DropProjectDatabase(dropCtx, projectID)
		if err != nil {
			return "", err
		}
		p.logger.InfoContext(ctx, "project delete: legacy PgVector database step done",
			"project_id", projectID, "summary", summary)
		return summary, nil
	}
	if p.legacyVectorDatabasesRetired {
		return NoteLegacyVectorDatabasesRetired, nil
	}
	// The step is on the list without a dropper only because the decision
	// recorded evidence (legacyVectorStepOwed).
	database := legacyDatabaseName(projectID)
	p.logger.ErrorContext(ctx, "project delete: the deleted project's legacy PgVector database is NOT dropped, so its chunk text and embeddings remain; the cleanup journal keeps the row open until it can drop it",
		"project_id", projectID, "database", database, "role", legacyRoleName(projectID),
		"set", LegacyPgvectorAdminURLEnv, "or_if_the_server_is_gone", LegacyPgvectorRetiredEnv+"=true")
	return "", fmt.Errorf("legacy PgVector database %s may remain and no admin URL is configured: set %s, or %s=true if that server is gone",
		database, LegacyPgvectorAdminURLEnv, LegacyPgvectorRetiredEnv)
}

// legacyDatabaseName and legacyRoleName are the old store's names; the same
// derivation as infra/legacypgvector, which this layer does not import.
func legacyDatabaseName(projectID int64) string { return "project_" + strconv.FormatInt(projectID, 10) }
func legacyRoleName(projectID int64) string     { return legacyDatabaseName(projectID) + "_user" }

// recordLegacyVectorEvidence reads, through the deciding transaction and
// before the tenant rows cascade, whether the project has a legacy PgVector
// database: the configuration row the old store wrote into its tenant, or the
// database or role on the platform database's server (a deployment whose old
// store shared it). Either sets HadVectorStore, which puts the drop step on
// the row even where no admin URL is configured, so the delete cannot finish
// silently over the data.
func recordLegacyVectorEvidence(ctx context.Context, transaction pgx.Tx, projectID int64, cleanup *deletionCleanup) error {
	var onServer bool
	if err := transaction.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_database WHERE datname = $1)
    OR EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = $2)`,
		legacyDatabaseName(projectID), legacyRoleName(projectID)).Scan(&onServer); err != nil {
		return fmt.Errorf("read the legacy PgVector database on the platform server: %w", err)
	}
	if onServer {
		cleanup.HadVectorStore = true
		return nil
	}
	schema := (&provisionState{projectID: projectID}).tenantSchema()
	var tenantPresent bool
	if err := transaction.QueryRow(ctx, `SELECT to_regclass($1::text) IS NOT NULL`,
		schema+".configuration").Scan(&tenantPresent); err != nil {
		return fmt.Errorf("resolve the tenant configuration table: %w", err)
	}
	if !tenantPresent {
		return nil
	}
	// The schema name is derived from an integer id and quoted; the title is
	// bound. Type and section are compared as the retired-type gate compares
	// them (configurations/retired.go), case- and space-insensitively.
	var recorded bool
	if err := transaction.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+pgx.Identifier{schema, "configuration"}.Sanitize()+`
WHERE elitea_title = $1 AND lower(btrim(type)) = 'pgvector')`, legacyPgvectorTitle).Scan(&recorded); err != nil {
		return fmt.Errorf("read the legacy PgVector configuration row: %w", err)
	}
	cleanup.HadVectorStore = recorded
	return nil
}
