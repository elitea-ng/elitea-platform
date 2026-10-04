package nativeauth

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// RevokeUserSessions is the DEACTIVATION HOOK (ADR-0025 WP3, coordinator
// decision 10): it revokes every live native device session of one user AND
// every live server-side browser session of that user, inside the CALLER's
// transaction, so both commit or roll back with the suspension that caused
// them. It returns how many native families it revoked.
//
// Every write that sets auth_core__user.suspended to true must call it; the
// guard test TestEverySuspensionWriteRevokesNativeSessions fails the build
// when a new write site does not.
//
// reason is normally ReasonUserDeactivated; revokedBy is the acting operator's
// user id, or nil for an identity-provider push.
//
// Revocation is permanent: reactivating the user resurrects no device and no
// browser session. The read-time owner checks (AccessValidator, Refresh, the
// principal validator) are the backstop for a write path that forgot to call
// this.
//
// The browser half closes an adjacent defect: browsersession.Manager.RevokeUser
// had no caller, so a suspended user's browser session stayed valid until
// its own expiry (the principal re-check refused each request, but the
// session row still read as live).
func RevokeUserSessions(ctx context.Context, tx pgx.Tx, userID int64, reason string, revokedBy *int64) (int64, error) {
	if tx == nil {
		return 0, fmt.Errorf("nativeauth: revoke user sessions: no transaction")
	}
	ids, err := collectIDs(ctx, tx, `
		SELECT id::text FROM elitea_auth.native_sessions
		WHERE user_id = $1 AND revoked_at IS NULL
		ORDER BY id
		FOR UPDATE`, userID)
	if err != nil {
		return 0, fmt.Errorf("nativeauth: list user sessions: %w", err)
	}
	var count int64
	for _, id := range ids {
		revoked, err := revokeFamily(ctx, tx, id, reason, revokedBy)
		if err != nil {
			return 0, fmt.Errorf("nativeauth: revoke user session: %w", err)
		}
		if revoked {
			count++
		}
	}
	// The same SQL as browsersession.PostgresStore.RevokeAllForUser, on the
	// caller's transaction.
	if _, err := tx.Exec(ctx, `
		UPDATE elitea_auth.browser_sessions
		SET revoked_at = now()
		WHERE user_id = $1 AND revoked_at IS NULL`, userID); err != nil {
		return 0, fmt.Errorf("nativeauth: revoke browser sessions: %w", err)
	}
	return count, nil
}
