package cost

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// imageDB answers the image price statement with one canned result and counts
// the queries, so a test can see the cache.
type imageDB struct {
	perImage, per1MPixels *int64
	err                   error
	queries               int
	args                  []any
}

type imageRow struct {
	d *imageDB
}

func (d *imageDB) QueryRow(_ context.Context, _ string, args ...any) pgxRow {
	d.queries++
	d.args = args
	return imageRow{d}
}

func (r imageRow) Scan(dest ...any) error {
	if r.d.err != nil {
		return r.d.err
	}
	*dest[0].(**int64) = r.d.perImage
	*dest[1].(**int64) = r.d.per1MPixels
	return nil
}

func TestImageUnitNano(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		db     *imageDB
		size   string
		want   int64
		wantOK bool
	}{
		{"flat per-image price", &imageDB{perImage: ptr(120_000_000)}, "1792x1024", 120_000_000, true},
		// 0.05 USD per 1M pixels = 50,000,000 nano per 1M pixels; 1024*1024 pixels.
		{"per-pixel price takes the size", &imageDB{per1MPixels: ptr(50_000_000)}, "1024x1024", 52_428_800, true},
		{"per-pixel price with a size spelled the sheet way", &imageDB{per1MPixels: ptr(50_000_000)}, "1024-x-1024", 52_428_800, true},
		{"per-pixel price and no readable size is unpriced", &imageDB{per1MPixels: ptr(50_000_000)}, "auto", 0, false},
		{"zero price is no price", &imageDB{perImage: ptr(0)}, "1024x1024", 0, false},
		{"no row", &imageDB{err: pgx.ErrNoRows}, "1024x1024", 0, false},
		{"missing table", &imageDB{err: &pgconn.PgError{Code: "42P01"}}, "1024x1024", 0, false},
		{"read error", &imageDB{err: errors.New("boom")}, "1024x1024", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New(Config{DB: tc.db})
			got, ok := c.ImageUnitNano(ctx, "openai", "dall-e-3", tc.size, "hd")
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("ImageUnitNano = %d, %v; want %d, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestImageUnitNanoCachesAnswersAndMisses(t *testing.T) {
	db := &imageDB{perImage: ptr(40_000_000)}
	c := New(Config{DB: db})
	for range 3 {
		if got, ok := c.ImageUnitNano(context.Background(), "openai", "dall-e-3", "1024x1024", "standard"); !ok || got != 40_000_000 {
			t.Fatalf("got %d, %v", got, ok)
		}
	}
	if db.queries != 1 {
		t.Fatalf("queries = %d, want 1 (cached)", db.queries)
	}
	miss := &imageDB{err: pgx.ErrNoRows}
	c = New(Config{DB: miss})
	c.ImageUnitNano(context.Background(), "openai", "x", "", "")
	c.ImageUnitNano(context.Background(), "openai", "x", "", "")
	if miss.queries != 1 {
		t.Fatalf("a miss queried %d times, want 1 (negative cache)", miss.queries)
	}
}

func TestImageUnitNanoSendsTheNanoScaleAndNormalizedRequest(t *testing.T) {
	db := &imageDB{perImage: ptr(1)}
	c := New(Config{DB: db})
	c.ImageUnitNano(context.Background(), "openai", "dall-e-3", " 1792X1024 ", " HD ")
	if db.args[2] != NanoUSD || db.args[3] != "1792x1024" || db.args[4] != "hd" {
		t.Fatalf("args = %v", db.args)
	}
}

func TestImageUnitNanoWithoutADatabase(t *testing.T) {
	if _, ok := New(Config{}).ImageUnitNano(context.Background(), "openai", "dall-e-3", "1024x1024", ""); ok {
		t.Fatal("no database must mean no catalog price")
	}
}

func TestImagePixels(t *testing.T) {
	for in, want := range map[string]int64{"1024x1024": 1048576, "1792X1024": 1835008, "256-x-256": 65536} {
		if got, ok := ImagePixels(in); !ok || got != want {
			t.Errorf("ImagePixels(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "auto", "0x5", "axb", "-1x4", "99999999x99999999"} {
		if _, ok := ImagePixels(in); ok {
			t.Errorf("ImagePixels(%q) must not parse", in)
		}
	}
}
