package projectprovisioning

// Project deletion — the symmetric half issue #333 asks for.
//
// The reference is legacy/plugins/projects/api/v2/project.py's `delete_project`,
// which walks the create steps in REVERSE calling each step's `delete`, and
// answers 200 whatever happens. This delete reuses the same `remove` functions
// the create path compensates with, but it is shaped differently (#1211):
//
//  1. ONE TRANSACTION DECIDES (decideDeletion). It locks the project row
//     FOR UPDATE, refuses while the project has work in flight, REVOKES THE
//     PROJECT'S IDENTITY (the vault, the system token and user, the project
//     roles and their memberships, the token bindings), records what is left to
//     clean up in the journal (centry.project_deletions, shared/0160), deletes
//     every row that references the project and the project row itself, and
//     commits. The credentials therefore die atomically with the row. If any of
//     it fails the project is unchanged and fully usable, and the delete answers
//     ErrProjectNotRemoved.
//  2. THE SLOW AND EXTERNAL CLEANUP RUNS FROM THE JOURNAL (runCleanup), after
//     the commit: the artifact bytes (object store), the tenant schema, and the
//     PgVector database (another server). Every step is idempotent and marks
//     itself done in the journal row. The request waits for them only for a
//     short budget (WithCleanupBudget); whatever is left is reported as pending,
//     and ProjectDeletionReconciler (ClaimNextDeletion / ResumeDeletion) retries
//     it with backoff until the journal row is complete.
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
// WHY THE IDENTITY IS NOT IN THE JOURNAL. A credential that outlives the
// deleted row for the length of a retry backoff is a window in which a deleted
// project's system PAT still validates. The identity rows are plain SQL in this
// database, so they go in the deciding transaction and die with the row.
//
// WHY THE TENANT SCHEMA IS STILL IN THE JOURNAL. DROP SCHEMA ... CASCADE takes
// an ACCESS EXCLUSIVE lock on every table in the schema, so inside the deciding
// transaction it would wait behind any session that still reads a tenant table
// (a UI poll, a late worker write) while holding the project row lock, and the
// table files are unlinked at commit. The schema is not a credential: nothing
// can reach it through a deleted project, and cmd/elitea-migrate reads projects
// and not schemas, so it can never see a project row whose schema is missing.
// It stays a journal step (#374).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
// drop itself. bookkeepingTimeout bounds each journal write a run makes, on a
// context of its own so a run that hit its deadline still records what it did.
// leaseMargin is added to a run's bound to make its lease. Variables so a test
// can shrink them.
var (
	cleanupTimeout     = 5 * time.Minute
	vectorDropTimeout  = 2 * time.Minute
	bookkeepingTimeout = 10 * time.Second
	leaseMargin        = time.Minute
)

// cleanupLease is how long a run bounded by bound owns a journal row: the bound
// plus a margin. A run that dies leaves the lease to expire, and the reconciler
// picks the row up again after it.
func cleanupLease(bound time.Duration) time.Duration { return bound + leaseMargin }

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
	// budget is how long the call waits for the cleanup after the decision
	// commits. Zero with handOff false means the full cleanupTimeout.
	budget  time.Duration
	handOff bool
}

// SkipActiveWorkCheck makes Deprovision skip the count of non-terminal work. For
// the one caller that deletes a project it knows cannot be running anything: the
// personal-project ensurer repairing a project whose creation never finished
// (create_success=false). The project's job rows are deleted with it, as for
// any delete. An explicit DELETE request never passes it.
func SkipActiveWorkCheck() DeprovisionOption {
	return func(c *deprovisionConfig) { c.skipActiveWork = true }
}

// WithCleanupBudget bounds how long Deprovision waits for the journal's cleanup
// steps after the decision commits. When the budget runs out, the steps still
// undone are reported in Result.Pending and the journal (the reconciler)
// finishes them. The default, with no option, is to wait for the whole run
// (cleanupTimeout).
func WithCleanupBudget(budget time.Duration) DeprovisionOption {
	return func(c *deprovisionConfig) { c.budget = budget }
}

// HandOffCleanup makes Deprovision return as soon as the decision commits,
// without running any cleanup step: every step is left to the journal. For the
// login path, which must not wait for an object store or a vector server.
func HandOffCleanup() DeprovisionOption {
	return func(c *deprovisionConfig) { c.handOff = true }
}

// deletionCleanup is the journal record: what the deciding transaction found is
// left to clean up, and which cleanup steps are done. It is everything a later
// run needs, because by then the project row (and with it every way to
// re-derive these facts) is gone.
//
// It holds only what a step reads. The tenant schema name derives from the
// project id; the identity rows are gone with the decision; the bucket rows are
// the artifact step's own handle and it lists them itself.
type deletionCleanup struct {
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
// bytes go first (the bucket rows are their only handle), the tenant schema and
// the PgVector database after.
//
// The identity steps (the vault, the system token and user, the project roles
// and the memberships that cascade from them, the token bindings) are not here:
// they ran in the deciding transaction. The PgVector configuration row lives in
// the tenant schema and goes with it.
func cleanupSteps() []cleanupStep {
	return []cleanupStep{
		{name: StepArtifactBuckets, run: cleanupArtifactBuckets},
		{name: StepProjectSchema, run: withState(removeProjectSchema)},
		{name: StepProjectPgvectorDrop, run: cleanupVectorStore},
	}
}

// decisionSteps are the steps the deciding transaction performs, in the order
// the response reports them. They are reported OK once the decision commits.
func decisionSteps() []string {
	return []string{StepProjectSecrets, StepSystemToken, StepSystemUser, StepProjectPermissions}
}

// withState adapts a create step's remove function to a journal step.
func withState(remove func(context.Context, *Provisioner, *provisionState) error) func(context.Context, *Provisioner, int64, *deletionCleanup) (string, error) {
	return func(ctx context.Context, p *Provisioner, projectID int64, _ *deletionCleanup) (string, error) {
		return "", remove(ctx, p, &provisionState{projectID: projectID})
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

// quietRetry marks a step failure the journal retries and the client is not
// told about: the leftover of a database the project was never recorded as
// having. It is logged, not reported (see cleanupVectorStore).
type quietRetry struct{ err error }

func (q quietRetry) Error() string { return q.err.Error() }
func (q quietRetry) Unwrap() error { return q.err }

// cleanupVectorStore drops the project's PgVector database and role, under its
// own bound inside the run's.
//
// With a vector store configured on the provisioner the drop is ALWAYS
// attempted, whatever the configuration-row probe recorded: it is idempotent,
// and a database can exist without the row (a provisioning run that died
// between the two). What the probe decides is how a failure is told:
//
//   - recorded store, drop failed: reported (ErrVectorStoreNotDropped, the
//     database named), the journal retries it.
//   - no recorded store, drop failed (the server is unreachable, say): the
//     journal retries it, and it is logged, not reported. The client is not told
//     a database was left that nothing says existed.
//   - no vector store configured: a skip when none was recorded, and a leak,
//     reported, when one was.
func cleanupVectorStore(ctx context.Context, p *Provisioner, projectID int64, cleanup *deletionCleanup) (string, error) {
	if p.vectorStore == nil {
		if !cleanup.HadVectorStore {
			return "skipped: no vector store configured", nil
		}
		return "", errors.New("this provisioner has no vector store configured")
	}
	dropCtx, cancel := context.WithTimeout(ctx, vectorDropTimeout)
	defer cancel()
	database, err := p.vectorStore.DropProjectVectorStore(dropCtx, projectID, cleanup.HadVectorStore)
	if cleanup.HadVectorStore && database != "" {
		cleanup.VectorDatabase = database
	}
	if err != nil && !cleanup.HadVectorStore {
		return "", quietRetry{err: err}
	}
	if err == nil && database == "" {
		return "skipped: no vector store", nil
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
//   - nil: the project is gone and its identity is revoked. Result.Pending names
//     the cleanup steps still to run (all of them after HandOffCleanup, those the
//     CleanupBudget did not reach otherwise); empty means every step is done.
//   - otherwise: the project is gone, and the joined error names every cleanup
//     step that ran and failed (ErrArtifactsNotRemoved, ErrTenantSchemaNotRemoved,
//     ErrVectorStoreNotDropped, ErrCleanupIncomplete). They are in Result.Pending
//     too, and the journal retries them.
//
// Result.RollbackSteps reports project_model (the deciding transaction), the
// identity steps it performed, and then every journal step.
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

	// The run's bound: the budget when one was given, the full timeout otherwise.
	// The lease the journal row carries is this bound plus a margin.
	bound := cleanupTimeout
	if config.budget > 0 && config.budget < bound {
		bound = config.budget
	}
	cleanup, err := p.decideDeletion(ctx, projectID, config.skipActiveWork, !config.handOff, bound)
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

	decided := []StepStatus{}
	model := StepStatus{Step: StepProjectModel, Initialized: true}
	model.setOK()
	decided = append(decided, model)
	for _, name := range decisionSteps() {
		status := StepStatus{Step: name, Initialized: true}
		status.setOK()
		decided = append(decided, status)
	}

	var (
		result     Result
		cleanupErr error
	)
	if config.handOff {
		// The journal row was written without a lease: the reconciler takes it.
		result = Result{ProjectID: projectID}
		for _, step := range cleanupSteps() {
			result.Pending = append(result.Pending, step.name)
			result.RollbackSteps = append(result.RollbackSteps, StepStatus{Step: step.name})
		}
	} else {
		// The row is gone. The cleanup is DETACHED from the request: a client that
		// hangs up (or a proxy timeout) must not abandon it half way. It gets its
		// own bound instead, and the journal row (leased by the deciding
		// transaction for that bound) keeps the reconciler off it meanwhile.
		runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bound)
		defer cancel()
		result, cleanupErr = p.runCleanup(runCtx, projectID, cleanup)
	}
	result.RollbackSteps = append(decided, result.RollbackSteps...)
	return result, cleanupErr
}

// decideDeletion is the one transaction that decides a delete (#1211).
//
//  1. Lock the project row FOR UPDATE; no row is ErrProjectNotFound. A second
//     delete of the same project waits here and then finds the row gone.
//  2. Unless skipActiveWork, count the project's non-terminal execution_jobs. Any
//     is ErrProjectWorkActive: roll back, nothing has changed.
//  3. Record whether the project has a vector store. A probe that fails fails
//     the whole decision (ErrProjectNotRemoved, safe to retry): guessing would
//     either leak a database or report a drop that was never needed.
//  4. Insert the journal row. With lease it is leased to this delete for one
//     run of the given bound; without, the reconciler may take it at once.
//  5. Revoke the identity: the vault, the system token and user, the project
//     roles (and the memberships that cascade from them) and the token
//     bindings. They die atomically with the row.
//  6. Delete every row that references the project, and the project row.
//  7. Commit.
//
// Any failure after step 1 is wrapped in ErrProjectNotRemoved: the transaction
// rolls back and the project is exactly as it was. A commit that returns an
// error is the one ambiguous case (the server may have committed before the
// connection broke); it is settled by asking on a fresh connection.
func (p *Provisioner) decideDeletion(ctx context.Context, projectID int64, skipActiveWork, lease bool, bound time.Duration) (*deletionCleanup, error) {
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
	// A journal row for this id that is COMPLETE belongs to an earlier project
	// that had the same id (a restored database, a test that resets the
	// sequence): reopen it. One that is INCOMPLETE would mean its cleanup is
	// still owed for a project whose row exists again, which cannot happen (the
	// row went in the transaction that wrote the journal); refuse rather than
	// overwrite a record of work not done.
	leaseSeconds := 0.0
	if lease {
		leaseSeconds = cleanupLease(bound).Seconds()
	}
	var journaled int64
	switch err := transaction.QueryRow(ctx, `
INSERT INTO centry.project_deletions (project_id, cleanup, claimed_until)
VALUES ($1, $2::jsonb, CASE WHEN $3::float8 > 0 THEN now() + make_interval(secs => $3::float8) END)
ON CONFLICT (project_id) DO UPDATE
SET cleanup = EXCLUDED.cleanup,
    created_at = now(),
    attempts = 0,
    last_error = NULL,
    next_attempt_at = now(),
    claimed_until = EXCLUDED.claimed_until,
    completed_at = NULL
WHERE centry.project_deletions.completed_at IS NOT NULL
RETURNING project_id`,
		projectID, string(encoded), leaseSeconds,
	).Scan(&journaled); {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, notRemoved("write cleanup journal",
			errors.New("an incomplete cleanup journal row already exists for this project id; it must finish before the id can be deleted again"))
	case err != nil:
		return nil, notRemoved("write cleanup journal", err)
	}

	if err := p.revokeIdentity(ctx, transaction, projectID); err != nil {
		return nil, notRemoved("revoke identity", err)
	}
	if err := deleteProjectRows(ctx, transaction, projectID); err != nil {
		return nil, notRemoved("delete project rows", err)
	}
	// Committed even if the request was cancelled meanwhile: a commit cut off by
	// the client would leave the caller unsure whether the row went.
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	commit := p.commit
	if commit == nil {
		commit = func(ctx context.Context, tx pgx.Tx) error { return tx.Commit(ctx) }
	}
	if err := commit(commitCtx, transaction); err != nil {
		// The answer to a failed commit is not the error: the server may have
		// applied the transaction before the connection failed. Ask a fresh
		// connection. Row gone and journal present is a delete that happened.
		committed, checkErr := p.deletionCommitted(ctx, projectID)
		switch {
		case checkErr != nil:
			return nil, notRemoved("commit (outcome unknown: "+checkErr.Error()+")", err)
		case !committed:
			return nil, notRemoved("commit", err)
		}
		p.logger.WarnContext(ctx, "the commit of a project delete returned an error, but the delete is in place",
			"project_id", projectID, "err", err)
	}
	return cleanup, nil
}

// deletionCommitted reports, on a connection of its own, whether a delete of the
// project took effect: the project row is gone AND its journal row exists. The
// two are written in one transaction, so seeing both is seeing the commit.
func (p *Provisioner) deletionCommitted(ctx context.Context, projectID int64) (bool, error) {
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bookkeepingTimeout)
	defer cancel()
	var rowExists, journalExists bool
	if err := p.pool.QueryRow(checkCtx, `
SELECT EXISTS (SELECT 1 FROM centry.project WHERE id = $1),
       EXISTS (SELECT 1 FROM centry.project_deletions WHERE project_id = $1)`,
		projectID).Scan(&rowExists, &journalExists); err != nil {
		return false, err
	}
	return !rowExists && journalExists, nil
}

// revokeIdentity removes, through the deciding transaction, every credential and
// membership the project owns: the vault, the system token and user, the token
// bindings and the project roles (the assignments cascade from the role and from
// the user). They are the same functions the create path compensates with. A
// project provisioned before the system user existed has none; an absent row is
// not an error.
func (p *Provisioner) revokeIdentity(ctx context.Context, transaction pgx.Tx, projectID int64) error {
	state := &provisionState{projectID: projectID}
	if err := p.vault.RemoveProjectVaultTx(ctx, transaction, state.projectIDString()); err != nil {
		return fmt.Errorf("remove project secrets vault: %w", err)
	}
	var systemUserID int64
	switch err := transaction.QueryRow(ctx,
		`SELECT id FROM public.auth_core__user WHERE email = $1`, systemUserEmail(projectID),
	).Scan(&systemUserID); {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return fmt.Errorf("read system user: %w", err)
	}
	if err := deleteSystemToken(ctx, transaction, systemUserID); err != nil {
		return err
	}
	if err := deleteSystemUser(ctx, transaction, systemUserID); err != nil {
		return err
	}
	return deleteProjectPermissions(ctx, transaction, projectID)
}

// recordCleanup reads, through the deciding transaction, what the cleanup
// journal has to remember: whether the project has a vector store.
func (p *Provisioner) recordCleanup(ctx context.Context, transaction pgx.Tx, projectID int64) (*deletionCleanup, error) {
	cleanup := &deletionCleanup{Done: map[string]bool{}}
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
//
// The journal writes use a context of their own (bookkeepingContext), so a run
// whose context ended at its deadline still records the steps it finished and
// its backoff. A step not reached because the context ended is pending, not
// failed: it has no status of its own and no error, and the journal row stays
// open for the next run.
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
		if ctx.Err() != nil {
			// The run's bound is spent: leave the step to the journal.
			result.RollbackSteps = append(result.RollbackSteps, StepStatus{Step: step.name})
			result.Pending = append(result.Pending, step.name)
			failedSteps = append(failedSteps, step.name)
			continue
		}
		note, err := step.run(ctx, p, projectID, cleanup)
		var quiet quietRetry
		switch {
		case err == nil:
			status.setOK()
			status.Msg = note
			cleanup.Done[step.name] = true
			p.markCleanupStep(ctx, projectID, step.name)
		case errors.As(err, &quiet):
			// Retried by the journal, not reported: see cleanupVectorStore.
			p.logger.ErrorContext(ctx, "project delete cleanup step failed for a store the project was not recorded as having; the cleanup journal retries it",
				"step", step.name, "project_id", projectID, "err", err)
			status.setOK()
			status.Msg = "skipped: no vector store recorded"
			failedSteps = append(failedSteps, step.name)
		default:
			p.logger.ErrorContext(ctx, "project delete cleanup step failed; the cleanup journal retries it",
				"step", step.name, "project_id", projectID, "err", err)
			status.setFailed(safeStepMessage(step.name))
			failures = append(failures, stepError(step.name, cleanup, err))
			failedSteps = append(failedSteps, step.name)
			result.Pending = append(result.Pending, step.name)
			if step.name == StepProjectPgvectorDrop {
				result.VectorDatabase = cleanup.VectorDatabase
			}
		}
		result.RollbackSteps = append(result.RollbackSteps, status)
	}
	p.finishCleanupRun(ctx, projectID, failedSteps)
	return result, errors.Join(failures...)
}

// bookkeepingContext is the context for one journal write: detached from the
// run's (which may have ended at its deadline) with a short bound of its own.
func bookkeepingContext(runCtx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(runCtx), bookkeepingTimeout)
}

// markCleanupStep records one finished step in the journal row. A failure is
// logged only: the step is idempotent, so the worst outcome is that a later run
// repeats it.
func (p *Provisioner) markCleanupStep(runCtx context.Context, projectID int64, step string) {
	ctx, cancel := bookkeepingContext(runCtx)
	defer cancel()
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
func (p *Provisioner) finishCleanupRun(runCtx context.Context, projectID int64, failedSteps []string) {
	ctx, cancel := bookkeepingContext(runCtx)
	defer cancel()
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

// ClaimNextDeletion claims ONE incomplete journal row that is older than grace,
// past its backoff and not leased, and leases it for one cleanup run
// (cleanupTimeout plus a margin). ok is false when there is none.
//
// One row per claim, and the claim is its own short transaction: FOR UPDATE
// SKIP LOCKED picks the row (a row another replica is claiming right now is
// skipped, not waited for) and the same statement stamps the lease. The caller
// works that row (ResumeDeletion), then claims the next. A lease is therefore
// held only by a row that is being worked, never by one waiting in a local
// queue behind slow neighbours, and no lock or connection is held while the
// steps run.
func (p *Provisioner) ClaimNextDeletion(ctx context.Context, grace time.Duration) (projectID int64, ok bool, err error) {
	if p.pool == nil {
		return 0, false, errors.New("projectprovisioning: provisioner is not configured")
	}
	if grace < 0 {
		return 0, false, errors.New("projectprovisioning: claim needs a non-negative grace")
	}
	switch err := p.pool.QueryRow(ctx, `
WITH claimable AS (
    SELECT project_id
    FROM centry.project_deletions
    WHERE completed_at IS NULL
      AND created_at < now() - make_interval(secs => $1)
      AND next_attempt_at <= now()
      AND (claimed_until IS NULL OR claimed_until < now())
    ORDER BY next_attempt_at, project_id
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
UPDATE centry.project_deletions AS journal
SET claimed_until = now() + make_interval(secs => $2)
FROM claimable
WHERE journal.project_id = claimable.project_id
RETURNING journal.project_id`,
		grace.Seconds(), cleanupLease(cleanupTimeout).Seconds(),
	).Scan(&projectID); {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("projectprovisioning: claim project deletion: %w", err)
	}
	return projectID, true, nil
}

// ResumeDeletion runs the remaining cleanup steps of a journal row the caller
// has claimed (ClaimNextDeletion). It answers like the second phase of
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
