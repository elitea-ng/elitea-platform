package browsersession

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// PostgresStore is elitea_auth.browser_sessions.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore builds the store. A nil pool is refused: a store that
// cannot read would answer ErrNotFound for every live session, which reads as
// "everybody is signed out" and is the worst possible failure shape here.
func NewPostgresStore(pool *pgxpool.Pool) (*PostgresStore, error) {
	if pool == nil {
		return nil, errors.New("browsersession: a database pool is required")
	}
	return &PostgresStore{pool: pool}, nil
}

func (s *PostgresStore) Insert(ctx context.Context, session Session) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO elitea_auth.browser_sessions (
			id, user_id, email, provider, provider_session_index,
			created_at, last_seen_at, expires_at, idle_timeout_seconds,
			authenticated_at, authentication_proven
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		session.ID, session.UserID, session.Email, session.Provider,
		session.ProviderSessionIndex, session.CreatedAt, session.LastSeenAt,
		session.ExpiresAt, int32(session.IdleTimeout/time.Second),
		authenticatedAtParameter(session.AuthenticatedAt),
		session.AuthenticationProven && !session.AuthenticatedAt.IsZero(),
	)
	if err != nil {
		return fmt.Errorf("browsersession: insert session: %w", err)
	}
	return nil
}

// authenticatedAtParameter writes NULL for a zero time: an unknown
// authentication time, which the reader answers as zero again.
func authenticatedAtParameter(at time.Time) any {
	if at.IsZero() {
		return nil
	}
	return at
}

func (s *PostgresStore) Get(ctx context.Context, id string) (Session, error) {
	var (
		session         Session
		idle            int32
		authenticatedAt *time.Time
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, email, provider, provider_session_index,
		       created_at, last_seen_at, expires_at, idle_timeout_seconds, revoked_at,
		       ref, ended_at, COALESCE(end_reason, ''),
		       authenticated_at, authentication_proven
		FROM elitea_auth.browser_sessions
		WHERE id = $1`, id,
	).Scan(
		&session.ID, &session.UserID, &session.Email, &session.Provider,
		&session.ProviderSessionIndex, &session.CreatedAt, &session.LastSeenAt,
		&session.ExpiresAt, &idle, &session.RevokedAt,
		&session.Ref, &session.EndedAt, &session.EndReason,
		&authenticatedAt, &session.AuthenticationProven,
	)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Session{}, ErrNotFound
	case err != nil:
		// NOT ErrNotFound. A pool timeout is not a statement that the session
		// does not exist, and reporting it as one signs out every user for as
		// long as the database is unreachable. apimw.Auth answers 503 for an
		// error it cannot classify.
		return Session{}, fmt.Errorf("browsersession: read session: %w", err)
	}
	session.IdleTimeout = time.Duration(idle) * time.Second
	// NULL is an unknown authentication time (an id_token without auth_time,
	// or a row a replica of the previous release wrote during the upgrade):
	// the zero time, which no recent-authentication check accepts. Rows that
	// existed before shared 0262 were backfilled from created_at.
	if authenticatedAt != nil {
		session.AuthenticatedAt = authenticatedAt.UTC()
	} else {
		session.AuthenticationProven = false
	}
	return session, nil
}

func (s *PostgresStore) Touch(ctx context.Context, id string, seenAt time.Time) error {
	// last_seen_at ONLY. authenticated_at (shared 0262) is the person's last
	// authentication, and a request is not one: a refresh that moved it would
	// let a session kept alive by background polling pass every "signed in
	// within the last 15 minutes" check forever.
	//
	// The WHERE clause repeats the throttle. Two replicas can decide to touch
	// the same session in the same second, and without it the later one writes
	// an older timestamp over a newer one.
	_, err := s.pool.Exec(ctx, `
		UPDATE elitea_auth.browser_sessions
		SET last_seen_at = $2
		WHERE id = $1 AND last_seen_at < $2`, id, seenAt)
	if err != nil {
		return fmt.Errorf("browsersession: touch session: %w", err)
	}
	return nil
}

func (s *PostgresStore) Revoke(ctx context.Context, id string, revokedAt time.Time) error {
	// `revoked_at IS NULL` keeps the FIRST revocation's timestamp. A logout
	// replayed by a browser retry must not rewrite when the session ended.
	// The end (shared 0200) keeps the first end too: a session that already
	// expired stays expired in its audit record.
	//
	// The elevation bound to the session ends with it, in the same
	// transaction, with the end reason `logout` (ADR-0059 R2.11). Only THAT
	// session's elevation: another session of the same person keeps its own.
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		adminaccess.LockSessionElevations(ctx, tx, id, 0)
		refs, err := revokeReturningRefs(ctx, tx, `
		UPDATE elitea_auth.browser_sessions
		SET revoked_at = $2,
		    ended_at = COALESCE(ended_at, $2),
		    end_reason = COALESCE(end_reason, 'logout')
		WHERE id = $1 AND revoked_at IS NULL
		RETURNING ref`, id, revokedAt)
		if err != nil {
			return err
		}
		for _, ref := range refs {
			adminaccess.EndSessionElevations(ctx, tx, elevationAppender(), ref, adminaccess.ElevationEndLogout, revokedAt)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("browsersession: revoke session: %w", err)
	}
	return nil
}

func (s *PostgresStore) RevokeAllForUser(
	ctx context.Context, userID int64, revokedAt time.Time,
) (int64, error) {
	// Each session's elevation ends with it, with the end reason
	// `session_revoked` (ADR-0059 R2.11).
	var revoked int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		adminaccess.LockSessionElevations(ctx, tx, "", userID)
		refs, err := revokeReturningRefs(ctx, tx, `
		UPDATE elitea_auth.browser_sessions
		SET revoked_at = $2,
		    ended_at = COALESCE(ended_at, $2),
		    end_reason = COALESCE(end_reason, 'revoked')
		WHERE user_id = $1 AND revoked_at IS NULL
		RETURNING ref`, userID, revokedAt)
		if err != nil {
			return err
		}
		revoked = int64(len(refs))
		for _, ref := range refs {
			adminaccess.EndSessionElevations(ctx, tx, elevationAppender(), ref,
				adminaccess.ElevationEndSessionRevoked, revokedAt)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("browsersession: revoke sessions of user: %w", err)
	}
	return revoked, nil
}

// revokeReturningRefs runs a revocation that returns the refs it revoked.
func revokeReturningRefs(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]int64, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// elevationAppender writes the `ended` events of the elevations a
// revocation ends. A failed append never fails the revocation
// (adminaccess.EndSessionElevations).
func elevationAppender() securityaudit.Appender {
	appender, err := securityaudit.NewPostgresAppender(adminaccess.PersonEndAppenderSource)
	if err != nil {
		return securityaudit.FailingAppender{Err: err}
	}
	return appender
}

// MarkEnded stamps a session ended at its deadline, once (EndMarker). A row
// already ended or revoked keeps its first end.
func (s *PostgresStore) MarkEnded(ctx context.Context, id, reason string, endedAt time.Time) error {
	switch reason {
	case EndIdle, EndAbsolute:
	default:
		return fmt.Errorf("browsersession: %q is not an expiry", reason)
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE elitea_auth.browser_sessions
		SET ended_at = $3, end_reason = $2
		WHERE id = $1 AND ended_at IS NULL AND revoked_at IS NULL`, id, reason, endedAt)
	if err != nil {
		return fmt.Errorf("browsersession: record the session end: %w", err)
	}
	return nil
}

// DeleteExpired removes rows nothing can ever authenticate with again.
//
// It is a SWEEP, not part of validation: a row past its absolute deadline
// already fails Usable, so this only bounds the table. `grace` keeps a
// just-expired session readable for a short while, so an operator answering
// "why was I signed out" still has the row.
func (s *PostgresStore) DeleteExpired(ctx context.Context, now time.Time, grace time.Duration) (int64, error) {
	if grace < 0 {
		grace = 0
	}
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM elitea_auth.browser_sessions
		WHERE expires_at < $1`, now.Add(-grace))
	if err != nil {
		return 0, fmt.Errorf("browsersession: sweep expired sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}
