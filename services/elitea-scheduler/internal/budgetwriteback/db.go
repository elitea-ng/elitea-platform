package budgetwriteback

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Row is the minimal single-row scan surface (pgx.Row satisfies it). Used for
// the dedup INSERT ... ON CONFLICT DO NOTHING RETURNING event_id probe: a
// pgx.ErrNoRows from Scan means the event was already applied (§8.6).
type Row interface {
	Scan(dest ...any) error
}

// Tx is the minimal transaction surface the consumer needs, kept narrow so
// tests inject a fake without a live Postgres (mirrors pricesync):
//   - QueryRow: the dedup INSERT ... RETURNING probe.
//   - ExecAffected: the guarded accumulator delta-UPSERT; the rows-affected
//     count distinguishes an applied row (>=1) from an outage-deferred row (0),
//     which is how the §8.5 disjointness guard is detected.
//   - Commit/Rollback.
type Tx interface {
	QueryRow(ctx context.Context, sql string, args ...any) Row
	ExecAffected(ctx context.Context, sql string, args ...any) (int64, error)
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

// DB opens a transaction. *pgxpool.Pool is adapted to it via PoolDB.
type DB interface {
	Begin(ctx context.Context) (Tx, error)
}

// PoolDB adapts a *pgxpool.Pool to the DB interface.
type PoolDB struct {
	Pool *pgxpool.Pool
}

var _ DB = (*PoolDB)(nil)

// NewPoolDB wraps a pgxpool.Pool for use with the consumer.
func NewPoolDB(pool *pgxpool.Pool) *PoolDB { return &PoolDB{Pool: pool} }

// Begin starts a pgx transaction and wraps it in the narrow Tx interface.
func (p *PoolDB) Begin(ctx context.Context) (Tx, error) {
	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &pgxTx{tx: tx}, nil
}

// QueryRow runs one statement on the pool, outside any transaction (the
// reasoning column probe).
func (p *PoolDB) QueryRow(ctx context.Context, sql string, args ...any) Row {
	return p.Pool.QueryRow(ctx, sql, args...)
}

var _ ColumnQuerier = (*PoolDB)(nil)

// pgxTx adapts pgx.Tx (whose Exec returns pgconn.CommandTag and whose QueryRow
// returns pgx.Row) to the consumer's Tx interface.
type pgxTx struct {
	tx pgx.Tx
}

func (t *pgxTx) QueryRow(ctx context.Context, sql string, args ...any) Row {
	return t.tx.QueryRow(ctx, sql, args...)
}

func (t *pgxTx) ExecAffected(ctx context.Context, sql string, args ...any) (int64, error) {
	tag, err := t.tx.Exec(ctx, sql, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (t *pgxTx) Commit(ctx context.Context) error   { return t.tx.Commit(ctx) }
func (t *pgxTx) Rollback(ctx context.Context) error { return t.tx.Rollback(ctx) }

// compile-time assertion that pgx.Row satisfies the consumer's Row.
var _ Row = (pgx.Row)(nil)
