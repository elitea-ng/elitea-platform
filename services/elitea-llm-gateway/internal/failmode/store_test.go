package failmode

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// --- fakes -------------------------------------------------------------------

// scriptedRow returns preset scan values (or an error) for a single-row read.
type scriptedRow struct {
	vals    []any
	scanErr error
}

func (r scriptedRow) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	if len(dest) != len(r.vals) {
		return errors.New("scriptedRow: dest/vals arity mismatch")
	}
	for i, v := range r.vals {
		if err := assign(dest[i], v); err != nil {
			return err
		}
	}
	return nil
}

// assign copies v into the pointer dest for the scalar types the store scans.
func assign(dest, v any) error {
	switch p := dest.(type) {
	case *bool:
		*p = v.(bool)
	case *int64:
		*p = v.(int64)
	case *int:
		*p = v.(int)
	case *float64:
		*p = v.(float64)
	case **string:
		if v == nil {
			*p = nil
		} else {
			s := v.(string)
			*p = &s
		}
	case *string:
		*p = v.(string)
	default:
		return errors.New("scriptedRow: unsupported dest type")
	}
	return nil
}

// storeFakeDB drives Store: one QueryRow script for the point-read, and a
// begun-tx recorder for PersistOutageDelta.
type storeFakeDB struct {
	queryRow Row
	tx       *storeFakeTx
	beginErr error
}

func (d *storeFakeDB) QueryRow(_ context.Context, _ string, _ ...any) Row { return d.queryRow }

func (d *storeFakeDB) Begin(_ context.Context) (Tx, error) {
	if d.beginErr != nil {
		return nil, d.beginErr
	}
	return d.tx, nil
}

type storeFakeTx struct {
	execErr    error
	commitErr  error
	execArgs   []any
	execRan    bool
	committed  bool
	rolledBack bool
	// dedupTaken models the write-back consumer having already applied this
	// event id, so the outage-window claim finds a conflict (issue #515).
	dedupTaken bool
	// dedupErr models the claim itself failing.
	dedupErr error
	// claimRan records that the claim statement ran at all.
	claimRan bool
	// execSQL records every ExecAffected statement in order.
	execSQL []string
	// affectedBy / execErrBy script the rows-affected count and the error of
	// the statement whose text contains the key (default: 1 row, no error).
	affectedBy map[string]int64
	execErrBy  map[string]error
}

func (t *storeFakeTx) QueryRow(_ context.Context, sql string, args ...any) Row {
	if strings.Contains(sql, "processed_event_ids") {
		t.claimRan = true
		switch {
		case t.dedupErr != nil:
			return scriptedRow{scanErr: t.dedupErr}
		case t.dedupTaken:
			return scriptedRow{scanErr: pgx.ErrNoRows}
		}
		id, _ := args[0].(string)
		return scriptedRow{vals: []any{id}}
	}
	return scriptedRow{scanErr: errors.New("unused")}
}
func (t *storeFakeTx) Query(context.Context, string, ...any) (Rows, error) {
	return nil, errors.New("unused")
}
func (t *storeFakeTx) ExecAffected(_ context.Context, sql string, args ...any) (int64, error) {
	t.execRan = true
	t.execArgs = args
	t.execSQL = append(t.execSQL, sql)
	if t.execErr != nil {
		return 0, t.execErr
	}
	for marker, err := range t.execErrBy {
		if strings.Contains(sql, marker) {
			return 0, err
		}
	}
	for marker, n := range t.affectedBy {
		if strings.Contains(sql, marker) {
			return n, nil
		}
	}
	return 1, nil
}
func (t *storeFakeTx) Commit(context.Context) error {
	if t.commitErr != nil {
		return t.commitErr
	}
	t.committed = true
	return nil
}
func (t *storeFakeTx) Rollback(context.Context) error { t.rolledBack = true; return nil }

// --- ReadSnapshot ------------------------------------------------------------

func TestReadSnapshot_Found(t *testing.T) {
	// cols: is_unlimited, hard_limit_nano, accumulated_nano, soft_alert_pct,
	//       nats_fail_mode, acc_found, age_seconds, soft_alerts_disabled
	db := &storeFakeDB{queryRow: scriptedRow{vals: []any{
		false, int64(100) * NanoUSD, int64(42) * NanoUSD, 80, "tiered_hybrid", true, 30.0, false,
	}}}
	s := NewStore(db)
	snap, err := s.ReadSnapshot(context.Background(), 7, "project", "7", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if snap.IsUnlimited || snap.HardLimitNano != 100*NanoUSD || snap.AccumulatedNano != 42*NanoUSD {
		t.Fatalf("bad snapshot: %+v", snap)
	}
	if snap.SoftAlertPct != 80 || !snap.Found {
		t.Fatalf("bad snapshot flags: %+v", snap)
	}
	if snap.Age != 30*time.Second {
		t.Fatalf("age = %v, want 30s", snap.Age)
	}
}

func TestReadSnapshot_MissingAccumulatorRowIsFreshZero(t *testing.T) {
	// acc_found=false ⇒ Age must be left zero regardless of the age column.
	db := &storeFakeDB{queryRow: scriptedRow{vals: []any{
		false, int64(100) * NanoUSD, int64(0), 80, nil, false, 0.0, false,
	}}}
	s := NewStore(db)
	snap, err := s.ReadSnapshot(context.Background(), 7, "project", "7", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Found || snap.Age != 0 {
		t.Fatalf("expected unfound zero-age snapshot: %+v", snap)
	}
}

func TestReadSnapshot_NoBudgetRow(t *testing.T) {
	db := &storeFakeDB{queryRow: scriptedRow{scanErr: pgx.ErrNoRows}}
	s := NewStore(db)
	_, err := s.ReadSnapshot(context.Background(), 7, "project", "7", 1000)
	if !errors.Is(err, ErrNoBudgetRow) {
		t.Fatalf("err = %v, want ErrNoBudgetRow", err)
	}
}

func TestReadSnapshot_ScanError(t *testing.T) {
	db := &storeFakeDB{queryRow: scriptedRow{scanErr: errors.New("boom")}}
	s := NewStore(db)
	_, err := s.ReadSnapshot(context.Background(), 7, "project", "7", 1000)
	if err == nil || errors.Is(err, ErrNoBudgetRow) {
		t.Fatalf("err = %v, want wrapped scan error", err)
	}
}

// --- PerProjectFailMode ------------------------------------------------------

func TestPerProjectFailMode(t *testing.T) {
	t.Run("set", func(t *testing.T) {
		db := &storeFakeDB{queryRow: scriptedRow{vals: []any{"fail_open"}}}
		got, err := NewStore(db).PerProjectFailMode(context.Background(), 1)
		if err != nil || got != "fail_open" {
			t.Fatalf("got %q err %v", got, err)
		}
	})
	t.Run("null inherits", func(t *testing.T) {
		db := &storeFakeDB{queryRow: scriptedRow{vals: []any{nil}}}
		got, err := NewStore(db).PerProjectFailMode(context.Background(), 1)
		if err != nil || got != "" {
			t.Fatalf("got %q err %v", got, err)
		}
	})
	t.Run("no row", func(t *testing.T) {
		db := &storeFakeDB{queryRow: scriptedRow{scanErr: pgx.ErrNoRows}}
		_, err := NewStore(db).PerProjectFailMode(context.Background(), 1)
		if !errors.Is(err, ErrNoBudgetRow) {
			t.Fatalf("err = %v", err)
		}
	})
}

// --- PersistOutageDelta ------------------------------------------------------

func validDelta() OutageDelta {
	return OutageDelta{
		ProjectID:    7,
		Scope:        "project",
		ScopeID:      "7",
		EventID:      "33333333-3333-3333-3333-333333333333",
		PeriodStart:  1000,
		PeriodEnd:    2000,
		DeltaNanoUSD: 5 * NanoUSD,
	}
}

func TestPersistOutageDelta_Commits(t *testing.T) {
	tx := &storeFakeTx{}
	db := &storeFakeDB{tx: tx}
	if err := NewStore(db).PersistOutageDelta(context.Background(), validDelta()); err != nil {
		t.Fatal(err)
	}
	if !tx.execRan || !tx.committed {
		t.Fatalf("expected exec+commit, got exec=%v commit=%v", tx.execRan, tx.committed)
	}
	// The delta nano must be forwarded as the 7th UPSERT arg.
	if len(tx.execArgs) < 7 || tx.execArgs[6].(int64) != 5*NanoUSD {
		t.Fatalf("delta arg not forwarded: %+v", tx.execArgs)
	}
}

func TestPersistOutageDelta_Validation(t *testing.T) {
	const evt = "44444444-4444-4444-4444-444444444444"
	bad := []OutageDelta{
		{Scope: "project", ScopeID: "7", EventID: evt, PeriodStart: 1, PeriodEnd: 2},               // project_id<1
		{ProjectID: 1, ScopeID: "7", EventID: evt, PeriodStart: 1, PeriodEnd: 2},                   // empty scope
		{ProjectID: 1, Scope: "project", EventID: evt, PeriodStart: 1, PeriodEnd: 2},               // empty scope_id
		{ProjectID: 1, Scope: "project", ScopeID: "7", PeriodStart: 1, PeriodEnd: 2},               // empty event_id
		{ProjectID: 1, Scope: "project", ScopeID: "7", EventID: evt, PeriodEnd: 2},                 // period_start<=0
		{ProjectID: 1, Scope: "project", ScopeID: "7", EventID: evt, PeriodStart: 5, PeriodEnd: 5}, // end<=start
	}
	for i, d := range bad {
		tx := &storeFakeTx{}
		db := &storeFakeDB{tx: tx}
		if err := NewStore(db).PersistOutageDelta(context.Background(), d); err == nil {
			t.Fatalf("case %d: expected validation error", i)
		}
		if tx.execRan {
			t.Fatalf("case %d: exec ran despite invalid delta", i)
		}
	}
}

func TestPersistOutageDelta_ExecErrorRollsBack(t *testing.T) {
	tx := &storeFakeTx{execErr: errors.New("upsert failed")}
	db := &storeFakeDB{tx: tx}
	err := NewStore(db).PersistOutageDelta(context.Background(), validDelta())
	if err == nil || !strings.Contains(err.Error(), "persist outage delta") {
		t.Fatalf("err = %v", err)
	}
	if tx.committed || !tx.rolledBack {
		t.Fatalf("expected rollback, got commit=%v rollback=%v", tx.committed, tx.rolledBack)
	}
}

func TestPersistOutageDelta_CommitErrorReturned(t *testing.T) {
	tx := &storeFakeTx{commitErr: errors.New("commit failed")}
	db := &storeFakeDB{tx: tx}
	err := NewStore(db).PersistOutageDelta(context.Background(), validDelta())
	if err == nil || !strings.Contains(err.Error(), "commit outage delta") {
		t.Fatalf("err = %v", err)
	}
}

func TestPersistOutageDelta_BeginError(t *testing.T) {
	db := &storeFakeDB{beginErr: errors.New("no conn")}
	err := NewStore(db).PersistOutageDelta(context.Background(), validDelta())
	if err == nil {
		t.Fatal("expected begin error")
	}
}

// TestOutageUpsertSQL_ResetsReconciled asserts FIX 4: the ON CONFLICT DO UPDATE
// clause must reset reconciled=false (and reconciliation_in_progress=false) so
// that a re-entered outage on an already-reconciled row is picked up by the
// recovery pass rather than being silently skipped (spend lost).
func TestOutageUpsertSQL_ResetsReconciled(t *testing.T) {
	if !strings.Contains(outageUpsertSQL, "reconciled = false") {
		t.Error("outageUpsertSQL ON CONFLICT clause missing 'reconciled = false'; re-entered outage would be skipped by recovery")
	}
	if !strings.Contains(outageUpsertSQL, "reconciliation_in_progress = false") {
		t.Error("outageUpsertSQL ON CONFLICT clause missing 'reconciliation_in_progress = false'; stale crash-marker would persist")
	}
}

func TestDurationFromSeconds(t *testing.T) {
	if durationFromSeconds(1.5) != 1500*time.Millisecond {
		t.Fatal("fractional seconds")
	}
	if durationFromSeconds(-3) != 0 {
		t.Fatal("negative guarded to 0")
	}
}
