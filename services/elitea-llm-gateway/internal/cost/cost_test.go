package cost

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// quietLogger keeps the deliberate WARN/ERROR lines these tests provoke out of
// the test output. A test that asserts a loud signal asserts the metric, which
// is the thing an operator alarms on.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeRow is a canned pgxRow: it copies its six nullable price columns into the
// *int64 scan targets, or returns scanErr.
//
// The column order here MUST match modelPriceSQL. A test that fills the wrong
// field would price the wrong basis and still pass, so the count is asserted:
// six targets, in the order the SELECT names them.
type fakeRow struct {
	srcInput  *int64
	srcOutput *int64
	// The four audio rates (migration 0086). nil means the SQL NULL that every
	// text model carries in these columns.
	srcInputSeconds  *int64
	srcOutputSeconds *int64
	srcInputChars    *int64
	srcOutputChars   *int64
	scanErr          error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	src := []*int64{
		r.srcInput, r.srcOutput,
		r.srcInputSeconds, r.srcOutputSeconds,
		r.srcInputChars, r.srcOutputChars,
	}
	// TWO statements read this row: the widened one (six columns) and the
	// pre-0086 token-only one (two). Any other count is a caller that built a
	// statement and a scan target list that disagree.
	switch len(dest) {
	case len(src): // modelPriceSQL
	case 2: // modelTokenPriceSQL
		src = src[:2]
	default:
		return errors.New("fakeRow: expected 6 scan targets (modelPriceSQL) or 2 (modelTokenPriceSQL)")
	}
	for i, s := range src {
		*(dest[i].(**int64)) = s
	}
	return nil
}

// fakeDB is a canned rowQuerier keyed by "provider:model"; it counts calls so a
// cache-hit test can prove the DB is not re-queried.
type fakeDB struct {
	rows map[string]fakeRow
	def  fakeRow // returned when key not present (defaults to pgx.ErrNoRows)
	// calls is atomic: TestPrice_ConcurrentCacheDoesNotOverwriteFresher drives 50
	// goroutines through Price, and every cache miss reaches QueryRow. A plain
	// int++ here is a read-modify-write race that `go test -race` reports
	// intermittently — roughly 1 run in 6. The race was in the FAKE, not in
	// cost.Calculator, whose own locking was never implicated.
	calls atomic.Int64
	// audioColumnsMissing makes this DB behave like one that has not run
	// migration 0086: the widened statement is refused with Postgres 42703 and
	// only the two token columns can be read.
	audioColumnsMissing bool
	// wideCalls counts attempts at the widened statement, so a test can prove
	// the calculator stops retrying it for the probe interval.
	wideCalls atomic.Int64
}

func (d *fakeDB) QueryRow(_ context.Context, sql string, args ...any) pgxRow {
	d.calls.Add(1)
	if strings.Contains(sql, "input_cost_per_1m_seconds") {
		d.wideCalls.Add(1)
		if d.audioColumnsMissing {
			// The shape Postgres really returns: SQLSTATE 42703 naming the
			// first column it could not resolve.
			return fakeRow{scanErr: &pgconn.PgError{
				Code:    "42703",
				Message: `column "input_cost_per_1m_seconds" does not exist`,
			}}
		}
	}
	provider, _ := args[0].(string)
	model, _ := args[1].(string)
	if r, ok := d.rows[provider+":"+model]; ok {
		return r
	}
	if d.def.scanErr == nil && d.def.srcInput == nil {
		return fakeRow{scanErr: pgx.ErrNoRows}
	}
	return d.def
}

func ptr(v int64) *int64 { return &v }

// usdToNano converts a USD/1M price to nano-USD/1M for building fake catalog rows.
func usdToNano(usd float64) int64 { return int64(math.Round(usd * float64(NanoUSD))) }

// TestCostNano_ReferenceVectors pins costNano to independently computed
// float-USD reference values. The reference is the plain per-1M formula,
// cost_usd = (tokens / 1e6) * price_per_1M; each case asserts the nano-USD
// value AND the USD round-trip, and also that a per-1k divisor (the 1000x
// denomination bug) would NOT reproduce the reference — so a vector set that
// cannot tell the two denominations apart fails here rather than passing
// silently.
func TestCostNano_ReferenceVectors(t *testing.T) {
	cases := []struct {
		name          string
		inTok, outTok int64
		inUSD, outUSD float64 // price per 1M USD
		wantInNano    int64
		wantOutNano   int64
	}{
		// gpt-4o (2.50/10.00), 1000 in / 500 out.
		{"basic_gpt4o", 1000, 500, 2.50, 10.00, 2_500_000, 5_000_000},
		// Large token count: 1M in / 500k out.
		{"large_gpt4o", 1_000_000, 500_000, 2.50, 10.00, 2_500_000_000, 5_000_000_000},
		// Zero tokens: zero under any divisor, so not a 1000x discriminator.
		{"zero", 0, 0, 2.50, 10.00, 0, 0},
		// Precision: gpt-4o-mini (0.15/0.60), 333 in / 666 out.
		//   0.15/1M*333 = 0.00004995 USD = 49_950 nano; 0.60/1M*666 = 0.0003996 USD = 399_600 nano.
		{"precision_mini", 333, 666, 0.15, 0.60, 49_950, 399_600},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotIn, okIn := costNano(tc.inTok, usdToNano(tc.inUSD), TokensPer1M)
			gotOut, okOut := costNano(tc.outTok, usdToNano(tc.outUSD), TokensPer1M)
			if !okIn || !okOut {
				t.Fatalf("costNano returned ok=false for realistic price vector %q", tc.name)
			}
			if gotIn != tc.wantInNano {
				t.Errorf("input nano = %d, want %d", gotIn, tc.wantInNano)
			}
			if gotOut != tc.wantOutNano {
				t.Errorf("output nano = %d, want %d", gotOut, tc.wantOutNano)
			}
			// USD round-trip against the float reference.
			gotUSD := float64(gotIn+gotOut) / float64(NanoUSD)
			wantUSD := (float64(tc.inTok)/1e6)*tc.inUSD + (float64(tc.outTok)/1e6)*tc.outUSD
			if math.Abs(gotUSD-wantUSD) > 1e-9 {
				t.Errorf("total USD = %.9f, want %.9f", gotUSD, wantUSD)
			}
			// The guard's guard: with the per-1k divisor the same vector must
			// disagree with the reference at micro-USD granularity. If it did
			// not, this vector could not catch the 1000x bug.
			if tc.inTok == 0 && tc.outTok == 0 {
				return
			}
			badIn, _ := costNano(tc.inTok, usdToNano(tc.inUSD), 1_000)
			badOut, _ := costNano(tc.outTok, usdToNano(tc.outUSD), 1_000)
			wantMicro := int64(math.Round(wantUSD * 1e6))
			badMicro := int64(math.Round(float64(badIn+badOut) / 1e3))
			if badMicro == wantMicro {
				t.Errorf("per-1k divisor reproduces the reference (%d µUSD); the vector does not discriminate the 1000x bug", wantMicro)
			}
		})
	}
}

func TestCostNano_ThousandXBugGuard(t *testing.T) {
	// The load-bearing invariant: prices are per-1M. If a per-token price
	// (1e6× smaller) were fed as if per-1M, or the divisor were 1e3 instead of
	// 1e6, cost would be off by a factor of 1e6 / 1e3. This test pins that a
	// per-1M price of $2.50 over 1M tokens is exactly $2.50 (2.5e9 nano) — not
	// $2500 (per-1k bug) and not $0.0000025 (per-token confusion).
	const priceNanoPer1M = 2_500_000_000 // $2.50/1M
	got, ok := costNano(1_000_000, priceNanoPer1M, TokensPer1M)
	if !ok {
		t.Fatal("costNano returned ok=false for representable value")
	}
	if got != 2_500_000_000 {
		t.Fatalf("cost for 1M tokens @ $2.50/1M = %d nano, want 2_500_000_000 ($2.50)", got)
	}
	// A per-1k divisor would give 1000× this. Assert we are NOT that.
	perKBug := (int64(1_000_000) * priceNanoPer1M) / 1_000
	if got == perKBug {
		t.Fatal("cost matches the per-1k (1000×) bug value")
	}
}

func TestCostNano_NegativeAndZeroClamp(t *testing.T) {
	if got, ok := costNano(-5, 2_500_000_000, TokensPer1M); got != 0 || !ok {
		t.Errorf("negative tokens: got %d ok=%v, want 0 true", got, ok)
	}
	if got, ok := costNano(1000, -1, TokensPer1M); got != 0 || !ok {
		t.Errorf("negative price: got %d ok=%v, want 0 true", got, ok)
	}
	if got, ok := costNano(0, 2_500_000_000, TokensPer1M); got != 0 || !ok {
		t.Errorf("zero tokens: got %d ok=%v, want 0 true", got, ok)
	}
}

func TestCostNano_NoInt64OverflowOnLargeBatch(t *testing.T) {
	// A naive int64 multiply of tokens*priceNano overflows well before these
	// values; math/big must keep the result exact. 10e9 tokens @ $600/1M
	// (o1-pro output) = 6e12 USD... unrealistic, but proves no overflow/panic.
	// tokens * priceNano = 1e10 * 6e11 = 6e21 > int64 max (9.2e18).
	got, ok := costNano(10_000_000_000, 600_000_000_000, TokensPer1M)
	if !ok {
		t.Fatal("costNano returned ok=false for large but representable result")
	}
	// Expected: 1e10 * 6e11 / 1e6 = 6e15 nano-USD.
	want := int64(6_000_000_000_000_000)
	if got != want {
		t.Fatalf("large-batch cost = %d, want %d (overflow?)", got, want)
	}
}

func TestCostNano_OversizedPriceReturnsFalse(t *testing.T) {
	// FIX 4: a corrupt DB price so large that the final int64 quotient overflows
	// must return ok=false rather than truncating silently.
	//
	// Overflow condition: tokens * priceNanoPer1M / TokensPer1M > int64max.
	// With tokens=2_000_000 and priceNanoPer1M=math.MaxInt64 (≈9.22e18):
	//   2e6 * 9.22e18 / 1e6 = 2 * 9.22e18 = 1.844e19 > int64max (9.22e18).
	_, ok := costNano(2_000_000, math.MaxInt64, TokensPer1M)
	if ok {
		t.Error("expected ok=false for oversized price that overflows int64 after divide")
	}
}

func TestCost_OversizedCatalogPriceFallsBackToDefault(t *testing.T) {
	// FIX 4: when a catalog price is so large that costNano overflows int64,
	// Cost must fall back to the default price (fail-open) rather than returning
	// a silently truncated wrong value or blocking the /llm path.
	//
	// Same overflow trigger: 2M tokens * math.MaxInt64 / 1e6 > int64max.
	oversizedPrice := int64(math.MaxInt64)
	db := &fakeDB{rows: map[string]fakeRow{
		"openai:gpt-4o": {srcInput: &oversizedPrice, srcOutput: &oversizedPrice},
	}}
	c := New(Config{DB: db})
	got := c.Cost(context.Background(), "openai", "gpt-4o", 2_000_000, 2_000_000)
	// Must fall back to the default price for gpt-4o, not return a garbage int64.
	defaultP := defaultPrice("gpt-4o")
	if got.Source != defaultP.Source {
		t.Errorf("source = %q, want %q (fallback source)", got.Source, defaultP.Source)
	}
	// The default input price for gpt-4o is $2.50/1M = 2_500_000_000 nano/1M.
	// 2_000_000 tokens at that rate = 5_000_000_000 nano.
	if got.InputNanoUSD != 5_000_000_000 {
		t.Errorf("InputNanoUSD = %d, want 5_000_000_000 (default price for gpt-4o @ 2M tokens)", got.InputNanoUSD)
	}
}

func TestPrice_CatalogHit(t *testing.T) {
	db := &fakeDB{rows: map[string]fakeRow{
		"openai:gpt-4o": {srcInput: ptr(usdToNano(2.50)), srcOutput: ptr(usdToNano(10.00))},
	}}
	c := New(Config{DB: db})
	p := c.Price(context.Background(), "openai", "gpt-4o")
	if p.Source != "catalog" {
		t.Fatalf("source = %q, want catalog", p.Source)
	}
	if p.InputNanoPer1M != 2_500_000_000 || p.OutputNanoPer1M != 10_000_000_000 {
		t.Fatalf("price = %+v, want 2.5e9/1e10", p)
	}
}

func TestPrice_CatalogNullOutputUsesInputTimes3(t *testing.T) {
	// Reference rule: output_cost = output or input*3 when output is None.
	db := &fakeDB{rows: map[string]fakeRow{
		"custom:m": {srcInput: ptr(usdToNano(5.00)), srcOutput: nil},
	}}
	c := New(Config{DB: db})
	p := c.Price(context.Background(), "custom", "m")
	if p.OutputNanoPer1M != 15_000_000_000 { // 5*3 = 15 USD
		t.Fatalf("output = %d, want 15e9 (input*3)", p.OutputNanoPer1M)
	}
}

func TestLookupCatalog_InputTimes3OverflowGuard(t *testing.T) {
	// inputNano just above MaxInt64/3: input*3 would silently overflow int64
	// and wrap to a small/negative output price instead of the correct
	// fallback. lookupCatalog must refuse the row so the caller falls back to
	// the default table.
	db := &fakeDB{rows: map[string]fakeRow{
		"custom:m": {srcInput: ptr(math.MaxInt64/3 + 1), srcOutput: nil},
	}}
	c := New(Config{DB: db})
	p, ok := c.lookupCatalog(context.Background(), "custom", "m")
	if ok {
		t.Fatalf("ok = true, want false (overflow guard should reject the row)")
	}
	if p != (Price{}) {
		t.Fatalf("price = %+v, want zero value", p)
	}
}

func TestPrice_CatalogNullInputFallsBackToDefault(t *testing.T) {
	// A row with NULL input price is treated as uncatalogued → default table.
	db := &fakeDB{rows: map[string]fakeRow{
		"openai:gpt-4o": {srcInput: nil, srcOutput: ptr(usdToNano(10.00))},
	}}
	c := New(Config{DB: db})
	p := c.Price(context.Background(), "openai", "gpt-4o")
	if p.Source != "default" {
		t.Fatalf("source = %q, want default", p.Source)
	}
	if p.InputNanoPer1M != 2_500_000_000 { // gpt-4o default
		t.Fatalf("input = %d, want gpt-4o default 2.5e9", p.InputNanoPer1M)
	}
}

func TestPrice_DBErrorFallsBackToDefault(t *testing.T) {
	db := &fakeDB{def: fakeRow{scanErr: errors.New("connection refused")}}
	c := New(Config{DB: db})
	p := c.Price(context.Background(), "openai", "gpt-4o-mini")
	if p.Source != "default" {
		t.Fatalf("source = %q, want default (DB error → fail-open to defaults)", p.Source)
	}
	if p.InputNanoPer1M != 150_000_000 {
		t.Fatalf("input = %d, want gpt-4o-mini default 0.15e9", p.InputNanoPer1M)
	}
}

func TestPrice_NoDB_UsesDefaultTable(t *testing.T) {
	c := New(Config{DB: nil})
	p := c.Price(context.Background(), "openai", "gpt-4")
	if p.Source != "default" || p.InputNanoPer1M != 30_000_000_000 {
		t.Fatalf("price = %+v, want gpt-4 default 30e9", p)
	}
}

func TestPrice_UnknownModelUsesFallback(t *testing.T) {
	c := New(Config{DB: nil})
	p := c.Price(context.Background(), "who", "unknown-model-xyz")
	if p.Source != "fallback" {
		t.Fatalf("source = %q, want fallback", p.Source)
	}
	if p.InputNanoPer1M != fallbackInputNano || p.OutputNanoPer1M != fallbackOutputNano {
		t.Fatalf("price = %+v, want 1.0/3.0 USD fallback", p)
	}
}

func TestPrice_CacheHitDoesNotRequeryDB(t *testing.T) {
	db := &fakeDB{rows: map[string]fakeRow{
		"openai:gpt-4o": {srcInput: ptr(usdToNano(2.50)), srcOutput: ptr(usdToNano(10.00))},
	}}
	c := New(Config{DB: db})
	_ = c.Price(context.Background(), "openai", "gpt-4o")
	_ = c.Price(context.Background(), "openai", "gpt-4o")
	_ = c.Price(context.Background(), "openai", "gpt-4o")
	if db.calls.Load() != 1 {
		t.Fatalf("db.calls = %d, want 1 (subsequent reads served from cache)", db.calls.Load())
	}
}

func TestPrice_CacheExpiryRequeriesDB(t *testing.T) {
	db := &fakeDB{rows: map[string]fakeRow{
		"openai:gpt-4o": {srcInput: ptr(usdToNano(2.50)), srcOutput: ptr(usdToNano(10.00))},
	}}
	base := time.Unix(1_700_000_000, 0)
	clock := base
	c := New(Config{DB: db, CacheTTL: time.Minute, Now: func() time.Time { return clock }})
	_ = c.Price(context.Background(), "openai", "gpt-4o")
	clock = base.Add(2 * time.Minute) // past TTL
	_ = c.Price(context.Background(), "openai", "gpt-4o")
	if db.calls.Load() != 2 {
		t.Fatalf("db.calls = %d, want 2 (cache expired → re-query)", db.calls.Load())
	}
}

func TestCost_EndToEnd_CatalogAndSource(t *testing.T) {
	db := &fakeDB{rows: map[string]fakeRow{
		"openai:gpt-4o": {srcInput: ptr(usdToNano(2.50)), srcOutput: ptr(usdToNano(10.00))},
	}}
	c := New(Config{DB: db})
	got := c.Cost(context.Background(), "openai", "gpt-4o", 1000, 500)
	if got.InputNanoUSD != 2_500_000 || got.OutputNanoUSD != 5_000_000 {
		t.Fatalf("cost = %+v, want in=2.5e6 out=5e6", got)
	}
	if got.TotalNanoUSD != 7_500_000 {
		t.Fatalf("total = %d, want 7_500_000", got.TotalNanoUSD)
	}
	if got.Source != "catalog" {
		t.Fatalf("source = %q, want catalog", got.Source)
	}
}

// TestPrice_ConcurrentCacheDoesNotOverwriteFresher asserts Fix #5 (cost): two
// goroutines that both miss the cache simultaneously and then both attempt to
// write must not overwrite a fresher entry with a stale one. The re-check under
// the write lock ensures only one goroutine writes (or both write the same value
// if the DB returns the same row), and neither goroutine can extend the cache
// TTL by re-writing a new entry after a fresh one has already been stored.
func TestPrice_ConcurrentCacheDoesNotOverwriteFresher(t *testing.T) {
	t.Parallel()

	db := &fakeDB{rows: map[string]fakeRow{
		"openai:gpt-4o": {srcInput: ptr(usdToNano(2.50)), srcOutput: ptr(usdToNano(10.00))},
	}}
	c := New(Config{DB: db, CacheTTL: time.Minute})

	// Launch many concurrent Price calls; none should panic or race.
	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			p := c.Price(context.Background(), "openai", "gpt-4o")
			if p.Source != "catalog" {
				t.Errorf("concurrent Price: source=%q, want catalog", p.Source)
			}
		}()
	}
	wg.Wait()
}

func TestDefaultPrice_OrderedPrefixMatch(t *testing.T) {
	// The ordering guard: "gpt-4o" must NOT be shadowed by "gpt-4", and
	// "gpt-4o-mini" must win over "gpt-4o". Go map iteration would break this.
	cases := []struct {
		model     string
		wantInput int64
	}{
		{"gpt-4o-mini", 150_000_000},
		{"gpt-4o-mini-2024-07-18", 150_000_000},
		{"gpt-4o", 2_500_000_000},
		{"gpt-4o-2024-08-06", 2_500_000_000},
		{"gpt-4-turbo", 10_000_000_000},
		{"gpt-4", 30_000_000_000},
		{"claude-3-5-sonnet-20241022", 3_000_000_000},
		{"claude-3-5-haiku-20241022", 800_000_000},
		{"O1-PRO", 150_000_000_000}, // case-insensitive
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			p := defaultPrice(tc.model)
			if p.InputNanoPer1M != tc.wantInput {
				t.Errorf("defaultPrice(%q).input = %d, want %d", tc.model, p.InputNanoPer1M, tc.wantInput)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Issue #323: the seconds and characters bases.
//
// Read this before you change a number here. Every "want" below is written as
// an exact nano-USD integer, and the comment states the wrong value the most
// likely mistake would produce. A test that only asserts "not zero" would pass
// against the 1000x denomination bug these bases exist to guard.
// ---------------------------------------------------------------------------

// TestCostNano_SecondsDivisorIsMillisBased is the 1000x guard for the seconds
// basis, at the level of the helper.
//
// The quantity is MILLISECONDS and the price is per 1,000,000 SECONDS, so the
// divisor is MillisPer1MSeconds (1e9). Using TokensPer1M (1e6) here bills every
// audio second one thousand times over — the exact denomination bug the money
// rules name.
func TestCostNano_SecondsDivisorIsMillisBased(t *testing.T) {
	// 1,000,000 seconds = 1,000,000,000 milliseconds, at $2.50 per 1M seconds.
	// The answer is $2.50 = 2.5e9 nano.
	got, ok := costNano(1_000_000_000, 2_500_000_000, MillisPer1MSeconds)
	if !ok {
		t.Fatal("costNano returned ok=false for a representable seconds vector")
	}
	if got != 2_500_000_000 {
		t.Fatalf("1e6 s @ $2.50 per 1M s = %d nano, want 2_500_000_000 ($2.50)", got)
	}
	// The 1000x mistake, spelled out so the failure names itself.
	wrong, _ := costNano(1_000_000_000, 2_500_000_000, TokensPer1M)
	if got == wrong {
		t.Fatalf("the millis-based divisor gives the same answer as TokensPer1M (%d); the 1000x guard is not guarding", wrong)
	}
	if wrong != 2_500_000_000_000 {
		t.Fatalf("sanity: the TokensPer1M mistake should give 2_500_000_000_000, got %d", wrong)
	}
}

// TestCostUnits_SecondsUsesTheCatalogSecondsRate is the same guard end to end,
// on a realistic price. whisper-1 lists at $0.006 per minute, which the catalog
// carries as $0.36 per 1,000,000 seconds... no: $0.006/min is $0.0001/second,
// so 1,000,000 seconds cost $100.
//
//	60 s at $100 per 1M s = 60 * 100 / 1e6 = $0.006 = 6_000_000 nano.
//
// A TokensPer1M divisor would return 6_000_000_000 nano ($6.00) for one minute
// of audio, which is 1000x the list price.
func TestCostUnits_SecondsUsesTheCatalogSecondsRate(t *testing.T) {
	db := &fakeDB{rows: map[string]fakeRow{
		"openai:whisper-1": {srcInputSeconds: ptr(usdToNano(100.00))},
	}}
	c := New(Config{DB: db})

	got := c.CostUnits(context.Background(), "openai", "whisper-1", Units{InputMillis: 60_000})
	if got.Basis != BasisSeconds {
		t.Fatalf("basis = %q, want %q", got.Basis, BasisSeconds)
	}
	if got.TotalNanoUSD != 6_000_000 {
		t.Fatalf("60 s of whisper-1 = %d nano, want 6_000_000 ($0.006); 6_000_000_000 is the 1000x divisor bug",
			got.TotalNanoUSD)
	}
	if got.InputNanoUSD != 6_000_000 || got.OutputNanoUSD != 0 {
		t.Fatalf("cost = %+v, want the whole amount on the INPUT side", got)
	}
}

// TestCostUnits_CharactersUsesTheCatalogCharacterRate pins the third basis.
//
//	1000 characters at $16 per 1M characters = $0.016 = 16_000_000 nano.
func TestCostUnits_CharactersUsesTheCatalogCharacterRate(t *testing.T) {
	db := &fakeDB{rows: map[string]fakeRow{
		"elevenlabs:eleven_v3": {srcInputChars: ptr(usdToNano(16.00))},
	}}
	c := New(Config{DB: db})

	got := c.CostUnits(context.Background(), "elevenlabs", "eleven_v3", Units{InputChars: 1000})
	if got.Basis != BasisCharacters {
		t.Fatalf("basis = %q, want %q", got.Basis, BasisCharacters)
	}
	if got.TotalNanoUSD != 16_000_000 {
		t.Fatalf("1000 chars = %d nano, want 16_000_000 ($0.016)", got.TotalNanoUSD)
	}
}

// TestCostUnits_NeverSumsTwoBases is the double-billing guard, and it enters
// EVERY arm.
//
// gpt-4o-mini-tts publishes BOTH a token price and a per-second price upstream,
// so one response can report several quantities. Exactly one rate may pay.
//
// The earlier version of this test set InputTokens, so the tokens arm always
// won and the seconds and characters arms were never executed. A reviewer added
// the character cost INTO the seconds arm and the whole package stayed green.
// Each case below therefore sets every quantity the precedence still allows
// alongside the one that must pay, and asserts the EXACT nano amount: a sum
// across bases fails, and so does an arm that reaches for the wrong rate,
// because the four rates are deliberately different numbers.
func TestCostUnits_NeverSumsTwoBases(t *testing.T) {
	// One row, four rates, all distinct:
	//   tokens      $0.60 in / $12.00 out per 1M tokens
	//   seconds     $100.00 out per 1M seconds
	//   characters  $16.00 in / $8.00 out per 1M characters
	db := &fakeDB{rows: map[string]fakeRow{
		"openai:gpt-4o-mini-tts": {
			srcInput:         ptr(usdToNano(0.60)),
			srcOutput:        ptr(usdToNano(12.00)),
			srcOutputSeconds: ptr(usdToNano(100.00)),
			srcInputChars:    ptr(usdToNano(16.00)),
			srcOutputChars:   ptr(usdToNano(8.00)),
		},
	}}
	c := New(Config{DB: db})

	cases := []struct {
		name      string
		u         Units
		wantBasis string
		wantIn    int64
		wantOut   int64
		// alsoOnOffer is what the OTHER bases would have added, spelled out so
		// a failure names the double-bill instead of a bare number mismatch.
		alsoOnOffer string
	}{{
		name: "the tokens arm ignores the seconds and the characters",
		u: Units{
			InputTokens:  1000,
			OutputMillis: 60_000,
			InputChars:   1000,
		},
		wantBasis:   BasisTokens,
		wantIn:      600_000, // 1000 tokens at $0.60/1M
		wantOut:     0,
		alsoOnOffer: "6_000_000 for the 60 s and 16_000_000 for the 1000 chars",
	}, {
		// The arm the mutation lived in. Characters are still on the response;
		// the seconds rate pays and nothing is added to it.
		name: "the seconds arm ignores the characters",
		u: Units{
			OutputMillis: 60_000,
			InputChars:   1000,
		},
		wantBasis:   BasisSeconds,
		wantIn:      0,
		wantOut:     6_000_000, // 60 s at $100 per 1M s
		alsoOnOffer: "16_000_000 for the 1000 chars",
	}, {
		// The characters arm can only be reached with no tokens and no
		// duration, so nothing else CAN be summed onto it. The exact amounts
		// are still asserted per direction: an arm that paid the input
		// character rate for the output characters, or reached for the
		// per-second rate, gives a different number.
		name: "the characters arm pays each direction at its own rate",
		u: Units{
			InputChars:  1000,
			OutputChars: 500,
		},
		wantBasis:   BasisCharacters,
		wantIn:      16_000_000, // 1000 chars at $16/1M
		wantOut:     4_000_000,  // 500 chars at $8/1M
		alsoOnOffer: "nothing: no other quantity may be present for this arm to run",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := c.CostUnits(context.Background(), "openai", "gpt-4o-mini-tts", tc.u)
			if got.Basis != tc.wantBasis {
				t.Fatalf("basis = %q, want %q", got.Basis, tc.wantBasis)
			}
			if got.InputNanoUSD != tc.wantIn || got.OutputNanoUSD != tc.wantOut {
				t.Fatalf("cost = in %d / out %d, want in %d / out %d",
					got.InputNanoUSD, got.OutputNanoUSD, tc.wantIn, tc.wantOut)
			}
			if want := tc.wantIn + tc.wantOut; got.TotalNanoUSD != want {
				t.Fatalf("total = %d nano, want %d (the %s rate alone; a sum would add %s)",
					got.TotalNanoUSD, want, tc.wantBasis, tc.alsoOnOffer)
			}
		})
	}
}

// TestCostUnits_NoCatalogAudioRateIsUnpricedNotFree is rule 2 of the money
// path, and it is the reason Price carries AudioFromCatalog.
//
// defaultPrice fabricates 1.0 / 3.0 USD per 1M TOKENS for any model no prefix
// matches. If the audio rates rode that struct with no flag, a per-second
// request against an uncatalogued model would either bill an INVENTED figure or
// bill zero and read as priced. Both put a wrong number on the authoritative
// counter. The answer is an empty Basis, which the caller counts as UNPRICED.
func TestCostUnits_NoCatalogAudioRateIsUnpricedNotFree(t *testing.T) {
	cases := []struct {
		name string
		db   rowQuerier
	}{
		{"no database at all", nil},
		{"no row for the model", &fakeDB{}},
		{"a row with token prices only", &fakeDB{rows: map[string]fakeRow{
			"openai:whisper-1": {srcInput: ptr(usdToNano(2.50)), srcOutput: ptr(usdToNano(10.00))},
		}}},
		{"a row whose seconds rate is NULL on the side that is used", &fakeDB{rows: map[string]fakeRow{
			// An OUTPUT seconds rate cannot pay for INPUT audio. Guessing the
			// other direction is inventing a price.
			"openai:whisper-1": {srcOutputSeconds: ptr(usdToNano(100.00))},
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := New(Config{DB: tc.db})
			got := c.CostUnits(context.Background(), "openai", "whisper-1", Units{InputMillis: 60_000})
			if got.Basis != "" {
				t.Fatalf("basis = %q, want \"\" (UNPRICED)", got.Basis)
			}
			if got.TotalNanoUSD != 0 {
				t.Fatalf("total = %d, want 0: an unpriced request must not bill an invented amount", got.TotalNanoUSD)
			}
		})
	}
}

// TestPrice_SecondsOnlyRowIsStillACatalogRow covers the row whisper-1 actually
// has: NULL token prices and a per-second rate.
//
// Before the audio columns existed, lookupCatalog refused any row with a NULL
// input_cost_per_1m_tokens. Keeping that rule would drop the ONE row this
// feature is for, and send whisper-1 down the unpriced path with its price
// sitting in the catalog.
func TestPrice_SecondsOnlyRowIsStillACatalogRow(t *testing.T) {
	db := &fakeDB{rows: map[string]fakeRow{
		"openai:whisper-1": {srcInputSeconds: ptr(usdToNano(100.00))},
	}}
	c := New(Config{DB: db})
	p := c.Price(context.Background(), "openai", "whisper-1")

	if !p.AudioFromCatalog {
		t.Fatal("AudioFromCatalog = false; a row with a per-second rate is a catalog row")
	}
	if p.InputNanoPer1MSeconds != usdToNano(100.00) {
		t.Fatalf("seconds rate = %d, want %d", p.InputNanoPer1MSeconds, usdToNano(100.00))
	}
	// The token prices come from the default table: the row has none to give.
	// "whisper-1" matches no default prefix, so it takes the ultimate fallback.
	if p.Source != "fallback" {
		t.Fatalf("source = %q, want fallback: the TOKEN price came from the default table", p.Source)
	}
	if p.InputNanoPer1M != fallbackInputNano {
		t.Fatalf("token input price = %d, want the fallback %d", p.InputNanoPer1M, fallbackInputNano)
	}
}

// TestDefaultPrice_CarriesNoAudioRate pins rule 2 at its source. The default
// table prices tokens and nothing else, and the ultimate fallback must not
// present itself as an audio price.
func TestDefaultPrice_CarriesNoAudioRate(t *testing.T) {
	for _, model := range []string{"gpt-4o-mini", "whisper-1", "tts-1", "a-model-nobody-has-heard-of"} {
		t.Run(model, func(t *testing.T) {
			p := defaultPrice(model)
			if p.AudioFromCatalog {
				t.Error("AudioFromCatalog = true; defaultPrice must never claim a catalog audio rate")
			}
			if p.InputNanoPer1MSeconds != 0 || p.OutputNanoPer1MSeconds != 0 ||
				p.InputNanoPer1MChars != 0 || p.OutputNanoPer1MChars != 0 {
				t.Errorf("default price carries an audio rate: %+v", p)
			}
		})
	}
}

// TestCost_IsCostUnitsWithTokensOnly proves the two entry points are one
// implementation. If Cost ever grew a second code path, a change to the
// rounding or the divisor could land in one and not the other.
func TestCost_IsCostUnitsWithTokensOnly(t *testing.T) {
	db := &fakeDB{rows: map[string]fakeRow{
		"openai:gpt-4o": {srcInput: ptr(usdToNano(2.50)), srcOutput: ptr(usdToNano(10.00))},
	}}
	c := New(Config{DB: db})
	legacy := c.Cost(context.Background(), "openai", "gpt-4o", 1000, 500)
	units := c.CostUnits(context.Background(), "openai", "gpt-4o", Units{InputTokens: 1000, OutputTokens: 500})
	if legacy != units {
		t.Fatalf("Cost = %+v, CostUnits = %+v; they must be the same call", legacy, units)
	}
	if legacy.Basis != BasisTokens {
		t.Fatalf("basis = %q, want %q", legacy.Basis, BasisTokens)
	}
}

// TestUnits_BasisPrecedence pins the order the audio routes rely on.
func TestUnits_BasisPrecedence(t *testing.T) {
	cases := []struct {
		name string
		u    Units
		want string
	}{
		{"empty reads as tokens", Units{}, BasisTokens},
		{"tokens only", Units{InputTokens: 1}, BasisTokens},
		{"millis only", Units{InputMillis: 1}, BasisSeconds},
		{"chars only", Units{OutputChars: 1}, BasisCharacters},
		{"tokens beat millis", Units{OutputTokens: 1, InputMillis: 1}, BasisTokens},
		{"millis beat chars", Units{OutputMillis: 1, InputChars: 1}, BasisSeconds},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.u.Basis(); got != tc.want {
				t.Fatalf("Basis() = %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// A database older than the binary (the rolling-deploy skew).
// ---------------------------------------------------------------------------

// TestPrice_MissingAudioColumnsStillPricesTokensFromTheCatalog is the
// catalog-wide mis-bill guard.
//
// A gateway pod that rolls out ahead of elitea-migrate reads a gateway_models
// table with no migration-0086 audio columns. Postgres then refuses the widened
// SELECT with 42703 for EVERY model, not only for an audio one. lookupCatalog
// reads any non-ErrNoRows error as "uncatalogued", so before the degraded read
// existed the whole catalog silently billed at the DEFAULT price table for the
// length of the skew window.
//
// The catalog price here is deliberately NOT the default price for gpt-4o. A
// test written with the same numbers on both sides passes against the bug.
func TestPrice_MissingAudioColumnsStillPricesTokensFromTheCatalog(t *testing.T) {
	db := &fakeDB{
		audioColumnsMissing: true,
		rows: map[string]fakeRow{
			// $7.77 / $19.19 per 1M — nothing in the default table matches.
			"openai:gpt-4o": {srcInput: ptr(usdToNano(7.77)), srcOutput: ptr(usdToNano(19.19))},
		},
	}
	c := New(Config{DB: db, Logger: quietLogger()})

	p := c.Price(context.Background(), "openai", "gpt-4o")
	if p.Source != "catalog" {
		t.Fatalf("source = %q, want catalog: a missing AUDIO column must not un-price the TOKEN basis", p.Source)
	}
	if p.InputNanoPer1M != usdToNano(7.77) || p.OutputNanoPer1M != usdToNano(19.19) {
		t.Fatalf("price = %d/%d, want %d/%d (the catalog row, not the default table)",
			p.InputNanoPer1M, p.OutputNanoPer1M, usdToNano(7.77), usdToNano(19.19))
	}
	// The audio bases are the ONLY thing the skew costs: no rate was readable,
	// so an audio request against this model is UNPRICED and counted.
	if p.AudioFromCatalog {
		t.Fatal("AudioFromCatalog = true; no audio column could be read")
	}
	got := c.CostUnits(context.Background(), "openai", "whisper-1", Units{InputMillis: 60_000})
	if got.Basis != "" || got.TotalNanoUSD != 0 {
		t.Fatalf("audio cost = %+v, want UNPRICED while the audio columns are missing", got)
	}

	// The signal an operator alarms on: a gauge for the process, not a WARN per
	// model per cache TTL.
	if catalogSchemaBehind.Value() != 1 {
		t.Fatalf("%s = %d, want 1 while the schema is behind",
			MetricPriceCatalogSchemaBehind, catalogSchemaBehind.Value())
	}
}

// TestPrice_MissingAudioColumnsProbesOncePerInterval pins the cost of the
// degraded read. A refused query per model per cache miss would put a 42703 in
// the Postgres log for every model in the catalog; the latch makes it one per
// probe interval for the whole process.
func TestPrice_MissingAudioColumnsProbesOncePerInterval(t *testing.T) {
	db := &fakeDB{
		audioColumnsMissing: true,
		rows: map[string]fakeRow{
			"openai:gpt-4o":      {srcInput: ptr(usdToNano(7.77))},
			"openai:gpt-4o-mini": {srcInput: ptr(usdToNano(0.11))},
		},
	}
	base := time.Unix(1_700_000_000, 0)
	clock := base
	c := New(Config{DB: db, Logger: quietLogger(), Now: func() time.Time { return clock }})

	_ = c.Price(context.Background(), "openai", "gpt-4o")
	_ = c.Price(context.Background(), "openai", "gpt-4o-mini")
	if got := db.wideCalls.Load(); got != 1 {
		t.Fatalf("widened statement attempted %d times, want 1: the latch must hold for the interval", got)
	}

	clock = base.Add(schemaProbeInterval + time.Second)
	_ = c.Price(context.Background(), "openai", "gpt-4o-mini")
	if got := db.wideCalls.Load(); got != 2 {
		t.Fatalf("widened statement attempted %d times after the probe interval, want 2", got)
	}
}

// TestPrice_SchemaBehindRecoversWhenTheMigrationLands proves the latch lifts on
// its own. A latch that never expired would keep every audio request unpriced
// until an operator restarted the pods — long after elitea-migrate ran — and
// would leave the gauge reading 1 with nothing wrong.
func TestPrice_SchemaBehindRecoversWhenTheMigrationLands(t *testing.T) {
	db := &fakeDB{
		audioColumnsMissing: true,
		rows: map[string]fakeRow{
			"openai:whisper-1": {srcInputSeconds: ptr(usdToNano(100.00))},
		},
	}
	base := time.Unix(1_700_000_000, 0)
	clock := base
	c := New(Config{DB: db, Logger: quietLogger(), CacheTTL: time.Minute,
		Now: func() time.Time { return clock }})

	if p := c.Price(context.Background(), "openai", "whisper-1"); p.AudioFromCatalog {
		t.Fatal("AudioFromCatalog = true while the audio columns are missing")
	}
	if catalogSchemaBehind.Value() != 1 {
		t.Fatalf("%s = %d, want 1", MetricPriceCatalogSchemaBehind, catalogSchemaBehind.Value())
	}

	// elitea-migrate runs; the probe interval elapses.
	db.audioColumnsMissing = false
	clock = base.Add(schemaProbeInterval + time.Second)

	p := c.Price(context.Background(), "openai", "whisper-1")
	if !p.AudioFromCatalog || p.InputNanoPer1MSeconds != usdToNano(100.00) {
		t.Fatalf("price = %+v, want the catalog per-second rate once the columns exist", p)
	}
	if catalogSchemaBehind.Value() != 0 {
		t.Fatalf("%s = %d, want 0 once the widened statement succeeds",
			MetricPriceCatalogSchemaBehind, catalogSchemaBehind.Value())
	}
}

// TestPrice_SchemaBehindIsCounted proves the event is countable and not only
// visible while it lasts. A pod that heals before anyone looks leaves the gauge
// at 0, and this counter is then the only evidence the window existed.
func TestPrice_SchemaBehindIsCounted(t *testing.T) {
	before := catalogSchemaBehindTotal.Value()
	db := &fakeDB{audioColumnsMissing: true, rows: map[string]fakeRow{
		"openai:gpt-4o": {srcInput: ptr(usdToNano(7.77))},
	}}
	c := New(Config{DB: db, Logger: quietLogger()})
	_ = c.Price(context.Background(), "openai", "gpt-4o")

	if got := catalogSchemaBehindTotal.Value() - before; got != 1 {
		t.Fatalf("%s moved by %d, want 1", MetricPriceCatalogSchemaBehindTotal, got)
	}
}

// TestMetrics_CarriesKindAndHelpForEveryName stops the silent-skip trap at the
// source: a variable this package names must be published, and must say whether
// it is a gauge or a counter. A gauge scraped as a counter is a lie an alarm
// acts on.
func TestMetrics_CarriesKindAndHelpForEveryName(t *testing.T) {
	metrics := Metrics()
	if len(metrics) != 2 {
		t.Fatalf("Metrics() returned %d entries, want 2 (the gauge and the counter)", len(metrics))
	}
	for _, m := range metrics {
		if m.Kind != "gauge" && m.Kind != "counter" {
			t.Errorf("metric %q has kind %q, want gauge or counter", m.Name, m.Kind)
		}
		if m.Help == "" {
			t.Errorf("metric %q has no help text", m.Name)
		}
	}
}

// TestPrice_ANonSchemaDBErrorStillFallsBackToDefaults keeps the pre-existing
// behaviour for every OTHER error. Only 42703 says "the database is older than
// this binary"; a connection failure says nothing about the schema and must not
// latch the degraded statement.
func TestPrice_ANonSchemaDBErrorStillFallsBackToDefaults(t *testing.T) {
	db := &fakeDB{def: fakeRow{scanErr: errors.New("connection refused")}}
	c := New(Config{DB: db, Logger: quietLogger()})

	p := c.Price(context.Background(), "openai", "gpt-4o-mini")
	if p.Source != "default" {
		t.Fatalf("source = %q, want default", p.Source)
	}
	if !c.audioColumnsReadable() {
		t.Fatal("a connection error latched the degraded statement; only 42703 may")
	}
}

// TestAudioCost_SourceNamesTheRateThatPaid is the "the log tells the truth"
// guard.
//
// whisper-1 has a real catalog per-second rate and NULL token columns, so
// lookupCatalog fills its token prices from the default table and its
// Price.Source reads "fallback". The cost of a duration-billed request came
// 100% from the catalog, and budget_gate logs Cost.Source on the ONE line an
// operator reads to confirm exactly that.
func TestAudioCost_SourceNamesTheRateThatPaid(t *testing.T) {
	db := &fakeDB{rows: map[string]fakeRow{
		"openai:whisper-1": {srcInputSeconds: ptr(usdToNano(100.00))},
	}}
	c := New(Config{DB: db})

	p := c.Price(context.Background(), "openai", "whisper-1")
	if p.Source != "fallback" {
		t.Fatalf("Price.Source = %q, want fallback: the TOKEN prices are fabricated for this row", p.Source)
	}

	got := c.CostUnits(context.Background(), "openai", "whisper-1", Units{InputMillis: 60_000})
	if got.Source != "catalog" {
		t.Fatalf("Cost.Source = %q, want catalog: the per-second rate that paid came from the catalog", got.Source)
	}
	// An unpriced request has no source at all: no rate paid.
	unpriced := c.CostUnits(context.Background(), "openai", "whisper-1", Units{OutputMillis: 60_000})
	if unpriced.Basis != "" {
		t.Fatalf("basis = %q, want \"\" (no OUTPUT per-second rate on this row)", unpriced.Basis)
	}
	if unpriced.Source != "" {
		t.Fatalf("Cost.Source = %q, want \"\": nothing priced this request", unpriced.Source)
	}
}

// TestAudioCost_RefusesAPositiveRateNotFromTheCatalog makes the
// AudioFromCatalog guard falsifiable.
//
// Every Price this package builds today either comes from the catalog with the
// flag set, or comes from defaultPrice with all four audio rates at zero — and
// audioCost already refuses a zero rate. So the guard cannot be reached from
// the public API, and a reviewer who deleted it saw the package stay green.
// The rule it states is still load-bearing: a rate that did not come from the
// catalog must never pay, whatever its value. This test builds that Price
// directly and fails the moment the check is removed.
func TestAudioCost_RefusesAPositiveRateNotFromTheCatalog(t *testing.T) {
	fabricated := Price{
		InputNanoPer1MSeconds: usdToNano(100.00),
		AudioFromCatalog:      false, // NOT read from gateway_models
		Source:                "fallback",
	}
	got := audioCost(fabricated, BasisSeconds, 60_000, 0,
		fabricated.InputNanoPer1MSeconds, fabricated.OutputNanoPer1MSeconds, MillisPer1MSeconds)

	if got.Basis != "" || got.TotalNanoUSD != 0 {
		t.Fatalf("cost = %+v, want UNPRICED: a per-second rate that did not come from the catalog "+
			"must never reach the authoritative budget counter", got)
	}
}
