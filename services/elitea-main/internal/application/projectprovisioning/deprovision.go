package projectprovisioning

// Project deletion — the symmetric half issue #333 asks for.
//
// The reference is legacy/plugins/projects/api/v2/project.py's `delete_project`,
// which walks the same step list in REVERSE calling each step's `delete`. This
// reuses the very same `remove` functions the create path compensates with, so
// the two directions cannot drift apart: a step added to createSteps() is
// deleted here automatically, and a compensation bug shows up in both.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

// ErrProjectNotFound reports a delete for a project id that has no row.
var ErrProjectNotFound = errors.New("projectprovisioning: project does not exist")

// ErrProjectNotRemoved reports a delete that ran every step and still left the
// project row behind.
var ErrProjectNotRemoved = errors.New("projectprovisioning: project was not removed")

// ErrTenantSchemaNotRemoved reports a delete that removed the project row and
// left the tenant schema behind.
//
// The schema is dropped last (#374), so this is the only residue a failed
// delete can leave. It is a storage leak and not a stopper: cmd/elitea-migrate
// reads projects and not schemas, so a schema with no project row does not
// block migration. The delete still reports it, because a route that answers
// 200 for a job it did not finish is the failure mode this package exists to
// avoid. Another delete of the same id clears it.
var ErrTenantSchemaNotRemoved = errors.New("projectprovisioning: tenant schema was not removed")

// ErrArtifactsNotRemoved reports a delete that removed the project and left
// live artifact buckets behind: their object purge failed, or no object store
// is configured to run it.
//
// The bucket rows are kept on purpose. They are the only handle on the bytes
// under p/<id>/, and a project id is never reused, so nothing else could find
// those bytes again. Another delete of the same id retries the purge. The
// route answers 500 with the per-step detail, so an operator sees the leak.
var ErrArtifactsNotRemoved = errors.New("projectprovisioning: artifact buckets were not purged")

// ErrProjectWorkActive reports a delete refused because the project still has
// work that is not terminal (#1211): an index run, an agent execution or a
// toolkit call. All of it uses the project's PgVector database (checkpoints,
// index tools), so dropping it under running work would kill that work
// mid-write. The delete waits: stop or let the work finish, then retry. It is
// retryable by construction, and the first check runs before any step so that a
// refused delete leaves the project exactly as it was.
var ErrProjectWorkActive = errors.New("projectprovisioning: project has active runs; stop them or wait for them to finish, then retry the delete")

// ErrVectorStoreNotDropped reports a delete that removed the project and could
// not drop its PgVector database or role. The indexed data is still in
// Postgres. A second delete answers 404 because the project is gone, so the
// leftover is cleared with cmd/pgvector-orphans (#1211). Result.VectorDatabase
// names the database.
var ErrVectorStoreNotDropped = errors.New("projectprovisioning: project vector store was not dropped")

// activeWorkSQL counts the project's execution_jobs in any non-terminal state,
// whatever the capability. The state list is the shared definition in
// domain/execution (the same states the admission-capacity predicate of
// migration 0033 counts), not a copy. QUARANTINED is terminal for this purpose:
// a quarantined job is not executing, and counting it would make a project with
// one stuck job undeletable.
var activeWorkSQL = `
SELECT count(*) FROM elitea_runtime.execution_jobs
WHERE resource_project_id = $1
  AND state IN (` + nonTerminalStatesSQL() + `)`

func nonTerminalStatesSQL() string {
	states := []execution.JobState{
		execution.JobPending, execution.JobDispatched, execution.JobClaimed,
		execution.JobRunning, execution.JobSettling,
	}
	quoted := make([]string, len(states))
	for i, s := range states {
		quoted[i] = "'" + string(s) + "'"
	}
	return strings.Join(quoted, ", ")
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// activeWork counts the project's non-terminal executions. A deployment whose
// runtime schema is not installed has none.
func activeWork(ctx context.Context, q queryRower, projectID int64) (int64, error) {
	var present bool
	if err := q.QueryRow(ctx,
		`SELECT to_regclass('elitea_runtime.execution_jobs') IS NOT NULL`).Scan(&present); err != nil {
		return 0, err
	}
	if !present {
		return 0, nil
	}
	var n int64
	if err := q.QueryRow(ctx, activeWorkSQL, projectID).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// Deprovision removes a project and everything provisioning created for it.
//
// TWO DELIBERATE DEVIATIONS from the reference:
//
// First, the reference answers 200 whatever happens. Every one of its undo
// steps is individually try/except'd and the failures are only logged, so a
// delete that removed nothing at all is reported as a success — the
// "answers 200, did nothing" shape this codebase keeps re-shipping. Here the
// per-step results are still best-effort, because stopping at the first failure
// would strand the remaining resources; but the project row is RE-READ at the
// end, and a project that survived is an error rather than a 200.
//
// Second, the reference leaves auth_core__project_role and
// auth_core__project_user_role rows behind for every deleted project, because
// its ProjectPermissions and ProjectAdmin steps have no-op deletes. Those rows
// describe a project that no longer exists, and they resurface if the id is
// reused. removeProjectPermissions clears them.
//
// THE ORDER (#374). The steps run in reverse, with one exception: the tenant
// schema is dropped LAST, and only after the project row is proved gone. The
// order is the whole fix, because the schema holds the tenant data and the
// project row does not. Two outcomes are possible, and both are safe:
//
//   - the row goes, then the schema goes. The project is deleted.
//   - the row stays, so the schema stays. The project is unchanged, this
//     returns ErrProjectNotRemoved, and the route answers 500.
//
// The third outcome — a project row whose schema is gone — is the one this
// order makes unreachable. cmd/elitea-migrate refuses to run against it, and
// that refusal stops migration for every tenant in the deployment.
//
// Every row that references centry.project(id) goes in the same transaction as
// the project row. See removeProjectModel and referencingDeletes.
func (p *Provisioner) Deprovision(ctx context.Context, projectID int64) (Result, error) {
	if projectID <= 0 {
		return Result{}, ErrProjectNotFound
	}
	// The vault is required here for the same reason Provision requires it: a
	// delete that skipped the vault would leave rows keyed `project-<id>` with
	// no project, and the next project to draw that id would adopt them.
	if p.pool == nil || p.vault == nil {
		return Result{}, errors.New("projectprovisioning: provisioner is not configured")
	}

	// A project is present when its row is there OR when its schema is there
	// OR when it still has a live artifact bucket. The row alone would make
	// the delete of a leftover schema impossible: a delete that removed the
	// row and could not drop the schema would answer "not found" on every
	// retry, and the schema would stay for ever. A live bucket is the same
	// residue for the bytes in object storage (ErrArtifactsNotRemoved).
	// Nothing present is a real not-found.
	state := &provisionState{projectID: projectID}
	var exists bool
	if err := p.pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM centry.project WHERE id = $1)
    OR EXISTS (SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname = $2)`,
		projectID, state.tenantSchema(),
	).Scan(&exists); err != nil {
		return Result{}, fmt.Errorf("projectprovisioning: resolve project %d: %w", projectID, err)
	}
	if !exists {
		live, err := p.liveBucketCount(ctx, projectID)
		if err != nil {
			return Result{}, fmt.Errorf("projectprovisioning: resolve project %d: %w", projectID, err)
		}
		exists = live > 0
	}
	if !exists {
		return Result{}, ErrProjectNotFound
	}

	// Refuse BEFORE touching anything while any work is active: cancelling would
	// be a side effect of a delete that then did not happen, and FORCE-dropping
	// the database would kill the work mid-write. removeProjectModel repeats the
	// count under a lock on the project row, which is the fence against work
	// admitted after this point.
	if p.vectorStore != nil {
		active, err := activeWork(ctx, p.pool, projectID)
		if err != nil {
			return Result{}, fmt.Errorf("projectprovisioning: check active work for project %d: %w", projectID, err)
		}
		if active > 0 {
			p.logger.WarnContext(ctx, "project delete refused: work is active",
				"project_id", projectID, "active_runs", active)
			return Result{}, ErrProjectWorkActive
		}
	}
	state.deleting = true

	// removeSystemUser deletes by id, so the identity has to be resolved first.
	// An absent row is not an error: a project provisioned before this step
	// existed, or one whose system user was already removed, still deletes.
	var systemUserID *int64
	if err := p.pool.QueryRow(ctx,
		`SELECT id FROM public.auth_core__user WHERE email = $1`,
		systemUserEmail(projectID),
	).Scan(&systemUserID); err != nil {
		p.logger.InfoContext(ctx, "no system user to remove for project",
			"project_id", projectID, "err", err)
	} else if systemUserID != nil {
		state.systemUserID = *systemUserID
	}

	// Every step is "attempted" for deletion: unlike compensation, there is no
	// progress list to consult, and each remove tolerates a resource that was
	// never created.
	attempted := make([]StepStatus, 0, len(createSteps()))
	for _, step := range createSteps() {
		attempted = append(attempted, StepStatus{Step: step.name, Initialized: true})
	}
	result := Result{
		ProjectID:     projectID,
		RollbackSteps: p.compensate(ctx, state, attempted),
	}

	// The discriminating check: did both halves actually go?
	var rowSurvived, schemaSurvived bool
	if err := p.pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM centry.project WHERE id = $1),
       EXISTS (SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname = $2)`,
		projectID, state.tenantSchema(),
	).Scan(&rowSurvived, &schemaSurvived); err != nil {
		return result, fmt.Errorf("projectprovisioning: verify project %d removal: %w", projectID, err)
	}
	if rowSurvived {
		// The schema and the PgVector database are still there too, because
		// both drops are held back while the row is there. The project is
		// unchanged and it stays usable.
		if state.workActive {
			return result, ErrProjectWorkActive
		}
		return result, ErrProjectNotRemoved
	}

	// The row is gone. From here every failure is reported, and none hides
	// another: a retry answers 404 for a project whose row is gone, so whatever
	// is not named in this return is invisible to the operator.
	var failures []error
	if schemaSurvived {
		failures = append(failures, ErrTenantSchemaNotRemoved)
	}
	live, err := p.liveBucketCount(ctx, projectID)
	switch {
	case err != nil:
		failures = append(failures, fmt.Errorf("projectprovisioning: verify project %d removal: %w", projectID, err))
	case live > 0:
		p.logger.ErrorContext(ctx, "project deleted with artifact buckets left to purge",
			"project_id", projectID, "live_buckets", live, "prefix", fmt.Sprintf("p/%d/", projectID))
		failures = append(failures, ErrArtifactsNotRemoved)
	}

	// The irreversible PgVector drop runs only now, with the row proved gone, so
	// a failed row delete cannot leave a surviving project without its vectors.
	// New work cannot be admitted for a deleted project: execution_jobs carries
	// a foreign key to centry.project, and removeProjectModel held the row lock
	// while it counted active work.
	if p.vectorStore != nil {
		database, dropErr := p.vectorStore.DropProjectVectorStore(ctx, projectID)
		if dropErr == nil {
			status := StepStatus{Step: StepProjectPgvectorDrop, Initialized: true}
			status.setOK()
			result.RollbackSteps = append(result.RollbackSteps, status)
		} else {
			p.logger.ErrorContext(ctx, "project deleted but its PgVector database or role was not dropped",
				"project_id", projectID, "database", database, "err", dropErr)
			result.VectorDatabase = database
			status := StepStatus{Step: StepProjectPgvectorDrop, Initialized: true}
			status.setFailed(safeStepMessage(StepProjectPgvectorDrop))
			result.RollbackSteps = append(result.RollbackSteps, status)
			failures = append(failures, fmt.Errorf("%w: database %q remains for pgvector-orphans: %w",
				ErrVectorStoreNotDropped, database, dropErr))
		}
	}
	return result, errors.Join(failures...)
}
