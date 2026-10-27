package securityauditretention

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

type fakeStore struct {
	affected []int64
	calls    int
	args     [][]any
	sql      string
}

func (f *fakeStore) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.sql = sql
	f.args = append(f.args, args)
	n := int64(0)
	if f.calls < len(f.affected) {
		n = f.affected[f.calls]
	}
	f.calls++
	return pgconn.NewCommandTag("DELETE " + strconv.FormatInt(n, 10)), nil
}

func closedGate(context.Context) bool { return false }

func lookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func TestTheDefaultIsNoDeletion(t *testing.T) {
	cfg, err := ConfigFromEnv(lookup(nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(&fakeStore{}, closedGate, cfg, nil); !errors.Is(err, ErrDisabled) {
		t.Fatalf("New with no configuration = %v, want ErrDisabled", err)
	}
}

func TestConfigFromEnvRefusesBadValues(t *testing.T) {
	for name, values := range map[string]map[string]string{
		"days":     {EnvRetentionDays: "x"},
		"negative": {EnvRetentionDays: "-1"},
		"batch":    {EnvBatchSize: "1.5"},
		"interval": {EnvInterval: "10s"},
		"garbage":  {EnvInterval: "soon"},
	} {
		if _, err := ConfigFromEnv(lookup(values)); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	cfg, err := ConfigFromEnv(lookup(map[string]string{EnvRetentionDays: "30", EnvInterval: "2h", EnvBatchSize: "10", EnvMaxBatches: "3"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg != (Config{RetentionDays: 30, Interval: 2 * time.Hour, BatchSize: 10, MaxBatchesPerPass: 3}) {
		t.Fatalf("cfg %+v", cfg)
	}
}

func TestSweepDeletesOnlyExportedAndStoredRows(t *testing.T) {
	// The guards live in the statement; this pins them so an edit that drops
	// one fails here and not in an incident.
	for _, guard := range []string{
		"o.exported_at IS NOT NULL",
		"o.exported_at < $1",
		"elitea_identity.security_audit_events d",
		"d.hash = o.hash",
		"FOR UPDATE OF o SKIP LOCKED",
		"LIMIT $2",
	} {
		if !strings.Contains(deleteBatchSQL, guard) {
			t.Errorf("delete statement lost the guard %q", guard)
		}
	}
	if strings.Contains(deleteBatchSQL, "DELETE FROM elitea_identity") {
		t.Error("the sweeper deletes from the durable store")
	}
}

func TestSweepBatchesUntilShort(t *testing.T) {
	store := &fakeStore{affected: []int64{10, 10, 3}}
	sweeper, err := New(store, closedGate, Config{RetentionDays: 30, BatchSize: 10, MaxBatchesPerPass: 5}, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	sweeper.now = func() time.Time { return now }
	stats, err := sweeper.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Deleted != 23 || stats.Batches != 3 || !stats.Drained {
		t.Fatalf("stats %+v", stats)
	}
	if cutoff := store.args[0][0].(time.Time); !cutoff.Equal(now.Add(-30 * 24 * time.Hour)) {
		t.Fatalf("cutoff %s", cutoff)
	}
}

func TestSweepStopsAtTheBatchCeilingAndInMaintenance(t *testing.T) {
	store := &fakeStore{affected: []int64{10, 10, 10}}
	sweeper, _ := New(store, closedGate, Config{RetentionDays: 30, BatchSize: 10, MaxBatchesPerPass: 2}, nil)
	stats, err := sweeper.Sweep(context.Background())
	if err != nil || stats.Batches != 2 || stats.Drained {
		t.Fatalf("stats %+v err %v", stats, err)
	}
	gated := &fakeStore{}
	sweeper, _ = New(gated, func(context.Context) bool { return true }, Config{RetentionDays: 30}, nil)
	stats, err = sweeper.Sweep(context.Background())
	if err != nil || !stats.Suppressed || gated.calls != 0 {
		t.Fatalf("maintenance: stats %+v calls %d err %v", stats, gated.calls, err)
	}
}

func TestNewRequiresStoreAndGate(t *testing.T) {
	if _, err := New(nil, closedGate, Config{RetentionDays: 30}, nil); err == nil {
		t.Error("nil store accepted")
	}
	if _, err := New(&fakeStore{}, nil, Config{RetentionDays: 30}, nil); err == nil {
		t.Error("nil gate accepted")
	}
}
