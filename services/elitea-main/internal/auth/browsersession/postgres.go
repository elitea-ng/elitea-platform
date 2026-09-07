package browsersession

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
			created_at, last_seen_at, expires_at, idle_timeout_seconds
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		session.ID, session.UserID, session.Email, session.Provider,
		session.ProviderSessionIndex, session.CreatedAt, session.LastSeenAt,
		session.ExpiresAt, int32(session.IdleTimeout/time.Second),
	)
	if err != nil {
		return fmt.Errorf("browsersession: insert session: %w", err)
	}
	return nil
}

func (s *PostgresStore) Get(ctx context.Context, id string) (Session, error) {
	var (
		session Session
		idle    int32
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, email, provider, provider_session_index,
		       created_at, last_seen_at, expires_at, idle_timeout_seconds, revoked_at
		FROM elitea_auth.browser_sessions
		WHERE id = $1`, id,
	).Scan(
		&session.ID, &session.UserID, &session.Email, &session.Provider,
		&session.ProviderSessionIndex, &session.CreatedAt, &session.LastSeenAt,
		&session.ExpiresAt, &idle, &session.RevokedAt,
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
	return session, nil
}

func (s *PostgresStore) Touch(ctx context.Context, id string, seenAt time.Time) error {
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
	_, err := s.pool.Exec(ctx, `
		UPDATE elitea_auth.browser_sessions
		SET revoked_at = $2
		WHERE id = $1 AND revoked_at IS NULL`, id, revokedAt)
	if err != nil {
		return fmt.Errorf("browsersession: revoke session: %w", err)
	}
	return nil
}

func (s *PostgresStore) RevokeAllForUser(
	ctx context.Context, userID int64, revokedAt time.Time,
) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE elitea_auth.browser_sessions
		SET revoked_at = $2
		WHERE user_id = $1 AND revoked_at IS NULL`, userID, revokedAt)
	if err != nil {
		return 0, fmt.Errorf("browsersession: revoke sessions of user: %w", err)
	}
	return tag.RowsAffected(), nil
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
