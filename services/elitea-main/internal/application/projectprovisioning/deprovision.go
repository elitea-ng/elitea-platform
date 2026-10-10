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
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/pgvector"
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
// retryable by construction. It is returned ONLY by the fence at the start of
// Deprovision, before any step runs and before the project is marked deleting,
// so a refused delete leaves the project exactly as it was and the 409 the
// route answers is true.
var ErrProjectWorkActive = errors.New("projectprovisioning: project has active runs; stop them or wait for them to finish, then retry the delete")

// ErrProjectDeletionInProgress reports a delete refused because another delete
// of the same project is running right now, in this process or in another
// replica (a client retry, a second operator, the tombstone reconciler). Deletes
// of one project are serialized by a per-project session advisory lock; the
// loser does not queue behind the winner, it is told to try again later. The
// route answers 409 "deletion already in progress". Nothing was changed by the
// refused call.
var ErrProjectDeletionInProgress = errors.New("projectprovisioning: a delete of this project is already in progress; try again later")

// deleteLockNamespace is the first half of the two-integer advisory lock key a
// delete holds for its whole duration: ASCII "PDEL". The second half is the
// project id (centry.project's key is an int4). It is deliberately NOT the
// "ELIT" namespace infra/pgvector uses for its provision/drop lock: that lock is
// taken on the PgVector bootstrap connection, possibly on the same server, and
// Deprovision calls the drop WHILE it holds this one. A shared key would make
// the drop wait for a lock its own caller holds (and only a session-level
// advisory lock held by a different session blocks, so it would be a timeout,
// not an error anyone could read). Distinct namespaces cannot collide.
const deleteLockNamespace int32 = 0x5044454c

// ErrVectorStoreNotDropped reports a delete that removed the project and could
// not drop its PgVector database or role. The indexed data is still in
// Postgres. A second delete answers 404 because the project is gone, so the
// leftover is cleared with cmd/pgvector-orphans (#1211). Result.VectorDatabase
// names the database.
var ErrVectorStoreNotDropped = errors.New("projectprovisioning: project vector store was not dropped")

// vectorDropTimeout bounds the post-commit PgVector drop: the advisory-lock wait
// plus the drop itself. The drop runs detached from the request (see
// Deprovision), so this is its only deadline. A variable so a test can shrink it.
var vectorDropTimeout = 2 * time.Minute

// activeWorkSQL counts the project's execution_jobs in any non-terminal state,
// whatever the capability. The state list is the shared definition in
// domain/execution (execution.NonTerminalJobStates; migration 0033 carries a SQL
// copy of the same set), not a copy. QUARANTINED is terminal for this purpose: a
// quarantined job is not executing, and counting it would make a project with
// one stuck job undeletable.
//
// A job counts when the project is EITHER side of it: resource_project_id (whose
// resources the job uses) or projection_project_id (where it projects its
// results). That is exactly the pair the admission trigger of shared/0160
// guards, so a job the trigger would have refused to admit is a job the fence
// waits for.
var activeWorkSQL = `
SELECT count(*) FROM elitea_runtime.execution_jobs
WHERE (resource_project_id = $1 OR projection_project_id = $1)
  AND state IN (` + nonTerminalStatesSQL() + `)`

func nonTerminalStatesSQL() string {
	states := execution.NonTerminalJobStates()
	quoted := make([]string, len(states))
	for i, s := range states {
		quoted[i] = "'" + string(s) + "'"
	}
	return strings.Join(quoted, ", ")
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// activeWork counts the project's non-terminal executions. The runtime schema is
// part of the shared migration corpus (0030), and shared/0160 (which the
// tombstone column comes from) needs the table for its trigger, so the table is
// always there when this runs.
func activeWork(ctx context.Context, q queryRower, projectID int64) (int64, error) {
	var n int64
	if err := q.QueryRow(ctx, activeWorkSQL, projectID).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// DeprovisionOption adjusts one Deprovision call.
type DeprovisionOption func(*deprovisionConfig)

type deprovisionConfig struct {
	skipActiveWork bool
}

// SkipActiveWorkCheck makes Deprovision tombstone the project without counting
// its non-terminal work first. For the one caller that deletes a project it
// knows cannot be running anything: the personal-project ensurer repairing a
// project whose creation never finished (create_success=false). The tombstone
// is still set, so no new work is admitted; only the "refuse while work is
// active" step is skipped. An explicit DELETE request never passes it.
func SkipActiveWorkCheck() DeprovisionOption {
	return func(c *deprovisionConfig) { c.skipActiveWork = true }
}

// deletionState is what the fence records on the tombstone
// (centry.project.deleting_state), together with deleting_at, because the walk
// destroys what it is derived from. A retry reads it and never re-derives it:
// by then the configuration row that says "this project had a vector store" is
// gone, and asking again would turn a leaked database into "skipped".
type deletionState struct {
	HadVectorStore bool   `json:"had_vector_store"`
	VectorDatabase string `json:"vector_database,omitempty"`
}

// cautiousDeletionState is the state assumed when none was recorded or it cannot
// be read: the project had a store. The cautious answer reports a drop that
// could not run instead of a silent skip.
func cautiousDeletionState(projectID int64) deletionState {
	return deletionState{HadVectorStore: true, VectorDatabase: pgvector.ProjectDatabaseName(projectID)}
}

// deriveDeletionState reads, from the live configuration, whether the project has
// a vector store. It is only meaningful BEFORE the walk removes the configuration
// row, which is why the fence calls it and records the answer.
func (p *Provisioner) deriveDeletionState(ctx context.Context, projectID int64) deletionState {
	if p.vectorStore == nil {
		return deletionState{}
	}
	had, err := p.vectorStore.ProjectHasVectorStore(ctx, projectID)
	if err != nil {
		// Unknown is treated as "had one".
		p.logger.WarnContext(ctx, "could not tell whether the project had a vector store",
			"project_id", projectID, "err", err)
		return cautiousDeletionState(projectID)
	}
	if !had {
		return deletionState{}
	}
	return deletionState{HadVectorStore: true, VectorDatabase: pgvector.ProjectDatabaseName(projectID)}
}

// lockDelete takes the per-project delete lock: a SESSION advisory lock on a
// connection held for the whole delete (a transaction-scoped lock would end
// with the fence transaction, long before the walk). The attempt does not wait:
// a delete already running makes this one ErrProjectDeletionInProgress. The
// returned release must run when the delete is over; it unlocks, and discards
// the connection if it cannot prove the lock is gone.
func (p *Provisioner) lockDelete(ctx context.Context, projectID int64) (release func(), err error) {
	connection, err := p.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("projectprovisioning: acquire delete lock connection for project %d: %w", projectID, err)
	}
	var locked bool
	if err := connection.QueryRow(ctx,
		`SELECT pg_try_advisory_lock($1, $2)`, deleteLockNamespace, int32(projectID)).Scan(&locked); err != nil {
		// The state of the lock is unknown: do not hand the connection back.
		_ = connection.Conn().Close(context.WithoutCancel(ctx))
		connection.Release()
		return nil, fmt.Errorf("projectprovisioning: take delete lock for project %d: %w", projectID, err)
	}
	if !locked {
		connection.Release()
		return nil, ErrProjectDeletionInProgress
	}
	return func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		var unlocked bool
		if err := connection.QueryRow(unlockCtx,
			`SELECT pg_advisory_unlock($1, $2)`, deleteLockNamespace, int32(projectID)).Scan(&unlocked); err != nil || !unlocked {
			// Closing the session releases a session lock; the pool replaces it.
			_ = connection.Conn().Close(unlockCtx)
		}
		connection.Release()
	}, nil
}

// fenceDelete is the first thing an explicit delete does (#1211): it decides, in
// one transaction, whether the delete may start, and marks the project deleting
// so that nothing new can start behind it.
//
//  1. Lock the project row FOR UPDATE. Admitting work inserts an
//     execution_jobs row that references the project, which needs a KEY SHARE
//     lock on it, so an admission racing this transaction waits for it.
//  2. Count the project's non-terminal executions, whatever the capability
//     (on a deployment with a vector store: the database is what running work
//     depends on, and without one the delete keeps removing the job rows).
//  3. Any: roll back and answer ErrProjectWorkActive. Nothing has changed, so
//     "retry later" is true: no secret, bucket, permission or system user has
//     been touched.
//  4. None: set deleting_at and commit. The admission trigger of shared/0160 now
//     refuses new work for the project, and the lock held until here means no
//     job slipped in between the count and the mark.
//
// A project that is already marked is a retried delete: the fence passed on an
// earlier attempt and nothing could have been admitted since, so it continues
// the walk instead of counting again. A project with no row (a leftover schema
// or bucket) has nothing to fence.
//
// The fence also RECORDS the deletion state (deletionState) in the same UPDATE
// that sets deleting_at. A retried delete returns the recorded state instead of
// deriving it again. A project with no row has nothing recorded, and its state is
// derived from whatever configuration is left.
func (p *Provisioner) fenceDelete(ctx context.Context, projectID int64, skipActiveWork bool) (deletionState, error) {
	transaction, err := p.pool.Begin(ctx)
	if err != nil {
		return deletionState{}, fmt.Errorf("projectprovisioning: begin delete fence for project %d: %w", projectID, err)
	}
	defer func() { _ = transaction.Rollback(context.WithoutCancel(ctx)) }()

	var (
		tombstone *time.Time
		recorded  []byte
	)
	err = transaction.QueryRow(ctx,
		`SELECT deleting_at, deleting_state FROM centry.project WHERE id = $1 FOR UPDATE`, projectID,
	).Scan(&tombstone, &recorded)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return p.deriveDeletionState(ctx, projectID), nil
	case err != nil:
		return deletionState{}, fmt.Errorf("projectprovisioning: lock project %d: %w", projectID, err)
	case tombstone != nil:
		var state deletionState
		if len(recorded) == 0 || json.Unmarshal(recorded, &state) != nil {
			p.logger.WarnContext(ctx, "tombstoned project has no readable deletion state; assuming it had a vector store",
				"project_id", projectID)
			return cautiousDeletionState(projectID), nil
		}
		return state, nil
	}

	// Only a deployment with a vector store has a database that running work
	// depends on. Without one the delete keeps its established behaviour (the
	// project's job rows go with it), so there is nothing to wait for.
	if p.vectorStore != nil && !skipActiveWork {
		active, err := activeWork(ctx, transaction, projectID)
		if err != nil {
			return deletionState{}, fmt.Errorf("projectprovisioning: check active work for project %d: %w", projectID, err)
		}
		if active > 0 {
			p.logger.WarnContext(ctx, "project delete refused: work is active",
				"project_id", projectID, "active_runs", active)
			return deletionState{}, ErrProjectWorkActive
		}
	}
	state := p.deriveDeletionState(ctx, projectID)
	encoded, err := json.Marshal(state)
	if err != nil {
		return deletionState{}, fmt.Errorf("projectprovisioning: encode deletion state for project %d: %w", projectID, err)
	}
	if _, err := transaction.Exec(ctx,
		`UPDATE centry.project SET deleting_at = now(), deleting_state = $2::jsonb WHERE id = $1`,
		projectID, string(encoded)); err != nil {
		return deletionState{}, fmt.Errorf("projectprovisioning: mark project %d deleting: %w", projectID, err)
	}
	if err := transaction.Commit(ctx); err != nil {
		return deletionState{}, fmt.Errorf("projectprovisioning: commit delete fence for project %d: %w", projectID, err)
	}
	return state, nil
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
// THE TOMBSTONE (#1211). Before any step runs, fenceDelete counts the project's
// non-terminal work and sets centry.project.deleting_at in one transaction that
// holds the project row's lock. Work in flight refuses the delete (ErrProjectWorkActive)
// with nothing changed; otherwise the project is marked, the admission trigger of
// shared/0160 refuses new work for it, and the personal-project ensurer will not
// reuse it. The mark is never cleared. A step that fails afterwards leaves it, and
// a retried delete finds it, skips the fence and resumes the walk, which is
// idempotent.
//
// Every row that references centry.project(id) goes in the same transaction as
// the project row. See removeProjectModel and referencingDeletes.
//
// THE DELETE LOCK. Deprovision holds a per-project session advisory lock for its
// whole duration, so two deletes of one project never walk together. The second
// gets ErrProjectDeletionInProgress at once rather than racing the first through
// the same destructive steps.
func (p *Provisioner) Deprovision(ctx context.Context, projectID int64, options ...DeprovisionOption) (Result, error) {
	if projectID <= 0 || projectID > math.MaxInt32 {
		return Result{}, ErrProjectNotFound
	}
	var config deprovisionConfig
	for _, option := range options {
		option(&config)
	}
	// The vault is required here for the same reason Provision requires it: a
	// delete that skipped the vault would leave rows keyed `project-<id>` with
	// no project, and the next project to draw that id would adopt them.
	if p.pool == nil || p.vault == nil {
		return Result{}, errors.New("projectprovisioning: provisioner is not configured")
	}

	release, err := p.lockDelete(ctx, projectID)
	if err != nil {
		return Result{}, err
	}
	defer release()

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

	// THE FENCE. Refuse BEFORE touching anything while any work is active, and
	// mark the project deleting in the same transaction so nothing is admitted
	// afterwards. Cancelling would be a side effect of a delete that then did not
	// happen, and FORCE-dropping the database would kill the work mid-write.
	// Everything below this line runs on a project that is already tombstoned, so
	// a step that fails leaves the tombstone and a retry resumes here.
	//
	// Whether the project had a vector store comes back from the fence, which
	// either derived it just now (before the walk removes the configuration row)
	// or read what the first attempt recorded. The drop needs it to tell "nothing
	// to drop" from "a database that cannot be dropped because the bootstrap is
	// gone", and a retry must not re-derive it from a configuration the previous
	// attempt already removed.
	deletion, err := p.fenceDelete(ctx, projectID, config.skipActiveWork)
	if err != nil {
		return Result{}, err
	}
	hadVectorStore := deletion.HadVectorStore

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
		// both drops are held back while the row is there. The project stays
		// tombstoned (no new work, not reusable), and another delete resumes the
		// walk without fencing again.
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
	// New work cannot have been admitted for the project: it was tombstoned
	// before the walk, and the admission trigger refuses a tombstoned project.
	//
	// The drop is DETACHED from the request. The row is already gone, so a client
	// that hangs up (or a proxy timeout) must not abandon the drop half way and
	// leave an orphan that no retry can reach (the retry answers 404). It gets its
	// own bound instead: the advisory-lock wait plus the drop.
	if p.vectorStore != nil {
		dropCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), vectorDropTimeout)
		database, dropErr := p.vectorStore.DropProjectVectorStore(dropCtx, projectID, hadVectorStore)
		cancel()
		switch {
		case dropErr == nil && database == "":
			status := StepStatus{Step: StepProjectPgvectorDrop, Initialized: true}
			status.setOK()
			status.Msg = "skipped: no vector store"
			result.RollbackSteps = append(result.RollbackSteps, status)
		case dropErr == nil:
			status := StepStatus{Step: StepProjectPgvectorDrop, Initialized: true}
			status.setOK()
			result.RollbackSteps = append(result.RollbackSteps, status)
		default:
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
