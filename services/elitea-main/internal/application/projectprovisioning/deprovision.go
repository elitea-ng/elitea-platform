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
//     itself done in the journal row. The run is DETACHED from the request: it
//     starts in a goroutine of its own, under its own deadline (cleanupTimeout)
//     and holding the row's lease, and the request waits for it only for a short
//     budget (WithCleanupBudget). When the budget runs out the request answers
//     with the steps still pending and the run goes on, so "cleanup continues in
//     the background" is true even with the reconciler off. The run is bound to
//     the process lifecycle (WithLifecycleContext), not to the request: a
//     shutdown stops it and releases the lease WITHOUT counting an attempt or
//     growing the backoff. ProjectDeletionReconciler (ClaimNextDeletion /
//     ResumeDeletion) retries whatever a run did not finish, with backoff, until
//     the journal row is complete.
//
// AN ID CAN COME BACK. A project id is normally never reused, but explicit ids
// and restored databases make it possible, and the journal is the only record of
// work still owed for an id. Two guards keep the two apart. Before EVERY
// destructive step runCleanup checks, on its own connection, that no
// centry.project row exists for the id; if one does, the journal row is closed
// as superseded (cleanup.superseded) and nothing is touched. And Provision
// refuses to create a project whose id has an incomplete journal row.
//
// LEFTOVERS WITH NO JOURNAL ROW. A project deleted by an older build, or by a
// delete that died before the journal existed, can have no row and still have its
// tenant schema, live buckets or PgVector database. A DELETE that finds no
// project row looks for those leftovers; if any exist and no journal row does, it
// adopts them (inserts the journal row) and cleans up as usual. Nothing left and
// no journal row is the 404.
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
// THE TENANT ROWS ARE CASCADED IN THE DECISION, THE EMPTY SCHEMA LATER. The
// tenant tables carry owner_id foreign keys to centry.project(id) ON DELETE
// CASCADE (tenant 0131), so deleting the project row deletes the tenant's rows
// in the deciding transaction. That cascade takes row locks on the tenant rows
// and waits behind a session that holds one (a UI write, a late worker write)
// while this transaction holds the project row lock, so the transaction runs
// under a lock_timeout and a statement_timeout: a blocked or slow cascade fails
// the decision cleanly (ErrProjectNotRemoved), the project is unchanged and the
// delete can be retried. What stays for the journal is the schema OBJECT:
// DROP SCHEMA ... CASCADE takes an ACCESS EXCLUSIVE lock on every table in it,
// which would wait behind any session that still reads a tenant table, and the
// table files are unlinked at commit. The empty schema is not a credential:
// nothing can reach it through a deleted project, and cmd/elitea-migrate reads
// projects and not schemas, so it can never see a project row whose schema is
// missing. It stays a journal step (#374).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
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

// ErrProjectIDInCleanup reports a Provision that drew a project id whose delete
// has not finished cleaning up. The create is refused and rolled back; the id is
// consumed and the next create draws another.
var ErrProjectIDInCleanup = errors.New("projectprovisioning: project id still has cleanup to finish")

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

// Defaults of the options that tune a delete (WithQuietDropLimit,
// WithDecisionTimeouts).
const (
	// DefaultQuietDropLimit is how many times a drop that fails for a store the
	// project was never recorded as having is attempted before the journal gives
	// up on it.
	DefaultQuietDropLimit = 5
	// DefaultDecisionLockTimeout bounds a lock wait inside the deciding
	// transaction once it holds the project row (the tenant cascade).
	DefaultDecisionLockTimeout = 5 * time.Second
	// DefaultDecisionStatementTimeout bounds each statement of the deciding
	// transaction.
	DefaultDecisionStatementTimeout = 60 * time.Second
)

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
	// commits. Zero with handOff false means it waits for the whole run.
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
// steps after the decision commits. The run itself is not bounded by it: when the
// budget runs out Deprovision returns the steps still undone in Result.Pending
// and the run continues in the background, under its own deadline
// (cleanupTimeout). The reconciler finishes whatever that run leaves. The
// default, with no option, is to wait for the whole run.
func WithCleanupBudget(budget time.Duration) DeprovisionOption {
	return func(c *deprovisionConfig) { c.budget = budget }
}

// HandOffCleanup makes Deprovision return as soon as the decision commits. The
// cleanup run is started in the background exactly as for a budget that has
// already run out; every step is reported pending. For the login path, which
// must not wait for an object store or a vector server.
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
	// QuietDropFailures counts the drops that failed for a store the project was
	// not recorded as having; at the limit the journal gives up (Notes).
	QuietDropFailures int `json:"quiet_drop_failures,omitempty"`
	// Notes holds a message per step that was marked done without doing its
	// work ("gave up: ...").
	Notes map[string]string `json:"notes,omitempty"`
	// Superseded is set when the journal row was closed because a project with
	// the id exists again; nothing was cleaned.
	Superseded bool `json:"superseded,omitempty"`
}

// cleanupStep is one idempotent journal step. note is an optional message for a
// step that succeeded without doing anything ("skipped: ...").
type cleanupStep struct {
	name string
	// notDone is the sentinel a failure of the step is reported as.
	notDone error
	run     func(ctx context.Context, p *Provisioner, projectID int64, cleanup *deletionCleanup) (note string, err error)
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
		{name: StepArtifactBuckets, notDone: ErrArtifactsNotRemoved, run: cleanupArtifactBuckets},
		{name: StepProjectSchema, notDone: ErrTenantSchemaNotRemoved, run: withState(removeProjectSchema)},
		{name: StepProjectPgvectorDrop, notDone: ErrVectorStoreNotDropped, run: cleanupVectorStore},
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
//     a database was left that nothing says existed. The retries are bounded
//     (WithQuietDropLimit, five by default): after that the step is marked done
//     with the note "gave up: no vector store recorded" and a warning is logged,
//     so a store that was never there cannot keep a journal row open for ever.
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

// failure is the joined error a failed step contributes: the sentinel a caller
// maps, and for the drop the database name.
func (step cleanupStep) failure(cleanup *deletionCleanup, err error) error {
	if step.name == StepProjectPgvectorDrop {
		return fmt.Errorf("%w: database %q remains, the cleanup journal retries it: %w",
			step.notDone, cleanup.VectorDatabase, err)
	}
	return fmt.Errorf("%w: %w", step.notDone, err)
}

// Deprovision removes a project and everything provisioning created for it. See
// the file comment for the two phases.
//
// The answers:
//
//   - ErrProjectNotFound: no project row, no journal row for the id, and nothing
//     left over (also the loser of two concurrent deletes: it waits for the
//     winner's row lock, then finds the row gone and the journal row present).
//   - ErrProjectWorkActive: refused, nothing changed.
//   - ErrProjectNotRemoved: the deciding transaction failed (a blocked or slow
//     cascade included), nothing changed.
//   - nil: the project is gone and its identity is revoked. Result.Pending names
//     the cleanup steps still to run (all of them after HandOffCleanup, those the
//     budget did not reach otherwise); empty means every step is done. Pending
//     steps are being run in the background.
//   - otherwise: the project is gone, and the joined error names every cleanup
//     step that ran and failed (ErrArtifactsNotRemoved, ErrTenantSchemaNotRemoved,
//     ErrVectorStoreNotDropped). They are in Result.Pending too, and the journal
//     retries them.
//
// A project row that is already gone but whose tenant schema, live buckets or
// PgVector database remain, with no journal row, is ADOPTED: a journal row is
// written for the leftovers and they are cleaned as for any delete.
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

	cleanup, err := p.decideDeletion(ctx, projectID, config.skipActiveWork)
	adopted := false
	if errors.Is(err, ErrProjectNotFound) {
		var adoptErr error
		cleanup, adoptErr = p.adoptLeftovers(ctx, projectID)
		switch {
		case adoptErr != nil:
			p.logger.ErrorContext(ctx, "project delete: the project row is gone and the check for its leftovers failed",
				"project_id", projectID, "err", adoptErr)
			status := StepStatus{Step: StepProjectModel, Initialized: true}
			status.setFailed(safeStepMessage(StepProjectModel))
			return Result{ProjectID: projectID, RollbackSteps: []StepStatus{status}},
				fmt.Errorf("%w: look for leftovers of project %d: %w", ErrProjectNotRemoved, projectID, adoptErr)
		case cleanup == nil:
			return Result{}, ErrProjectNotFound
		}
		adopted, err = true, nil
	}
	if err != nil {
		if errors.Is(err, ErrProjectWorkActive) {
			return Result{}, err
		}
		p.logger.ErrorContext(ctx, "project delete did not happen: the deciding transaction failed",
			"project_id", projectID, "err", err)
		status := StepStatus{Step: StepProjectModel, Initialized: true}
		status.setFailed(safeStepMessage(StepProjectModel))
		return Result{ProjectID: projectID, RollbackSteps: []StepStatus{status}}, err
	}

	decided := []StepStatus{}
	if !adopted {
		model := StepStatus{Step: StepProjectModel, Initialized: true}
		model.setOK()
		decided = append(decided, model)
		for _, name := range decisionSteps() {
			status := StepStatus{Step: name, Initialized: true}
			status.setOK()
			decided = append(decided, status)
		}
	}

	// The row is gone. The cleanup runs DETACHED from the request: a client that
	// hangs up (or a proxy timeout) must not abandon it half way, and neither
	// must the request's budget. It has a deadline of its own and holds the
	// journal row's lease meanwhile.
	// A client that hangs up does not end the wait early: the budget does (and
	// the run, which has a bound of its own), so the answer is never a guess.
	finished := p.startCleanup(projectID, cleanup)

	var (
		result     Result
		cleanupErr error
	)
	if config.handOff {
		result = pendingResult(projectID, nil)
	} else {
		var expiry <-chan time.Time
		if config.budget > 0 {
			timer := time.NewTimer(config.budget)
			defer timer.Stop()
			expiry = timer.C
		}
		select {
		case outcome := <-finished:
			result, cleanupErr = outcome.result, outcome.err
		case <-expiry:
			result = p.pendingFromJournal(ctx, projectID)
		}
	}
	result.RollbackSteps = append(decided, result.RollbackSteps...)
	return result, cleanupErr
}

// cleanupOutcome is what a detached run reports to the request that started it.
type cleanupOutcome struct {
	result Result
	err    error
}

// startCleanup starts one cleanup run in a goroutine of its own and returns the
// channel its outcome arrives on (buffered: a request that stopped waiting does
// not strand the run). The run derives from the provisioner's lifecycle context,
// not from any request.
func (p *Provisioner) startCleanup(projectID int64, cleanup *deletionCleanup) <-chan cleanupOutcome {
	finished := make(chan cleanupOutcome, 1)
	p.inflight.Add(1)
	go func() {
		defer p.inflight.Done()
		result, _, err := p.runCleanup(p.lifecycle, projectID, cleanup)
		finished <- cleanupOutcome{result: result, err: err}
	}()
	return finished
}

// pendingResult builds the answer for a delete whose cleanup is still running:
// the steps in done are reported OK, every other step pending.
func pendingResult(projectID int64, done map[string]bool) Result {
	result := Result{ProjectID: projectID}
	for _, step := range cleanupSteps() {
		if done[step.name] {
			status := StepStatus{Step: step.name, Initialized: true}
			status.setOK()
			result.RollbackSteps = append(result.RollbackSteps, status)
			continue
		}
		result.Pending = append(result.Pending, step.name)
		result.RollbackSteps = append(result.RollbackSteps, StepStatus{Step: step.name})
	}
	return result
}

// pendingFromJournal answers a request whose budget ran out while the run goes
// on: it reads which steps the journal records as done. A row that is complete
// has nothing pending; a read that fails reports every step pending, which is
// true of anything not yet recorded.
func (p *Provisioner) pendingFromJournal(runCtx context.Context, projectID int64) Result {
	ctx, cancel := bookkeepingContext(runCtx)
	defer cancel()
	var (
		encoded   []byte
		completed *time.Time
	)
	if err := p.pool.QueryRow(ctx,
		`SELECT cleanup, completed_at FROM centry.project_deletions WHERE project_id = $1`, projectID,
	).Scan(&encoded, &completed); err != nil {
		return pendingResult(projectID, nil)
	}
	var cleanup deletionCleanup
	if err := json.Unmarshal(encoded, &cleanup); err != nil {
		return pendingResult(projectID, nil)
	}
	done := cleanup.Done
	if completed != nil {
		done = map[string]bool{}
		for _, step := range cleanupSteps() {
			done[step.name] = true
		}
	}
	return pendingResult(projectID, done)
}

// decideDeletion is the one transaction that decides a delete (#1211).
//
//  1. Bound the transaction: statement_timeout from here, lock_timeout once the
//     project row is held. The cascade of step 6 can wait behind a session that
//     holds a tenant row; it must fail the decision and not hold the project row
//     lock for ever.
//  2. Lock the project row FOR UPDATE; no row is ErrProjectNotFound. A second
//     delete of the same project waits here and then finds the row gone.
//  3. Unless skipActiveWork, count the project's non-terminal execution_jobs. Any
//     is ErrProjectWorkActive: roll back, nothing has changed.
//  4. Record whether the project has a vector store. A probe that fails fails
//     the whole decision (ErrProjectNotRemoved, safe to retry): guessing would
//     either leak a database or report a drop that was never needed.
//  5. Insert the journal row, leased to the run that follows.
//  6. Revoke the identity: the vault, the system token and user, the project
//     roles (and the memberships that cascade from them) and the token
//     bindings. They die atomically with the row.
//  7. Delete every row that references the project, and the project row (which
//     cascades through the tenant tables' owner_id foreign keys).
//  8. Commit.
//
// Any failure after step 1 is wrapped in ErrProjectNotRemoved: the transaction
// rolls back and the project is exactly as it was. A commit that returns an
// error is the one ambiguous case (the server may have committed before the
// connection broke); it is settled by asking on a fresh connection.
func (p *Provisioner) decideDeletion(ctx context.Context, projectID int64, skipActiveWork bool) (*deletionCleanup, error) {
	notRemoved := func(what string, err error) error {
		return fmt.Errorf("%w: %s for project %d: %w", ErrProjectNotRemoved, what, projectID, err)
	}
	transaction, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, notRemoved("begin", err)
	}
	defer func() { _ = transaction.Rollback(context.WithoutCancel(ctx)) }()

	// set_config(..., true) is SET LOCAL with a bind parameter: both settings end
	// with the transaction.
	if _, err := transaction.Exec(ctx, `SELECT set_config('statement_timeout', $1, true)`,
		timeoutSetting(p.decisionStatementTimeout)); err != nil {
		return nil, notRemoved("bound statements", err)
	}

	var locked int64
	switch err := transaction.QueryRow(ctx,
		`SELECT id FROM centry.project WHERE id = $1 FOR UPDATE`, projectID,
	).Scan(&locked); {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, ErrProjectNotFound
	case err != nil:
		return nil, notRemoved("lock project", err)
	}
	// After the project row lock, so a second delete that waits for the first
	// is bounded by the statement timeout and then finds the row gone, rather
	// than failing on the short lock bound.
	if _, err := transaction.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`,
		timeoutSetting(p.decisionLockTimeout)); err != nil {
		return nil, notRemoved("bound lock waits", err)
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
	// still owed for a project whose row exists again; refuse rather than
	// overwrite a record of work not done (the cleanup supersedes such a row
	// itself when it finds the project).
	//
	// The lease is stamped with clock_timestamp(), not now(): now() is the
	// transaction's start, and a decision that waited on a lock would hand the
	// run a lease that already ran down. The run restamps it when it starts.
	var journaled int64
	switch err := transaction.QueryRow(ctx, `
INSERT INTO centry.project_deletions (project_id, cleanup, created_at, next_attempt_at, claimed_until)
VALUES ($1, $2::jsonb, clock_timestamp(), clock_timestamp(),
        clock_timestamp() + make_interval(secs => $3::float8))
ON CONFLICT (project_id) DO UPDATE
SET cleanup = EXCLUDED.cleanup,
    created_at = EXCLUDED.created_at,
    attempts = 0,
    last_error = NULL,
    next_attempt_at = EXCLUDED.next_attempt_at,
    claimed_until = EXCLUDED.claimed_until,
    completed_at = NULL
WHERE centry.project_deletions.completed_at IS NOT NULL
RETURNING project_id`,
		projectID, string(encoded), cleanupLease(cleanupTimeout).Seconds(),
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

// timeoutSetting renders a duration as a Postgres timeout setting in
// milliseconds (at least 1, since 0 would disable the bound).
func timeoutSetting(timeout time.Duration) string {
	return strconv.FormatInt(max(timeout.Milliseconds(), 1), 10) + "ms"
}

// adoptLeftovers handles a DELETE that found no project row. It looks for what a
// delete would still have to clean up for that id: the tenant schema, live
// artifact buckets and, when the vector store can be asked, the PgVector
// database. A journal row for the id (complete or not) means the delete already
// happened and the cleanup is the journal's: not adopted. With leftovers and no
// journal row it inserts one, recording whether the database exists, and returns
// the record for the cleanup run. It returns nil when there is nothing to adopt.
//
// The insert is conditional on the project row still being absent and on no
// journal row existing, so it cannot cross a concurrent Provision or delete: a
// project that appears afterwards finds the journal row incomplete and the
// cleanup supersedes it (see runCleanup).
func (p *Provisioner) adoptLeftovers(ctx context.Context, projectID int64) (*deletionCleanup, error) {
	var journaled bool
	if err := p.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM centry.project_deletions WHERE project_id = $1)`, projectID,
	).Scan(&journaled); err != nil {
		return nil, fmt.Errorf("read cleanup journal: %w", err)
	}
	if journaled {
		return nil, nil
	}

	var schema bool
	if err := p.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname = $1)`,
		(&provisionState{projectID: projectID}).tenantSchema(),
	).Scan(&schema); err != nil {
		return nil, fmt.Errorf("look for the tenant schema: %w", err)
	}
	live, err := p.liveBucketCount(ctx, projectID)
	if err != nil {
		return nil, err
	}
	database := false
	if probe, ok := p.vectorStore.(VectorDatabaseProbe); ok {
		exists, checkable, probeErr := probe.ProjectVectorDatabaseExists(ctx, projectID)
		switch {
		case probeErr != nil:
			// An unreachable vector server must not turn every delete of an id
			// that never existed into an error; the schema and the buckets
			// still decide. A database found later is the orphan command's.
			p.logger.WarnContext(ctx, "could not look for the vector database of a project with no row",
				"project_id", projectID, "err", probeErr)
		case checkable:
			database = exists
		}
	}
	if !schema && live == 0 && !database {
		return nil, nil
	}

	cleanup := &deletionCleanup{Done: map[string]bool{}, HadVectorStore: database}
	if database {
		cleanup.VectorDatabase = pgvector.ProjectDatabaseName(projectID)
	}
	encoded, err := json.Marshal(cleanup)
	if err != nil {
		return nil, fmt.Errorf("encode cleanup: %w", err)
	}
	var inserted int64
	switch err := p.pool.QueryRow(ctx, `
INSERT INTO centry.project_deletions (project_id, cleanup, created_at, next_attempt_at, claimed_until)
SELECT $1::bigint, $2::jsonb, clock_timestamp(), clock_timestamp(),
       clock_timestamp() + make_interval(secs => $3::float8)
WHERE NOT EXISTS (SELECT 1 FROM centry.project WHERE id = $1::bigint)
ON CONFLICT (project_id) DO NOTHING
RETURNING project_id`,
		projectID, string(encoded), cleanupLease(cleanupTimeout).Seconds(),
	).Scan(&inserted); {
	case errors.Is(err, pgx.ErrNoRows):
		// A project or a journal row appeared meanwhile: nothing to adopt.
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("adopt leftovers: %w", err)
	}
	p.logger.WarnContext(ctx, "adopted the leftovers of a project that has no row and no cleanup journal",
		"project_id", projectID, "tenant_schema", schema, "live_buckets", live, "vector_database", database)
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
// step and joined; none hides another. It reports whether the journal row ended
// complete.
//
// parent is the context whose end means the PROCESS is stopping (the
// provisioner's lifecycle for a delete's own run, the reconciler's context for a
// resumed one). The run's own deadline (cleanupTimeout) is derived from it, so
// the two are told apart: a step cut off by the deadline is a failure and backs
// off; one cut off by parent is an interruption, and the run releases the lease
// without counting an attempt or growing the backoff (last_error is
// "interrupted").
//
// The journal writes use a context of their own (bookkeepingContext), so a run
// whose context ended still records the steps it finished. A step not reached
// because the deadline passed is pending, not failed: it has no status of its own
// and no error, and the journal row stays open for the next run.
//
// BEFORE EVERY STEP the run checks, on its own connection, that no project row
// exists for the id. All three steps destroy data keyed by the id, and an id can
// come back (explicit ids, a restored database): if a project exists, the
// journal row is closed as superseded and nothing is touched. Provision's own
// check of the journal closes the race from the other side.
func (p *Provisioner) runCleanup(parent context.Context, projectID int64, cleanup *deletionCleanup) (Result, bool, error) {
	if cleanup.Done == nil {
		cleanup.Done = map[string]bool{}
	}
	ctx, cancel := context.WithTimeout(parent, cleanupTimeout)
	defer cancel()
	// The lease the decision stamped may have run down while that transaction
	// waited on a lock; the run owns the row for its full bound from here.
	p.restampLease(ctx, projectID)

	result := Result{ProjectID: projectID}
	var (
		failures    []error
		failedSteps []string
		interrupted bool
	)
	for _, step := range cleanupSteps() {
		status := StepStatus{Step: step.name, Initialized: true}
		if cleanup.Done[step.name] {
			status.setOK()
			result.RollbackSteps = append(result.RollbackSteps, status)
			continue
		}
		if parent.Err() != nil {
			interrupted = true
			result.RollbackSteps = append(result.RollbackSteps, StepStatus{Step: step.name})
			result.Pending = append(result.Pending, step.name)
			continue
		}
		if ctx.Err() != nil {
			// The run's bound is spent: leave the step to the journal.
			result.RollbackSteps = append(result.RollbackSteps, StepStatus{Step: step.name})
			result.Pending = append(result.Pending, step.name)
			failedSteps = append(failedSteps, step.name)
			continue
		}

		var (
			note string
			err  error
		)
		if exists, checkErr := p.projectRowExists(ctx, projectID); checkErr != nil {
			err = fmt.Errorf("check that the project id is still free: %w", checkErr)
		} else if exists {
			p.logger.WarnContext(ctx, "project delete cleanup superseded: a project with this id exists again, so nothing is cleaned",
				"project_id", projectID, "step", step.name)
			cleanup.Superseded = true
			completed := p.closeSuperseded(ctx, projectID)
			return Result{ProjectID: projectID, RollbackSteps: result.RollbackSteps}, completed, nil
		} else {
			note, err = step.run(ctx, p, projectID, cleanup)
		}

		var quiet quietRetry
		switch {
		case err == nil:
			status.setOK()
			status.Msg = note
			cleanup.Done[step.name] = true
			p.markCleanupStep(ctx, projectID, step.name, "")
		case parent.Err() != nil:
			// The process is stopping: this is not the step's failure.
			interrupted = true
			result.RollbackSteps = append(result.RollbackSteps, StepStatus{Step: step.name})
			result.Pending = append(result.Pending, step.name)
			continue
		case errors.As(err, &quiet):
			// Retried by the journal, not reported (see cleanupVectorStore), and
			// not for ever: a store nothing says existed is not worth an endless
			// retry.
			cleanup.QuietDropFailures++
			status.setOK()
			if cleanup.QuietDropFailures >= p.quietDropLimit {
				p.logger.WarnContext(ctx, "project delete cleanup gave up on a vector store the project was not recorded as having",
					"step", step.name, "project_id", projectID, "attempts", cleanup.QuietDropFailures, "err", err)
				status.Msg = quietGaveUpNote
				cleanup.Done[step.name] = true
				p.markCleanupStep(ctx, projectID, step.name, quietGaveUpNote)
				break
			}
			p.logger.ErrorContext(ctx, "project delete cleanup step failed for a store the project was not recorded as having; the cleanup journal retries it",
				"step", step.name, "project_id", projectID, "attempt", cleanup.QuietDropFailures,
				"limit", p.quietDropLimit, "err", err)
			p.recordQuietFailures(ctx, projectID, cleanup.QuietDropFailures)
			status.Msg = "skipped: no vector store recorded"
			failedSteps = append(failedSteps, step.name)
		default:
			p.logger.ErrorContext(ctx, "project delete cleanup step failed; the cleanup journal retries it",
				"step", step.name, "project_id", projectID, "err", err)
			status.setFailed(safeStepMessage(step.name))
			failures = append(failures, step.failure(cleanup, err))
			failedSteps = append(failedSteps, step.name)
			result.Pending = append(result.Pending, step.name)
			if step.name == StepProjectPgvectorDrop {
				result.VectorDatabase = cleanup.VectorDatabase
			}
		}
		result.RollbackSteps = append(result.RollbackSteps, status)
	}
	if interrupted {
		p.logger.InfoContext(ctx, "project delete cleanup interrupted by shutdown; the journal row stays open for the next run",
			"project_id", projectID)
		p.releaseInterrupted(ctx, projectID)
		return result, false, errors.Join(failures...)
	}
	completed := p.finishCleanupRun(ctx, projectID, failedSteps)
	return result, completed, errors.Join(failures...)
}

// quietGaveUpNote is the note of a step the journal marked done after the
// retries of a quiet failure ran out.
const quietGaveUpNote = "gave up: no vector store recorded"

// projectRowExists reports whether a project with the id exists, on a connection
// of the run's own.
func (p *Provisioner) projectRowExists(ctx context.Context, projectID int64) (bool, error) {
	var exists bool
	if err := p.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM centry.project WHERE id = $1)`, projectID,
	).Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}

// bookkeepingContext is the context for one journal write: detached from the
// run's (which may have ended at its deadline) with a short bound of its own.
func bookkeepingContext(runCtx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(runCtx), bookkeepingTimeout)
}

// restampLease gives the run its full lease from now: the decision stamped one
// when it inserted the row, and a decision that waited on a lock has used part of
// it. A failure is logged only; the decision's lease still stands.
func (p *Provisioner) restampLease(runCtx context.Context, projectID int64) {
	ctx, cancel := bookkeepingContext(runCtx)
	defer cancel()
	if _, err := p.pool.Exec(ctx, `
UPDATE centry.project_deletions
SET claimed_until = clock_timestamp() + make_interval(secs => $2::float8)
WHERE project_id = $1 AND completed_at IS NULL`,
		projectID, cleanupLease(cleanupTimeout).Seconds()); err != nil {
		p.logger.WarnContext(ctx, "could not restamp the lease of a cleanup run",
			"project_id", projectID, "err", err)
	}
}

// markCleanupStep records one finished step in the journal row, with an optional
// note. A failure is logged only: the step is idempotent, so the worst outcome is
// that a later run repeats it.
func (p *Provisioner) markCleanupStep(runCtx context.Context, projectID int64, step, note string) {
	ctx, cancel := bookkeepingContext(runCtx)
	defer cancel()
	if _, err := p.pool.Exec(ctx, `
UPDATE centry.project_deletions
SET cleanup = CASE WHEN $3::text = '' THEN jsonb_set(cleanup, ARRAY['done', $2::text], 'true'::jsonb, true)
    ELSE jsonb_set(jsonb_set(cleanup, ARRAY['done', $2::text], 'true'::jsonb, true),
                   '{notes}', COALESCE(cleanup->'notes', '{}'::jsonb) || jsonb_build_object($2::text, $3::text), true)
    END
WHERE project_id = $1`, projectID, step, note); err != nil {
		p.logger.WarnContext(ctx, "could not record a finished cleanup step; a later run repeats it",
			"project_id", projectID, "step", step, "err", err)
	}
}

// recordQuietFailures stores how many quiet drop failures the journal has seen,
// so the limit holds across runs and restarts.
func (p *Provisioner) recordQuietFailures(runCtx context.Context, projectID int64, count int) {
	ctx, cancel := bookkeepingContext(runCtx)
	defer cancel()
	if _, err := p.pool.Exec(ctx, `
UPDATE centry.project_deletions
SET cleanup = jsonb_set(cleanup, '{quiet_drop_failures}', to_jsonb($2::int), true)
WHERE project_id = $1`, projectID, count); err != nil {
		p.logger.WarnContext(ctx, "could not record a quiet drop failure; the limit counts from the next run",
			"project_id", projectID, "err", err)
	}
}

// closeSuperseded closes a journal row whose id belongs to a live project again:
// completed, flagged cleanup.superseded, lease released. It reports whether the
// row is closed.
func (p *Provisioner) closeSuperseded(runCtx context.Context, projectID int64) bool {
	ctx, cancel := bookkeepingContext(runCtx)
	defer cancel()
	if _, err := p.pool.Exec(ctx, `
UPDATE centry.project_deletions
SET cleanup = jsonb_set(cleanup, '{superseded}', 'true'::jsonb, true),
    completed_at = clock_timestamp(),
    claimed_until = NULL,
    last_error = NULL
WHERE project_id = $1 AND completed_at IS NULL`, projectID); err != nil {
		p.logger.WarnContext(ctx, "could not close a superseded cleanup journal row; the next run closes it",
			"project_id", projectID, "err", err)
		return false
	}
	return true
}

// releaseInterrupted ends a run the process stopped: the lease is released, the
// row records "interrupted", and neither attempts nor the backoff move, so a
// restart does not make the cleanup of every in-flight delete wait longer.
func (p *Provisioner) releaseInterrupted(runCtx context.Context, projectID int64) {
	ctx, cancel := bookkeepingContext(runCtx)
	defer cancel()
	if _, err := p.pool.Exec(ctx, `
UPDATE centry.project_deletions
SET last_error = 'interrupted', claimed_until = NULL
WHERE project_id = $1 AND completed_at IS NULL`, projectID); err != nil {
		p.logger.WarnContext(ctx, "could not release the lease of an interrupted cleanup run; it expires on its own",
			"project_id", projectID, "err", err)
	}
}

// finishCleanupRun closes one run in the journal row. With no failed step the
// row is complete. Otherwise last_error names the failed steps (names only: a
// raw error can carry SQL or addresses) and next_attempt_at backs off by the
// number of runs so far. It reports whether the row is now complete.
func (p *Provisioner) finishCleanupRun(runCtx context.Context, projectID int64, failedSteps []string) bool {
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
    completed_at = CASE WHEN $2::text IS NULL THEN clock_timestamp() END,
    next_attempt_at = CASE WHEN $2::text IS NULL THEN next_attempt_at
        ELSE clock_timestamp() + make_interval(secs => LEAST($3::float8 * power(2, LEAST(attempts, 30)), $4::float8)) END,
    claimed_until = NULL
WHERE project_id = $1`,
		projectID, lastError, deletionRetryBase.Seconds(), deletionRetryMax.Seconds(),
	); err != nil {
		p.logger.WarnContext(ctx, "could not close the cleanup run in the journal; the reconciler retries the row",
			"project_id", projectID, "err", err)
		return false
	}
	return len(failedSteps) == 0
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
      AND created_at < clock_timestamp() - make_interval(secs => $1)
      AND next_attempt_at <= clock_timestamp()
      AND (claimed_until IS NULL OR claimed_until < clock_timestamp())
    ORDER BY next_attempt_at, project_id
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
UPDATE centry.project_deletions AS journal
SET claimed_until = clock_timestamp() + make_interval(secs => $2)
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
// has claimed (ClaimNextDeletion). completed is true only when the journal row
// ends complete: every step done, the row superseded by a live project, or the
// row already complete. A run that leaves a step undone is (false, err), where
// err joins the leftovers, or (false, nil) when the process stopped it (the
// lease is released and the attempt is not counted). A project with no journal
// row is ErrProjectNotFound.
//
// ctx is the context whose end means the process is stopping.
func (p *Provisioner) ResumeDeletion(ctx context.Context, projectID int64) (completed bool, err error) {
	if p.pool == nil || p.vault == nil {
		return false, errors.New("projectprovisioning: provisioner is not configured")
	}
	var (
		encoded []byte
		done    *time.Time
	)
	switch err := p.pool.QueryRow(ctx,
		`SELECT cleanup, completed_at FROM centry.project_deletions WHERE project_id = $1`, projectID,
	).Scan(&encoded, &done); {
	case errors.Is(err, pgx.ErrNoRows):
		return false, ErrProjectNotFound
	case err != nil:
		return false, fmt.Errorf("projectprovisioning: read cleanup journal of project %d: %w", projectID, err)
	}
	if done != nil {
		return true, nil
	}
	var cleanup deletionCleanup
	if err := json.Unmarshal(encoded, &cleanup); err != nil {
		return false, fmt.Errorf("projectprovisioning: decode cleanup journal of project %d: %w", projectID, err)
	}
	_, completed, err = p.runCleanup(ctx, projectID, &cleanup)
	return completed, err
}
