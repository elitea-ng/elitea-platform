package budgetwriteback

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// --- fakes -------------------------------------------------------------------

// fakeRow returns a preset value (or error) for the dedup INSERT ... RETURNING
// probe. scanErr=pgx.ErrNoRows models an already-applied event.
type fakeRow struct {
	returnID string
	scanErr  error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	if len(dest) == 1 {
		if p, ok := dest[0].(*string); ok {
			*p = r.returnID
		}
	}
	return nil
}

// fakeTx records dedup probes and the upsert, and lets a test script per-call
// behaviour: which event_ids are "already applied" and the upsert rows-affected.
type fakeTx struct {
	// alreadyApplied is the set of event_ids the dedup probe reports as conflict
	// (ErrNoRows) — i.e. already committed by an earlier delivery.
	alreadyApplied map[string]bool
	// upsertAffected is the rows-affected the guarded UPSERT returns (0 = outage
	// deferral, >=1 = applied).
	upsertAffected int64

	// injected errors
	dedupErr  error
	upsertErr error
	commitErr error

	// usageErr is injected on the usage-ledger insert.
	usageErr error

	// recorded
	dedupProbes  []string
	upsertArgs   []any
	usageInserts [][]any
	upsertRan    bool
	committed    bool
	rolledBack   bool
}

func (t *fakeTx) QueryRow(_ context.Context, sql string, args ...any) Row {
	if !strings.Contains(sql, "processed_event_ids") {
		return fakeRow{scanErr: errors.New("unexpected QueryRow sql")}
	}
	id, _ := args[0].(string)
	t.dedupProbes = append(t.dedupProbes, id)
	if t.dedupErr != nil {
		return fakeRow{scanErr: t.dedupErr}
	}
	if t.alreadyApplied[id] {
		return fakeRow{scanErr: pgx.ErrNoRows}
	}
	return fakeRow{returnID: id}
}

func (t *fakeTx) ExecAffected(_ context.Context, sql string, args ...any) (int64, error) {
	// The usage-ledger insert (issue #320) rides the same transaction as the
	// accumulator UPSERT. It is recorded separately so a test can assert one
	// ledger row per NEW event without disturbing the accumulator assertions.
	if strings.Contains(sql, "llm_usage_events") {
		t.usageInserts = append(t.usageInserts, args)
		if t.usageErr != nil {
			return 0, t.usageErr
		}
		return 1, nil
	}
	if !strings.Contains(sql, "llm_budget_accumulators") {
		return 0, errors.New("unexpected Exec sql")
	}
	t.upsertRan = true
	t.upsertArgs = args
	if t.upsertErr != nil {
		return 0, t.upsertErr
	}
	return t.upsertAffected, nil
}

func (t *fakeTx) Commit(context.Context) error {
	if t.commitErr != nil {
		return t.commitErr
	}
	t.committed = true
	return nil
}

// Rollback mirrors pgx semantics: a Rollback after a successful Commit is a
// no-op (real pgx returns ErrTxClosed and changes nothing). Without this, the
// Store's unconditional `defer tx.Rollback()` would spuriously mark every
// committed tx as rolled back, masking a genuine rollback in assertions.
func (t *fakeTx) Rollback(context.Context) error {
	if t.committed {
		return nil
	}
	t.rolledBack = true
	return nil
}

type fakeDB struct {
	// tx, when set, is returned by the FIRST Begin (back-compat with tests that
	// pre-build one tx and inspect it). Subsequent Begins mint a FRESH tx so a
	// multi-key-group batch cannot share mutable tx state across groups — that
	// sharing previously hid cross-group contamination (a committed/rolledBack
	// flag set by group A would appear on group B's "tx").
	tx       *fakeTx
	beginErr error

	// newTx, when set, builds a fresh tx per Begin (overrides tx). Each minted
	// tx is recorded in txs so tests can assert per-group isolation.
	newTx func() *fakeTx
	txs   []*fakeTx
}

func (d *fakeDB) Begin(context.Context) (Tx, error) {
	if d.beginErr != nil {
		return nil, d.beginErr
	}
	if d.newTx != nil {
		tx := d.newTx()
		d.txs = append(d.txs, tx)
		return tx, nil
	}
	// First Begin returns the pre-built tx; any further Begin gets its own fresh
	// tx (so shared mutable state never bleeds across key-groups).
	if d.tx != nil && len(d.txs) == 0 {
		d.txs = append(d.txs, d.tx)
		return d.tx, nil
	}
	tx := &fakeTx{alreadyApplied: map[string]bool{}, upsertAffected: 1}
	d.txs = append(d.txs, tx)
	return tx, nil
}

// --- tests -------------------------------------------------------------------

func TestApply_NewEvent_UpsertsAndCommits(t *testing.T) {
	tx := &fakeTx{alreadyApplied: map[string]bool{}, upsertAffected: 1}
	s := NewStore(&fakeDB{tx: tx})

	outcome, err := s.Apply(context.Background(), []BudgetDelta{validDelta()})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if outcome != outcomeApplied {
		t.Fatalf("outcome = %v, want applied", outcome)
	}
	if !tx.upsertRan {
		t.Error("expected UPSERT to run")
	}
	if !tx.committed {
		t.Error("expected commit")
	}
	// arg order: project_id, org_id, scope, scope_id, period_start, period_end, sumNano
	if got := tx.upsertArgs[0]; got != 42 {
		t.Errorf("project_id arg = %v, want 42", got)
	}
	if got := tx.upsertArgs[6]; got != int64(2_500_000_000) {
		t.Errorf("sum nano arg = %v, want 2500000000", got)
	}
}

func TestApply_CoalescesSumOfGroup(t *testing.T) {
	a := validDelta()
	b := validDelta()
	b.EventID = "22222222-2222-2222-2222-222222222222"
	b.DeltaNanoUSD = 500_000_000 // $0.50
	tx := &fakeTx{alreadyApplied: map[string]bool{}, upsertAffected: 1}
	s := NewStore(&fakeDB{tx: tx})

	if _, err := s.Apply(context.Background(), []BudgetDelta{a, b}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// One UPSERT with the summed delta; two dedup probes.
	if len(tx.dedupProbes) != 2 {
		t.Errorf("dedup probes = %d, want 2", len(tx.dedupProbes))
	}
	if got := tx.upsertArgs[6]; got != int64(3_000_000_000) {
		t.Errorf("coalesced sum = %v, want 3000000000", got)
	}
}

func TestApply_SkipsAlreadyAppliedEvent(t *testing.T) {
	a := validDelta() // new
	b := validDelta()
	b.EventID = "22222222-2222-2222-2222-222222222222"
	b.DeltaNanoUSD = 500_000_000 // already applied — must NOT contribute
	tx := &fakeTx{
		alreadyApplied: map[string]bool{b.EventID: true},
		upsertAffected: 1,
	}
	s := NewStore(&fakeDB{tx: tx})

	if _, err := s.Apply(context.Background(), []BudgetDelta{a, b}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Only a's delta counts; b was already applied.
	if got := tx.upsertArgs[6]; got != int64(2_500_000_000) {
		t.Errorf("sum with dedup = %v, want 2500000000 (b excluded)", got)
	}
}

func TestApply_AllAlreadyApplied_NoUpsertButCommits(t *testing.T) {
	d := validDelta()
	tx := &fakeTx{
		alreadyApplied: map[string]bool{d.EventID: true},
		upsertAffected: 1,
	}
	s := NewStore(&fakeDB{tx: tx})

	outcome, err := s.Apply(context.Background(), []BudgetDelta{d})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if outcome != outcomeApplied {
		t.Fatalf("outcome = %v, want applied (benign no-op)", outcome)
	}
	if tx.upsertRan {
		t.Error("no UPSERT expected when every event already applied")
	}
	if !tx.committed {
		t.Error("empty transaction should still commit so messages ACK")
	}
}

func TestApply_OutageRow_DefersAndRollsBack(t *testing.T) {
	tx := &fakeTx{alreadyApplied: map[string]bool{}, upsertAffected: 0} // guard blocked
	s := NewStore(&fakeDB{tx: tx})

	outcome, err := s.Apply(context.Background(), []BudgetDelta{validDelta()})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if outcome != outcomeDeferred {
		t.Fatalf("outcome = %v, want deferred", outcome)
	}
	if tx.committed {
		t.Error("deferred group MUST NOT commit (dedup rows must not persist)")
	}
	if !tx.rolledBack {
		t.Error("deferred group must roll back")
	}
}

func TestApply_BeginError(t *testing.T) {
	s := NewStore(&fakeDB{beginErr: errors.New("pool exhausted")})
	if _, err := s.Apply(context.Background(), []BudgetDelta{validDelta()}); err == nil {
		t.Fatal("expected begin error")
	}
}

func TestApply_DedupProbeError(t *testing.T) {
	tx := &fakeTx{alreadyApplied: map[string]bool{}, dedupErr: errors.New("deadlock")}
	s := NewStore(&fakeDB{tx: tx})
	if _, err := s.Apply(context.Background(), []BudgetDelta{validDelta()}); err == nil {
		t.Fatal("expected dedup probe error")
	}
	if tx.committed {
		t.Error("must not commit on dedup error")
	}
}

func TestApply_UpsertError(t *testing.T) {
	tx := &fakeTx{alreadyApplied: map[string]bool{}, upsertErr: errors.New("serialization failure")}
	s := NewStore(&fakeDB{tx: tx})
	if _, err := s.Apply(context.Background(), []BudgetDelta{validDelta()}); err == nil {
		t.Fatal("expected upsert error")
	}
	if tx.committed {
		t.Error("must not commit on upsert error")
	}
}

func TestApply_CommitError(t *testing.T) {
	tx := &fakeTx{alreadyApplied: map[string]bool{}, upsertAffected: 1, commitErr: errors.New("conn reset")}
	s := NewStore(&fakeDB{tx: tx})
	if _, err := s.Apply(context.Background(), []BudgetDelta{validDelta()}); err == nil {
		t.Fatal("expected commit error")
	}
}

func TestApply_AllAlreadyApplied_CommitError(t *testing.T) {
	// The empty-transaction commit can still fail (conn reset); the error must
	// surface so the batch NAKs rather than silently ACKing unpersisted work.
	d := validDelta()
	tx := &fakeTx{
		alreadyApplied: map[string]bool{d.EventID: true},
		commitErr:      errors.New("conn reset"),
	}
	s := NewStore(&fakeDB{tx: tx})
	if _, err := s.Apply(context.Background(), []BudgetDelta{d}); err == nil {
		t.Fatal("expected commit error on empty (all-applied) transaction")
	}
	if tx.upsertRan {
		t.Error("no UPSERT expected when every event already applied")
	}
}

// TestUpsertSQL_UsesNanoDivisorAndGuard asserts the load-bearing SQL invariants
// are present: the exact nano-USD divisor and the outage disjointness guard.
func TestUpsertSQL_UsesNanoDivisorAndGuard(t *testing.T) {
	if !strings.Contains(upsertSQL, "/ 1000000000") {
		t.Error("upsertSQL must divide nano-USD by 1e9 (NanoUSD) in SQL numeric")
	}
	if !strings.Contains(upsertSQL, "WHERE NOT (acc.outage_mode AND NOT acc.reconciled)") {
		t.Error("upsertSQL must guard against un-reconciled outage rows (§8.5/§8.6)")
	}
	if !strings.Contains(dedupSQL, "ON CONFLICT DO NOTHING RETURNING event_id") {
		t.Error("dedupSQL must be the same-txn dedup probe (§8.6)")
	}
}
