package cost

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// meteredRow answers meteredPriceSQL with one fixed unit and nano-USD price.
type meteredRow struct {
	unit string
	nano int64
	err  error
}

func (r meteredRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*string) = r.unit
	*dest[1].(*int64) = r.nano
	return nil
}

type meteredDB struct {
	rows  map[string]meteredRow
	err   error
	calls atomic.Int64
}

func (d *meteredDB) QueryRow(_ context.Context, _ string, args ...any) pgxRow {
	d.calls.Add(1)
	if d.err != nil {
		return meteredRow{err: d.err}
	}
	row, ok := d.rows[args[0].(string)+"/"+args[1].(string)]
	if !ok {
		return meteredRow{err: pgx.ErrNoRows}
	}
	return row
}

func newMeteredCalculator(db *meteredDB, now func() time.Time) *Calculator {
	return New(Config{DB: db, Now: now, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
}

func TestCostMeteredExactNano(t *testing.T) {
	db := &meteredDB{rows: map[string]meteredRow{
		"tavily/web_search": {unit: MeteredUnitCredit, nano: 8_000_000},
		"direct/web_fetch":  {unit: MeteredUnitPage, nano: 0},
	}}
	calc := newMeteredCalculator(db, nil)

	got, ok := calc.CostMetered(context.Background(), "tavily", "web_search", MeteredUnits{Requests: 1, Credits: 3})
	if !ok {
		t.Fatal("tavily/web_search is priced, CostMetered says unpriced")
	}
	// The credits count, not the requests: the price is per credit.
	if got.Units != 3 || got.TotalNanoUSD != 24_000_000 || got.Price.Unit != MeteredUnitCredit {
		t.Fatalf("CostMetered = %+v, want 3 credits at 24000000 nano-USD", got)
	}

	free, ok := calc.CostMetered(context.Background(), "direct", "web_fetch", MeteredUnits{Pages: 2})
	if !ok || free.TotalNanoUSD != 0 || free.Units != 2 {
		t.Fatalf("a zero price is a real price: got %+v ok=%v", free, ok)
	}

	if _, ok := calc.CostMetered(context.Background(), "brave", "web_search", MeteredUnits{Requests: 1}); ok {
		t.Fatal("an operation with no row must be unpriced")
	}
}

func TestMeteredPriceCachesAbsentRowButNotAnError(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	now := func() time.Time { return clock }

	absent := &meteredDB{rows: map[string]meteredRow{}}
	calc := newMeteredCalculator(absent, now)
	for range 3 {
		if _, ok := calc.MeteredPrice(context.Background(), "brave", "web_search"); ok {
			t.Fatal("no row must read as unpriced")
		}
	}
	if n := absent.calls.Load(); n != 1 {
		t.Fatalf("an absent row is cached for the TTL; %d reads, want 1", n)
	}
	clock = clock.Add(DefaultCacheTTL + time.Second)
	calc.MeteredPrice(context.Background(), "brave", "web_search")
	if n := absent.calls.Load(); n != 2 {
		t.Fatalf("an expired entry is read again; %d reads, want 2", n)
	}

	failing := &meteredDB{err: errors.New("connection reset")}
	calc = newMeteredCalculator(failing, now)
	for range 3 {
		if _, ok := calc.MeteredPrice(context.Background(), "tavily", "web_search"); ok {
			t.Fatal("a failed read must refuse the operation as unpriced")
		}
	}
	if n := failing.calls.Load(); n != 3 {
		t.Fatalf("a failed read is not cached; %d reads, want 3", n)
	}
}

func TestMeteredPriceRefusesANegativePrice(t *testing.T) {
	db := &meteredDB{rows: map[string]meteredRow{"tavily/web_search": {unit: MeteredUnitCredit, nano: -1}}}
	if _, ok := newMeteredCalculator(db, nil).MeteredPrice(context.Background(), "tavily", "web_search"); ok {
		t.Fatal("a negative price must read as unpriced")
	}
}

func TestPriceMeteredRefusesAnOverflow(t *testing.T) {
	price := MeteredPrice{Unit: MeteredUnitRequest, NanoUSDPerUnit: 1_000_000_000}
	if _, ok := PriceMetered(price, MeteredUnits{Requests: math.MaxInt64 / 10}); ok {
		t.Fatal("an int64 overflow must refuse, not wrap")
	}
	if got, ok := PriceMetered(price, MeteredUnits{Requests: -5}); !ok || got.TotalNanoUSD != 0 {
		t.Fatalf("a negative count bills nothing: got %+v ok=%v", got, ok)
	}
}

func TestMeteredPriceWithoutDatabaseIsUnpriced(t *testing.T) {
	if _, ok := New(Config{}).MeteredPrice(context.Background(), "ddg_instant", "web_search"); ok {
		t.Fatal("with no catalog every metered operation is unpriced; there is no default table")
	}
}
