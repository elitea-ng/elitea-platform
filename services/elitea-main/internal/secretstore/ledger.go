package secretstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// MinSweepAge is the youngest staged row SweepStale touches. It must exceed
// the longest backend write a StagedWrite can be in, or the sweeper would
// retire a version whose write is about to commit.
const MinSweepAge = time.Minute

// LedgerDB is the database surface the ledger uses. *pgxpool.Pool satisfies it.
type LedgerDB interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Ledger is elitea_identity.secret_write_ledger (migration 0182): the
// staged-write record that makes a backend write recoverable after a crash.
type Ledger struct {
	db LedgerDB
}

// NewLedger returns the ledger over db.
func NewLedger(db LedgerDB) (*Ledger, error) {
	if db == nil {
		return nil, errors.New("secretstore: ledger database is required")
	}
	return &Ledger{db: db}, nil
}

// StagedWrite writes value through store with the ledger around it: stage a
// row, write the backend, mark the row committed.
//
//   - A definitive refusal (conflict, denied, misconfigured, not found,
//     invalid) wrote nothing, so the row is marked abandoned at once.
//   - An unknown outcome (unavailable, a cancelled context) leaves the row
//     staged; SweepStale reconciles it.
//   - A failure to mark the row committed is returned as ErrUnavailable. The
//     caller then never references the version, and the sweeper retires it.
func (l *Ledger) StagedWrite(ctx context.Context, store SecretStore, sc StoreContext, ref SecretRef, value RuntimeSecret, casVersion int64) (int64, error) {
	if l == nil || l.db == nil || store == nil {
		return 0, Errorf(ErrMisconfigured, OpWrite, "ledger or store is not configured")
	}
	if err := sc.Validate(OpWrite); err != nil {
		return 0, err
	}
	if casVersion < 0 || ref.Path == "" || len(ref.Path) > 512 || len(ref.Mount) > 256 {
		return 0, Errorf(ErrInvalidRequest, OpWrite, "malformed reference or check-and-set version")
	}
	intended := casVersion + 1
	var id int64
	err := l.db.QueryRow(ctx, `
INSERT INTO elitea_identity.secret_write_ledger
    (credential_ref_id, tenant_project_id, workload_scope, policy_revision_id,
     backend, locator_mount, locator_path, intended_version)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id`,
		sc.CredentialRefID, sc.TenantProjectID, sc.WorkloadScope, sc.PolicyRevisionID,
		ref.Backend, ref.Mount, ref.Path, intended).Scan(&id)
	if err != nil {
		return 0, Errorf(ErrUnavailable, OpWrite, "stage the ledger row")
	}

	version, writeErr := store.Write(ctx, sc, ref, value, casVersion)
	if writeErr != nil {
		if definitiveRefusal(writeErr) {
			// Best effort: a row left staged here is swept, and the sweep's
			// retire of a version nobody wrote is a no-op.
			_ = l.transition(context.WithoutCancel(ctx), id, "abandoned")
		}
		return 0, writeErr
	}
	if version != intended {
		// The backend created a version the ledger did not stage. Leave the
		// row staged so the sweeper retires the intended one if it exists,
		// and tell the caller nothing was committed.
		return 0, Errorf(ErrMisconfigured, OpWrite, "backend created an unexpected version")
	}
	if err := l.transition(context.WithoutCancel(ctx), id, "committed"); err != nil {
		return 0, Errorf(ErrUnavailable, OpWrite, "commit the ledger row")
	}
	return version, nil
}

func definitiveRefusal(err error) bool {
	for _, kind := range []error{ErrVersionConflict, ErrDenied, ErrMisconfigured, ErrNotFound, ErrInvalidRequest} {
		if errors.Is(err, kind) {
			return true
		}
	}
	return false
}

func (l *Ledger) transition(ctx context.Context, id int64, state string) error {
	tag, err := l.db.Exec(ctx, `
UPDATE elitea_identity.secret_write_ledger
SET state = $2, state_changed_at = clock_timestamp()
WHERE id = $1 AND state = 'staged'`, id, state)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("secretstore: ledger row is no longer staged")
	}
	return nil
}

// SweepResult counts what one SweepStale call did.
type SweepResult struct {
	// Abandoned rows had their backend version retired (or found none).
	Abandoned int
	// Superseded rows were abandoned without a retire because a committed
	// row owns the same version.
	Superseded int
	// Deferred rows stayed staged: the backend was unavailable or is not in
	// the store map. The next sweep tries again.
	Deferred int
}

// SweepStale reconciles up to limit staged rows older than olderThan
// (at least MinSweepAge). Each row is handled in its own transaction under
// FOR UPDATE SKIP LOCKED, so concurrent sweepers on several replicas never
// handle one row twice:
//
//  1. If a committed row owns the same backend version, the staged row lost a
//     check-and-set race; it is marked abandoned and nothing is retired.
//  2. Otherwise the backend version is retired through stores[row.backend]
//     (not found counts as retired) and the row is marked abandoned.
//  3. If the retire fails for any other reason, the row stays staged.
//
// The caller schedules it (a periodic routine); it holds no timer itself.
func (l *Ledger) SweepStale(ctx context.Context, stores map[string]SecretStore, olderThan time.Duration, limit int) (SweepResult, error) {
	var result SweepResult
	if l == nil || l.db == nil {
		return result, Errorf(ErrMisconfigured, OpRetire, "ledger is not configured")
	}
	if olderThan < MinSweepAge {
		olderThan = MinSweepAge
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	// The age is measured on the database clock, the clock created_at uses.
	ageSeconds := olderThan.Seconds()
	skipped := make([]int64, 0, limit)
	for handled := 0; handled < limit; handled++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		outcome, id, err := l.sweepOne(ctx, stores, ageSeconds, skipped)
		if err != nil {
			return result, err
		}
		switch outcome {
		case sweepNone:
			return result, nil
		case sweepAbandoned:
			result.Abandoned++
		case sweepSuperseded:
			result.Superseded++
		case sweepDeferred:
			result.Deferred++
			skipped = append(skipped, id)
		}
	}
	return result, nil
}

type sweepOutcome int

const (
	sweepNone sweepOutcome = iota
	sweepAbandoned
	sweepSuperseded
	sweepDeferred
)

func (l *Ledger) sweepOne(ctx context.Context, stores map[string]SecretStore, ageSeconds float64, skipped []int64) (sweepOutcome, int64, error) {
	tx, err := l.db.Begin(ctx)
	if err != nil {
		return sweepNone, 0, fmt.Errorf("secretstore: begin ledger sweep: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var (
		id       int64
		sc       StoreContext
		ref      SecretRef
		intended int64
	)
	err = tx.QueryRow(ctx, `
SELECT id, credential_ref_id, tenant_project_id, workload_scope, policy_revision_id,
       backend, locator_mount, locator_path, intended_version
FROM elitea_identity.secret_write_ledger
WHERE state = 'staged'
  AND created_at < clock_timestamp() - make_interval(secs => $1::double precision)
  AND NOT (id = ANY($2::bigint[]))
ORDER BY created_at
LIMIT 1
FOR UPDATE SKIP LOCKED`, ageSeconds, skipped).Scan(
		&id, &sc.CredentialRefID, &sc.TenantProjectID, &sc.WorkloadScope, &sc.PolicyRevisionID,
		&ref.Backend, &ref.Mount, &ref.Path, &intended)
	if errors.Is(err, pgx.ErrNoRows) {
		return sweepNone, 0, nil
	}
	if err != nil {
		return sweepNone, 0, fmt.Errorf("secretstore: select stale ledger row: %w", err)
	}

	var owned bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM elitea_identity.secret_write_ledger
    WHERE state = 'committed' AND backend = $1 AND locator_mount = $2
      AND locator_path = $3 AND intended_version = $4)`,
		ref.Backend, ref.Mount, ref.Path, intended).Scan(&owned); err != nil {
		return sweepNone, 0, fmt.Errorf("secretstore: check committed ledger row: %w", err)
	}
	outcome := sweepSuperseded
	if !owned {
		store := stores[ref.Backend]
		if store == nil {
			return sweepDeferred, id, nil
		}
		retireErr := store.Retire(ctx, sc, ref, intended)
		if retireErr != nil && !errors.Is(retireErr, ErrNotFound) {
			return sweepDeferred, id, nil
		}
		outcome = sweepAbandoned
	}
	if _, err := tx.Exec(ctx, `
UPDATE elitea_identity.secret_write_ledger
SET state = 'abandoned', state_changed_at = clock_timestamp()
WHERE id = $1 AND state = 'staged'`, id); err != nil {
		return sweepNone, 0, fmt.Errorf("secretstore: abandon ledger row: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return sweepNone, 0, fmt.Errorf("secretstore: commit ledger sweep: %w", err)
	}
	return outcome, id, nil
}
