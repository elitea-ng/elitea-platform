package authstateretention

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

// failingStore answers every statement with an error, and counts them.
type failingStore struct {
	err     error
	queries int
}

type errorRow struct{ err error }

func (r errorRow) Scan(...any) error { return r.err }

func (s *failingStore) QueryRow(context.Context, string, ...any) pgx.Row {
	s.queries++
	return errorRow{err: s.err}
}

func TestNewRefusesAMissingStoreOrGate(t *testing.T) {
	if _, err := New(nil, func(context.Context) bool { return false }, Config{}, nil); err == nil {
		t.Fatal("a sweeper without a store was built")
	}
	if _, err := New(&failingStore{}, nil, Config{}, nil); err == nil {
		t.Fatal("a sweeper without a maintenance gate was built")
	}
	sweeper, err := New(&failingStore{}, func(context.Context) bool { return false }, Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sweeper.cfg.Interval != DefaultInterval || sweeper.cfg.BatchSize != DefaultBatchSize ||
		sweeper.cfg.MaxBatchesPerPass != DefaultMaxBatchesPerPass {
		t.Fatalf("defaults = %+v", sweeper.cfg)
	}
}

// Maintenance mode suppresses the pass before any statement runs.
func TestMaintenanceModeSuppressesThePass(t *testing.T) {
	store := &failingStore{err: errors.New("must not be called")}
	sweeper, err := New(store, func(context.Context) bool { return true }, Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := sweeper.Sweep(context.Background())
	if err != nil || !stats.Suppressed || store.queries != 0 {
		t.Fatalf("stats=%+v err=%v queries=%d", stats, err, store.queries)
	}
}

func TestADatabaseFailureFailsThePassAndNamesTheTable(t *testing.T) {
	store := &failingStore{err: errors.New("connection reset")}
	sweeper, err := New(store, func(context.Context) bool { return false }, Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sweeper.Sweep(context.Background()); err == nil || store.queries != 1 {
		t.Fatalf("err=%v queries=%d", err, store.queries)
	}
}
