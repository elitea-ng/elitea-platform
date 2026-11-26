package adminaccess

// An elevation ends with its session and with its person (ADR-0059 decision
// 5, "Bound to one session", "A sealed credential does not outlive its
// person"; decision 3, "A refused deprovisioning still ends the access";
// story R2.11, issue 2525).
//
// Logout ends the elevation of THAT session (EndSessionElevations, called by
// the browser session store in the transaction of the revocation). A
// suspension, a deletion and a SCIM deactivation end every elevation of the
// person and revoke the sealed credentials bound to them, in the caller's
// transaction (RevokePersonAccess, the wrapper of the ADR-0025 deactivation
// hook nativeauth.RevokeUserSessions that every such write calls; the guards
// of internal/nativeauth fail the build when one does not). The refused
// deprovisioning of the last administrator calls the same wrapper.
//
// Every end goes through EndElevation, so each writes its `ended` event with
// the row and moves the elevation to review_pending.
//
// THE ONE PLACE WHERE AUDIT DOES NOT FAIL CLOSED. An end whose event cannot
// be written must not undo the logout or the suspension that caused it:
// rolling back a session revocation because of an audit append would leave a
// person signed in. So each end runs on a savepoint (EndElevation's own) and
// a failure is logged and left: the revocation commits, the elevation already
// grants nothing (its session is revoked, or its person cannot sign in), and
// the reconciler (reconciler.go, endOrphaned) records the end with its event
// on its next pass. A sealed credential whose credential_revoked event cannot
// be written is still revoked, without the event, for the same reason: a
// person who has left must not keep an envelope that works.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/nativeauth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// PersonEndAppenderSource is the source of the events a person-level end
// writes with its default appender.
const PersonEndAppenderSource = "elitea-main/access-end"

// EndSessionElevations ends every open elevation bound to the browser session
// sessionRef, with reason (logout or session_revoked), on tx. It answers how
// many it ended; a failure is logged, never returned (see the file comment).
func EndSessionElevations(ctx context.Context, tx pgx.Tx, appender securityaudit.Appender,
	sessionRef int64, reason string, at time.Time,
) int {
	if sessionRef <= 0 {
		return 0
	}
	return endOpenElevations(ctx, tx, appender, `browser_session_ref = $1`, sessionRef,
		ElevationEnd{Reason: reason, At: at})
}

// LockSessionElevations takes the row locks of the open elevations bound to
// the browser sessions a revocation is about to end (the session id
// sessionID, or every session of userID when sessionID is empty), BEFORE the
// session rows are updated. The reconciler locks an elevation row and then
// updates its sealed session row, so a revocation that locked the session
// first could deadlock with it; taking the elevation first is the same
// order. A failure is logged, never returned: a logout is never refused for
// the ledger's sake (R2.11), and EndSessionElevations runs anyway.
func LockSessionElevations(ctx context.Context, tx pgx.Tx, sessionID string, userID int64) {
	if tx == nil || (sessionID == "" && userID <= 0) {
		return
	}
	if present, err := relationPresent(ctx, tx, "elitea_auth.admin_elevations"); err != nil || !present {
		if err != nil {
			slog.ErrorContext(ctx, "the elevation ledger could not be looked up", "err", err)
		}
		return
	}
	// A savepoint, so a failed lock leaves the revocation's transaction
	// usable.
	err := withSavepoint(ctx, tx, func(sp pgx.Tx) error {
		_, err := sp.Exec(ctx, `
SELECT elevation.id
FROM elitea_auth.admin_elevations elevation
JOIN elitea_auth.browser_sessions session ON session.ref = elevation.browser_session_ref
WHERE elevation.ended_at IS NULL AND session.revoked_at IS NULL
  AND (($1 <> '' AND session.id = $1) OR ($1 = '' AND session.user_id = $2))
ORDER BY elevation.id
FOR UPDATE OF elevation`, sessionID, userID)
		return err
	})
	if err != nil {
		slog.ErrorContext(ctx, "the elevations of the revoked sessions could not be locked first", "err", err)
	}
}

// EndPersonElevations ends every open elevation of the person userID, with
// reason, on tx; actorUserID is who suspended or deleted them (0 for an
// identity provider push). It answers how many it ended; a failure is
// logged, never returned.
func EndPersonElevations(ctx context.Context, tx pgx.Tx, appender securityaudit.Appender,
	userID int64, reason string, actorUserID int64, at time.Time,
) int {
	return endOpenElevations(ctx, tx, appender, `user_id = $1`, userID,
		ElevationEnd{Reason: reason, At: at, ActorUserID: actorUserID})
}

func endOpenElevations(ctx context.Context, tx pgx.Tx, appender securityaudit.Appender,
	where string, value int64, end ElevationEnd,
) int {
	if tx == nil || value <= 0 {
		return 0
	}
	if present, err := relationPresent(ctx, tx, "elitea_auth.admin_elevations"); err != nil || !present {
		if err != nil {
			slog.ErrorContext(ctx, "the elevation ledger could not be looked up", "err", err)
		}
		return 0
	}
	var ids []uuid.UUID
	if err := withSavepoint(ctx, tx, func(sp pgx.Tx) error {
		rows, err := sp.Query(ctx, `
SELECT id FROM elitea_auth.admin_elevations
WHERE `+where+` AND started_at IS NOT NULL AND ended_at IS NULL
ORDER BY started_at, id
FOR UPDATE`, value)
		if err != nil {
			return err
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		return err
	}); err != nil {
		slog.ErrorContext(ctx, "the open elevations could not be read; the reconciler ends them",
			"end_reason", end.Reason, "err", err)
		return 0
	}
	ended := 0
	for _, id := range ids {
		end.ID = id
		_, changed, err := EndElevation(ctx, tx, appender, end)
		if err != nil {
			slog.ErrorContext(ctx, "an elevation end could not be recorded; access is already ended "+
				"and the reconciler records the end", "elevation_id", id.String(), "end_reason", end.Reason, "err", err)
			continue
		}
		if changed {
			ended++
		}
	}
	return ended
}

// RevokePersonSealedCredentials revokes every unused, unrevoked sealed
// credential bound to userID, each with its credential_revoked event, on tx.
// A credential whose event cannot be written is revoked without it. It
// answers how many it revoked.
func RevokePersonSealedCredentials(ctx context.Context, tx pgx.Tx, appender securityaudit.Appender,
	userID, actorUserID int64, at time.Time,
) (int, error) {
	if tx == nil || userID <= 0 {
		return 0, nil
	}
	// A database built without the break-glass tables (a projection fixture)
	// holds no credential to revoke.
	if present, err := relationPresent(ctx, tx, "elitea_auth.sealed_credentials"); err != nil || !present {
		return 0, err
	}
	rows, err := tx.Query(ctx, `
SELECT id FROM elitea_auth.sealed_credentials
WHERE user_id = $1 AND used_at IS NULL AND revoked_at IS NULL
ORDER BY issued_at, id
FOR UPDATE`, userID)
	if err != nil {
		return 0, fmt.Errorf("read the sealed credentials of the person: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return 0, fmt.Errorf("read the sealed credentials of the person: %w", err)
	}
	source := securityaudit.ElevationSourceSystem
	revoked := 0
	for _, id := range ids {
		_, changed, err := RevokeSealedCredential(ctx, tx, appender, SealedRevoke{
			ID: id, At: at, Source: source, ActorUserID: actorUserID,
		})
		if err != nil {
			slog.ErrorContext(ctx, "a sealed credential of a person who lost access was revoked without its event",
				"credential_id", id.String(), "err", err)
			if _, err := tx.Exec(ctx, `
UPDATE elitea_auth.sealed_credentials SET revoked_at = GREATEST($2::timestamptz, issued_at)
WHERE id = $1 AND revoked_at IS NULL AND used_at IS NULL`, id, at.UTC()); err != nil {
				return revoked, fmt.Errorf("revoke a sealed credential of the person: %w", err)
			}
			changed = true
		}
		if changed {
			revoked++
		}
	}
	return revoked, nil
}

// PersonAccessEnd says why a person loses access.
type PersonAccessEnd struct {
	// ElevationEnd is the end reason of the person's elevations:
	// user_suspended, user_deleted or scim_deactivated.
	ElevationEnd string
	// RevokedBy is the acting operator, nil for an identity provider push.
	RevokedBy *int64
	// Appender writes the events; nil takes the default appender.
	Appender securityaudit.Appender
	// At is the time; zero takes the wall clock.
	At time.Time
}

// RevokePersonAccess is the deactivation hook of a suspension, a deletion and
// a SCIM deactivation: the person's native device sessions and browser
// sessions are revoked (nativeauth.RevokeUserSessions), every open elevation
// of the person is ended, and the sealed credentials bound to the person are
// revoked, all in the caller's transaction. It answers how many native
// families it revoked. Only the session revocation and the credential
// revocation fail it; an elevation end that cannot be recorded never does.
func RevokePersonAccess(ctx context.Context, tx pgx.Tx, userID int64, end PersonAccessEnd) (int64, error) {
	if !knownElevationEndReason(end.ElevationEnd) {
		return 0, fmt.Errorf("%w %q", ErrElevationEndReason, end.ElevationEnd)
	}
	families, err := nativeauth.RevokeUserSessions(ctx, tx, userID, nativeauth.ReasonUserDeactivated, end.RevokedBy)
	if err != nil {
		return 0, err
	}
	appender := end.Appender
	if appender == nil {
		appender = defaultPersonEndAppender()
	}
	at := end.At
	if at.IsZero() {
		at = time.Now()
	}
	var actor int64
	if end.RevokedBy != nil {
		actor = *end.RevokedBy
	}
	if _, err := RevokePersonSealedCredentials(ctx, tx, appender, userID, actor, at); err != nil {
		return 0, err
	}
	EndPersonElevations(ctx, tx, appender, userID, end.ElevationEnd, actor, at)
	return families, nil
}

// relationPresent reports whether the table exists. Every deployment has the
// break-glass tables (the shared corpus creates them); a fixture built from a
// schema projection may not.
func relationPresent(ctx context.Context, q Querier, name string) (bool, error) {
	var present bool
	if err := q.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&present); err != nil {
		return false, fmt.Errorf("look up %s: %w", name, err)
	}
	return present, nil
}

func defaultPersonEndAppender() securityaudit.Appender {
	appender, err := securityaudit.NewPostgresAppender(PersonEndAppenderSource)
	if err != nil {
		return securityaudit.FailingAppender{Err: err}
	}
	return appender
}
