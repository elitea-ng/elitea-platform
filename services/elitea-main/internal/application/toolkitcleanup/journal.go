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
// Inventory type gets one — IF the replica taking the delete has the Inventory
// client wired; a replica without it logs the gap and records nothing, since a
// deployment with no engine has nothing to call.
//
// THE ROW RECORDS THE APPLICATION. The reconciler that works a row uses the
// client for the row's application; a replica that has none leaves the row
// pending (logged) rather than closing it, so the work waits for a replica
// that can do it.
//
// OUTCOMES. The engine's answer closes the row with an outcome and a note:
// deleted, nothing_to_delete, unsupported (UNIMPLEMENTED: the engine keeps
// nothing of this kind; warned), failed_permanent (the engine refused this
// process's certificate, or the request was invalid: retrying cannot fix it;
// the reason is the note and an error is logged), superseded (a toolkit with
// the id exists again). A transient failure (the engine is still stopping an
// ingest, is unreachable, or failed) leaves the row open with a backoff.
package toolkitcleanup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/platformops"
)

// ErrRowNotFound reports a Resume for a journal id that has no row.
var ErrRowNotFound = errors.New("toolkitcleanup: journal row does not exist")

// Remover deletes what one application's engine holds for a toolkit. It is
// satisfied by *platformops.Client; the errors it returns are platformops's
// (ErrRetryLater, ErrNotSupported, ErrNotAllowed, ErrConfig).
type Remover interface {
	DeleteToolkitData(ctx context.Context, projectID, toolkitID int64) (platformops.ToolkitDeletion, error)
}

// The outcomes a closed row records (migration shared/0163).
const (
	OutcomeDeleted         = "deleted"
	OutcomeNothingToDelete = "nothing_to_delete"
	OutcomeUnsupported     = "unsupported"
	OutcomeFailedPermanent = "failed_permanent"
	OutcomeSuperseded      = "superseded"
)

// Notes of the rows closed without the engine deleting anything.
const (
	NoteUnsupported = "engine does not support deletion"
	noteSuperseded  = "a toolkit with this id exists again; nothing was touched"
)

// The last_error classes a failed run records (never the raw error: it can
// carry an address).
const (
	lastErrorRetryLater  = "engine holds the toolkit's data or is unavailable; retry"
	lastErrorFailed      = "engine call failed; retry"
	lastErrorCheckFailed = "could not check whether the toolkit exists again; retry"
	lastErrorNotWired    = "no client for the application on the replica that claimed the row"
	lastErrorInterrupted = "interrupted"
)

// runTimeout bounds one run of a row: the engine call (which the client bounds
// itself, at most the facade's timeout or two minutes) and the bookkeeping
// around it. One row is one call, so no step can starve another.
// bookkeepingTimeout bounds each journal write, on a context of its own so a
// run that hit its deadline still records what it did. leaseMargin is added to
// the run bound to make the lease. Variables so a test can shrink them.
var (
	runTimeout         = 3 * time.Minute
	bookkeepingTimeout = 10 * time.Second
	leaseMargin        = time.Minute
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
	logger   *slog.Logger
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
		if app == "" || remover == nil || isNilPointer(remover) {
			return
		}
		j.removers[app] = remover
	}
}

func isNilPointer(value any) bool {
	v := reflect.ValueOf(value)
	return v.Kind() == reflect.Pointer && v.IsNil()
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
	apps := make([]string, 0, len(j.removers))
	for app := range j.removers {
		apps = append(apps, app)
	}
	sort.Strings(apps)
	return apps
}

// AppToEnqueue names the application a toolkit of the type owes a deletion to
// on THIS replica, or "" when none: the type has no engine, the application
// keeps nothing per toolkit (DeepWiki), or this replica has no client for it
// (logged at Warn: the data stays, and the log is where an operator learns it).
func (j *Journal) AppToEnqueue(ctx context.Context, projectID, toolkitID int64, toolkitType string) string {
	app := platformops.AppForToolkitType(toolkitType)
	if app == "" || !platformops.AppKeepsToolkitData(app) {
		return ""
	}
	if _, wired := j.removers[app]; !wired {
		j.logger.WarnContext(ctx, "toolkit delete: the application's engine is not configured on this process, so its data for the toolkit is not deleted",
			"app", app, "project_id", projectID, "toolkit_id", toolkitID, "toolkit_type", toolkitType)
		return ""
	}
	return app
}

// Enqueue records, in tx (the transaction that deletes the toolkit row), the
// engine-side deletion the toolkit owes. It returns the application queued, or
// "" when the type owes none here (AppToEnqueue). A row for the same toolkit
// and application is reopened: attempts, backoff and outcome reset.
func (j *Journal) Enqueue(ctx context.Context, tx pgx.Tx, projectID, toolkitID int64, toolkitType string) (string, error) {
	app := j.AppToEnqueue(ctx, projectID, toolkitID, toolkitType)
	if app == "" {
		return "", nil
	}
	if tx == nil {
		return "", errors.New("toolkitcleanup: enqueue needs the delete's transaction")
	}
	var id int64
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
    completed_at = NULL
RETURNING id`, projectID, toolkitID, app, toolkitType).Scan(&id); err != nil {
		return "", fmt.Errorf("toolkitcleanup: write cleanup journal: %w", err)
	}
	j.logger.InfoContext(ctx, "toolkit delete: engine-side deletion journaled",
		"app", app, "project_id", projectID, "toolkit_id", toolkitID, "toolkit_type", toolkitType, "journal_id", id)
	return app, nil
}

// ClaimNext claims ONE incomplete journal row older than grace, past its
// backoff and not leased, and leases it for one run. ok is false when there is
// none. One row per claim in a short FOR UPDATE SKIP LOCKED statement that
// stamps the lease, as projectprovisioning.ClaimNextDeletion does.
func (j *Journal) ClaimNext(ctx context.Context, grace time.Duration) (id int64, ok bool, err error) {
	if j.pool == nil {
		return 0, false, errors.New("toolkitcleanup: journal has no database")
	}
	if grace < 0 {
		return 0, false, errors.New("toolkitcleanup: claim needs a non-negative grace")
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
RETURNING journal.id`,
		grace.Seconds(), (runTimeout + leaseMargin).Seconds(),
	).Scan(&id); {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("toolkitcleanup: claim: %w", err)
	}
	return id, true, nil
}

// row is one journal row as a run reads it.
type row struct {
	id          int64
	projectID   int64
	toolkitID   int64
	app         string
	toolkitType string
}

// Resume runs the journal row the caller has claimed (ClaimNext). completed is
// true only when the row ended closed. A run that leaves it open is
// (false, err) with the cause, or (false, nil) when ctx ended (the lease is
// released and the attempt is not counted). A row that no longer exists is
// ErrRowNotFound.
//
// ctx is the context whose end means the process is stopping.
func (j *Journal) Resume(ctx context.Context, id int64) (bool, error) {
	if j.pool == nil {
		return false, errors.New("toolkitcleanup: journal has no database")
	}
	var (
		r         row
		completed *time.Time
	)
	switch err := j.pool.QueryRow(ctx, `
SELECT id, project_id, toolkit_id, app, toolkit_type, completed_at
FROM centry.toolkit_deletions WHERE id = $1`, id,
	).Scan(&r.id, &r.projectID, &r.toolkitID, &r.app, &r.toolkitType, &completed); {
	case errors.Is(err, pgx.ErrNoRows):
		return false, ErrRowNotFound
	case err != nil:
		return false, fmt.Errorf("toolkitcleanup: read journal row %d: %w", id, err)
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
	attrs := []any{"journal_id", r.id, "app", r.app, "project_id", r.projectID, "toolkit_id", r.toolkitID}

	exists, err := j.toolkitRowExists(ctx, r)
	switch {
	case err != nil && parent.Err() != nil:
		j.releaseInterrupted(ctx, r.id)
		return false, nil
	case err != nil:
		j.logger.ErrorContext(ctx, "toolkit delete cleanup: could not check whether the toolkit exists again; the journal retries it", append(attrs, "err", err)...)
		return false, j.fail(ctx, r.id, lastErrorCheckFailed, err)
	case exists:
		j.logger.WarnContext(ctx, "toolkit delete cleanup superseded: a toolkit with this id exists again, so nothing is deleted", attrs...)
		return j.close(ctx, r.id, OutcomeSuperseded, noteSuperseded), nil
	}

	outcome := j.deleteInEngine(ctx, r)
	switch {
	case outcome.kind != outcomeDone && parent.Err() != nil:
		// The process is stopping: this is not the row's failure.
		j.releaseInterrupted(ctx, r.id)
		return false, nil
	case outcome.kind == outcomeNotWired:
		// Pending, not failed: the row waits for a replica with the client.
		// It still backs off, so an unwired replica does not spin on it.
		return false, j.fail(ctx, r.id, lastErrorNotWired, outcome.cause)
	case outcome.kind == outcomeRetry:
		return false, j.fail(ctx, r.id, outcome.lastError, outcome.cause)
	}
	return j.close(ctx, r.id, outcome.result, outcome.note), nil
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
	remover, wired := j.removers[r.app]
	if !wired {
		j.logger.WarnContext(ctx, "toolkit delete cleanup: this replica has no client for the application; the row is left for one that has", attrs...)
		return outcome{kind: outcomeNotWired, cause: fmt.Errorf("toolkitcleanup: no %s client on this replica", r.app)}
	}
	deletion, err := remover.DeleteToolkitData(ctx, r.projectID, r.toolkitID)
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
		j.logger.ErrorContext(ctx, "toolkit delete cleanup FAILED PERMANENTLY: the engine refused this process or the request is invalid; the toolkit's data stays in the engine until an operator intervenes",
			append(attrs, "err", err)...)
		return outcome{kind: outcomeDone, result: OutcomeFailedPermanent, note: "failed permanently: " + err.Error()}
	case errors.Is(err, platformops.ErrRetryLater):
		j.logger.WarnContext(ctx, "toolkit delete cleanup: the engine could not delete the toolkit's data now; the journal retries it", append(attrs, "err", err)...)
		return outcome{kind: outcomeRetry, lastError: lastErrorRetryLater, cause: err}
	}
	j.logger.ErrorContext(ctx, "toolkit delete cleanup: the engine call failed; the journal retries it", append(attrs, "err", err)...)
	return outcome{kind: outcomeRetry, lastError: lastErrorFailed, cause: err}
}

// toolkitRowExists reports whether a toolkit with the row's id exists again in
// its project (the id came back: a restored database). A project with no row,
// or a tenant schema with no elitea_tools table, has no such toolkit.
func (j *Journal) toolkitRowExists(ctx context.Context, r row) (bool, error) {
	var projectExists bool
	if err := j.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM centry.project WHERE id = $1)`, r.projectID,
	).Scan(&projectExists); err != nil {
		return false, fmt.Errorf("look for the project: %w", err)
	}
	if !projectExists {
		return false, nil
	}
	schema, err := tenantschema.QuoteInt(r.projectID)
	if err != nil {
		return false, err
	}
	var table bool
	if err := j.pool.QueryRow(ctx,
		`SELECT to_regclass($1) IS NOT NULL`, schema+".elitea_tools",
	).Scan(&table); err != nil {
		return false, fmt.Errorf("look for the tenant tools table: %w", err)
	}
	if !table {
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

// bookkeepingContext is the context for one journal write: detached from the
// run's (which may have ended at its deadline) with a short bound of its own.
func bookkeepingContext(runCtx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(runCtx), bookkeepingTimeout)
}

// close ends a row with its outcome and note, the lease released. It reports
// whether the row is closed; a failed write is logged and the row is worked
// again by a later run (the engine call is idempotent).
func (j *Journal) close(runCtx context.Context, id int64, result, note string) bool {
	ctx, cancel := bookkeepingContext(runCtx)
	defer cancel()
	if _, err := j.pool.Exec(ctx, `
UPDATE centry.toolkit_deletions
SET outcome = $2, note = $3, attempts = attempts + 1, last_error = NULL,
    completed_at = clock_timestamp(), claimed_until = NULL
WHERE id = $1`, id, result, note); err != nil {
		j.logger.WarnContext(ctx, "could not close a toolkit cleanup journal row; a later run repeats the (idempotent) call",
			"journal_id", id, "err", err)
		return false
	}
	return true
}

// fail closes one failed run: attempts counted, last_error set to its class,
// next_attempt_at backed off by the number of runs so far, lease released. It
// returns cause wrapped for the reconciler's log.
func (j *Journal) fail(runCtx context.Context, id int64, lastError string, cause error) error {
	ctx, cancel := bookkeepingContext(runCtx)
	defer cancel()
	if _, err := j.pool.Exec(ctx, `
UPDATE centry.toolkit_deletions
SET attempts = attempts + 1,
    last_error = $2,
    next_attempt_at = clock_timestamp() + make_interval(secs => LEAST($3::float8 * power(2, LEAST(attempts, 30)), $4::float8)),
    claimed_until = NULL
WHERE id = $1 AND completed_at IS NULL`,
		id, lastError, retryBase.Seconds(), retryMax.Seconds()); err != nil {
		j.logger.WarnContext(ctx, "could not record a failed toolkit cleanup run; the lease expires on its own",
			"journal_id", id, "err", err)
	}
	return fmt.Errorf("toolkitcleanup: %s: %w", lastError, cause)
}

// releaseInterrupted ends a run the process stopped: the lease is released and
// neither attempts nor the backoff move.
func (j *Journal) releaseInterrupted(runCtx context.Context, id int64) {
	ctx, cancel := bookkeepingContext(runCtx)
	defer cancel()
	if _, err := j.pool.Exec(ctx, `
UPDATE centry.toolkit_deletions
SET last_error = $2, claimed_until = NULL
WHERE id = $1 AND completed_at IS NULL`, id, lastErrorInterrupted); err != nil {
		j.logger.WarnContext(ctx, "could not release the lease of an interrupted toolkit cleanup run; it expires on its own",
			"journal_id", id, "err", err)
	}
}
