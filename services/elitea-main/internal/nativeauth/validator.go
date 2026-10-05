package nativeauth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// lastSeenTouchInterval bounds how often a request writes last_seen_at, as
// browsersession.TouchInterval does for browser sessions.
const lastSeenTouchInterval = time.Minute

// AccessValidator authenticates an `elnat_` access token in one round trip.
//
// Outcomes:
//   - no row, or the access token expired: auth.ErrCredentialRejected — the
//     client refreshes;
//   - the family is revoked, its anchor is gone, its owner is suspended or
//     deleted, or its absolute cap passed: auth.ErrDeviceRevoked (which wraps
//     ErrCredentialRejected) — the client wipes;
//   - any other database error: auth.ErrCredentialValidationUnavailable — 503.
//
// On success it returns exactly the principal LocalValidator returns for a
// personal access token (AuthType "token", TokenID = the anchor), so the
// principal re-check, legacy RBAC and the edge kernel accept it unchanged.
type AccessValidator struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewAccessValidator builds the validator.
func NewAccessValidator(pool *pgxpool.Pool) *AccessValidator {
	return &AccessValidator{pool: pool, now: time.Now}
}

// SetClock replaces the validator's clock. Tests only.
func (v *AccessValidator) SetClock(now func() time.Time) { v.now = now }

// ValidateToken implements the middleware's TokenValidator.
func (v *AccessValidator) ValidateToken(ctx context.Context, token string) (auth.User, error) {
	if v == nil || v.pool == nil {
		return auth.User{}, fmt.Errorf("%w: native token store is not configured", auth.ErrCredentialValidationUnavailable)
	}
	if !WellFormed(token, PrefixAccessToken) {
		return auth.User{}, fmt.Errorf("%w: malformed native access token", auth.ErrCredentialRejected)
	}
	var (
		sessionID          string
		clientID           string
		userID             int64
		tokenID            *int64
		revokedAt          *time.Time
		expiresAt          *time.Time
		lastSeenAt         time.Time
		accessExpiresAt    time.Time
		ownerActive        bool
		email              string
		projectID          *int32
		boundProjectActive *bool
	)
	err := v.pool.QueryRow(ctx, `
		SELECT s.id::text, s.client_id, s.user_id, s.token_id, s.revoked_at, s.expires_at, s.last_seen_at,
		       a.expires_at,
		       (owner.id IS NOT NULL AND owner.suspended = false),
		       COALESCE(owner.email, ''),
		       binding.project_id,
		       (bound_project.suspended IS FALSE AND bound_project.create_success IS TRUE)
		FROM elitea_auth.native_access_tokens AS a
		JOIN elitea_auth.native_sessions AS s ON s.id = a.session_id
		LEFT JOIN public.auth_core__user AS owner ON owner.id = s.user_id
		LEFT JOIN elitea_identity.token_project_binding AS binding ON binding.token_id = s.token_id
		LEFT JOIN centry.project AS bound_project ON bound_project.id = binding.project_id
		WHERE a.token_hash = $1`, HashSecret(token)).Scan(
		&sessionID, &clientID, &userID, &tokenID, &revokedAt, &expiresAt, &lastSeenAt,
		&accessExpiresAt, &ownerActive, &email, &projectID, &boundProjectActive)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return auth.User{}, contextErr
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return auth.User{}, fmt.Errorf("%w: native access token not found", auth.ErrCredentialRejected)
		}
		return auth.User{}, fmt.Errorf("%w: native token lookup failed: %w", auth.ErrCredentialValidationUnavailable, err)
	}
	now := v.now()
	if revokedAt != nil || tokenID == nil || !ownerActive || (expiresAt != nil && !now.Before(*expiresAt)) {
		return auth.User{}, auth.ErrDeviceRevoked
	}
	if !now.Before(accessExpiresAt) {
		return auth.User{}, fmt.Errorf("%w: native access token expired", auth.ErrCredentialRejected)
	}
	if *tokenID <= 0 || userID <= 0 {
		return auth.User{}, fmt.Errorf("%w: native principal has invalid identity data", auth.ErrCredentialValidationUnavailable)
	}
	if now.Sub(lastSeenAt) > lastSeenTouchInterval {
		// Non-fatal, like browsersession's touch: a failed write costs a
		// stale "last seen" in the device list, never a refused request.
		_, _ = v.pool.Exec(ctx, `
			UPDATE elitea_auth.native_sessions SET last_seen_at = $2
			WHERE id = $1 AND last_seen_at < $2`, sessionID, now.UTC())
	}

	user := auth.User{
		ID:       formatID(userID),
		UserID:   formatID(userID),
		TokenID:  formatID(*tokenID),
		Email:    email,
		AuthType: "token",
		// The one producer of NativeClientID: read from the session row.
		NativeClientID: clientID,
	}
	if projectID != nil && *projectID > 0 {
		bound := int64(*projectID)
		user.TokenProjectID = &bound
		active := boundProjectActive != nil && *boundProjectActive
		user.TokenProjectActive = &active
	}
	return user, nil
}

// ClientID resolves the registered client of an `elnat_` access token without
// authenticating it: no revocation, expiry or owner check and no last-seen
// write. It exists for one caller, the minimum-version gate
// (apimw.ClientVersion), on a request whose principal was established by the
// edge's forwarded identity rather than by this validator, so the principal
// carries no NativeClientID although the bearer is a native token. ok is
// false for an unknown or malformed token.
func (v *AccessValidator) ClientID(ctx context.Context, token string) (string, bool, error) {
	if v == nil || v.pool == nil || !WellFormed(token, PrefixAccessToken) {
		return "", false, nil
	}
	var clientID string
	err := v.pool.QueryRow(ctx, `
		SELECT s.client_id
		FROM elitea_auth.native_access_tokens AS a
		JOIN elitea_auth.native_sessions AS s ON s.id = a.session_id
		WHERE a.token_hash = $1`, HashSecret(token)).Scan(&clientID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("nativeauth: resolve access token client: %w", err)
	}
	return clientID, true, nil
}
