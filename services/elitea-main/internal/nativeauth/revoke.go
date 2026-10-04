package nativeauth

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// RevokeUserSessions revokes every live native device session of one user
// inside the CALLER's transaction, so the revocation commits or rolls back
// with the write that caused it. It returns how many families it revoked.
//
// reason is normally ReasonUserDeactivated; revokedBy is the acting operator's
// user id, or nil for an identity-provider push.
//
// Revocation is permanent: reactivating the user resurrects no device. The
// read-time owner check in AccessValidator and in Refresh is the backstop for a
// write path that forgot to call this.
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
	return count, nil
}
