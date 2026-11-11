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

// codeRegistrationTx is the isolation of a Code visit or intent
// registration (B3, B6). Its first statement is lockAccess's
// `FOR UPDATE OF j,c,ws,command` on the run's job and claim rows, so the
// registrations of one run (the items of a Map register together) run one
// after another, and under READ COMMITTED every later statement takes a fresh
// snapshot after that wait: it sees the registrations committed before it.
// The invariants are held by that row lock and the unique constraints:
// the per-run visit cap counts under the lock; a visit or intent row is
// inserted ON CONFLICT DO NOTHING and read back FOR UPDATE and compared
// byte for byte; the claim, fence and lease are re-read locked. PostgreSQL
// releases the row locks on commit, rollback or connection loss.
var codeRegistrationTx = pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}

// codeRegistrationAttempts bounds how often a registration runs when
// PostgreSQL still reports a serialization failure or a deadlock.
const codeRegistrationAttempts = 3

// codeRegistrationBackoff is the jittered wait before attempt n+1 (n >= 1).
var codeRegistrationBackoff = func(attempt int) time.Duration {
	base := time.Duration(attempt) * 4 * time.Millisecond
	return base + rand.N(base+1)
}

// withinCodeRegistrationTx runs fn in one codeRegistrationTx transaction
// with the store's own executor (never a wrapper: the production permission
// check reads through it), and runs it again, at most
// codeRegistrationAttempts times in all, when it failed with a serialization
// failure (40001) or a deadlock (40P01). After the last attempt the failure
// is code.ErrRegistrationContended, which callers answer as retryable.
func withinCodeRegistrationTx(ctx context.Context, store sharedStore, fn func(sqlExecutor) error) error {
	for attempt := 1; ; attempt++ {
		err := store.WithinTx(ctx, codeRegistrationTx, fn)
		if err == nil || !isRegistrationConflict(err) {
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

func isRegistrationConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01")
}

// codeRegistrationStage names the step of a registration that refused or
// failed, for Main's operator fault line. It never carries request data.
type codeRegistrationStage struct {
	stage string
	err   error
}

func (e *codeRegistrationStage) Error() string { return e.stage + ": " + e.err.Error() }
func (e *codeRegistrationStage) Unwrap() error { return e.err }

// Stage of a registration failure, for the operator line.
func (e *codeRegistrationStage) Stage() string { return e.stage }

// staged labels err with the registration step it came from; nil stays nil
// and an already labelled error keeps its first (innermost) step.
func staged(stage string, err error) error {
	var labelled *codeRegistrationStage
	if err == nil || errors.As(err, &labelled) {
		return err
	}
	return &codeRegistrationStage{stage: stage, err: err}
}
