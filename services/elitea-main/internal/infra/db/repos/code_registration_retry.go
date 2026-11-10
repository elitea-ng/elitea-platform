package repos

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	code "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/codesandbox"
)

// codeRegistrationAttempts bounds how often one Serializable Code
// registration transaction runs when PostgreSQL aborts it for a conflict
// with a concurrent registration of the same run (B3: the items of one Map
// register their visits together).
const codeRegistrationAttempts = 3

// codeRegistrationBackoff is the jittered wait before attempt n+1 (n >= 1):
// n*4 ms plus up to the same again, so concurrent losers spread out.
var codeRegistrationBackoff = func(attempt int) time.Duration {
	base := time.Duration(attempt) * 4 * time.Millisecond
	return base + rand.N(base+1)
}

// runSerializedTxStore is a store that can hold a per-run lock while it
// runs a transaction, taken before the transaction's snapshot.
type runSerializedTxStore interface {
	WithinRunSerializedTx(ctx context.Context, execution string, generation uint64, opts pgx.TxOptions, fn func(sqlExecutor) error) error
}

// withinCodeRegistrationTx runs fn in a Serializable read-write transaction.
//
// The registrations of one run (the items of a Map register their visits
// together) take a per-run lock BEFORE the transaction begins, so each one's
// snapshot already sees the registrations committed before it. Without it a
// registration waiting on the run's claim lock keeps the snapshot it took
// before that wait and PostgreSQL aborts it with 40001 (B3): every waiter
// loses to every earlier holder, so retrying alone cannot settle a burst.
//
// A residual conflict with other work is retried: fn runs again, at most
// codeRegistrationAttempts times in all, when any of its statements or its
// commit met a serialization failure (40001) or a deadlock (40P01). The
// PostgreSQL store records the conflict where it happens, so a callback that
// maps a failed read to code.ErrRejected is still retried rather than
// answered as a refusal; fn always gets the store's own executor. After the
// last attempt the failure is
// code.ErrRegistrationContended, which callers answer as retryable.
func withinCodeRegistrationTx(ctx context.Context, store sharedStore, execution string, generation uint64, fn func(sqlExecutor) error) error {
	options := pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadWrite}
	for attempt := 1; ; attempt++ {
		var err error
		if serialized, ok := store.(runSerializedTxStore); ok {
			err = serialized.WithinRunSerializedTx(ctx, execution, generation, options, fn)
		} else {
			err = store.WithinTx(ctx, options, fn)
		}
		if err == nil {
			return nil
		}
		if !errors.Is(err, errStatementConflict) && !isRegistrationConflict(err) {
			return err
		}
		if attempt == codeRegistrationAttempts {
			return fmt.Errorf("%w after %d attempts: %w", code.ErrRegistrationContended, attempt, err)
		}
		timer := time.NewTimer(codeRegistrationBackoff(attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// errStatementConflict marks a transaction whose callback failed after one
// of its statements met a conflict, whatever error the callback returned.
var errStatementConflict = errors.New("a statement met a serialization conflict")

func isRegistrationConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01")
}

// conflictRecordingExecutor notes a conflict seen by any statement of the
// transaction; it changes no result.
type conflictRecordingExecutor struct {
	next     sqlExecutor
	conflict *bool
}

func (e conflictRecordingExecutor) note(err error) error {
	if isRegistrationConflict(err) {
		*e.conflict = true
	}
	return err
}

func (e conflictRecordingExecutor) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	tag, err := e.next.Exec(ctx, sql, arguments...)
	return tag, e.note(err)
}

func (e conflictRecordingExecutor) QueryRow(ctx context.Context, sql string, args ...any) sqlRow {
	return conflictRecordingRow{next: e.next.QueryRow(ctx, sql, args...), executor: e}
}

func (e conflictRecordingExecutor) Query(ctx context.Context, sql string, args ...any) (sqlRows, error) {
	rows, err := e.next.Query(ctx, sql, args...)
	if err != nil {
		return nil, e.note(err)
	}
	return conflictRecordingRows{next: rows, executor: e}, nil
}

type conflictRecordingRow struct {
	next     sqlRow
	executor conflictRecordingExecutor
}

func (r conflictRecordingRow) Scan(dest ...any) error {
	return r.executor.note(r.next.Scan(dest...))
}

type conflictRecordingRows struct {
	next     sqlRows
	executor conflictRecordingExecutor
}

func (r conflictRecordingRows) Close()                 { r.next.Close() }
func (r conflictRecordingRows) Err() error             { return r.executor.note(r.next.Err()) }
func (r conflictRecordingRows) Next() bool             { return r.next.Next() }
func (r conflictRecordingRows) Scan(dest ...any) error { return r.executor.note(r.next.Scan(dest...)) }

// WithinRunSerializedTx holds a session advisory lock for (execution,
// generation) on one connection, then runs fn in a transaction on it and
// releases the lock. The lock is taken before BEGIN, so the transaction's
// snapshot is taken after it. A connection whose unlock did not succeed is
// closed, never returned to the pool still holding the lock.
func (s *postgresSharedStore) WithinRunSerializedTx(ctx context.Context, execution string, generation uint64, opts pgx.TxOptions, fn func(sqlExecutor) error) (runErr error) {
	if fn == nil {
		return errors.New("transaction callback is required")
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	locked, unlocked := false, false
	defer func() {
		if locked && !unlocked {
			_ = conn.Conn().Close(context.WithoutCancel(ctx))
		}
		conn.Release()
	}()
	key := fmt.Sprintf("code_registration:%s:%d", execution, generation)
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1::text, 0))`, key); err != nil {
		// A cancelled wait may still have taken the lock: never reuse it.
		locked = true
		return fmt.Errorf("lock run registrations: %w", err)
	}
	locked = true
	runErr = func() error {
		tx, err := conn.BeginTx(ctx, opts)
		if err != nil {
			return fmt.Errorf("begin transaction: %w", err)
		}
		defer func() {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}()
		// The conflict is recorded where it happens: a callback that maps a
		// failed read to a refusal is still retried, never answered refused.
		conflict := false
		if err := fn(conflictRecordingExecutor{next: pgxExecutor{queryer: tx}, conflict: &conflict}); err != nil {
			if conflict {
				return fmt.Errorf("%w: %w", errStatementConflict, err)
			}
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit transaction: %w", err)
		}
		return nil
	}()
	var released bool
	if err := conn.QueryRow(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtextextended($1::text, 0))`, key).Scan(&released); err == nil && released {
		unlocked = true
	}
	return runErr
}
