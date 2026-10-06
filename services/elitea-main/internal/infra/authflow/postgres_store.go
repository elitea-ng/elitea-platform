// Package authflow persists one-time browser authentication transactions in
// PostgreSQL (elitea_auth.form_login_transactions, shared migration 0145).
//
// The originating session ID is the Form session cookie's bearer value. A row
// never holds it: the binding column and the record's originating_session_id
// both hold sessionHash(id), the hex SHA-256 of the ID (the same row key the
// session store uses). Consume restores the caller's ID on the returned value.
package authflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browserflow"
)

const (
	MaxRecordBytes = 64 << 10
	createAttempts = 4
	// operationLimit bounds one store operation, as the Redis client's
	// three-second command limit did. Running out of it fails closed.
	operationLimit = 3 * time.Second
)

var (
	ErrInvalidConfiguration = errors.New("invalid browser authentication transaction store configuration")
	ErrInvalidID            = errors.New("invalid browser authentication transaction ID")
	ErrInvalidRecord        = errors.New("invalid browser authentication transaction record")
	ErrUnavailable          = errors.New("browser authentication transaction store unavailable")
	ErrIDCollision          = errors.New("browser authentication transaction ID collision limit reached")
)

type idGenerator func() (string, error)
type clock func() time.Time

// PostgresStore does not own or close the injected pool and is safe for
// concurrent use.
type PostgresStore struct {
	pool     *pgxpool.Pool
	generate idGenerator
	now      clock
}

func NewPostgresStore(pool *pgxpool.Pool) (*PostgresStore, error) {
	return newPostgresStore(pool, browserflow.NewTransactionID, time.Now)
}

func newPostgresStore(pool *pgxpool.Pool, generate idGenerator, now clock) (*PostgresStore, error) {
	if pool == nil || generate == nil || now == nil {
		return nil, ErrInvalidConfiguration
	}
	return &PostgresStore{pool: pool, generate: generate, now: now}, nil
}

// Create stores transaction and its binding atomically under a fresh canonical
// 256-bit ID. The row lifetime is derived from ExpiresAt on every collision
// attempt, so retries never extend the transaction lifetime. A live row is
// never overwritten; an expired one is absent and its ID is free.
func (s *PostgresStore) Create(ctx context.Context, transaction browserflow.Transaction) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := transaction.Validate(); err != nil {
		return "", err
	}
	stored := transaction
	stored.OriginatingSessionID = sessionHash(transaction.OriginatingSessionID)
	record, err := encodeTransaction(stored)
	if err != nil {
		return "", err
	}
	opCtx, cancel := context.WithTimeout(ctx, operationLimit)
	defer cancel()

	for range createAttempts {
		now := s.now().UTC()
		if !transaction.ActiveAt(now) {
			return "", browserflow.ErrTransactionRejected
		}
		ttlMilliseconds := transaction.ExpiresAt.Sub(now).Milliseconds()
		if ttlMilliseconds <= 0 {
			return "", browserflow.ErrTransactionRejected
		}

		id, err := s.generate()
		if err != nil {
			return "", fmt.Errorf("generate browser authentication transaction ID: %w", err)
		}
		if browserflow.ValidateTransactionID(id) != nil {
			return "", ErrInvalidID
		}
		tag, err := s.pool.Exec(opCtx, `
			INSERT INTO elitea_auth.form_login_transactions AS current
			    (id, provider, originating_session_hash, record, expires_at)
			VALUES ($1, $2, $3, $4, now() + make_interval(secs => $5::double precision / 1000))
			ON CONFLICT (id) DO UPDATE
			    SET provider = EXCLUDED.provider,
			        originating_session_hash = EXCLUDED.originating_session_hash,
			        record = EXCLUDED.record,
			        expires_at = EXCLUDED.expires_at
			    WHERE current.expires_at <= now()`,
			id, stored.Provider, stored.OriginatingSessionID, record, ttlMilliseconds,
		)
		if err != nil {
			return "", storeError(ctx, "create")
		}
		if tag.RowsAffected() == 1 {
			return id, nil
		}
	}
	return "", ErrIDCollision
}

// Consume atomically verifies the provider and originating-session binding
// before it deletes and returns the transaction. One DELETE does both: a
// binding mismatch matches no row and consumes nothing, and of two concurrent
// consumers of one ID the second finds the row gone. Missing, expired,
// application-expired, mismatched and replayed IDs all return
// browserflow.ErrTransactionRejected.
func (s *PostgresStore) Consume(
	ctx context.Context,
	id string,
	provider string,
	originatingSessionID string,
) (browserflow.Transaction, error) {
	if err := ctx.Err(); err != nil {
		return browserflow.Transaction{}, err
	}
	if browserflow.ValidateTransactionID(id) != nil {
		return browserflow.Transaction{}, ErrInvalidID
	}
	if browserflow.ValidateProvider(provider) != nil ||
		browserflow.ValidateOpaqueID(originatingSessionID) != nil {
		return browserflow.Transaction{}, browserflow.ErrTransactionRejected
	}
	opCtx, cancel := context.WithTimeout(ctx, operationLimit)
	defer cancel()

	originatingHash := sessionHash(originatingSessionID)
	var record []byte
	err := s.pool.QueryRow(opCtx, `
		DELETE FROM elitea_auth.form_login_transactions
		WHERE id = $1 AND provider = $2 AND originating_session_hash = $3
		  AND expires_at > now()
		RETURNING record`,
		id, provider, originatingHash,
	).Scan(&record)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return browserflow.Transaction{}, browserflow.ErrTransactionRejected
	case err != nil:
		return browserflow.Transaction{}, storeError(ctx, "consume")
	}

	transaction, err := decodeTransaction(record)
	if err != nil {
		return browserflow.Transaction{}, err
	}
	if transaction.Provider != provider || transaction.OriginatingSessionID != originatingHash {
		return browserflow.Transaction{}, ErrInvalidRecord
	}
	// The binding matched; hand the caller back the ID it presented.
	transaction.OriginatingSessionID = originatingSessionID
	if !transaction.ActiveAt(s.now().UTC()) {
		return browserflow.Transaction{}, browserflow.ErrTransactionRejected
	}
	return transaction, nil
}

// sessionHash is the stored form of an originating session ID: the lowercase
// hex SHA-256 of the ID string, as authsession.IDHash keys session rows.
func sessionHash(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

func encodeTransaction(transaction browserflow.Transaction) ([]byte, error) {
	if err := transaction.Validate(); err != nil {
		return nil, err
	}
	record, err := json.Marshal(transaction)
	if err != nil {
		return nil, fmt.Errorf("%w: encode", ErrInvalidRecord)
	}
	if len(record) == 0 || len(record) > MaxRecordBytes {
		return nil, fmt.Errorf("%w: encoded size", ErrInvalidRecord)
	}
	return record, nil
}

func decodeTransaction(record []byte) (browserflow.Transaction, error) {
	if len(record) == 0 || len(record) > MaxRecordBytes {
		return browserflow.Transaction{}, fmt.Errorf("%w: encoded size", ErrInvalidRecord)
	}
	decoder := json.NewDecoder(bytes.NewReader(record))
	decoder.DisallowUnknownFields()
	var transaction browserflow.Transaction
	if err := decoder.Decode(&transaction); err != nil {
		return browserflow.Transaction{}, fmt.Errorf("%w: decode", ErrInvalidRecord)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return browserflow.Transaction{}, fmt.Errorf("%w: trailing data", ErrInvalidRecord)
	}
	if err := transaction.Validate(); err != nil {
		return browserflow.Transaction{}, fmt.Errorf("%w: value", ErrInvalidRecord)
	}
	canonical, err := json.Marshal(transaction)
	if err != nil || !bytes.Equal(canonical, record) {
		return browserflow.Transaction{}, fmt.Errorf("%w: non-canonical JSON", ErrInvalidRecord)
	}
	return transaction, nil
}

// storeError keeps the caller's cancellation and deadline visible. Every
// other failure, including this store's own operation limit, is
// ErrUnavailable: the store fails closed. The cause is not included; it can
// carry the PKCE verifier's surroundings from a driver message.
func storeError(ctx context.Context, operation string) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	return fmt.Errorf("%w during %s", ErrUnavailable, operation)
}
