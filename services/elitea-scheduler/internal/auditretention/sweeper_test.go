package auditretention

// What is worth pinning here is not that a DELETE is issued, but the five
// decisions around it: which rows the cutoff selects, how a pass is bounded,
// what a maintenance window does to it, which configurations are refused, and
// that the composition root actually starts the thing.
//
// The boundary itself — a row exactly on the edge and one either side — is
// asserted against a real PostgreSQL in
// sweeper_postgres_integration_test.go, because `timestamp < cutoff` is the
// database's comparison and a fake store repeating it would only prove that
// this file agrees with itself.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// recordingStore answers each Exec with a chosen row count and keeps every call
// so a test asserts the arguments the sweeper actually sent.
type recordingStore struct {
	// removals is consumed one entry per Exec; a call past the end returns 0.
	removals []int64
	err      error
	calls    []recordedExec
}

type recordedExec struct {
	sql  string
	args []any
}

func (s *recordingStore) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	s.calls = append(s.calls, recordedExec{sql: sql, args: args})
	if s.err != nil {
		return pgconn.CommandTag{}, s.err
	}
	var removed int64
	if len(s.calls) <= len(s.removals) {
		removed = s.removals[len(s.calls)-1]
	}
	return pgconn.NewCommandTag("DELETE " + strconv.FormatInt(removed, 10)), nil
}

func openGate(context.Context) bool   { return false }
func closedGate(context.Context) bool { return true }

// newTestSweeper builds a sweeper with a frozen clock, so a cutoff can be
// compared exactly rather than within a tolerance.
func newTestSweeper(t *testing.T, store Store, gate Gate, cfg Config, at time.Time) *Sweeper {
	t.Helper()
	sweeper, err := New(store, gate, cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sweeper.now = func() time.Time { return at }
	return sweeper
}

// TestTheCutoffIsTheWindowBehindNow.
//
// The cutoff is the only thing that decides which rows die, and it is computed
// rather than stored. A window applied in the wrong direction, or in hours
// instead of days, deletes the whole table on the first pass — so the instant
// itself is asserted, not merely that some parameter was passed.
func TestTheCutoffIsTheWindowBehindNow(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	store := &recordingStore{removals: []int64{3}}
	sweeper := newTestSweeper(t, store, openGate, Config{
		RetentionDays: 30, BatchSize: 500, MaxBatchesPerPass: 10,
	}, now)

	stats, err := sweeper.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	want := now.Add(-30 * 24 * time.Hour)
	if !stats.Cutoff.Equal(want) {
		t.Errorf("cutoff = %s, want %s", stats.Cutoff, want)
	}
	if len(store.calls) != 1 {
		t.Fatalf("issued %d statement(s), want 1", len(store.calls))
	}
	sent, ok := store.calls[0].args[0].(time.Time)
	if !ok || !sent.Equal(want) {
		t.Errorf("the DELETE was given %v as its cutoff, want %s", store.calls[0].args[0], want)
	}
	if store.calls[0].args[1] != 500 {
		t.Errorf("the DELETE was given %v as its batch size, want 500", store.calls[0].args[1])
	}
	if stats.Deleted != 3 || stats.Batches != 1 || !stats.Drained {
		t.Errorf("stats = %+v, want 3 rows in 1 drained batch", stats)
	}
}

// TestTheStatementDeletesOnlyOlderRows reads the SQL itself.
//
// Every other assertion in this file goes through a store that returns whatever
// it is told, so none of them would notice a `>` where a `<` belongs. The
// integration test does notice, and it skips without a database — which is
// exactly the condition under which a wrong comparison would reach main.
func TestTheStatementDeletesOnlyOlderRows(t *testing.T) {
	t.Parallel()

	for _, fragment := range []string{
		"centry.audit_events",
		"timestamp < $1",
		"ORDER BY timestamp",
		"LIMIT $2",
		"FOR UPDATE SKIP LOCKED",
	} {
		if !strings.Contains(deleteBatchSQL, fragment) {
			t.Errorf("the batch statement no longer contains %q:\n%s", fragment, deleteBatchSQL)
		}
	}
	for _, forbidden := range []string{"INSERT", "UPDATE centry", "TRUNCATE"} {
		if strings.Contains(deleteBatchSQL, forbidden) {
			t.Errorf("the sweeper writes as well as deletes: %q appears in the statement", forbidden)
		}
	}
}

// TestAPassIsBoundedByItsBatchCeiling.
//
// A full batch means there may be more, so the pass continues; the ceiling is
// what stops it. A pass that ended at its ceiling must NOT report that it
// drained the table — "drained" is what tells an operator the bound is being
// met, and a pass that claims it while a backlog remains is the same class of
// lie as a schedule stamping `last_run` for work nothing performed.
func TestAPassIsBoundedByItsBatchCeiling(t *testing.T) {
	t.Parallel()

	store := &recordingStore{removals: []int64{100, 100, 100, 100, 100}}
	sweeper := newTestSweeper(t, store, openGate, Config{
		RetentionDays: 90, BatchSize: 100, MaxBatchesPerPass: 3,
	}, time.Now())

	stats, err := sweeper.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if stats.Batches != 3 {
		t.Errorf("the pass issued %d statements, want its ceiling of 3", stats.Batches)
	}
	if stats.Deleted != 300 {
		t.Errorf("deleted = %d, want 300", stats.Deleted)
	}
	if stats.Drained {
		t.Error("a pass that stopped at its ceiling reported that it drained the table")
	}
}

// TestAShortBatchEndsThePass — the other exit. One statement fewer than the
// batch size means the eligible rows are gone, and issuing another DELETE would
// be a statement that can only find nothing.
func TestAShortBatchEndsThePass(t *testing.T) {
	t.Parallel()

	store := &recordingStore{removals: []int64{100, 100, 42}}
	sweeper := newTestSweeper(t, store, openGate, Config{
		RetentionDays: 90, BatchSize: 100, MaxBatchesPerPass: 20,
	}, time.Now())

	stats, err := sweeper.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if stats.Batches != 3 || stats.Deleted != 242 || !stats.Drained {
		t.Errorf("stats = %+v, want 242 rows in 3 batches, drained", stats)
	}
	if len(store.calls) != 3 {
		t.Errorf("issued %d statements after a short batch, want 3", len(store.calls))
	}
}

// TestAMaintenanceWindowDeletesNothing.
//
// The gate is asked BEFORE the cutoff is computed and before any statement is
// issued, so a suppressed pass is provably a pass that touched no rows — not
// one whose deletes happened to find nothing.
func TestAMaintenanceWindowDeletesNothing(t *testing.T) {
	t.Parallel()

	store := &recordingStore{removals: []int64{500}}
	sweeper := newTestSweeper(t, store, closedGate, Config{
		RetentionDays: 90, BatchSize: 500, MaxBatchesPerPass: 10,
	}, time.Now())

	stats, err := sweeper.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if !stats.Suppressed {
		t.Error("a pass inside a maintenance window did not report itself suppressed")
	}
	if stats.Deleted != 0 || stats.Batches != 0 {
		t.Errorf("a suppressed pass deleted %d row(s) in %d batch(es)", stats.Deleted, stats.Batches)
	}
	if len(store.calls) != 0 {
		t.Errorf("a suppressed pass issued %d statement(s): %+v", len(store.calls), store.calls)
	}
}

// TestASuppressedPassConsumesNoWindow.
//
// The recoverable half of the gate, and the reason there is no cursor in this
// package. The pass after the window closes must remove exactly what the
// suppressed pass would have removed, so a maintenance window delays the bound
// rather than punching a hole in it.
func TestASuppressedPassConsumesNoWindow(t *testing.T) {
	t.Parallel()

	var closed bool
	gate := func(context.Context) bool { return closed }
	store := &recordingStore{removals: []int64{7}}
	sweeper := newTestSweeper(t, store, gate, Config{
		RetentionDays: 90, BatchSize: 500, MaxBatchesPerPass: 10,
	}, time.Now())

	closed = true
	if _, err := sweeper.Sweep(context.Background()); err != nil {
		t.Fatalf("suppressed pass: %v", err)
	}
	closed = false
	stats, err := sweeper.Sweep(context.Background())
	if err != nil {
		t.Fatalf("pass after the window: %v", err)
	}
	if stats.Deleted != 7 {
		t.Errorf("the pass after the window deleted %d row(s), want the 7 the suppressed pass left", stats.Deleted)
	}
}

// TestAFailedStatementReportsWhatItDidFirst.
//
// The count matters as much as the error: an operator reading "the sweep failed"
// needs to know whether it failed before or after removing rows.
func TestAFailedStatementReportsWhatItDidFirst(t *testing.T) {
	t.Parallel()

	failing := errors.New("deadlock detected")
	store := &recordingStore{err: failing}
	sweeper := newTestSweeper(t, store, openGate, Config{
		RetentionDays: 90, BatchSize: 100, MaxBatchesPerPass: 10,
	}, time.Now())

	stats, err := sweeper.Sweep(context.Background())
	if !errors.Is(err, failing) {
		t.Fatalf("Sweep err = %v, want the store's error", err)
	}
	if stats.Deleted != 0 || stats.Drained {
		t.Errorf("a failed pass reported %+v; it must never claim to have drained", stats)
	}
}

// TestACancelledContextStopsBetweenBatches. The pass is the unit of shutdown,
// not the whole loop: a daemon told to stop must not sit through another
// forty-nine DELETEs first.
func TestACancelledContextStopsBetweenBatches(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := &recordingStore{removals: []int64{100, 100, 100}}
	sweeper := newTestSweeper(t, store, openGate, Config{
		RetentionDays: 90, BatchSize: 100, MaxBatchesPerPass: 10,
	}, time.Now())

	if _, err := sweeper.Sweep(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sweep err = %v, want context.Canceled", err)
	}
	if len(store.calls) != 0 {
		t.Errorf("a cancelled pass issued %d statement(s)", len(store.calls))
	}
}

// TestTheConfigurationsThatAreRefused.
//
// Each refusal is a deletion that does NOT happen, which is why they are stated
// one by one rather than as "New returns an error". A short window is refused
// rather than clamped: acting on a likely typo destroys an audit trail that no
// later configuration change brings back.
func TestTheConfigurationsThatAreRefused(t *testing.T) {
	t.Parallel()

	store := &recordingStore{}
	for name, testCase := range map[string]struct {
		store Store
		gate  Gate
		cfg   Config
		want  error
	}{
		"no store":         {store: nil, gate: openGate, cfg: Config{RetentionDays: 30}},
		"no gate":          {store: store, gate: nil, cfg: Config{RetentionDays: 30}},
		"window of zero":   {store: store, gate: openGate, cfg: Config{RetentionDays: 0}, want: ErrDisabled},
		"negative window":  {store: store, gate: openGate, cfg: Config{RetentionDays: -1}, want: ErrDisabled},
		"a one-day typo":   {store: store, gate: openGate, cfg: Config{RetentionDays: 1}, want: ErrWindowTooShort},
		"just below floor": {store: store, gate: openGate, cfg: Config{RetentionDays: MinimumRetentionDays - 1}, want: ErrWindowTooShort},
	} {
		sweeper, err := New(testCase.store, testCase.gate, testCase.cfg, nil)
		if err == nil {
			t.Errorf("%s: New returned a sweeper that would delete rows", name)
			continue
		}
		if sweeper != nil {
			t.Errorf("%s: New returned both an error and a sweeper", name)
		}
		if testCase.want != nil && !errors.Is(err, testCase.want) {
			t.Errorf("%s: New err = %v, want %v", name, err, testCase.want)
		}
	}

	// The floor itself is accepted — a boundary that refused its own value
	// would make the constant a lie.
	if _, err := New(store, openGate, Config{RetentionDays: MinimumRetentionDays}, nil); err != nil {
		t.Errorf("New refused the floor itself (%d days): %v", MinimumRetentionDays, err)
	}
}

// TestAPartlyFilledConfigGetsBounds.
//
// A zero BatchSize would otherwise send `LIMIT 0` — a pass that deletes nothing
// and reports itself drained, i.e. an inert sweeper that looks healthy. A zero
// ceiling would end every pass before its first statement.
func TestAPartlyFilledConfigGetsBounds(t *testing.T) {
	t.Parallel()

	store := &recordingStore{removals: []int64{5}}
	sweeper := newTestSweeper(t, store, openGate, Config{RetentionDays: 30}, time.Now())

	stats, err := sweeper.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(store.calls) != 1 {
		t.Fatalf("issued %d statement(s), want 1", len(store.calls))
	}
	if store.calls[0].args[1] != DefaultBatchSize {
		t.Errorf("batch size = %v, want the default %d", store.calls[0].args[1], DefaultBatchSize)
	}
	if stats.Deleted != 5 {
		t.Errorf("deleted = %d, want 5", stats.Deleted)
	}
	if got := sweeper.passTimeout(); got <= 0 {
		t.Errorf("passTimeout = %v; a non-positive timeout cancels every pass immediately", got)
	}
}

// TestTheSweeperIsStartedByTheDaemon.
//
// The defect this guards is the one that has landed here repeatedly: a
// correctly implemented, fully unit-tested worker that no composition root ever
// constructs. Every test above would pass on a binary that never calls New.
//
// cmd/elitea-scheduler has no seam a test can call, so the source is read. It
// is the same instrument internal/scheduler/maintenance_test.go uses on
// elitea-main's platformconfig, and it fails on the change that matters —
// deleting the wiring — rather than on formatting.
func TestTheSweeperIsStartedByTheDaemon(t *testing.T) {
	t.Parallel()

	mainPath := filepath.Join("..", "..", "cmd", "elitea-scheduler", "main.go")
	source, err := os.ReadFile(filepath.Clean(mainPath))
	if err != nil {
		t.Fatalf("read %s: %v", mainPath, err)
	}
	text := string(source)

	for defect, fragment := range map[string]string{
		"the sweeper is never constructed":                   "auditretention.New(",
		"the sweeper is constructed and never run":           "auditSweeper.Run(ctx)",
		"the sweeper reads its own switch instead of config": "cfg.AuditRetentionDays",
		"the sweeper does not share the dispatch gate":       "sched.MaintenanceActive",
	} {
		if !strings.Contains(text, fragment) {
			t.Errorf("%s: %q no longer appears in %s", defect, fragment, mainPath)
		}
	}
}

// TestTheDeploymentFilesDeclareEveryKeyTheConfigReads.
//
// The env names are a deployment contract with no compiler behind them, and the
// failure mode is silent in exactly the direction that matters: a key the chart
// never sets leaves the sweeper on its built-in default forever, and an
// operator who edits the chart sees nothing change. Issues #394 and #395 are
// two flags that sat in this state.
//
// Both shipped stacks are checked. The files are read from the source tree, so
// this skips rather than fails where the deploy directory is not checked out.
func TestTheDeploymentFilesDeclareEveryKeyTheConfigReads(t *testing.T) {
	t.Parallel()

	keys := []string{
		"AUDIT_RETENTION_DAYS",
		"AUDIT_RETENTION_INTERVAL",
		"AUDIT_RETENTION_BATCH_SIZE",
		"AUDIT_RETENTION_MAX_BATCHES",
	}

	for _, path := range []string{
		filepath.Join("..", "..", "..", "..", "deploy", "helm", "elitea", "values.yaml"),
		filepath.Join("..", "..", "..", "..", "deploy", "docker-compose.yml"),
	} {
		raw, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			t.Skipf("%s is not checked out beside this module: %v", path, err)
		}
		for _, key := range keys {
			if !strings.Contains(string(raw), key+":") {
				t.Errorf("%s does not set %s, so no operator can change it there", path, key)
			}
		}
	}
}
