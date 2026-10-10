package projectprovisioning

// Project deletion — the symmetric half issue #333 asks for.
//
// The reference is legacy/plugins/projects/api/v2/project.py's `delete_project`,
// which walks the create steps in REVERSE calling each step's `delete`, and
// answers 200 whatever happens. This delete reuses the same `remove` functions
// the create path compensates with, but it is shaped differently (#1211):
//
//  1. ONE TRANSACTION DECIDES (decideDeletion). It locks the project row
//     FOR UPDATE, refuses while the project has work in flight, records what the
//     project owns that has to be cleaned up, writes that record to the cleanup
//     journal (centry.project_deletions, shared/0160), deletes every row that
//     references the project and the project row itself, and commits. If any of
//     it fails the project is unchanged and fully usable, and the delete answers
//     ErrProjectNotRemoved.
//  2. THE CLEANUP RUNS FROM THE JOURNAL (runCleanup), after the commit, detached
//     from the request and with its own bound: the artifact purge, the vault,
//     the system token and user, the project roles, the tenant schema, and the
//     PgVector database. Every step is idempotent and marks itself done in the
//     journal row. Whatever is left is reported by name, and the reconciler
//     (ClaimStaleDeletions / ResumeDeletion) retries it with backoff until the
//     journal row is complete.
//
// WHY THE ROW GOES FIRST. A project row that survives is a usable project, and a
// project whose row is gone can no longer get new work: execution_jobs carries
// foreign keys to centry.project(id) on both resource_project_id and
// projection_project_id, so an admission for a deleted project fails. Every
// irreversible step (the schema, the bytes, the vector database) therefore runs
// on a project that nothing can use any more, and a failure in the transaction
// that decides is a delete that did not happen. There is no in-between state for
// a resolver or a worker to see.
//
// The tenant schema, which used to be dropped last and only after the row was
// proved gone (#374), still is: it is a journal step, and the journal exists
// only once the row is gone. cmd/elitea-migrate can never see a project row
// whose schema is missing.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/pgvector"
)

// ErrProjectNotFound reports a delete for a project id that has no row.
var ErrProjectNotFound = errors.New("projectprovisioning: project does not exist")

// ErrProjectNotRemoved reports a delete whose deciding transaction failed. The
// project is unchanged and fully usable: nothing was removed and no cleanup was
// recorded. A retry is safe.
var ErrProjectNotRemoved = errors.New("projectprovisioning: project was not removed")

// ErrTenantSchemaNotRemoved reports a delete that removed the project row and
// has not dropped the tenant schema yet.
//
// It is a storage leak and not a stopper: cmd/elitea-migrate reads projects and
// not schemas, so a schema with no project row does not block migration. The
// delete still reports it, because a route that answers 200 for a job it did not
// finish is the failure mode this package exists to avoid. The cleanup journal
// retries it.
var ErrTenantSchemaNotRemoved = errors.New("projectprovisioning: tenant schema was not removed")

// ErrArtifactsNotRemoved reports a delete that removed the project and left live
// artifact buckets behind: their object purge failed, or no object store is
// configured to run it.
//
// The bucket rows are kept on purpose. They are the only handle on the bytes
// under p/<id>/, and a project id is never reused, so nothing else could find
// those bytes again. The cleanup journal retries the purge.
var ErrArtifactsNotRemoved = errors.New("projectprovisioning: artifact buckets were not purged")

// ErrCleanupIncomplete reports a delete that removed the project and could not
// finish one of the other cleanup steps (the vault, the system token or user,
// the project roles). The error names the step; the cleanup journal retries it.
var ErrCleanupIncomplete = errors.New("projectprovisioning: a cleanup step did not complete")

// ErrProjectWorkActive reports a delete refused because the project still has
// work that is not terminal (#1211): an index run, an agent execution or a
// toolkit call, on either side of the job (the project whose resources it uses,
// or the project it projects into). All of it uses the project's PgVector
// database and rows the delete removes, so deleting under running work would
// kill that work mid-write. The delete waits: stop or let the work finish, then
// retry. It is returned only by the deciding transaction, which then rolls
// back, so a refused delete leaves the project exactly as it was and the 409
// the route answers is true.
var ErrProjectWorkActive = errors.New("projectprovisioning: project has active runs; stop them or wait for them to finish, then retry the delete")

// ErrVectorStoreNotDropped reports a delete that removed the project and has
// not dropped its PgVector database or role yet. The indexed data is still in
// Postgres. Result.VectorDatabase names the database; the cleanup journal
// retries the drop (and cmd/pgvector-orphans leaves it to the journal).
var ErrVectorStoreNotDropped = errors.New("projectprovisioning: project vector store was not dropped")

// cleanupTimeout bounds one run of a project's cleanup steps. The run is
// detached from the request (the project row is already gone, so a client that
// hangs up must not abandon the cleanup half way), so this is its deadline.
// vectorDropTimeout bounds the drop inside it: the advisory-lock wait plus the
// drop itself. Variables so a test can shrink them.
var (
	cleanupTimeout    = 5 * time.Minute
	vectorDropTimeout = 2 * time.Minute
)

// cleanupLease is how long a run owns a journal row: the run's own bound plus a
// margin. A run that dies leaves the lease to expire, and the reconciler picks
// the row up again after it.
func cleanupLease() time.Duration { return cleanupTimeout + time.Minute }

// The journal's retry backoff: the first failed run waits deletionRetryBase,
// each further failure doubles it, up to deletionRetryMax. Stored in the row
// (next_attempt_at), so it survives restarts and is shared by replicas.
var (
	deletionRetryBase = time.Minute
	deletionRetryMax  = time.Hour
)

// activeWorkSQL counts the project's execution_jobs in any non-terminal state,
// whatever the capability. The state list is the shared definition in
// domain/execution (execution.NonTerminalJobStates; migration 0033 carries a SQL
// copy of the same set), bound as one text[] parameter. QUARANTINED is terminal
// for this purpose: a quarantined job is not executing, and counting it would
// make a project with one stuck job undeletable.
//
// A job counts when the project is EITHER side of it: resource_project_id (whose
// resources the job uses) or projection_project_id (where it projects its
// results).
const activeWorkSQL = `
SELECT count(*) FROM elitea_runtime.execution_jobs
WHERE (resource_project_id = $1 OR projection_project_id = $1)
  AND state = ANY($2::text[])`

func nonTerminalStates() []string {
	states := execution.NonTerminalJobStates()
	values := make([]string, len(states))
	for i, state := range states {
		values[i] = string(state)
	}
	return values
}

// DeprovisionOption adjusts one Deprovision call.
type DeprovisionOption func(*deprovisionConfig)

type deprovisionConfig struct {
	skipActiveWork bool
}

// SkipActiveWorkCheck makes Deprovision skip the count of non-terminal work. For
// the one caller that deletes a project it knows cannot be running anything: the
// personal-project ensurer repairing a project whose creation never finished
// (create_success=false). The project's job rows are deleted with it, as for
// any delete. An explicit DELETE request never passes it.
func SkipActiveWorkCheck() DeprovisionOption {
	return func(c *deprovisionConfig) { c.skipActiveWork = true }
}

// deletionCleanup is the journal record: what the deciding transaction found the
// project owns, and which cleanup steps are done. It is everything a later run
// needs, because by then the project row (and with it every way to re-derive
// these facts) is gone.
type deletionCleanup struct {
	TenantSchema   string          `json:"tenant_schema"`
	SystemUserID   int64           `json:"system_user_id,omitempty"`
	VaultProject   string          `json:"vault_project"`
	Buckets        []string        `json:"buckets,omitempty"`
	HadVectorStore bool            `json:"had_vector_store"`
	VectorDatabase string          `json:"vector_database,omitempty"`
	Done           map[string]bool `json:"done"`
}

// cleanupStep is one idempotent journal step. note is an optional message for a
// step that succeeded without doing anything ("skipped: ...").
type cleanupStep struct {
	name string
	run  func(ctx context.Context, p *Provisioner, projectID int64, cleanup *deletionCleanup) (note string, err error)
}

// cleanupSteps is the journal's step list, in the order a run takes it. The
// bytes go first (the bucket rows are their only handle), the identity rows
// after, the tenant schema and the PgVector database last.
//
// The memberships (project_admin's rows) go with the project roles: an
// assignment cascades from its role. The PgVector configuration row lives in
// the tenant schema and goes with it.
func cleanupSteps() []cleanupStep {
	return []cleanupStep{
		{name: StepArtifactBuckets, run: cleanupArtifactBuckets},
		{name: StepProjectSecrets, run: withState(removeProjectSecrets)},
		{name: StepSystemToken, run: withState(removeSystemToken)},
		{name: StepSystemUser, run: withState(removeSystemUser)},
		{name: StepProjectPermissions, run: withState(removeProjectPermissions)},
		{name: StepProjectSchema, run: withState(removeProjectSchema)},
		{name: StepProjectPgvectorDrop, run: cleanupVectorStore},
	}
}

// withState adapts a create step's remove function to a journal step. The
// recorded system user id is the one state the remove functions read.
func withState(remove func(context.Context, *Provisioner, *provisionState) error) func(context.Context, *Provisioner, int64, *deletionCleanup) (string, error) {
	return func(ctx context.Context, p *Provisioner, projectID int64, cleanup *deletionCleanup) (string, error) {
		state := &provisionState{projectID: projectID, systemUserID: cleanup.SystemUserID}
		return "", remove(ctx, p, state)
	}
}

// cleanupArtifactBuckets purges the project's live buckets, removes the
// soft-deleted rows the purge leaves, and fails while a live bucket remains.
// The soft-deleted rows are removed even when a purge failed, so a retry only
// ever sees the buckets still to purge.
func cleanupArtifactBuckets(ctx context.Context, p *Provisioner, projectID int64, _ *deletionCleanup) (string, error) {
	purgeErr := removeArtifactBuckets(ctx, p, &provisionState{projectID: projectID})
	var present bool
	if err := p.pool.QueryRow(ctx,
		`SELECT to_regclass('elitea_storage.buckets') IS NOT NULL`).Scan(&present); err != nil {
		return "", errors.Join(purgeErr, fmt.Errorf("resolve elitea_storage.buckets: %w", err))
	}
	if present {
		if _, err := p.pool.Exec(ctx,
			`DELETE FROM elitea_storage.buckets WHERE project_id = $1 AND deleted_at IS NOT NULL`, projectID); err != nil {
			return "", errors.Join(purgeErr, fmt.Errorf("delete purged bucket rows: %w", err))
		}
	}
	if purgeErr != nil {
		return "", purgeErr
	}
	live, err := p.liveBucketCount(ctx, projectID)
	if err != nil {
		return "", err
	}
	if live > 0 {
		return "", fmt.Errorf("%d live bucket(s) left to purge", live)
	}
	return "", nil
}

// cleanupVectorStore drops the project's PgVector database and role, under its
// own bound inside the run's.
func cleanupVectorStore(ctx context.Context, p *Provisioner, projectID int64, cleanup *deletionCleanup) (string, error) {
	if !cleanup.HadVectorStore {
		return "skipped: no vector store", nil
	}
	if p.vectorStore == nil {
		return "", errors.New("this provisioner has no vector store configured")
	}
	dropCtx, cancel := context.WithTimeout(ctx, vectorDropTimeout)
	defer cancel()
	database, err := p.vectorStore.DropProjectVectorStore(dropCtx, projectID, true)
	if database != "" {
		cleanup.VectorDatabase = database
	}
	return "", err
}

// stepError is the joined error a failed step contributes: the sentinel a
// caller maps, the step name, and for the drop the database name.
func stepError(step string, cleanup *deletionCleanup, err error) error {
	switch step {
	case StepArtifactBuckets:
		return fmt.Errorf("%w: %w", ErrArtifactsNotRemoved, err)
	case StepProjectSchema:
		return fmt.Errorf("%w: %w", ErrTenantSchemaNotRemoved, err)
	case StepProjectPgvectorDrop:
		return fmt.Errorf("%w: database %q remains, the cleanup journal retries it: %w",
			ErrVectorStoreNotDropped, cleanup.VectorDatabase, err)
	default:
		return fmt.Errorf("%w: step %s: %w", ErrCleanupIncomplete, step, err)
	}
}

// Deprovision removes a project and everything provisioning created for it. See
// the file comment for the two phases.
//
// The answers:
//
//   - ErrProjectNotFound: no project row (also the loser of two concurrent
//     deletes: it waits for the winner's row lock, then finds the row gone).
//   - ErrProjectWorkActive: refused, nothing changed.
//   - ErrProjectNotRemoved: the deciding transaction failed, nothing changed.
//   - nil: the project is gone and every cleanup step is done.
//   - otherwise: the project is gone, and the joined error names every cleanup
//     step left (ErrArtifactsNotRemoved, ErrTenantSchemaNotRemoved,
//     ErrVectorStoreNotDropped, ErrCleanupIncomplete). The journal retries them.
//
// Result.RollbackSteps reports project_model (the deciding transaction) and then
// every journal step.
//
// THE CONNECTIONS. The deciding transaction is one pool connection and takes no
// other while it holds the project row: everything it reads, it reads through
// the transaction. The cleanup takes one connection at a time per step. No
// session advisory lock and no dedicated connection is held, so two deletes on
// a pool of two connections cannot starve each other.
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
	// no project.
	if p.pool == nil || p.vault == nil {
		return Result{}, errors.New("projectprovisioning: provisioner is not configured")
	}

	cleanup, err := p.decideDeletion(ctx, projectID, config.skipActiveWork)
	if err != nil {
		if errors.Is(err, ErrProjectNotFound) || errors.Is(err, ErrProjectWorkActive) {
			return Result{}, err
		}
		p.logger.ErrorContext(ctx, "project delete did not happen: the deciding transaction failed",
			"project_id", projectID, "err", err)
		status := StepStatus{Step: StepProjectModel, Initialized: true}
		status.setFailed(safeStepMessage(StepProjectModel))
		return Result{ProjectID: projectID, RollbackSteps: []StepStatus{status}}, err
	}

	// The row is gone. The cleanup is DETACHED from the request: a client that
	// hangs up (or a proxy timeout) must not abandon it half way. It gets its own
	// bound instead, and the journal row (claimed by the deciding transaction for
	// that bound) keeps the reconciler off it meanwhile.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	result, cleanupErr := p.runCleanup(runCtx, projectID, cleanup)
	model := StepStatus{Step: StepProjectModel, Initialized: true}
	model.setOK()
	result.RollbackSteps = append([]StepStatus{model}, result.RollbackSteps...)
	return result, cleanupErr
}

// decideDeletion is the one transaction that decides a delete (#1211).
//
//  1. Lock the project row FOR UPDATE; no row is ErrProjectNotFound. A second
//     delete of the same project waits here and then finds the row gone.
//  2. Unless skipActiveWork, count the project's non-terminal execution_jobs. Any
//     is ErrProjectWorkActive: roll back, nothing has changed.
//  3. Record the cleanup: the tenant schema, the system user, the vault, the
//     live buckets, and whether the project has a vector store. A vector-store
//     probe that fails fails the whole decision (ErrProjectNotRemoved, safe to
//     retry): guessing would either leak a database or report a drop that was
//     never needed.
//  4. Insert the journal row, claimed by this delete for one cleanup run.
//  5. Delete every row that references the project, and the project row.
//  6. Commit.
//
// Any failure after step 1 is wrapped in ErrProjectNotRemoved: the transaction
// rolls back and the project is exactly as it was.
func (p *Provisioner) decideDeletion(ctx context.Context, projectID int64, skipActiveWork bool) (*deletionCleanup, error) {
	notRemoved := func(what string, err error) error {
		return fmt.Errorf("%w: %s for project %d: %w", ErrProjectNotRemoved, what, projectID, err)
	}
	transaction, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, notRemoved("begin", err)
	}
	defer func() { _ = transaction.Rollback(context.WithoutCancel(ctx)) }()

	var locked int64
	switch err := transaction.QueryRow(ctx,
		`SELECT id FROM centry.project WHERE id = $1 FOR UPDATE`, projectID,
	).Scan(&locked); {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, ErrProjectNotFound
	case err != nil:
		return nil, notRemoved("lock project", err)
	}

	if !skipActiveWork {
		var active int64
		if err := transaction.QueryRow(ctx, activeWorkSQL, projectID, nonTerminalStates()).Scan(&active); err != nil {
			return nil, notRemoved("count active work", err)
		}
		if active > 0 {
			p.logger.WarnContext(ctx, "project delete refused: work is active",
				"project_id", projectID, "active_runs", active)
			return nil, ErrProjectWorkActive
		}
	}

	cleanup, err := p.recordCleanup(ctx, transaction, projectID)
	if err != nil {
		return nil, notRemoved("record cleanup", err)
	}
	encoded, err := json.Marshal(cleanup)
	if err != nil {
		return nil, notRemoved("encode cleanup", err)
	}
	if _, err := transaction.Exec(ctx, `
INSERT INTO centry.project_deletions (project_id, cleanup, claimed_until)
VALUES ($1, $2::jsonb, now() + make_interval(secs => $3))`,
		projectID, string(encoded), cleanupLease().Seconds(),
	); err != nil {
		return nil, notRemoved("write cleanup journal", err)
	}
	if err := deleteProjectRows(ctx, transaction, projectID); err != nil {
		return nil, notRemoved("delete project rows", err)
	}
	// Committed even if the request was cancelled meanwhile: a commit cut off by
	// the client would leave the caller unsure whether the row went.
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := transaction.Commit(commitCtx); err != nil {
		return nil, notRemoved("commit", err)
	}
	return cleanup, nil
}

// recordCleanup reads, through the deciding transaction, what the project owns
// that the cleanup has to remove.
func (p *Provisioner) recordCleanup(ctx context.Context, transaction pgx.Tx, projectID int64) (*deletionCleanup, error) {
	state := &provisionState{projectID: projectID}
	cleanup := &deletionCleanup{
		TenantSchema: state.tenantSchema(),
		VaultProject: state.projectIDString(),
		Done:         map[string]bool{},
	}

	// removeSystemUser deletes by id. An absent row is not an error: a project
	// provisioned before the step existed has none.
	var systemUserID int64
	switch err := transaction.QueryRow(ctx,
		`SELECT id FROM public.auth_core__user WHERE email = $1`, systemUserEmail(projectID),
	).Scan(&systemUserID); {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return nil, fmt.Errorf("read system user: %w", err)
	default:
		cleanup.SystemUserID = systemUserID
	}

	var bucketsPresent bool
	if err := transaction.QueryRow(ctx,
		`SELECT to_regclass('elitea_storage.buckets') IS NOT NULL`).Scan(&bucketsPresent); err != nil {
		return nil, fmt.Errorf("resolve elitea_storage.buckets: %w", err)
	}
	if bucketsPresent {
		rows, err := transaction.Query(ctx,
			`SELECT name FROM elitea_storage.buckets WHERE project_id = $1 AND deleted_at IS NULL ORDER BY name`, projectID)
		if err != nil {
			return nil, fmt.Errorf("list live buckets: %w", err)
		}
		names, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, fmt.Errorf("list live buckets: %w", err)
		}
		cleanup.Buckets = names
	}

	if p.vectorStore != nil {
		had, err := p.vectorStore.ProjectHasVectorStore(ctx, transaction, projectID)
		if err != nil {
			return nil, fmt.Errorf("probe vector store: %w", err)
		}
		if had {
			cleanup.HadVectorStore = true
			cleanup.VectorDatabase = pgvector.ProjectDatabaseName(projectID)
		}
	}
	return cleanup, nil
}

// runCleanup runs every journal step not yet done, records each one that
// succeeds, and closes the run in the journal row: attempts, last_error, the
// next retry or completed_at, and the lease released. Failures are reported per
// step and joined; none hides another.
func (p *Provisioner) runCleanup(ctx context.Context, projectID int64, cleanup *deletionCleanup) (Result, error) {
	if cleanup.Done == nil {
		cleanup.Done = map[string]bool{}
	}
	result := Result{ProjectID: projectID}
	var (
		failures    []error
		failedSteps []string
	)
	for _, step := range cleanupSteps() {
		status := StepStatus{Step: step.name, Initialized: true}
		if cleanup.Done[step.name] {
			status.setOK()
			result.RollbackSteps = append(result.RollbackSteps, status)
			continue
		}
		note, err := step.run(ctx, p, projectID, cleanup)
		if err != nil {
			p.logger.ErrorContext(ctx, "project delete cleanup step failed; the cleanup journal retries it",
				"step", step.name, "project_id", projectID, "err", err)
			status.setFailed(safeStepMessage(step.name))
			failures = append(failures, stepError(step.name, cleanup, err))
			failedSteps = append(failedSteps, step.name)
			if step.name == StepProjectPgvectorDrop {
				result.VectorDatabase = cleanup.VectorDatabase
			}
		} else {
			status.setOK()
			status.Msg = note
			cleanup.Done[step.name] = true
			p.markCleanupStep(ctx, projectID, step.name)
		}
		result.RollbackSteps = append(result.RollbackSteps, status)
	}
	p.finishCleanupRun(ctx, projectID, failedSteps)
	return result, errors.Join(failures...)
}

// markCleanupStep records one finished step in the journal row. A failure is
// logged only: the step is idempotent, so the worst outcome is that a later run
// repeats it.
func (p *Provisioner) markCleanupStep(ctx context.Context, projectID int64, step string) {
	if _, err := p.pool.Exec(ctx, `
UPDATE centry.project_deletions
SET cleanup = jsonb_set(cleanup, ARRAY['done', $2::text], 'true'::jsonb, true)
WHERE project_id = $1`, projectID, step); err != nil {
		p.logger.WarnContext(ctx, "could not record a finished cleanup step; a later run repeats it",
			"project_id", projectID, "step", step, "err", err)
	}
}

// finishCleanupRun closes one run in the journal row. With no failed step the
// row is complete. Otherwise last_error names the failed steps (names only: a
// raw error can carry SQL or addresses) and next_attempt_at backs off by the
// number of runs so far.
func (p *Provisioner) finishCleanupRun(ctx context.Context, projectID int64, failedSteps []string) {
	var lastError *string
	if len(failedSteps) > 0 {
		message := "steps did not complete: " + fmt.Sprint(failedSteps)
		lastError = &message
	}
	if _, err := p.pool.Exec(ctx, `
UPDATE centry.project_deletions
SET attempts = attempts + 1,
    last_error = $2::text,
    completed_at = CASE WHEN $2::text IS NULL THEN now() END,
    next_attempt_at = CASE WHEN $2::text IS NULL THEN next_attempt_at
        ELSE now() + make_interval(secs => LEAST($3::float8 * power(2, LEAST(attempts, 30)), $4::float8)) END,
    claimed_until = NULL
WHERE project_id = $1`,
		projectID, lastError, deletionRetryBase.Seconds(), deletionRetryMax.Seconds(),
	); err != nil {
		p.logger.WarnContext(ctx, "could not close the cleanup run in the journal; the reconciler retries the row",
			"project_id", projectID, "err", err)
	}
}

// ClaimStaleDeletions claims up to limit incomplete journal rows that are older
// than grace, past their backoff and not leased to a run, and returns their
// project ids in ascending order. The claim is one short transaction: FOR
// UPDATE SKIP LOCKED picks the rows (a row another replica is claiming right
// now is skipped, not waited for) and the same statement stamps a lease on
// them. No lock or connection is held while the caller then runs the cleanup
// (ResumeDeletion), so a claim cannot starve the pool the steps need.
func (p *Provisioner) ClaimStaleDeletions(ctx context.Context, grace time.Duration, limit int) ([]int64, error) {
	if p.pool == nil {
		return nil, errors.New("projectprovisioning: provisioner is not configured")
	}
	if limit <= 0 || grace < 0 {
		return nil, errors.New("projectprovisioning: claim needs a positive limit and a non-negative grace")
	}
	rows, err := p.pool.Query(ctx, `
WITH claimable AS (
    SELECT project_id
    FROM centry.project_deletions
    WHERE completed_at IS NULL
      AND created_at < now() - make_interval(secs => $1)
      AND next_attempt_at <= now()
      AND (claimed_until IS NULL OR claimed_until < now())
    ORDER BY next_attempt_at, project_id
    LIMIT $2
    FOR UPDATE SKIP LOCKED
)
UPDATE centry.project_deletions AS journal
SET claimed_until = now() + make_interval(secs => $3)
FROM claimable
WHERE journal.project_id = claimable.project_id
RETURNING journal.project_id`,
		grace.Seconds(), limit, cleanupLease().Seconds())
	if err != nil {
		return nil, fmt.Errorf("projectprovisioning: claim project deletions: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("projectprovisioning: claim project deletions: %w", err)
	}
	slices.Sort(ids)
	return ids, nil
}

// ResumeDeletion runs the remaining cleanup steps of a journal row the caller
// has claimed (ClaimStaleDeletions). It answers like the second phase of
// Deprovision: nil when every step is done, otherwise the joined leftovers. A
// project with no journal row is ErrProjectNotFound; a complete row is nil.
func (p *Provisioner) ResumeDeletion(ctx context.Context, projectID int64) (Result, error) {
	if p.pool == nil || p.vault == nil {
		return Result{}, errors.New("projectprovisioning: provisioner is not configured")
	}
	var (
		encoded   []byte
		completed *time.Time
	)
	switch err := p.pool.QueryRow(ctx,
		`SELECT cleanup, completed_at FROM centry.project_deletions WHERE project_id = $1`, projectID,
	).Scan(&encoded, &completed); {
	case errors.Is(err, pgx.ErrNoRows):
		return Result{}, ErrProjectNotFound
	case err != nil:
		return Result{}, fmt.Errorf("projectprovisioning: read cleanup journal of project %d: %w", projectID, err)
	}
	if completed != nil {
		return Result{ProjectID: projectID}, nil
	}
	var cleanup deletionCleanup
	if err := json.Unmarshal(encoded, &cleanup); err != nil {
		return Result{}, fmt.Errorf("projectprovisioning: decode cleanup journal of project %d: %w", projectID, err)
	}
	runCtx, cancel := context.WithTimeout(ctx, cleanupTimeout)
	defer cancel()
	return p.runCleanup(runCtx, projectID, &cleanup)
}
