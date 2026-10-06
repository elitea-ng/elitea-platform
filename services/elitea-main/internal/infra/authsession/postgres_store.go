// Package authsession persists Form browser authentication sessions in
// PostgreSQL (elitea_auth.form_sessions, shared migration 0145).
//
// The session ID is the browser cookie's bearer value. A row never holds it:
// every row is keyed on IDHash(id), the hex SHA-256 of the ID. PostgreSQL is
// backed up, so a dump must not hand out live sessions; a plain hash is enough
// because the ID has 256 bits of entropy.
package authsession

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	sessionstate "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/session"
)

const (
	MaxRecordBytes       = 64 << 10
	sessionIDRandomBytes = 32
	createAttempts       = 4
	// operationLimit bounds one store operation, as the Redis client's
	// three-second command limit did. An operation that runs out of it fails
	// closed with ErrUnavailable.
	operationLimit = 3 * time.Second
	// lockTimeout bounds the wait for a row lock (rotation against logout).
	// A wait past it fails closed with ErrUnavailable.
	lockTimeout = "500ms"
)

var (
	ErrInvalidConfiguration = errors.New("invalid browser session store configuration")
	ErrInvalidID            = errors.New("invalid browser session ID")
	ErrNotFound             = sessionstate.ErrNotFound
	ErrUnavailable          = errors.New("browser session store unavailable")
	ErrIDCollision          = errors.New("browser session ID collision limit reached")
	ErrRotationConflict     = errors.New("browser session changed during rotation")
)

// Config holds the two server-side lifetimes.
//
// TTL is the lifetime of an AUTHENTICATED state (one with a user ID). It is
// the cookie lifetime. PreLoginTTL is the lifetime of every other state: the
// unauthenticated session a login begin creates. It is short on purpose: a
// pre-login session is only useful for the one login transaction bound to
// it, and that transaction lives five minutes.
type Config struct {
	TTL         time.Duration
	PreLoginTTL time.Duration
}

type idGenerator func() (string, error)

// PostgresStore does not own or close the injected pool. Its methods are safe
// for concurrent use.
type PostgresStore struct {
	pool        *pgxpool.Pool
	ttl         time.Duration
	preLoginTTL time.Duration
	generate    idGenerator
}

// NewPostgresStore creates an unmounted server-side session store.
func NewPostgresStore(pool *pgxpool.Pool, config Config) (*PostgresStore, error) {
	return newPostgresStore(pool, config, randomSessionID)
}

func newPostgresStore(pool *pgxpool.Pool, config Config, generate idGenerator) (*PostgresStore, error) {
	if pool == nil || generate == nil {
		return nil, ErrInvalidConfiguration
	}
	if config.TTL < time.Millisecond || config.PreLoginTTL < time.Millisecond ||
		config.PreLoginTTL > config.TTL {
		return nil, fmt.Errorf("%w: lifetimes must be at least one millisecond, pre-login at most the full lifetime",
			ErrInvalidConfiguration)
	}
	return &PostgresStore{
		pool:        pool,
		ttl:         config.TTL,
		preLoginTTL: config.PreLoginTTL,
		generate:    generate,
	}, nil
}

// insertSQL creates a row under a fresh ID. An existing LIVE row is never
// overwritten; an expired row under the same ID is absent to every read, so
// it is replaced, exactly as an expired Redis key no longer blocked SET NX.
const insertSQL = `
INSERT INTO elitea_auth.form_sessions AS current (id_hash, record, expires_at)
VALUES ($1, $2, now() + make_interval(secs => $3::double precision / 1000))
ON CONFLICT (id_hash) DO UPDATE
    SET record = EXCLUDED.record, expires_at = EXCLUDED.expires_at
    WHERE current.expires_at <= now()`

// Create stores state under a fresh opaque ID. Existing live rows are never
// overwritten; a bounded number of random-ID collisions is retried.
func (s *PostgresStore) Create(ctx context.Context, state sessionstate.State) (string, error) {
	record, ttl, err := s.encodeForCreate(state)
	if err != nil {
		return "", err
	}
	opCtx, cancel := context.WithTimeout(ctx, operationLimit)
	defer cancel()

	for range createAttempts {
		id, err := s.generate()
		if err != nil {
			return "", fmt.Errorf("generate browser session ID: %w", err)
		}
		if !validSessionID(id) {
			return "", ErrInvalidID
		}
		tag, err := s.pool.Exec(opCtx, insertSQL, IDHash(id), record, ttl.Milliseconds())
		if err != nil {
			return "", storeError(ctx, "create", err)
		}
		if tag.RowsAffected() == 1 {
			return id, nil
		}
	}
	return "", ErrIDCollision
}

// Read returns only a supported, bounded state whose row has not expired.
//
// Read runs on every edge request that carries a Form cookie (flow.Authorize)
// and is deliberately not throttled or cached. It is one primary-key SELECT on
// the shared pool, bounded by operationLimit; a cookie that is not a canonical
// 256-bit ID is refused before the database. A live session's request then
// pays a heavier principal re-validation query on this pool anyway, and a
// well-shaped random cookie costs one index miss: what any unauthenticated
// request to a database-backed route costs. A negative cache would add a
// revocation-visibility hazard (a logout on one replica unseen on another)
// for no saving that matters; a rate limit here would turn a flood into
// sign-outs for everyone.
// Provider authentication expiration remains data for the authorization and
// logout boundaries; it does not shorten the independent server-session
// lifetime. Malformed records, unknown schema versions, and dependency
// failures fail closed.
func (s *PostgresStore) Read(ctx context.Context, id string) (sessionstate.State, error) {
	state, _, err := s.readRecord(ctx, id)
	return state, err
}

// Delete invalidates id. It is idempotent so repeated logout requests succeed.
func (s *PostgresStore) Delete(ctx context.Context, id string) error {
	if !validSessionID(id) {
		return ErrInvalidID
	}
	opCtx, cancel := context.WithTimeout(ctx, operationLimit)
	defer cancel()
	if _, err := s.pool.Exec(opCtx, `DELETE FROM elitea_auth.form_sessions WHERE id_hash = $1`, IDHash(id)); err != nil {
		return storeError(ctx, "delete", err)
	}
	return nil
}

// ConsumeForLogout atomically reads and deletes the exact session ID. Missing
// and expired IDs are idempotent zero-value successes. This operation
// linearizes against RotateAndReplace for the same ID on the row lock: if
// logout wins, rotation finds no row; if rotation wins, the stale logout finds
// no row and cannot revoke the new ID. Session-family tombstones that would
// bridge the latter case are intentionally not in this slice.
func (s *PostgresStore) ConsumeForLogout(ctx context.Context, id string) (sessionstate.State, error) {
	if !validSessionID(id) {
		return sessionstate.State{}, ErrInvalidID
	}
	opCtx, cancel := context.WithTimeout(ctx, operationLimit)
	defer cancel()
	var (
		record []byte
		live   bool
	)
	err := s.pool.QueryRow(opCtx, `
		DELETE FROM elitea_auth.form_sessions
		WHERE id_hash = $1
		RETURNING record, expires_at > now()`, IDHash(id),
	).Scan(&record, &live)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return sessionstate.State{}, nil
	case err != nil:
		return sessionstate.State{}, storeError(ctx, "consume for logout", err)
	case !live:
		// An expired row is absent; deleting it is only housekeeping.
		return sessionstate.State{}, nil
	}
	if len(record) == 0 || len(record) > MaxRecordBytes {
		return sessionstate.State{}, fmt.Errorf("%w: encoded record size is invalid", sessionstate.ErrInvalidState)
	}
	return decodeRecord(record)
}

// Rotate atomically moves the existing record and its remaining lifetime to a
// fresh ID. The old ID is invalid after success.
func (s *PostgresStore) Rotate(ctx context.Context, id string) (string, error) {
	_, record, err := s.readRecord(ctx, id)
	if err != nil {
		return "", err
	}
	return s.rotateRecord(ctx, id, record, record, 0)
}

// RotateAndReplace atomically invalidates id and installs replacement under a
// fresh ID with the replacement's full server-session lifetime.
// Authentication callbacks use this operation so an unauthenticated session
// can never become authenticated under the same browser identifier.
func (s *PostgresStore) RotateAndReplace(
	ctx context.Context,
	id string,
	replacement sessionstate.State,
) (string, error) {
	_, currentRecord, err := s.readRecord(ctx, id)
	if err != nil {
		return "", err
	}
	replacementRecord, ttl, err := s.encodeForCreate(replacement)
	if err != nil {
		return "", err
	}
	return s.rotateRecord(ctx, id, currentRecord, replacementRecord, ttl)
}

// rotateRecord is the two-row compare-and-swap. In one transaction it locks
// the old row, requires it to be live and byte-equal to what the caller read,
// creates the new row (retrying an ID collision), and deletes the old row.
// ttl zero keeps the old row's remaining lifetime.
func (s *PostgresStore) rotateRecord(
	ctx context.Context,
	id string,
	currentRecord []byte,
	replacementRecord []byte,
	ttl time.Duration,
) (newID string, err error) {
	opCtx, cancel := context.WithTimeout(ctx, operationLimit)
	defer cancel()

	tx, err := s.pool.Begin(opCtx)
	if err != nil {
		return "", storeError(ctx, "rotate", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(context.WithoutCancel(opCtx))
		}
	}()
	if _, err := tx.Exec(opCtx, "SET LOCAL lock_timeout = '"+lockTimeout+"'"); err != nil {
		return "", storeError(ctx, "rotate", err)
	}

	var (
		stored    []byte
		expiresAt time.Time
	)
	err = tx.QueryRow(opCtx, `
		SELECT record, expires_at FROM elitea_auth.form_sessions
		WHERE id_hash = $1 AND expires_at > now()
		FOR UPDATE`, IDHash(id),
	).Scan(&stored, &expiresAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", ErrNotFound
	case err != nil:
		return "", storeError(ctx, "rotate", err)
	}
	if !bytes.Equal(stored, currentRecord) {
		return "", ErrRotationConflict
	}

	created := ""
	for range createAttempts {
		candidate, generateErr := s.generate()
		if generateErr != nil {
			return "", fmt.Errorf("generate browser session ID: %w", generateErr)
		}
		if !validSessionID(candidate) {
			return "", ErrInvalidID
		}
		var tag interface{ RowsAffected() int64 }
		if ttl > 0 {
			tag, err = tx.Exec(opCtx, insertSQL, IDHash(candidate), replacementRecord, ttl.Milliseconds())
		} else {
			tag, err = tx.Exec(opCtx, `
				INSERT INTO elitea_auth.form_sessions AS current (id_hash, record, expires_at)
				VALUES ($1, $2, $3)
				ON CONFLICT (id_hash) DO UPDATE
				    SET record = EXCLUDED.record, expires_at = EXCLUDED.expires_at
				    WHERE current.expires_at <= now()`,
				IDHash(candidate), replacementRecord, expiresAt)
		}
		if err != nil {
			return "", storeError(ctx, "rotate", err)
		}
		if tag.RowsAffected() == 1 {
			created = candidate
			break
		}
	}
	if created == "" {
		err = ErrIDCollision
		return "", err
	}
	if _, err = tx.Exec(opCtx, `DELETE FROM elitea_auth.form_sessions WHERE id_hash = $1`, IDHash(id)); err != nil {
		return "", storeError(ctx, "rotate", err)
	}
	if err = tx.Commit(opCtx); err != nil {
		return "", storeError(ctx, "rotate", err)
	}
	return created, nil
}

// encodeForCreate returns the record and the lifetime that its state earns:
// TTL for an authenticated state, PreLoginTTL for any other.
func (s *PostgresStore) encodeForCreate(state sessionstate.State) ([]byte, time.Duration, error) {
	if state.SchemaVersion == 0 {
		state.SchemaVersion = sessionstate.CurrentSchemaVersion
	}
	if len(state.ProviderAttributes) == 0 {
		state.ProviderAttributes = json.RawMessage("{}")
	}
	if state.Expiration != nil {
		expiration := state.Expiration.UTC()
		state.Expiration = &expiration
	}
	if err := state.Validate(); err != nil {
		return nil, 0, err
	}

	record, err := json.Marshal(state)
	if err != nil {
		return nil, 0, fmt.Errorf("encode browser session: %w", err)
	}
	if len(record) > MaxRecordBytes {
		return nil, 0, fmt.Errorf("%w: encoded record is too large", sessionstate.ErrInvalidState)
	}
	if state.UserID != nil {
		return record, s.ttl, nil
	}
	return record, s.preLoginTTL, nil
}

func (s *PostgresStore) readRecord(ctx context.Context, id string) (sessionstate.State, []byte, error) {
	if !validSessionID(id) {
		return sessionstate.State{}, nil, ErrInvalidID
	}
	opCtx, cancel := context.WithTimeout(ctx, operationLimit)
	defer cancel()
	var record []byte
	err := s.pool.QueryRow(opCtx, `
		SELECT record FROM elitea_auth.form_sessions
		WHERE id_hash = $1 AND expires_at > now()`, IDHash(id),
	).Scan(&record)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return sessionstate.State{}, nil, ErrNotFound
	case err != nil:
		return sessionstate.State{}, nil, storeError(ctx, "read", err)
	}
	if len(record) == 0 || len(record) > MaxRecordBytes {
		return sessionstate.State{}, nil, fmt.Errorf("%w: encoded record size is invalid", sessionstate.ErrInvalidState)
	}

	state, err := decodeRecord(record)
	if err != nil {
		// Remove this malformed immutable record only if it has not changed
		// since the read above. Best effort: the read already failed closed.
		_, _ = s.pool.Exec(opCtx,
			`DELETE FROM elitea_auth.form_sessions WHERE id_hash = $1 AND record = $2`, IDHash(id), record)
		return sessionstate.State{}, nil, err
	}
	return state, record, nil
}

func decodeRecord(record []byte) (sessionstate.State, error) {
	decoder := json.NewDecoder(bytes.NewReader(record))
	decoder.DisallowUnknownFields()
	var state sessionstate.State
	if err := decoder.Decode(&state); err != nil {
		return sessionstate.State{}, fmt.Errorf("%w: malformed encoded record", sessionstate.ErrInvalidState)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return sessionstate.State{}, fmt.Errorf("%w: trailing encoded data", sessionstate.ErrInvalidState)
	}
	if err := state.Validate(); err != nil {
		return sessionstate.State{}, err
	}
	return state, nil
}

func randomSessionID() (string, error) {
	random := make([]byte, sessionIDRandomBytes)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(random), nil
}

// IDHash is the row key of session id: the lowercase hex SHA-256 of the ID
// string (elitea_auth.form_sessions.id_hash). The login-transaction store
// binds a transaction to its originating session with the same hash.
func IDHash(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

func validSessionID(id string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(id)
	return err == nil && len(decoded) == sessionIDRandomBytes &&
		base64.RawURLEncoding.EncodeToString(decoded) == id
}

// storeError keeps the caller's cancellation and deadline visible. Every
// other failure, including this store's own operation limit and a lock wait
// past lock_timeout, is ErrUnavailable: the store fails closed.
func storeError(ctx context.Context, operation string, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	return fmt.Errorf("%w during %s: %v", ErrUnavailable, operation, err)
}
