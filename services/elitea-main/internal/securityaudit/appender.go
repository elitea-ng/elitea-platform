package securityaudit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/metric"
)

// Appender writes one event in the caller's transaction.
//
// It never buffers in memory and never retries on its own. An error means
// the event is not written, and the caller MUST roll its transaction back:
// a governed operation without its evidence then does not happen (it is
// denied). A rolled-back transaction leaves no event.
type Appender interface {
	Append(ctx context.Context, tx pgx.Tx, e SecurityEvent) error
}

// Compile-time checks.
var (
	_ Appender = (*PostgresAppender)(nil)
	_ Appender = FailingAppender{}
)

// ErrNoTransaction refuses an append outside a transaction.
var ErrNoTransaction = errors.New("security audit append requires a transaction")

// ErrAppendFailed wraps every database failure of an append.
var ErrAppendFailed = errors.New("security audit append failed")

// lockHeadSQL creates the partition head on first use and locks it. ON
// CONFLICT DO UPDATE takes the row lock, so two first writers of a partition
// serialise here instead of both reading last_seq 0.
const lockHeadSQL = `
INSERT INTO elitea_runtime.security_audit_heads AS head (partition_key)
VALUES ($1)
ON CONFLICT (partition_key) DO UPDATE SET partition_key = head.partition_key
RETURNING head.last_seq, head.last_hash`

// insertEventSQL appends the event and moves the head in one statement.
const insertEventSQL = `
WITH appended AS (
    INSERT INTO elitea_runtime.security_audit_outbox
        (partition_key, partition_seq, event_id, event_type, event, prev_hash, hash)
    VALUES ($1, $2, $3::text::uuid, $4, $5::text::jsonb, $6, $7)
    RETURNING seq, created_at
), moved AS (
    UPDATE elitea_runtime.security_audit_heads
       SET last_seq = $2, last_hash = $7, updated_at = clock_timestamp()
     WHERE partition_key = $1 AND last_seq = $2::bigint - 1
    RETURNING 1
)
SELECT appended.seq, appended.created_at, (SELECT count(*) FROM moved)
  FROM appended`

// PostgresAppender is the Appender over the 0183 outbox.
type PostgresAppender struct {
	source string
	now    func() time.Time
	newID  func() (uuid.UUID, error)
	// headLockWait times the chain-head lock of each append (#1480).
	headLockWait metric.Float64Histogram
}

// NewPostgresAppender builds the appender. source is the CloudEvents source
// for events that do not set one, "elitea-main/<instance>".
func NewPostgresAppender(source string) (*PostgresAppender, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return nil, errors.New("security audit appender needs a source")
	}
	if err := checkString("source", source); err != nil {
		return nil, err
	}
	return &PostgresAppender{source: source, now: time.Now, newID: uuid.NewV7, headLockWait: newHeadLockWait(nil)}, nil
}

// WithMeter records the appender's head-lock histogram on meter instead of
// the global one.
func (a *PostgresAppender) WithMeter(meter metric.Meter) *PostgresAppender {
	a.headLockWait = newHeadLockWait(meter)
	return a
}

// Append chains e after the partition head and writes it to the outbox.
func (a *PostgresAppender) Append(ctx context.Context, tx pgx.Tx, e SecurityEvent) error {
	_, err := a.AppendRecord(ctx, tx, e)
	return err
}

// AppendRecord is Append that also returns the chained record (its seq,
// chain position and hashes).
func (a *PostgresAppender) AppendRecord(ctx context.Context, tx pgx.Tx, e SecurityEvent) (Record, error) {
	if tx == nil {
		return Record{}, ErrNoTransaction
	}
	if e.ID == "" {
		id, err := a.newID()
		if err != nil {
			return Record{}, fmt.Errorf("%w: event id: %w", ErrAppendFailed, err)
		}
		e.ID = id.String()
	}
	if e.Source == "" {
		e.Source = a.source
	}
	if e.Time.IsZero() {
		e.Time = a.now()
	}
	e.Time = e.Time.UTC()
	if err := e.Validate(); err != nil {
		return Record{}, err
	}

	partition := e.PartitionKey()
	var (
		lastSeq  int64
		lastHash []byte
	)
	lockStarted := time.Now()
	err := tx.QueryRow(ctx, lockHeadSQL, partition).Scan(&lastSeq, &lastHash)
	recordHeadLockWait(ctx, a.headLockWait, e.TenantProjectID, err, time.Since(lockStarted))
	if err != nil {
		return Record{}, fmt.Errorf("%w: lock partition head: %w", ErrAppendFailed, err)
	}
	if len(lastHash) != len(Hash{}) {
		return Record{}, fmt.Errorf("%w: partition head hash has length %d", ErrAppendFailed, len(lastHash))
	}
	var prev Hash
	copy(prev[:], lastHash)

	partitionSeq := lastSeq + 1
	canonical, err := e.Canonical(partitionSeq)
	if err != nil {
		return Record{}, err
	}
	hash := ChainHash(prev, canonical)

	record := Record{
		PartitionKey: partition,
		PartitionSeq: partitionSeq,
		EventID:      e.ID,
		EventType:    e.Type,
		Event:        canonical,
		PrevHash:     prev,
		Hash:         hash,
	}
	var moved int64
	if err := tx.QueryRow(ctx, insertEventSQL,
		partition, partitionSeq, e.ID, string(e.Type), string(canonical), prev[:], hash[:],
	).Scan(&record.Seq, &record.CreatedAt, &moved); err != nil {
		return Record{}, fmt.Errorf("%w: insert event: %w", ErrAppendFailed, err)
	}
	if moved != 1 {
		// The head moved under a lock this transaction holds: only a writer
		// that bypassed the head lock does that. Fail; never fork the chain.
		return Record{}, fmt.Errorf("%w: partition head moved during the append", ErrAppendFailed)
	}
	return record, nil
}

// FailingAppender refuses every append. Tests and a misconfigured
// composition use it to prove that governed operations deny without
// evidence.
type FailingAppender struct{ Err error }

// Append returns the configured error.
func (f FailingAppender) Append(context.Context, pgx.Tx, SecurityEvent) error {
	if f.Err == nil {
		return ErrAppendFailed
	}
	return f.Err
}
