// Package toolkitcleanup is the cleanup journal of a toolkit delete (#1244):
// the engine-side deletion a toolkit row's removal owes, recorded in the
// transaction that removes the row and run afterwards by a reconciler.
//
// An Inventory toolkit's graph lives in the Inventory engine, not in the
// tenant's elitea_tools row, so `DELETE FROM elitea_tools` leaves it behind.
// The engine is another process reached over gRPC (internal/providerhost/
// platformops), which the row's transaction cannot span. So the delete is
// ROW-FIRST WITH A JOURNAL, the shape of a project delete (#1245,
// projectprovisioning): the handler's transaction deletes the toolkit row and,
// for a type whose application keeps data per toolkit, inserts a row in
// centry.toolkit_deletions (shared/0163); the request answers once that
// commits and never waits for the engine or depends on it being reachable. A
// reconciler (runtimecomposition.ToolkitDeletionReconciler) claims the rows
// and makes the call, with the journal's backoff, until each row is closed.
//
// WHICH TYPES GET A ROW. A type whose application has no engine (github, ...)
// gets none. A DeepWiki type gets none either: DeepWiki keeps nothing per
// toolkit (its index is per wiki and goes with the project), so there is no
// call to make and none is recorded (platformops.AppKeepsToolkitData). An
// Inventory type (platformops.AppForToolkitType, an exact allow-list) gets one
// — on EVERY replica, whether or not the replica taking the delete has the
// Inventory client wired. The row is the obligation; which replica can pay it
// is a separate question, answered when the row is drained. (The project
// journal records its application steps the same way.)
//
// THE ROW RECORDS THE APPLICATION. The reconciler that works a row uses the
// client for the row's application; a replica that has none leaves the row
// pending with a backoff and a logged reason, rather than closing it, so the
// work waits for a replica that can do it.
//
// A RUN OWNS ITS ROW BY GENERATION. Every claim reads the row's generation
// with its id, and every write a run makes (close, fail, release) requires that
// generation. A second delete of the same toolkit id reopens the row with the
// generation bumped and the lease cleared, so a run that was still working the
// previous obligation — its engine call outlasted the lease, or the row was
// reopened under it — can neither close the new obligation nor move its
// backoff: its writes match no row, and the run reports ErrClaimLost.
//
// OUTCOMES. The engine's answer closes the row with an outcome and a note:
// deleted, nothing_to_delete, unsupported (UNIMPLEMENTED: the engine keeps
// nothing of this kind; warned), failed_permanent (the engine refused this
// process's certificate, or the request was invalid: retrying cannot fix it;
// the note names the class and the raw error is logged), superseded (a toolkit
// with the id exists again). A transient failure (the engine is still stopping
// an ingest, is unreachable, or failed) leaves the row open with a backoff; so
// does a close that could not be written.
package toolkitcleanup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/cleanupjournal"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/platformops"
)

var (
	// ErrRowNotFound reports a Resume for a journal id that has no row.
	ErrRowNotFound = errors.New("toolkitcleanup: journal row does not exist")
	// ErrClaimLost reports a Resume whose claim no longer owns the row: the
	// row was reopened (a second delete of the same toolkit id) since the
	// claim, so its generation moved on. Nothing was done; the new obligation
	// is claimed on its own.
	ErrClaimLost = errors.New("toolkitcleanup: the claim no longer owns the journal row")
)

// Remover deletes what one application's engine holds for a toolkit. It is
// satisfied by *platformops.Client; the errors it returns are platformops's
// (ErrRetryLater, ErrNotSupported, ErrNotAllowed, ErrConfig).
type Remover interface {
	DeleteToolkitData(ctx context.Context, projectID, toolkitID int64) (platformops.ToolkitDeletion, error)
}

// AppMemory is the application a memory toolkit's rows are filed under: the
// vector store, which holds the toolkit's memories (every namespace of them,
// every owner's) keyed by the toolkit id (worker unit M9). It is not an engine
// in platformops's sense, so it has its own enqueue (EnqueueMemory) and its
// own remover (MemoryRemover).
const AppMemory = "memory"

// AppMemoryUser is the application a USER's personal-memory cleanup is filed
// under, in the same table with the same mechanics: the row's toolkit_id column
// holds the user id, and project_id is the project the user left, or 0 for
// every project (the user was deleted). It is the durable form of "a user's
// personal memories go with the user": the row is written in the transaction
// that removes the user or the membership, and the reconciler retries it until
// the vector store has done it. EnqueueMemoryUsers is the one place that writes
// these rows.
const AppMemoryUser = "memory_user"

// MemoryRemover deletes memory points from the vector store. *vectorclient.Client
// satisfies it; the errors are platformops's classes, like Remover's.
type MemoryRemover interface {
	// DeleteMemoryToolkit removes every memory point of one memory toolkit of
	// a project: every namespace, every owner.
	DeleteMemoryToolkit(ctx context.Context, projectID, toolkitID int64) error
	// DeleteMemoryUser removes every PERSONAL memory point of one user in one
	// project, or (projectID 0) in every project.
	DeleteMemoryUser(ctx context.Context, projectID, userID int64) error
}

// The outcomes a closed row records (migration shared/0163).
const (
	OutcomeDeleted         = "deleted"
	OutcomeNothingToDelete = "nothing_to_delete"
	OutcomeUnsupported     = "unsupported"
	OutcomeFailedPermanent = "failed_permanent"
	OutcomeSuperseded      = "superseded"
)

// Notes of the rows closed without the engine deleting anything. The notes of
// the terminal engine answers are class texts shared with the project journal
// (cleanupjournal): the engine's own message is logged, never stored.
const (
	NoteUnsupported    = cleanupjournal.NoteUnsupported
	NoteRefused        = cleanupjournal.NoteRefused
	NoteInvalidRequest = cleanupjournal.NoteInvalidRequest
	noteSuperseded     = "a toolkit with this id exists again; nothing was touched"
	// noteUserBack closes a memory_user row whose user is back: a member of
	// the project again (or, for a project-0 row, a user row with the id
	// exists). Re-adding the person cancels the pending cleanup, so the
	// memories they write after coming back are not deleted.
	noteUserBack = "the user is a member again; nothing was deleted"
)

// The last_error classes a failed run records (never the raw error: it can
// carry an address).
const (
	lastErrorRetryLater  = "engine holds the toolkit's data or is unavailable; retry"
	lastErrorFailed      = "engine call failed; retry"
	lastErrorCheckFailed = "could not check whether the toolkit exists again; retry"
	lastErrorCloseFailed = "engine answered but the row could not be closed; retry"
	lastErrorNotWired    = "no client for the application on the replica that claimed the row"
	lastErrorInterrupted = "interrupted"
)

// runTimeout bounds one run of a row: the engine call (which the client bounds
// itself, at most the facade's timeout or two minutes) and the bookkeeping
// around it. One row is one call, so no step can starve another. Each journal
// write is on a context of its own (cleanupjournal.BookkeepingContext), so a
// run that hit its deadline still records what it did. leaseMargin is added to
// the run bound to make the lease. Variables so a test can shrink them.
var (
	runTimeout  = 3 * time.Minute
	leaseMargin = time.Minute
)

// The retry backoff: the first failed run waits retryBase, each further failure
// doubles it, up to retryMax. Stored in the row, so it survives restarts and is
// shared by replicas.
var (
	retryBase = time.Minute
	retryMax  = time.Hour
)

// Journal is the cleanup journal: it enqueues rows inside a delete's
// transaction and runs them for the reconciler.
type Journal struct {
	pool     *pgxpool.Pool
	removers map[string]Remover
	// memory deletes a memory toolkit's points and a user's personal ones; nil
	// on a replica without an elitea-vector client. memoryRecorded says whether
	// a delete decided here owes the vector store a call at all (the
	// deployment keeps memories there: the Rust indexing runtime).
	memory         MemoryRemover
	memoryRecorded bool
	logger         *slog.Logger
}

// Option configures a Journal.
type Option func(*Journal)

// WithRemover wires one application's engine. A nil or typed-nil remover is
// ignored, for the reason projectprovisioning.WithApplicationData ignores
// one: the composition root declares the interface type, and a typed nil would
// pass the nil check and panic inside a run.
func WithRemover(app string, remover Remover) Option {
	return func(j *Journal) {
		app = strings.TrimSpace(app)
		if app == "" || remover == nil || cleanupjournal.IsNilPointer(remover) {
			return
		}
		j.removers[app] = remover
	}
}

// WithMemory wires the vector store for memory toolkits. recorded says that a
// delete decided on this replica owes the store a call (the deployment keeps
// memories in elitea-vector); a replica that only drains passes false. A nil
// remover (or a typed nil) leaves the rows this replica cannot pay pending.
func WithMemory(remover MemoryRemover, recorded bool) Option {
	return func(j *Journal) {
		j.memoryRecorded = recorded
		if remover == nil || cleanupjournal.IsNilPointer(remover) {
			return
		}
		j.memory = remover
	}
}

// New builds a journal over the shared database. pool may be nil in a
// composition that has no database; such a journal enqueues nothing and claims
// nothing.
func New(pool *pgxpool.Pool, logger *slog.Logger, options ...Option) *Journal {
	if logger == nil {
		logger = slog.Default()
	}
	j := &Journal{pool: pool, removers: map[string]Remover{}, logger: logger}
	for _, option := range options {
		if option != nil {
			option(j)
		}
	}
	return j
}

// AppsWired names the applications this journal can call, sorted. For the
// composition root's startup log and for tests.
func (j *Journal) AppsWired() []string {
	apps := make([]string, 0, len(j.removers)+1)
	for app := range j.removers {
		apps = append(apps, app)
	}
	if j.memory != nil {
		apps = append(apps, AppMemory, AppMemoryUser)
	}
	sort.Strings(apps)
	return apps
}

// AppToEnqueue names the application a toolkit of the type owes a deletion to,
// or "" when none: the type has no engine (platformops.AppForToolkitType), or
// the application keeps nothing per toolkit (DeepWiki). It does not depend on
// which clients this replica has: the obligation is recorded wherever the
// delete is taken, and drained by a replica that can.
func AppToEnqueue(toolkitType string) string {
	app := platformops.AppForToolkitType(toolkitType)
	if app == "" || !platformops.AppKeepsToolkitData(app) {
		return ""
	}
	return app
}

// Enqueue records, in tx (the transaction that deletes the toolkit row), the
// engine-side deletion the toolkit owes. It returns the application queued, or
// "" when the type owes none (AppToEnqueue). A row for the same toolkit and
// application is REOPENED: attempts, backoff and outcome reset, the lease
// cleared, and the generation bumped so a run still holding the old claim
// cannot touch the new obligation. A replica without the application's client
// enqueues all the same, and logs that another replica has to drain the row.
func (j *Journal) Enqueue(ctx context.Context, tx pgx.Tx, projectID, toolkitID int64, toolkitType string) (string, error) {
	app := AppToEnqueue(toolkitType)
	if app == "" {
		return "", nil
	}
	if tx == nil {
		return "", errors.New("toolkitcleanup: enqueue needs the delete's transaction")
	}
	var id, generation int64
	if err := tx.QueryRow(ctx, `
INSERT INTO centry.toolkit_deletions (project_id, toolkit_id, app, toolkit_type, created_at, next_attempt_at)
VALUES ($1, $2, $3, $4, clock_timestamp(), clock_timestamp())
ON CONFLICT (project_id, toolkit_id, app) DO UPDATE
SET toolkit_type = EXCLUDED.toolkit_type,
    outcome = NULL,
    note = NULL,
    created_at = EXCLUDED.created_at,
    attempts = 0,
    last_error = NULL,
    next_attempt_at = EXCLUDED.next_attempt_at,
    claimed_until = NULL,
    completed_at = NULL,
    generation = centry.toolkit_deletions.generation + 1
RETURNING id, generation`, projectID, toolkitID, app, toolkitType).Scan(&id, &generation); err != nil {
		return "", fmt.Errorf("toolkitcleanup: write cleanup journal: %w", err)
	}
	attrs := []any{"app", app, "project_id", projectID, "toolkit_id", toolkitID, "toolkit_type", toolkitType, "journal_id", id, "generation", generation}
	if _, wired := j.removers[app]; !wired {
		j.logger.WarnContext(ctx, "toolkit delete: engine-side deletion journaled, but this process has no client for the application; a replica that has one drains the row",
			attrs...)
	} else {
		j.logger.InfoContext(ctx, "toolkit delete: engine-side deletion journaled", attrs...)
	}
	return app, nil
}

// EnqueueMemory records, in tx (the transaction that deletes a memory
// toolkit's row), that the toolkit's memories owe the vector store a deletion:
// every point stamped with the toolkit id, whatever its namespace or owner. It
// returns AppMemory, or "" when this deployment keeps no memories in the vector
// store (the journal was built with recorded false). Like Enqueue, a row for the
// same toolkit is REOPENED, its generation bumped.
func (j *Journal) EnqueueMemory(ctx context.Context, tx pgx.Tx, projectID, toolkitID int64) (string, error) {
	return j.enqueueMemoryRows(ctx, tx, AppMemory, projectID, toolkitID)
}

// EnqueueMemoryUsers records, in tx (the transaction that deletes the users or
// removes them from the project), that each user's PERSONAL memories owe the
// vector store a deletion. projectID is the project the users left, or 0 when
// they are deleted outright (their personal memories go from every project,
// with no membership lookup). It is the ONE writer of these rows: every hook
// (user delete in the core and admin APIs, project-membership removal, SCIM)
// calls it, so no caller has a loop of its own. It returns AppMemoryUser, or ""
// when this deployment keeps no memories in the vector store or userIDs names
// nobody. Ids that are not positive are ignored. A row for the same user and
// project is REOPENED, its generation bumped.
func (j *Journal) EnqueueMemoryUsers(ctx context.Context, tx pgx.Tx, projectID int64, userIDs ...int64) (string, error) {
	ids := make([]int64, 0, len(userIDs))
	for _, id := range userIDs {
		if id > 0 {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return "", nil
	}
	return j.enqueueMemoryRows(ctx, tx, AppMemoryUser, projectID, ids...)
}

// enqueueMemoryRows writes one journal row per id in one statement: the
// toolkit id of AppMemory rows, the user ids of AppMemoryUser rows (both go in
// the toolkit_id column).
func (j *Journal) enqueueMemoryRows(ctx context.Context, tx pgx.Tx, app string, projectID int64, ids ...int64) (string, error) {
	if !j.memoryRecorded {
		return "", nil
	}
	if tx == nil {
		return "", errors.New("toolkitcleanup: enqueue needs the delete's transaction")
	}
	if projectID < 0 || (projectID == 0 && app != AppMemoryUser) {
		return "", fmt.Errorf("toolkitcleanup: project id %d is not valid for a %s row", projectID, app)
	}
	tag, err := tx.Exec(ctx, `
INSERT INTO centry.toolkit_deletions (project_id, toolkit_id, app, toolkit_type, created_at, next_attempt_at)
SELECT $1, id, $2, $2, clock_timestamp(), clock_timestamp() FROM unnest($3::bigint[]) AS id
ON CONFLICT (project_id, toolkit_id, app) DO UPDATE
SET toolkit_type = EXCLUDED.toolkit_type,
    outcome = NULL,
    note = NULL,
    created_at = EXCLUDED.created_at,
    attempts = 0,
    last_error = NULL,
    next_attempt_at = EXCLUDED.next_attempt_at,
    claimed_until = NULL,
    completed_at = NULL,
    generation = centry.toolkit_deletions.generation + 1`, projectID, app, ids)
	if err != nil {
		return "", fmt.Errorf("toolkitcleanup: write cleanup journal: %w", err)
	}
	attrs := []any{"app", app, "project_id", projectID, "rows", tag.RowsAffected()}
	if j.memory == nil {
		j.logger.WarnContext(ctx, "memory cleanup: the vector-store deletion is journaled, but this process has no elitea-vector client; a replica that has one drains the row", attrs...)
	} else {
		j.logger.InfoContext(ctx, "memory cleanup: the vector-store deletion is journaled", attrs...)
	}
	return app, nil
}

// Claim is one run's ownership of a journal row: the row's id and the
// generation it had when claimed. Every write the run makes requires the
// generation, so a row reopened under the run is left alone.
type Claim struct {
	ID         int64
	Generation int64
}

// ClaimNext claims ONE incomplete journal row older than grace, past its
// backoff and not leased, and leases it for one run. ok is false when there is
// none. One row per claim in a short FOR UPDATE SKIP LOCKED statement that
// stamps the lease, as projectprovisioning.ClaimNextDeletion does.
func (j *Journal) ClaimNext(ctx context.Context, grace time.Duration) (claim Claim, ok bool, err error) {
	if j.pool == nil {
		return Claim{}, false, errors.New("toolkitcleanup: journal has no database")
	}
	if grace < 0 {
		return Claim{}, false, errors.New("toolkitcleanup: claim needs a non-negative grace")
	}
	switch err := j.pool.QueryRow(ctx, `
WITH claimable AS (
    SELECT id
    FROM centry.toolkit_deletions
    WHERE completed_at IS NULL
      AND created_at < clock_timestamp() - make_interval(secs => $1)
      AND next_attempt_at <= clock_timestamp()
      AND (claimed_until IS NULL OR claimed_until < clock_timestamp())
    ORDER BY next_attempt_at, id
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
UPDATE centry.toolkit_deletions AS journal
SET claimed_until = clock_timestamp() + make_interval(secs => $2)
FROM claimable
WHERE journal.id = claimable.id
RETURNING journal.id, journal.generation`,
		grace.Seconds(), (runTimeout+leaseMargin).Seconds(),
	).Scan(&claim.ID, &claim.Generation); {
	case errors.Is(err, pgx.ErrNoRows):
		return Claim{}, false, nil
	case err != nil:
		return Claim{}, false, fmt.Errorf("toolkitcleanup: claim: %w", err)
	}
	return claim, true, nil
}

// row is one journal row as a run reads it.
type row struct {
	id          int64
	generation  int64
	projectID   int64
	toolkitID   int64
	app         string
	toolkitType string
}

// Resume runs the journal row the caller has claimed (ClaimNext). completed is
// true only when the row ended closed. A run that leaves it open is
// (false, err) with the cause, or (false, nil) when ctx ended (the lease is
// released and the attempt is not counted). A row that no longer exists is
// ErrRowNotFound; a row reopened since the claim is ErrClaimLost.
//
// ctx is the context whose end means the process is stopping.
func (j *Journal) Resume(ctx context.Context, claim Claim) (bool, error) {
	if j.pool == nil {
		return false, errors.New("toolkitcleanup: journal has no database")
	}
	var (
		r         row
		completed *time.Time
	)
	switch err := j.pool.QueryRow(ctx, `
SELECT id, generation, project_id, toolkit_id, app, toolkit_type, completed_at
FROM centry.toolkit_deletions WHERE id = $1`, claim.ID,
	).Scan(&r.id, &r.generation, &r.projectID, &r.toolkitID, &r.app, &r.toolkitType, &completed); {
	case errors.Is(err, pgx.ErrNoRows):
		return false, ErrRowNotFound
	case err != nil:
		return false, fmt.Errorf("toolkitcleanup: read journal row %d: %w", claim.ID, err)
	}
	if r.generation != claim.Generation {
		return false, fmt.Errorf("%w: row %d is at generation %d, the claim at %d", ErrClaimLost, r.id, r.generation, claim.Generation)
	}
	if completed != nil {
		return true, nil
	}
	return j.run(ctx, r)
}

// run works one row under the run bound: the superseded check, the engine
// call, and the row's closing or backoff.
func (j *Journal) run(parent context.Context, r row) (bool, error) {
	ctx, cancel := context.WithTimeout(parent, runTimeout)
	defer cancel()
	attrs := []any{"journal_id", r.id, "generation", r.generation, "app", r.app, "project_id", r.projectID, "toolkit_id", r.toolkitID}

	// A toolkit row is superseded by a toolkit with its id existing again; a
	// user's memory cleanup by the user being back (re-added to the project,
	// or a user row with the id existing again), re-checked at the moment the
	// row is worked, right before the delete.
	var exists bool
	var err error
	if r.app == AppMemoryUser {
		exists, err = j.memoryUserBack(ctx, r)
	} else {
		exists, err = j.toolkitRowExists(ctx, r)
	}
	switch {
	case err != nil && parent.Err() != nil:
		j.releaseInterrupted(ctx, r)
		return false, nil
	case err != nil:
		j.logger.ErrorContext(ctx, "toolkit delete cleanup: could not check whether the toolkit exists again; the journal retries it", append(attrs, "err", err)...)
		return false, j.fail(ctx, r, lastErrorCheckFailed, err)
	case exists && r.app == AppMemoryUser:
		j.logger.InfoContext(ctx, "memory cleanup cancelled: the user is a member of the project again, so their personal memories stay", attrs...)
		return j.closeOrFail(ctx, r, OutcomeSuperseded, noteUserBack)
	case exists:
		j.logger.WarnContext(ctx, "toolkit delete cleanup superseded: a toolkit with this id exists again, so nothing is deleted", attrs...)
		return j.closeOrFail(ctx, r, OutcomeSuperseded, noteSuperseded)
	}

	outcome := j.deleteInEngine(ctx, r)
	switch {
	case outcome.kind != outcomeDone && parent.Err() != nil:
		// The process is stopping: this is not the row's failure.
		j.releaseInterrupted(ctx, r)
		return false, nil
	case outcome.kind == outcomeNotWired:
		// Pending, not failed: the row waits for a replica with the client.
		// It still backs off, so an unwired replica does not spin on it.
		return false, j.fail(ctx, r, lastErrorNotWired, outcome.cause)
	case outcome.kind == outcomeRetry:
		return false, j.fail(ctx, r, outcome.lastError, outcome.cause)
	}
	return j.closeOrFail(ctx, r, outcome.result, outcome.note)
}

// closeOrFail closes the row, or — when the close could not be written —
// records the run as failed so the reconciler counts it and the row backs off
// (the engine call is idempotent, so a later run repeats it). A close that
// finds no row at the run's generation is a lost claim: the row was reopened,
// and the new obligation is not this run's to close or back off.
func (j *Journal) closeOrFail(ctx context.Context, r row, result, note string) (bool, error) {
	err := j.close(ctx, r, result, note)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrClaimLost):
		return false, err
	}
	j.logger.WarnContext(ctx, "toolkit delete cleanup: the engine answered but the row could not be closed; the journal retries the (idempotent) call",
		"journal_id", r.id, "generation", r.generation, "result", result, "err", err)
	return false, j.fail(ctx, r, lastErrorCloseFailed, err)
}

type outcomeKind int

const (
	outcomeDone outcomeKind = iota
	outcomeRetry
	outcomeNotWired
)

// outcome is how one engine call ended, classified for the journal.
type outcome struct {
	kind outcomeKind
	// result and note close a done row.
	result, note string
	// lastError and cause describe a failed run.
	lastError string
	cause     error
}

// deleteInEngine makes the engine call for a row and classifies its answer. It
// is the part of a run that needs no database.
func (j *Journal) deleteInEngine(ctx context.Context, r row) outcome {
	attrs := []any{"journal_id", r.id, "app", r.app, "project_id", r.projectID, "toolkit_id", r.toolkitID}
	var (
		deletion platformops.ToolkitDeletion
		err      error
	)
	if r.app == AppMemory || r.app == AppMemoryUser {
		if j.memory == nil {
			j.logger.WarnContext(ctx, "toolkit delete cleanup: this replica has no elitea-vector client; the memory row is left for one that has", attrs...)
			return outcome{kind: outcomeNotWired, cause: fmt.Errorf("toolkitcleanup: no %s client on this replica", r.app)}
		}
		if r.app == AppMemory {
			err = j.memory.DeleteMemoryToolkit(ctx, r.projectID, r.toolkitID)
		} else {
			err = j.memory.DeleteMemoryUser(ctx, r.projectID, r.toolkitID)
		}
		if err == nil {
			deletion.Deleted = true
		}
	} else {
		remover, wired := j.removers[r.app]
		if !wired {
			j.logger.WarnContext(ctx, "toolkit delete cleanup: this replica has no client for the application; the row is left for one that has", attrs...)
			return outcome{kind: outcomeNotWired, cause: fmt.Errorf("toolkitcleanup: no %s client on this replica", r.app)}
		}
		deletion, err = remover.DeleteToolkitData(ctx, r.projectID, r.toolkitID)
	}
	switch {
	case err == nil:
		result, note := OutcomeDeleted, "deleted the toolkit's data"
		if !deletion.Deleted {
			result, note = OutcomeNothingToDelete, "nothing to delete"
		}
		j.logger.InfoContext(ctx, "toolkit delete cleanup: application data removed", append(attrs, "result", result)...)
		return outcome{kind: outcomeDone, result: result, note: note}
	case errors.Is(err, platformops.ErrNotSupported):
		j.logger.WarnContext(ctx, "toolkit delete cleanup: the engine does not support deleting a toolkit's data; the row is closed as unsupported", attrs...)
		return outcome{kind: outcomeDone, result: OutcomeUnsupported, note: NoteUnsupported}
	case errors.Is(err, platformops.ErrNotAllowed), errors.Is(err, platformops.ErrConfig):
		// The reason is logged here, with the row's ids; the row's note is the
		// class only (the raw error can carry an address or the engine's
		// statement of what it refused).
		note := NoteInvalidRequest
		if errors.Is(err, platformops.ErrNotAllowed) {
			note = NoteRefused
		}
		j.logger.ErrorContext(ctx, "toolkit delete cleanup FAILED PERMANENTLY: the engine refused this process or the request is invalid; the toolkit's data stays in the engine until an operator intervenes",
			append(attrs, "note", note, "err", err)...)
		return outcome{kind: outcomeDone, result: OutcomeFailedPermanent, note: note}
	case errors.Is(err, platformops.ErrRetryLater):
		j.logger.WarnContext(ctx, "toolkit delete cleanup: the engine could not delete the toolkit's data now; the journal retries it", append(attrs, "err", err)...)
		return outcome{kind: outcomeRetry, lastError: lastErrorRetryLater, cause: err}
	}
	j.logger.ErrorContext(ctx, "toolkit delete cleanup: the engine call failed; the journal retries it", append(attrs, "err", err)...)
	return outcome{kind: outcomeRetry, lastError: lastErrorFailed, cause: err}
}

// toolkitRowExists reports whether a toolkit with the row's id exists again in
// its project (the id came back: a restored database). A project with no row,
// or a tenant schema with no elitea_tools table, has no such toolkit. Two
// round trips: one statement answers both preconditions, and the table itself
// is read only once it is known to exist (its name is dynamic, so it cannot be
// in the first statement).
func (j *Journal) toolkitRowExists(ctx context.Context, r row) (bool, error) {
	schema, err := tenantschema.QuoteInt(r.projectID)
	if err != nil {
		return false, err
	}
	var projectAndTable bool
	if err := j.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM centry.project WHERE id = $1) AND to_regclass($2) IS NOT NULL`,
		r.projectID, schema+".elitea_tools",
	).Scan(&projectAndTable); err != nil {
		return false, fmt.Errorf("look for the project and its tools table: %w", err)
	}
	if !projectAndTable {
		return false, nil
	}
	var exists bool
	if err := j.pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s.elitea_tools WHERE id = $1)`, schema), r.toolkitID,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("look for the toolkit row: %w", err)
	}
	return exists, nil
}

// memoryUserBack reports whether a memory_user row's user is back, so its
// cleanup is cancelled: for a project row, the user holds a role in the
// project again (public.auth_core__project_user_role, the membership the
// removal deleted); for a project-0 row (a deleted user), a user row with the
// id exists again. A database without the table has no such user. A failed
// read is an error (the row retries): a cleanup that cannot tell must not
// delete.
func (j *Journal) memoryUserBack(ctx context.Context, r row) (bool, error) {
	// The table is looked for on its own first: a statement naming a missing
	// table fails at planning, whatever guards it.
	table := "public.auth_core__project_user_role"
	query := `SELECT EXISTS (SELECT 1 FROM public.auth_core__project_user_role WHERE project_id = $1 AND user_id = $2)`
	if r.projectID == 0 {
		table = "public.auth_core__user"
		query = `SELECT $1::bigint = 0 AND EXISTS (SELECT 1 FROM public.auth_core__user WHERE id = $2)`
	}
	var present bool
	if err := j.pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&present); err != nil {
		return false, fmt.Errorf("look for the membership table: %w", err)
	}
	if !present {
		return false, nil
	}
	var back bool
	if err := j.pool.QueryRow(ctx, query, r.projectID, r.toolkitID).Scan(&back); err != nil {
		return false, fmt.Errorf("look for the user's membership: %w", err)
	}
	return back, nil
}

// close ends a row with its outcome and note, the lease released, if the row
// is still at the run's generation. It returns the write's error, or
// ErrClaimLost when no row matched (reopened, or gone).
func (j *Journal) close(runCtx context.Context, r row, result, note string) error {
	ctx, cancel := cleanupjournal.BookkeepingContext(runCtx)
	defer cancel()
	tag, err := j.pool.Exec(ctx, `
UPDATE centry.toolkit_deletions
SET outcome = $3, note = $4, attempts = attempts + 1, last_error = NULL,
    completed_at = clock_timestamp(), claimed_until = NULL
WHERE id = $1 AND generation = $2`, r.id, r.generation, result, note)
	if err != nil {
		return fmt.Errorf("close journal row %d: %w", r.id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: row %d is no longer at generation %d", ErrClaimLost, r.id, r.generation)
	}
	return nil
}

// fail closes one failed run: attempts counted, last_error set to its class,
// next_attempt_at backed off by the number of runs so far, lease released —
// if the row is still at the run's generation and open. It returns cause
// wrapped for the reconciler's log.
func (j *Journal) fail(runCtx context.Context, r row, lastError string, cause error) error {
	ctx, cancel := cleanupjournal.BookkeepingContext(runCtx)
	defer cancel()
	tag, err := j.pool.Exec(ctx, `
UPDATE centry.toolkit_deletions
SET attempts = attempts + 1,
    last_error = $3,
    next_attempt_at = clock_timestamp() + make_interval(secs => LEAST($4::float8 * power(2, LEAST(attempts, 30)), $5::float8)),
    claimed_until = NULL
WHERE id = $1 AND generation = $2 AND completed_at IS NULL`,
		r.id, r.generation, lastError, retryBase.Seconds(), retryMax.Seconds())
	switch {
	case err != nil:
		j.logger.WarnContext(ctx, "could not record a failed toolkit cleanup run; the lease expires on its own",
			"journal_id", r.id, "generation", r.generation, "err", err)
	case tag.RowsAffected() == 0:
		j.logger.InfoContext(ctx, "a failed toolkit cleanup run found its row reopened or closed meanwhile; nothing recorded",
			"journal_id", r.id, "generation", r.generation)
	}
	return fmt.Errorf("toolkitcleanup: %s: %w", lastError, cause)
}

// releaseInterrupted ends a run the process stopped: the lease is released and
// neither attempts nor the backoff move — if the row is still this run's.
func (j *Journal) releaseInterrupted(runCtx context.Context, r row) {
	ctx, cancel := cleanupjournal.BookkeepingContext(runCtx)
	defer cancel()
	if _, err := j.pool.Exec(ctx, `
UPDATE centry.toolkit_deletions
SET last_error = $3, claimed_until = NULL
WHERE id = $1 AND generation = $2 AND completed_at IS NULL`, r.id, r.generation, lastErrorInterrupted); err != nil {
		j.logger.WarnContext(ctx, "could not release the lease of an interrupted toolkit cleanup run; it expires on its own",
			"journal_id", r.id, "err", err)
	}
}
