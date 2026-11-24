package adminaccess

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// lookupCounter answers the seed-table lookup and counts it.
type lookupCounter struct {
	present bool
	lookups int
}

func (l *lookupCounter) Query(context.Context, string, ...any) (pgx.Rows, error) {
	panic("not used")
}

func (l *lookupCounter) QueryRow(context.Context, string, ...any) pgx.Row {
	l.lookups++
	return presentRow(l.present)
}

type presentRow bool

func (r presentRow) Scan(dest ...any) error {
	*dest[0].(*bool) = bool(r)
	return nil
}

// TestAnAbsentSeedTableIsLookedForOncePerMinute: without the table, the
// console does not pay one more query per request, and a table a migration
// adds later is still seen.
func TestAnAbsentSeedTableIsLookedForOncePerMinute(t *testing.T) {
	t.Parallel()
	pool := &lookupCounter{}
	clock := time.Unix(1_800_000_000, 0)
	roles := &PostgresRoles{pool: pool, now: func() time.Time { return clock }}

	for range 3 {
		if present, err := roles.seedsPresent(context.Background()); err != nil || present {
			t.Fatalf("present=%v err=%v, want absent", present, err)
		}
	}
	if pool.lookups != 1 {
		t.Fatalf("%d lookups within the minute, want 1", pool.lookups)
	}

	pool.present = true
	clock = clock.Add(absentFor)
	if present, _ := roles.seedsPresent(context.Background()); !present {
		t.Fatal("the table added after a minute was not seen")
	}
	roles.seedsPresent(context.Background())
	if pool.lookups != 2 {
		t.Fatalf("%d lookups, want 2: a present table is cached for good", pool.lookups)
	}
}
