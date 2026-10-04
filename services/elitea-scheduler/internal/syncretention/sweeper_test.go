package syncretention

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type unusedStore struct{ t *testing.T }

func (s unusedStore) Query(context.Context, string, ...any) (pgx.Rows, error) {
	s.t.Fatal("a suppressed pass touched the database")
	return nil, nil
}
func (s unusedStore) QueryRow(context.Context, string, ...any) pgx.Row {
	s.t.Fatal("a suppressed pass touched the database")
	return nil
}
func (s unusedStore) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	s.t.Fatal("a suppressed pass touched the database")
	return pgconn.CommandTag{}, nil
}

// elitea-main's changesync.TombstoneRetention is MaxOfflineRetentionDays (90)
// + 7 days; this floor must equal it or a served cursor can miss a deletion.
func TestMinimumRetentionMatchesTheCursorWindow(t *testing.T) {
	if MinimumRetentionDays != 90+7 {
		t.Fatalf("MinimumRetentionDays = %d, want 97", MinimumRetentionDays)
	}
}

func TestNewRaisesAWindowBelowTheFloor(t *testing.T) {
	gate := func(context.Context) bool { return false }
	for _, tc := range []struct {
		days       int
		wantDays   int
		wantRaised bool
	}{{0, 97, false}, {-1, 97, true}, {30, 97, true}, {97, 97, false}, {400, 400, false}} {
		sweeper, raised, err := New(unusedStore{t}, gate, Config{RetentionDays: tc.days}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if sweeper.Window() != time.Duration(tc.wantDays)*24*time.Hour || raised != tc.wantRaised {
			t.Errorf("days=%d: window=%v raised=%v, want %d days raised=%v", tc.days, sweeper.Window(), raised, tc.wantDays, tc.wantRaised)
		}
	}
	if _, _, err := New(unusedStore{t}, nil, Config{}, nil); err == nil {
		t.Fatal("a sweeper without a maintenance gate was built")
	}
}

func TestMaintenanceSuppressesThePass(t *testing.T) {
	sweeper, _, err := New(unusedStore{t}, func(context.Context) bool { return true }, Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := sweeper.Sweep(context.Background())
	if err != nil || !stats.Suppressed {
		t.Fatalf("stats=%+v err=%v, want a suppressed pass", stats, err)
	}
}

// The sweeper must be constructed, run and gated in the daemon, and both
// shipped stacks must declare its one knob.
func TestTheDaemonRunsTheSweeperAndTheStacksDeclareItsKnob(t *testing.T) {
	read := func(path string) string {
		raw, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			t.Skipf("%s is not checked out beside this module: %v", path, err)
		}
		return string(raw)
	}
	main := read(filepath.Join("..", "..", "cmd", "elitea-scheduler", "main.go"))
	for _, fragment := range []string{"syncretention.New(pool, sched.MaintenanceActive", "syncSweeper.Run(ctx)", "cfg.SyncTombstoneRetentionDays"} {
		if !strings.Contains(main, fragment) {
			t.Errorf("cmd/elitea-scheduler/main.go no longer contains %q", fragment)
		}
	}
	for _, path := range []string{
		filepath.Join("..", "..", "..", "..", "deploy", "helm", "elitea", "values.yaml"),
		filepath.Join("..", "..", "..", "..", "deploy", "docker-compose.yml"),
	} {
		if !strings.Contains(read(path), "SYNC_TOMBSTONE_RETENTION_DAYS:") {
			t.Errorf("%s does not set SYNC_TOMBSTONE_RETENTION_DAYS", path)
		}
	}
}
