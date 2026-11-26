package adminaccess

// The elevation reconciler (ADR-0059 decision 5, "Path 1" step 2 and "Review
// after use", decision 8, the actions `expired` and `request_expired`; story
// R2.16, issue 2609).
//
// Access ends by a predicate with no job running (ElevationRecord.ActiveAt;
// the resolver's query): the reconciler NEVER decides access. What it does is
// write the record that does not write itself:
//
//   - an elevation that ran out with nobody ending it is ended with the end
//     reason `expired` at its expiry, gets its `expired` admin.elevation
//     event and moves to review_pending (EndElevation, so the row and its
//     event are one savepoint);
//   - a request nobody decided within the request lifetime of the
//     break_glass section is decided `request_expired`, with its
//     `request_expired` event. An approval of it afterwards is refused by
//     the ledger's trigger: the decision is written once.
//
// Each row is its own transaction, so one row whose event cannot be written
// stays as it was and is retried on the next pass, and the others still
// complete. Every write is `UPDATE ... WHERE <state> RETURNING`, behind a
// `SELECT ... FOR UPDATE SKIP LOCKED` of the same row: two reconcilers
// running at once (two replicas, or the routine registry's local fallback
// during an outage) each take a row the other does not hold, and a row
// already reconciled matches no state and writes nothing. The time is the
// injected clock, never the wall clock, so a test clock reaches every row.
//
// It also ends the open elevations whose session was revoked or whose
// person was suspended or deleted when the revocation could not record the
// end itself (endOrphaned; R2.11, issue 2525).
//
// The overdue pass of R2.9b (issue 2603) is added here later.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// ElevationReconcilerRoutineID is the routine registry row of the reconciler.
const ElevationReconcilerRoutineID = "main.elevation_reconciler"

// ElevationReconcilerDefaultSchedule is the reconciler's default interval.
const ElevationReconcilerDefaultSchedule = "1m"

// defaultReconcileBatch bounds the rows one pass takes of each kind; the
// next pass takes the rest.
const defaultReconcileBatch = 500

// ReconcilerDB is what the reconciler needs from the database: a
// *pgxpool.Pool.
type ReconcilerDB interface {
	Beginner
	Querier
}

// ElevationReconciler writes the expiry of elevations and requests.
type ElevationReconciler struct {
	db       ReconcilerDB
	appender securityaudit.Appender
	// requestLifetime answers the request lifetime of the break_glass
	// section (platformconfig.BreakGlassSettings.RequestLifetime).
	requestLifetime func(context.Context) (time.Duration, error)
	now             func() time.Time
	batch           int
}

// ReconcileResult counts what one pass wrote.
type ReconcileResult struct {
	// Expired is the elevations recorded as expired.
	Expired int
	// RequestsExpired is the requests recorded as request_expired.
	RequestsExpired int
	// Orphaned is the open elevations recorded as ended because their
	// session was revoked or their person suspended or deleted, when the
	// revocation could not record the end itself (R2.11).
	Orphaned int
}

// NewElevationReconciler builds the reconciler. Every argument is required:
// the ledger takes no write without an appender to record it, and no time
// that is not the injected clock.
func NewElevationReconciler(db ReconcilerDB, appender securityaudit.Appender,
	requestLifetime func(context.Context) (time.Duration, error), now func() time.Time,
) (*ElevationReconciler, error) {
	switch {
	case db == nil:
		return nil, errors.New("elevation reconciler: no database")
	case appender == nil:
		return nil, ErrElevationNoAppender
	case requestLifetime == nil:
		return nil, errors.New("elevation reconciler: no request lifetime")
	case now == nil:
		return nil, errors.New("elevation reconciler: no clock")
	}
	return &ElevationReconciler{db: db, appender: appender, requestLifetime: requestLifetime, now: now,
		batch: defaultReconcileBatch}, nil
}

// Pass reconciles every elevation past its expiry and every request past its
// lifetime, one transaction per row. A row that fails is left for the next
// pass; the error joins every failure.
func (r *ElevationReconciler) Pass(ctx context.Context) (ReconcileResult, error) {
	var result ReconcileResult
	now := r.now().UTC()

	expired, err := r.expireElevations(ctx, now)
	result.Expired = expired
	var errs []error
	if err != nil {
		errs = append(errs, err)
	}
	orphaned, err := r.endOrphaned(ctx, now)
	result.Orphaned = orphaned
	if err != nil {
		errs = append(errs, err)
	}
	lifetime, err := r.requestLifetime(ctx)
	if err == nil && lifetime <= 0 {
		err = fmt.Errorf("request lifetime %s is not positive", lifetime)
	}
	if err != nil {
		errs = append(errs, fmt.Errorf("elevation reconciler: read the request lifetime: %w", err))
		return result, errors.Join(errs...)
	}
	requests, err := r.expireRequests(ctx, now, lifetime)
	result.RequestsExpired = requests
	if err != nil {
		errs = append(errs, err)
	}
	return result, errors.Join(errs...)
}

// expireElevations ends every started, unended elevation whose expiry has
// passed, with the reason `expired` and its event.
func (r *ElevationReconciler) expireElevations(ctx context.Context, now time.Time) (int, error) {
	ids, err := r.candidates(ctx, `
SELECT id FROM elitea_auth.admin_elevations
WHERE started_at IS NOT NULL AND ended_at IS NULL AND expires_at <= $1
ORDER BY expires_at, id LIMIT $2`, now, r.batch)
	if err != nil {
		return 0, fmt.Errorf("elevation reconciler: list the expired elevations: %w", err)
	}
	var errs []error
	expired := 0
	for _, id := range ids {
		changed, err := r.inTx(ctx, func(tx pgx.Tx) (bool, error) {
			locked, err := lockRow(ctx, tx, `
SELECT id FROM elitea_auth.admin_elevations
WHERE id = $1 AND started_at IS NOT NULL AND ended_at IS NULL AND expires_at <= $2
FOR UPDATE SKIP LOCKED`, id, now)
			if err != nil || !locked {
				return false, err
			}
			_, changed, err := EndElevation(ctx, tx, r.appender, ElevationEnd{
				ID: id, Reason: ElevationEndExpired, At: now, Source: securityaudit.ElevationSourceSystem,
			})
			return changed, err
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("elevation reconciler: expire elevation %s: %w", id, err))
			continue
		}
		if changed {
			expired++
		}
	}
	return expired, errors.Join(errs...)
}

// orphanedSQL lists the open, unexpired elevations whose access already
// stopped without the ledger saying so: the bound session is revoked, or the
// person is suspended or gone (R2.11; the revocation logs and leaves the end
// to this pass when its event cannot be written). The reason is the one the
// revocation would have recorded.
const orphanedSQL = `
SELECT e.id,
       CASE WHEN account.id IS NULL THEN 'user_deleted'
            WHEN account.suspended THEN 'user_suspended'
            WHEN session.end_reason = 'logout' THEN 'logout'
            ELSE 'session_revoked' END
FROM elitea_auth.admin_elevations e
LEFT JOIN public.auth_core__user account ON account.id = e.user_id
LEFT JOIN elitea_auth.browser_sessions session ON session.ref = e.browser_session_ref
WHERE e.started_at IS NOT NULL AND e.ended_at IS NULL AND e.expires_at > $1
  AND (account.id IS NULL OR account.suspended OR session.revoked_at IS NOT NULL)
ORDER BY e.started_at, e.id LIMIT $2`

// endOrphaned ends every elevation orphanedSQL lists, one transaction each.
func (r *ElevationReconciler) endOrphaned(ctx context.Context, now time.Time) (int, error) {
	rows, err := r.db.Query(ctx, orphanedSQL, now, r.batch)
	if err != nil {
		return 0, fmt.Errorf("elevation reconciler: list the orphaned elevations: %w", err)
	}
	type orphan struct {
		id     uuid.UUID
		reason string
	}
	orphans, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (orphan, error) {
		var o orphan
		return o, row.Scan(&o.id, &o.reason)
	})
	if err != nil {
		return 0, fmt.Errorf("elevation reconciler: list the orphaned elevations: %w", err)
	}
	var errs []error
	ended := 0
	for _, o := range orphans {
		changed, err := r.inTx(ctx, func(tx pgx.Tx) (bool, error) {
			locked, err := lockRow(ctx, tx, `
SELECT id FROM elitea_auth.admin_elevations
WHERE id = $1 AND started_at IS NOT NULL AND ended_at IS NULL
FOR UPDATE SKIP LOCKED`, o.id)
			if err != nil || !locked {
				return false, err
			}
			_, changed, err := EndElevation(ctx, tx, r.appender, ElevationEnd{
				ID: o.id, Reason: o.reason, At: now, Source: securityaudit.ElevationSourceSystem,
			})
			return changed, err
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("elevation reconciler: end orphaned elevation %s: %w", o.id, err))
			continue
		}
		if changed {
			ended++
		}
	}
	return ended, errors.Join(errs...)
}

// expireRequests decides every undecided request older than lifetime as
// request_expired, at the end of its lifetime, with its event.
func (r *ElevationReconciler) expireRequests(ctx context.Context, now time.Time, lifetime time.Duration) (int, error) {
	seconds := lifetime.Seconds()
	ids, err := r.candidates(ctx, `
SELECT id FROM elitea_auth.admin_elevations
WHERE decision IS NULL AND started_at IS NULL
  AND requested_at <= $1::timestamptz - make_interval(secs => $3)
ORDER BY requested_at, id LIMIT $2`, now, r.batch, seconds)
	if err != nil {
		return 0, fmt.Errorf("elevation reconciler: list the expired requests: %w", err)
	}
	var errs []error
	expired := 0
	for _, id := range ids {
		changed, err := r.inTx(ctx, func(tx pgx.Tx) (bool, error) {
			const state = `id = $1 AND decision IS NULL AND started_at IS NULL
  AND requested_at <= $2::timestamptz - make_interval(secs => $3)`
			locked, err := lockRow(ctx, tx, `SELECT id FROM elitea_auth.admin_elevations WHERE `+state+`
FOR UPDATE SKIP LOCKED`, id, now, seconds)
			if err != nil || !locked {
				return false, err
			}
			_, changed, err := expireRequest(ctx, tx, r.appender, id, now, lifetime)
			return changed, err
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("elevation reconciler: expire request %s: %w", id, err))
			continue
		}
		if changed {
			expired++
		}
	}
	return expired, errors.Join(errs...)
}

// expireRequest decides the request id `request_expired` at the end of its
// lifetime, with its `request_expired` event, on tx, when it is still
// undecided and unstarted and its lifetime has passed at now. It reports
// whether it wrote; a request in any other state is left as it is. The
// caller holds the row (FOR UPDATE). The reconciler and the decision routes
// (an answer that comes too late) both write the expiry here, so it is
// recorded the same way whoever sees it first.
func expireRequest(ctx context.Context, tx pgx.Tx, appender securityaudit.Appender, id uuid.UUID,
	now time.Time, lifetime time.Duration,
) (ElevationRecord, bool, error) {
	request, err := scanElevation(tx.QueryRow(ctx, `
UPDATE elitea_auth.admin_elevations
SET decision = 'request_expired', decided_at = requested_at + make_interval(secs => $3)
WHERE id = $1 AND decision IS NULL AND started_at IS NULL
  AND requested_at <= $2::timestamptz - make_interval(secs => $3)
RETURNING `+elevationColumns, id, now.UTC(), lifetime.Seconds()))
	if errors.Is(err, pgx.ErrNoRows) {
		return ElevationRecord{}, false, nil
	}
	if err != nil {
		return ElevationRecord{}, false, err
	}
	if err := AppendElevationEvent(ctx, tx, appender, ElevationEvent{
		Action:    securityaudit.ElevationRequestExpired,
		Source:    securityaudit.ElevationSourceSystem,
		Elevation: request,
	}); err != nil {
		return ElevationRecord{}, false, err
	}
	return request, true, nil
}

// candidates answers the ids a query lists.
func (r *ElevationReconciler) candidates(ctx context.Context, sql string, args ...any) ([]uuid.UUID, error) {
	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// inTx runs one row's reconciliation in its own transaction: committed when
// write succeeds, rolled back otherwise.
func (r *ElevationReconciler) inTx(ctx context.Context, write func(pgx.Tx) (bool, error)) (bool, error) {
	var changed bool
	err := pgx.BeginFunc(ctx, r.db, func(tx pgx.Tx) error {
		var err error
		changed, err = write(tx)
		return err
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// lockRow takes the row a query names. false: another transaction holds it,
// or it no longer matches, and the caller writes nothing.
func lockRow(ctx context.Context, tx pgx.Tx, sql string, args ...any) (bool, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, sql, args...).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock the row: %w", err)
	}
	return true, nil
}
