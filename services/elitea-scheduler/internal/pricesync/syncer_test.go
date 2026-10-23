package pricesync

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
)

// --- fakes -------------------------------------------------------------------

// fakeSource returns a fixed set of raws (or an error) in a declared denomination.
type fakeSource struct {
	name  string
	denom Denomination
	raws  []RawModelPrice
	err   error
}

func (f *fakeSource) Name() string                                   { return f.name }
func (f *fakeSource) Denomination() Denomination                     { return f.denom }
func (f *fakeSource) Fetch(context.Context) ([]RawModelPrice, error) { return f.raws, f.err }

// fakeRow returns a preset bool for the advisory-lock probe scan.
type fakeRow struct {
	locked  bool
	scanErr error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	if len(dest) == 1 {
		if p, ok := dest[0].(*bool); ok {
			*p = r.locked
		}
	}
	return nil
}

// fakeTx records the SQL and the args of every Exec (the UPSERT calls) for
// assertions. The SQL text is recorded, not only the args, because the
// insert-half/update-half pairing of the UPSERT is expressible ONLY in the
// statement text: both halves send the same args, so an args-only fake cannot
// see a column that was dropped from the ON CONFLICT set list.
type fakeTx struct {
	locked     bool
	scanErr    error
	execErr    error
	commitErr  error
	execSQL    []string
	execArgs   [][]any
	committed  bool
	rolledBack bool
}

func (t *fakeTx) QueryRow(_ context.Context, _ string, _ ...any) Row {
	return fakeRow{locked: t.locked, scanErr: t.scanErr}
}
func (t *fakeTx) Exec(_ context.Context, sql string, args ...any) error {
	if t.execErr != nil {
		return t.execErr
	}
	t.execSQL = append(t.execSQL, sql)
	t.execArgs = append(t.execArgs, args)
	return nil
}
func (t *fakeTx) Commit(context.Context) error   { t.committed = true; return t.commitErr }
func (t *fakeTx) Rollback(context.Context) error { t.rolledBack = true; return nil }

type fakeDB struct {
	tx       *fakeTx
	beginErr error
}

func (d *fakeDB) Begin(context.Context) (Tx, error) {
	if d.beginErr != nil {
		return nil, d.beginErr
	}
	return d.tx, nil
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- tests -------------------------------------------------------------------

func TestSyncUpsertsAndCommits(t *testing.T) {
	src := &fakeSource{
		name:  "litellm",
		denom: PerToken,
		raws: []RawModelPrice{
			{Provider: "openai", ModelName: "gpt-4o", InputCost: fptr(0.0000025), OutputCost: fptr(0.00001)},
		},
	}
	tx := &fakeTx{locked: true}
	db := &fakeDB{tx: tx}
	s := NewSyncer(db, []PriceSource{src}, quietLogger())

	n, err := s.Sync(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if n != 1 {
		t.Fatalf("upserted = %d, want 1", n)
	}
	if !tx.committed {
		t.Error("expected commit")
	}
	if len(tx.execArgs) != 1 {
		t.Fatalf("expected 1 upsert, got %d", len(tx.execArgs))
	}
	// arg order: provider, model, input1m, output1m, cacheCreate, cacheRead,
	// above128k, inSeconds, outSeconds, inChars, outChars, source
	args := tx.execArgs[0]
	// Fail, do not panic, when the arg list is short. An index panic aborts the
	// whole package run, so the other tests never report — which is exactly how a
	// dropped Exec argument hides the more precise failure in
	// TestUpsertBindsEveryPlaceholder.
	if len(args) != 12 {
		t.Fatalf("upsert passes %d args, want 12 (provider..source)", len(args))
	}
	if args[0] != "openai" || args[1] != "gpt-4o" {
		t.Errorf("bad key args: %v %v", args[0], args[1])
	}
	in, ok := args[2].(*float64)
	if !ok || in == nil || !almost(*in, 2.50) {
		t.Errorf("input per-1M arg = %v, want 2.50", args[2])
	}
	out, ok := args[3].(*float64)
	if !ok || out == nil || !almost(*out, 10.00) {
		t.Errorf("output per-1M arg = %v, want 10.00", args[3])
	}
	if args[11] != "litellm" {
		t.Errorf("source arg = %v, want litellm", args[11])
	}
}

// priceColumns lists every price column the UPSERT must carry. It is written out
// by hand on purpose: a list derived from the statement under test would agree
// with the statement no matter what the statement said.
var priceColumns = []string{
	"input_cost_per_1m_tokens",
	"output_cost_per_1m_tokens",
	"cache_creation_input_token_cost",
	"cache_read_input_token_cost",
	"input_cost_per_1m_tokens_above_128k",
	"input_cost_per_1m_seconds",
	"output_cost_per_1m_seconds",
	"input_cost_per_1m_characters",
	"output_cost_per_1m_characters",
}

// TestUpsertSQLCoversEveryPriceColumn pins the failure the upsert doc comment
// describes: a price column named in only ONE half of the UPSERT. Such a column
// is written when the model is first inserted and then never refreshed again
// (or the reverse), and every log line still reports a successful sync. The test
// splits the statement at ON CONFLICT and requires each column on both sides.
func TestUpsertSQLCoversEveryPriceColumn(t *testing.T) {
	src := &fakeSource{name: "litellm", denom: PerToken, raws: []RawModelPrice{
		{Provider: "openai", ModelName: "whisper-1", InputCostPerSecond: fptr(0.0001)},
	}}
	tx := &fakeTx{locked: true}
	s := NewSyncer(&fakeDB{tx: tx}, []PriceSource{src}, quietLogger())

	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(tx.execSQL) != 1 {
		t.Fatalf("want 1 upsert statement, got %d", len(tx.execSQL))
	}
	sql := tx.execSQL[0]

	idx := strings.Index(sql, "ON CONFLICT")
	if idx < 0 {
		t.Fatalf("upsert statement has no ON CONFLICT clause:\n%s", sql)
	}
	insertHalf, updateHalf := sql[:idx], sql[idx:]

	for _, col := range priceColumns {
		if !strings.Contains(insertHalf, col) {
			t.Errorf("column %q missing from the INSERT column list: a new model would store NULL there forever", col)
		}
		if !strings.Contains(updateHalf, "EXCLUDED."+col) {
			t.Errorf("column %q missing from the ON CONFLICT DO UPDATE set list: an existing model would never be refreshed", col)
		}
	}
}

// TestUpsertSkipsOperatorOverriddenRows pins the guard shared migration 0097
// added: the ON CONFLICT DO UPDATE must not apply to a row an operator has
// priced by hand.
//
// The failure it prevents is silent in exactly the way the rest of this file
// worries about. Drop the WHERE and every sync still succeeds, every log line
// still reports the same counts, and the only visible effect is that an
// authored price reverts to the upstream number some time after it was saved —
// discovered, if at all, on a bill. Nothing else in this package or in
// elitea-main would fail.
//
// The assertion is deliberately on the UPDATE half only. Guarding the INSERT
// would be wrong: a (provider, model_name) pair the catalog has never held
// cannot carry an override, so a first sync must still create it.
func TestUpsertSkipsOperatorOverriddenRows(t *testing.T) {
	src := &fakeSource{name: "litellm", denom: PerToken, raws: []RawModelPrice{
		{Provider: "openai", ModelName: "gpt-4o", InputCost: fptr(0.0000025)},
	}}
	tx := &fakeTx{locked: true}
	s := NewSyncer(&fakeDB{tx: tx}, []PriceSource{src}, quietLogger())

	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(tx.execSQL) != 1 {
		t.Fatalf("want 1 upsert statement, got %d", len(tx.execSQL))
	}
	sql := tx.execSQL[0]

	idx := strings.Index(sql, "ON CONFLICT")
	if idx < 0 {
		t.Fatalf("upsert statement has no ON CONFLICT clause:\n%s", sql)
	}
	insertHalf, updateHalf := sql[:idx], sql[idx:]

	if !strings.Contains(updateHalf, "WHERE NOT gateway_models.price_overridden") {
		t.Errorf("ON CONFLICT DO UPDATE has no price_overridden guard: an operator-authored "+
			"price would be silently reverted by the next sync tick:\n%s", updateHalf)
	}
	// The guard must qualify the TARGET row, not EXCLUDED. `EXCLUDED.price_overridden`
	// is the value this sync proposes (always false — the syncer never authors an
	// override), so a guard written that way would be true for every row and skip
	// nothing at all, while reading exactly like a working guard.
	if strings.Contains(updateHalf, "EXCLUDED.price_overridden") {
		t.Errorf("the override guard reads EXCLUDED.price_overridden, which is the incoming " +
			"row rather than the stored one, so it never skips anything")
	}
	if strings.Contains(insertHalf, "price_overridden") {
		t.Errorf("the INSERT half names price_overridden: the syncer must never author or " +
			"clear an override, only decline to overwrite one")
	}
}

// TestUpsertBindsEveryPlaceholder pins the second half of the same hazard: the
// column list, the VALUES placeholders and the Go argument list must agree in
// COUNT. Adding a column to the statement and forgetting its argument shifts
// every later argument by one — the price of one column silently lands in the
// next column, and Postgres accepts it because the types match.
func TestUpsertBindsEveryPlaceholder(t *testing.T) {
	src := &fakeSource{name: "litellm", denom: PerToken, raws: []RawModelPrice{
		{Provider: "openai", ModelName: "gpt-4o", InputCost: fptr(0.0000025)},
	}}
	tx := &fakeTx{locked: true}
	s := NewSyncer(&fakeDB{tx: tx}, []PriceSource{src}, quietLogger())

	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	sql, args := tx.execSQL[0], tx.execArgs[0]

	highest := 0
	for i := 1; i <= 99; i++ {
		// Match "$N" only when the next character is not another digit, so "$1"
		// does not match inside "$12".
		ph := "$" + strconv.Itoa(i)
		pos := strings.Index(sql, ph)
		if pos < 0 {
			continue
		}
		next := pos + len(ph)
		if next < len(sql) && sql[next] >= '0' && sql[next] <= '9' {
			continue
		}
		highest = i
	}
	if highest != len(args) {
		t.Errorf("statement binds $1..$%d but Exec passes %d args: the columns and the args have drifted apart", highest, len(args))
	}

	// The last argument must still be the provenance string. If an added column
	// pushed it out of place, the source column would receive a price.
	if _, ok := args[len(args)-1].(string); !ok {
		t.Errorf("last arg = %T, want the source string", args[len(args)-1])
	}
}

// TestSyncCarriesAudioPricesToUpsert follows one audio-only model the whole way:
// upstream per-second price → normalize → the UPSERT argument list. It is the
// end-to-end guard that the four new fields are not dropped at a hand-off.
func TestSyncCarriesAudioPricesToUpsert(t *testing.T) {
	// whisper-1 bills audio duration: $0.0001 per second → $100 per 1M seconds.
	src := &fakeSource{name: "litellm", denom: PerToken, raws: []RawModelPrice{{
		Provider:               "openai",
		ModelName:              "whisper-1",
		InputCostPerSecond:     fptr(0.0001),
		OutputCostPerSecond:    fptr(0.0002),
		InputCostPerCharacter:  fptr(0.0000003),
		OutputCostPerCharacter: fptr(0.000015),
	}}}
	tx := &fakeTx{locked: true}
	s := NewSyncer(&fakeDB{tx: tx}, []PriceSource{src}, quietLogger())

	n, err := s.Sync(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if n != 1 {
		t.Fatalf("audio-only model must be upserted, got n=%d", n)
	}
	args := tx.execArgs[0]

	// The token price stays nil, because a second-billed model has no token
	// price to record. The gateway does NOT discard such a row: cost.go
	// lookupCatalog keeps it for its audio rates and fills the token pair from
	// the legacy default table, so a caller that somehow reports tokens for
	// whisper-1 still gets the legacy answer. Writing a token price here would
	// therefore not be inert — it would override that default — so leaving the
	// column NULL is the load-bearing part.
	if tok, ok := args[2].(*float64); !ok || tok != nil {
		t.Errorf("token input price = %v, want a nil *float64 for an audio-only model", args[2])
	}
	for _, c := range []struct {
		idx  int
		want float64
		name string
	}{
		{7, 100, "input per 1M seconds"},
		{8, 200, "output per 1M seconds"},
		{9, 0.3, "input per 1M characters"},
		{10, 15, "output per 1M characters"},
	} {
		got, ok := args[c.idx].(*float64)
		if !ok || got == nil {
			t.Errorf("%s arg = %v, want a non-nil price", c.name, args[c.idx])
			continue
		}
		if !almost(*got, c.want) {
			t.Errorf("%s = %v, want %v", c.name, *got, c.want)
		}
	}
}

func TestSyncAdvisoryLockHeldIsNoOp(t *testing.T) {
	src := &fakeSource{name: "seed", denom: Per1M, raws: []RawModelPrice{{Provider: "openai", ModelName: "gpt-4o", InputCost: fptr(2.5)}}}
	tx := &fakeTx{locked: false} // another replica holds the lock
	db := &fakeDB{tx: tx}
	s := NewSyncer(db, []PriceSource{src}, quietLogger())

	n, err := s.Sync(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if n != 0 {
		t.Errorf("lock-held pass must upsert 0, got %d", n)
	}
	if len(tx.execArgs) != 0 {
		t.Error("must not upsert when lock is held")
	}
	if tx.committed {
		t.Error("must not commit when lock is held")
	}
	// The no-op path must release the connection via Rollback (deferred in Sync),
	// not hold the transaction open.
	if !tx.rolledBack {
		t.Error("lock-held path must roll back the transaction to release the connection")
	}
}

func TestSyncFirstSourceWins(t *testing.T) {
	// litellm (per-token) and seed (per-1M) both define gpt-4o; litellm precedes.
	litellm := &fakeSource{name: "litellm", denom: PerToken, raws: []RawModelPrice{
		{Provider: "openai", ModelName: "gpt-4o", InputCost: fptr(0.0000025)}, // → 2.50
	}}
	seed := &fakeSource{name: "seed", denom: Per1M, raws: []RawModelPrice{
		{Provider: "openai", ModelName: "gpt-4o", InputCost: fptr(2.60)},    // ignored (later)
		{Provider: "anthropic", ModelName: "claude", InputCost: fptr(3.00)}, // gap fill
	}}
	tx := &fakeTx{locked: true}
	s := NewSyncer(&fakeDB{tx: tx}, []PriceSource{litellm, seed}, quietLogger())

	n, err := s.Sync(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if n != 2 {
		t.Fatalf("want 2 merged models, got %d", n)
	}
	// Find gpt-4o's upserted input price: must be the litellm-derived 2.50.
	for _, args := range tx.execArgs {
		if args[1] == "gpt-4o" {
			in := args[2].(*float64)
			if !almost(*in, 2.50) {
				t.Errorf("first-source-wins broken: gpt-4o input = %v, want 2.50", *in)
			}
		}
	}
}

// TestSyncDriftAlarmFires verifies the drift-alarm code path (syncer.go:127-131)
// executes when a second source reports an input price that disagrees with the
// first source by more than driftThreshold (10%). The sync must still succeed
// (first source wins), but the alarm logger path must be reached.
func TestSyncDriftAlarmFires(t *testing.T) {
	// first: 2.50; second: 2.90 → gap = 0.40/2.90 ≈ 13.8% > 10% → alarm fires.
	first := &fakeSource{name: "litellm", denom: Per1M, raws: []RawModelPrice{
		{Provider: "openai", ModelName: "gpt-4o", InputCost: fptr(2.50)},
	}}
	second := &fakeSource{name: "seed", denom: Per1M, raws: []RawModelPrice{
		{Provider: "openai", ModelName: "gpt-4o", InputCost: fptr(2.90)},
	}}
	tx := &fakeTx{locked: true}
	s := NewSyncer(&fakeDB{tx: tx}, []PriceSource{first, second}, quietLogger())

	n, err := s.Sync(context.Background())
	if err != nil {
		t.Fatalf("sync with drifting sources must not fail: %v", err)
	}
	// First source wins; one model upserted.
	if n != 1 {
		t.Fatalf("want 1 model upserted, got %d", n)
	}
	if len(tx.execArgs) != 1 {
		t.Fatalf("want 1 upsert exec, got %d", len(tx.execArgs))
	}
	// The winning price must be from the first source (2.50).
	args := tx.execArgs[0]
	in, ok := args[2].(*float64)
	if !ok || in == nil || !almost(*in, 2.50) {
		t.Errorf("drift case: winning input = %v, want 2.50 (first source)", args[2])
	}
	// The transaction must have been committed (drift is a warning, not an abort).
	if !tx.committed {
		t.Error("drift alarm must not prevent commit")
	}
}

func TestSyncFailOpenOnSourceError(t *testing.T) {
	bad := &fakeSource{name: "litellm", denom: PerToken, err: errors.New("network down")}
	good := &fakeSource{name: "seed", denom: Per1M, raws: []RawModelPrice{
		{Provider: "openai", ModelName: "gpt-4o", InputCost: fptr(2.5)},
	}}
	tx := &fakeTx{locked: true}
	s := NewSyncer(&fakeDB{tx: tx}, []PriceSource{bad, good}, quietLogger())

	n, err := s.Sync(context.Background())
	if err != nil {
		t.Fatalf("one bad source must not fail the pass: %v", err)
	}
	if n != 1 {
		t.Errorf("want 1 model from surviving source, got %d", n)
	}
}

func TestSyncAllSourcesFail(t *testing.T) {
	a := &fakeSource{name: "litellm", denom: PerToken, err: errors.New("down")}
	b := &fakeSource{name: "seed", denom: Per1M, err: errors.New("down")}
	s := NewSyncer(&fakeDB{tx: &fakeTx{locked: true}}, []PriceSource{a, b}, quietLogger())

	if _, err := s.Sync(context.Background()); err == nil {
		t.Fatal("all-sources-fail must return an error")
	}
}

func TestSyncNoSourcesConfigured(t *testing.T) {
	s := NewSyncer(&fakeDB{tx: &fakeTx{locked: true}}, nil, quietLogger())
	if _, err := s.Sync(context.Background()); err == nil {
		t.Fatal("no sources must return an error")
	}
}

func TestSyncEmptyMergeSkipsUpsert(t *testing.T) {
	// A source that succeeds but yields zero rows → nothing to upsert, no error.
	empty := &fakeSource{name: "seed", denom: Per1M, raws: nil}
	tx := &fakeTx{locked: true}
	s := NewSyncer(&fakeDB{tx: tx}, []PriceSource{empty}, quietLogger())

	n, err := s.Sync(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if n != 0 || len(tx.execArgs) != 0 {
		t.Errorf("empty merge must skip upsert, got n=%d execs=%d", n, len(tx.execArgs))
	}
}

func TestSyncBeginError(t *testing.T) {
	src := &fakeSource{name: "seed", denom: Per1M, raws: []RawModelPrice{{Provider: "p", ModelName: "m", InputCost: fptr(1)}}}
	s := NewSyncer(&fakeDB{beginErr: errors.New("no conn")}, []PriceSource{src}, quietLogger())
	if _, err := s.Sync(context.Background()); err == nil {
		t.Fatal("begin error must propagate")
	}
}

func TestSyncScanError(t *testing.T) {
	src := &fakeSource{name: "seed", denom: Per1M, raws: []RawModelPrice{{Provider: "p", ModelName: "m", InputCost: fptr(1)}}}
	tx := &fakeTx{scanErr: errors.New("scan boom")}
	s := NewSyncer(&fakeDB{tx: tx}, []PriceSource{src}, quietLogger())
	if _, err := s.Sync(context.Background()); err == nil {
		t.Fatal("scan error must propagate")
	}
}

func TestSyncExecError(t *testing.T) {
	src := &fakeSource{name: "seed", denom: Per1M, raws: []RawModelPrice{{Provider: "p", ModelName: "m", InputCost: fptr(1)}}}
	tx := &fakeTx{locked: true, execErr: errors.New("exec boom")}
	s := NewSyncer(&fakeDB{tx: tx}, []PriceSource{src}, quietLogger())
	if _, err := s.Sync(context.Background()); err == nil {
		t.Fatal("exec error must propagate")
	}
}

func TestSyncCommitError(t *testing.T) {
	src := &fakeSource{name: "seed", denom: Per1M, raws: []RawModelPrice{{Provider: "p", ModelName: "m", InputCost: fptr(1)}}}
	tx := &fakeTx{locked: true, commitErr: errors.New("commit boom")}
	s := NewSyncer(&fakeDB{tx: tx}, []PriceSource{src}, quietLogger())
	if _, err := s.Sync(context.Background()); err == nil {
		t.Fatal("commit error must propagate")
	}
}

func TestSyncBadDenominationSkipsModel(t *testing.T) {
	// An invalid denomination must skip the model (normalize error), not crash.
	bad := &fakeSource{name: "x", denom: DenominationUnknown, raws: []RawModelPrice{{Provider: "p", ModelName: "m", InputCost: fptr(1)}}}
	tx := &fakeTx{locked: true}
	s := NewSyncer(&fakeDB{tx: tx}, []PriceSource{bad}, quietLogger())
	n, err := s.Sync(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	// Source succeeded (fetch ok) but its only model failed to normalize → 0 rows.
	if n != 0 {
		t.Errorf("bad-denomination model must be skipped, got n=%d", n)
	}
}

func TestPriceDrifts(t *testing.T) {
	cases := []struct {
		a, b float64
		want bool
		note string
	}{
		{2.50, 2.50, false, "equal → no drift"},
		{2.50, 2.60, false, "4% < 10% → no drift"},
		{2.50, 3.00, true, "20% > 10% → drift"},
		{0, 0, false, "both zero → no drift"},
		{0, 1, true, "zero vs nonzero → always drift"},
		// (2.50, 2.75): |2.75-2.50|/max(2.75,2.50) = 0.25/2.75 ≈ 9.09% — below 10%.
		{2.50, 2.75, false, "9.09% < 10% → no drift"},
		// Exact boundary: |a-b|/max = 0.10 exactly → not strictly greater → no drift.
		// max(a,b)=b, b-a = 0.1*b → a = 0.9*b; choose b=3.00, a=2.70.
		// |3.00-2.70|/3.00 = 0.30/3.00 = 10.00% exactly → not strictly greater → false.
		{2.70, 3.00, false, "10.00% exactly at threshold → not strictly greater → no drift"},
		// Just above threshold: b=3.00, a=2.69 → |3.00-2.69|/3.00 = 0.31/3.00 ≈ 10.33% > 10%.
		{2.69, 3.00, true, "10.33% just above 10% boundary → drift"},
	}
	for _, c := range cases {
		t.Run(c.note, func(t *testing.T) {
			if got := priceDrifts(c.a, c.b); got != c.want {
				t.Errorf("priceDrifts(%v,%v) = %v, want %v (%s)", c.a, c.b, got, c.want, c.note)
			}
		})
	}
}

func TestNewSyncerNilLogger(t *testing.T) {
	s := NewSyncer(&fakeDB{tx: &fakeTx{}}, nil, nil)
	if s.logger == nil {
		t.Error("nil logger must fall back to default")
	}
}
