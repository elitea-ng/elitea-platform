package auditexport

// The audit export relays against a real PostgreSQL built from the ledgered
// migration corpus (dbtest template, copied per test). Runs when
// ELITEA_TEST_DATABASE_URL is set; skips otherwise.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/dbtest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

const (
	databaseURLEnv  = "ELITEA_TEST_DATABASE_URL"
	bootstrapSchema = "../infra/db/migrations/001_initial.sql"
)

var (
	exportTemplate string
	exportDBSeq    atomic.Uint64
)

func TestMain(m *testing.M) {
	databaseURL := testpg.DSN(os.Getenv(databaseURLEnv))
	if databaseURL == "" {
		os.Exit(m.Run())
	}
	bootstrap, err := os.ReadFile(bootstrapSchema)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read bootstrap schema: %v\n", err)
		os.Exit(1)
	}
	ctx, cancel := dbtest.BuildContext(context.Background())
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open admin pool: %v\n", err)
		cancel()
		os.Exit(1)
	}
	templateName, err := dbtest.EnsureTemplate(ctx, adminPool, dbtest.Spec{
		Files: platformmigrations.Files,
		Seed:  string(bootstrap),
	})
	adminPool.Close()
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build auditexport template: %v\n", err)
		os.Exit(1)
	}
	exportTemplate = templateName
	os.Exit(m.Run())
}

func newExportPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := testpg.DSN(os.Getenv(databaseURLEnv))
	if databaseURL == "" {
		t.Skipf("set %s to run the audit export integration tests", databaseURLEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()
	databaseName := fmt.Sprintf("elitea_auditexport_%d_%d_%d", os.Getpid(), time.Now().UnixNano(), exportDBSeq.Add(1))
	if err := dbtest.CreateFromTemplate(ctx, adminPool, exportTemplate, databaseName); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = databaseName
	config.MaxConns = 6
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open isolated pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		dropPool, dropErr := pgxpool.New(dropCtx, databaseURL)
		if dropErr != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(dropCtx, "DROP DATABASE IF EXISTS "+pgx.Identifier{databaseName}.Sanitize()+" WITH (FORCE)")
	})
	return pool
}

// flakySink records every delivered event id and can be switched off.
type flakySink struct {
	name      string
	mu        sync.Mutex
	down      bool
	delivered []string
	// onSend runs before each delivery (tests that race the relay).
	onSend func()
}

func (f *flakySink) Name() string {
	if f.name == "" {
		return "test"
	}
	return f.name
}
func (f *flakySink) Send(_ context.Context, events []Event) error {
	if f.onSend != nil {
		f.onSend()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return errors.New("receiver down")
	}
	for _, e := range events {
		f.delivered = append(f.delivered, e.Ref())
	}
	return nil
}
func (f *flakySink) setDown(v bool) { f.mu.Lock(); f.down = v; f.mu.Unlock() }
func (f *flakySink) refs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.delivered...)
}

const insertAuditSQL = `
INSERT INTO centry.audit_events (timestamp, user_id, event_type, action, http_method, http_route, status_code, duration_ms, is_error)
VALUES (clock_timestamp() - make_interval(secs => $1), 7, 'api', 'GET /x', 'GET', '/x', 200, 1.5, false)
RETURNING id`

func insertAudit(t *testing.T, pool *pgxpool.Pool, n int, age time.Duration) []int64 {
	t.Helper()
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		var id int64
		if err := pool.QueryRow(context.Background(), insertAuditSQL, age.Seconds()).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func ref(id int64) string { return fmt.Sprintf("admin_audit:%d", id) }

func cursor(t *testing.T, pool *pgxpool.Pool, sink string) position {
	t.Helper()
	var at position
	if err := pool.QueryRow(context.Background(),
		`SELECT last_txid, last_id FROM elitea_runtime.audit_export_cursors WHERE stream = $1 AND sink = $2`,
		StreamAdminAudit, sink).Scan(&at.txid, &at.id); err != nil {
		t.Fatal(err)
	}
	return at
}

func newRelay(t *testing.T, pool *pgxpool.Pool, sink Sink, tweak func(*AuditEventsConfig)) *AuditEventsRelay {
	t.Helper()
	cfg := AuditEventsConfig{Sinks: []Sink{sink}, BatchSize: 3, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if tweak != nil {
		tweak(&cfg)
	}
	relay, err := NewAuditEventsRelay(pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.Close)
	return relay
}

// drainUntil drains until want events were settled in total. The relay reads
// only rows whose transaction is older than every running one, and a test
// database shares its cluster with other tests' transactions, so a row may
// wait a moment.
func drainUntil(t *testing.T, relay *AuditEventsRelay, want int) int {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	total := 0
	for {
		n, err := relay.Drain(context.Background())
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
		total += n
		if total >= want || time.Now().After(deadline) {
			return total
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestAuditEventsRelayStartsAtNewestRowAndDeliversNewOnes(t *testing.T) {
	pool := newExportPool(t)
	// A transaction open elsewhere, with an xid, holds the snapshot xmin
	// below the history rows. A start position taken from xmin alone would
	// then replay them; CI hit exactly this through other packages' tests.
	older, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := older.Exec(context.Background(), `SELECT pg_current_xact_id()`); err != nil {
		t.Fatal(err)
	}
	insertAudit(t, pool, 5, time.Hour) // history before export was enabled
	sink := &flakySink{}
	relay := newRelay(t, pool, sink, nil)

	if n, err := relay.Drain(context.Background()); err != nil || n != 0 {
		t.Fatalf("history must not be replayed by default: n=%d err=%v", n, err)
	}
	if err := older.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	fresh := insertAudit(t, pool, 7, 0)
	if n := drainUntil(t, relay, 7); n != 7 {
		t.Fatalf("drain = %d; want 7 in batches of 3", n)
	}
	if got := sink.refs(); len(got) != 7 || got[0] != ref(fresh[0]) || got[6] != ref(fresh[6]) {
		t.Fatalf("delivered = %v", got)
	}
	if cursor(t, pool, "test").id != fresh[6] {
		t.Fatalf("cursor = %+v, want id %d", cursor(t, pool, "test"), fresh[6])
	}
	if n, _ := relay.Drain(context.Background()); n != 0 {
		t.Fatal("exported the same rows twice")
	}
}

func TestAuditEventsRelayFromBeginningReplaysHistory(t *testing.T) {
	pool := newExportPool(t)
	insertAudit(t, pool, 4, time.Hour)
	// Rows from before migration 0196 carry export_txid 0.
	legacy := insertAudit(t, pool, 2, time.Hour)
	if _, err := pool.Exec(context.Background(), `UPDATE centry.audit_events SET export_txid = 0 WHERE id = ANY($1)`, legacy); err != nil {
		t.Fatal(err)
	}
	sink := &flakySink{}
	relay := newRelay(t, pool, sink, func(c *AuditEventsConfig) { c.FromBeginning = true })
	if n := drainUntil(t, relay, 6); n != 6 {
		t.Fatalf("drain = %d, want 6", n)
	}
	if got := sink.refs(); got[0] != ref(legacy[0]) || got[1] != ref(legacy[1]) {
		t.Fatalf("delivered = %v; rows from before 0196 come first", got)
	}
}

func TestAuditEventsRelayLosesNothingAcrossAReceiverOutage(t *testing.T) {
	pool := newExportPool(t)
	sink := &flakySink{}
	relay := newRelay(t, pool, sink, nil)
	_, _ = relay.Drain(context.Background()) // creates the cursor at the table's end
	start := cursor(t, pool, "test")
	ids := insertAudit(t, pool, 8, 0)

	sink.setDown(true)
	failed := false
	deadline := time.Now().Add(20 * time.Second)
	for !failed && time.Now().Before(deadline) {
		_, err := relay.Drain(context.Background())
		failed = err != nil
		time.Sleep(20 * time.Millisecond)
	}
	if !failed {
		t.Fatal("drain never reported the receiver outage")
	}
	_, _ = relay.Drain(context.Background())
	if cursor(t, pool, "test") != start {
		t.Fatal("the cursor advanced past undelivered rows")
	}
	more := insertAudit(t, pool, 2, 0)

	sink.setDown(false)
	if n := drainUntil(t, relay, 10); n != 10 {
		t.Fatalf("after restore: n=%d; want all 10", n)
	}
	seen := map[string]int{}
	for _, r := range sink.refs() {
		seen[r]++
	}
	for _, id := range append(ids, more...) {
		if seen[ref(id)] != 1 {
			t.Fatalf("row %d delivered %d times", id, seen[ref(id)])
		}
	}
}

// TestAuditEventsRelayNeverSkipsALateCommittingRow is the reason for
// export_txid: a row whose transaction is still open when a later row
// commits must not be passed by the cursor.
func TestAuditEventsRelayNeverSkipsALateCommittingRow(t *testing.T) {
	pool := newExportPool(t)
	sink := &flakySink{}
	relay := newRelay(t, pool, sink, nil)
	_, _ = relay.Drain(context.Background())

	slow, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = slow.Rollback(context.Background()) }()
	var lateID int64
	if err := slow.QueryRow(context.Background(), insertAuditSQL, 0.0).Scan(&lateID); err != nil {
		t.Fatal(err)
	}
	early := insertAudit(t, pool, 2, 0) // higher ids, committed first

	if n, err := relay.Drain(context.Background()); err != nil || n != 0 {
		t.Fatalf("n=%d err=%v; nothing may pass a transaction that is still open", n, err)
	}
	if err := slow.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := drainUntil(t, relay, 3); n != 3 {
		t.Fatalf("drain = %d, want 3", n)
	}
	got := sink.refs()
	want := []string{ref(lateID), ref(early[0]), ref(early[1])}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("delivered = %v, want %v (in commit order)", got, want)
	}
}

func TestAuditEventsRelayGivesEachSinkItsOwnPosition(t *testing.T) {
	pool := newExportPool(t)
	healthy := &flakySink{name: "otlp"}
	failing := &flakySink{name: "webhook", down: true}
	relay := newRelay(t, pool, healthy, func(c *AuditEventsConfig) { c.Sinks = []Sink{healthy, failing} })
	_, _ = relay.Drain(context.Background())

	var ids []int64
	for pass := 0; pass < 4; pass++ {
		ids = append(ids, insertAudit(t, pool, 4, 0)...)
		deadline := time.Now().Add(20 * time.Second)
		for len(healthy.refs()) < len(ids) && time.Now().Before(deadline) {
			_, _ = relay.Drain(context.Background()) // the failing sink makes it report an error
			time.Sleep(20 * time.Millisecond)
		}
	}
	if _, err := relay.Drain(context.Background()); err == nil {
		t.Fatal("drain reported success while one sink was down")
	}
	counts := map[string]int{}
	for _, r := range healthy.refs() {
		counts[r]++
	}
	for _, id := range ids {
		if counts[ref(id)] != 1 {
			t.Fatalf("healthy sink received row %d %d times across the passes, want once", id, counts[ref(id)])
		}
	}
	if len(failing.refs()) != 0 {
		t.Fatal("the failing sink accepted nothing")
	}

	failing.setDown(false)
	_, _ = relay.Drain(context.Background())
	if got := failing.refs(); len(got) != len(ids) || got[0] != ref(ids[0]) {
		t.Fatalf("failing sink resumed with %v, want all %d rows from its own position", got, len(ids))
	}
	if len(healthy.refs()) != len(ids) {
		t.Fatal("the recovered sink's catch-up resent rows to the healthy sink")
	}

	// A sink dropped from the configuration loses its position, so it no
	// longer holds the retention sweep back.
	solo := newRelay(t, pool, healthy, nil)
	relay.Close() // releases the relay lock at once, so solo's pass runs
	if _, err := solo.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM elitea_runtime.audit_export_cursors WHERE sink = 'webhook'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("removed sink's position rows = %d, %v", n, err)
	}
}

func TestAuditEventsRelayRefusesAnAdvanceWhenTheCursorMoved(t *testing.T) {
	pool := newExportPool(t)
	sink := &flakySink{}
	relay := newRelay(t, pool, sink, nil)
	_, _ = relay.Drain(context.Background())
	insertAudit(t, pool, 2, 0)
	// Another replica advances the cursor while this one is sending.
	sink.onSend = func() {
		if _, err := pool.Exec(context.Background(),
			`UPDATE elitea_runtime.audit_export_cursors SET last_id = last_id + 1000 WHERE stream = $1`, StreamAdminAudit); err != nil {
			t.Error(err)
		}
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		_, err := relay.Drain(context.Background())
		if errors.Is(err, ErrCursorMoved) {
			return
		}
		if err != nil || time.Now().After(deadline) {
			t.Fatalf("err = %v; want ErrCursorMoved", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestAuditEventsRelayHoldsNoTransactionWhileSending(t *testing.T) {
	pool := newExportPool(t)
	var open int
	sink := &flakySink{}
	sink.onSend = func() {
		if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM pg_stat_activity
 WHERE datname = current_database() AND pid <> pg_backend_pid() AND state LIKE 'idle in transaction%'`).Scan(&open); err != nil {
			t.Error(err)
		}
	}
	relay := newRelay(t, pool, sink, nil)
	_, _ = relay.Drain(context.Background())
	insertAudit(t, pool, 1, 0)
	if n := drainUntil(t, relay, 1); n != 1 {
		t.Fatalf("drain = %d", n)
	}
	if open != 0 {
		t.Fatalf("%d sessions idle in transaction during the send", open)
	}
}

func TestTwoRelaysNeverExportTheSameRowConcurrently(t *testing.T) {
	pool := newExportPool(t)
	sink := &flakySink{}
	a, b := newRelay(t, pool, sink, nil), newRelay(t, pool, sink, nil)
	_, _ = a.Drain(context.Background())
	ids := insertAudit(t, pool, 30, 0)
	var wg sync.WaitGroup
	for _, relay := range []*AuditEventsRelay{a, b, a, b} {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = relay.Drain(context.Background()) }()
	}
	wg.Wait()
	drainUntil(t, a, 30-len(sink.refs()))
	counts := map[string]int{}
	for _, r := range sink.refs() {
		counts[r]++
	}
	for _, id := range ids {
		if counts[ref(id)] != 1 {
			t.Fatalf("row %d delivered %d times", id, counts[ref(id)])
		}
	}
	// b never got the lock while a held it; once a lets go, b takes over.
	a.Close()
	more := insertAudit(t, pool, 1, 0)
	if n := drainUntil(t, b, 1); n != 1 || sink.refs()[len(sink.refs())-1] != ref(more[0]) {
		t.Fatalf("b did not take over: n=%d", n)
	}
}

func TestSecurityAuditRowStaysPendingUntilTheSinkAccepts(t *testing.T) {
	pool := newExportPool(t)
	appender, err := securityaudit.NewPostgresAppender("elitea-main/integration")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
			return appender.Append(context.Background(), tx, securityaudit.SecurityEvent{
				Type: securityaudit.TypeGrantChanged, TenantProjectID: 42, Action: "grant.revoke", PrincipalRef: "user:7",
			})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sink := &flakySink{down: true}
	relay, err := securityaudit.NewRelay(pool, securityaudit.RelayConfig{
		BacklogAlertThreshold: 100, Exporter: SecurityExporter{Sink: sink, DeadLetters: DeadLetterRecorder{DB: pool}}, BatchSize: 2,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := relay.Drain(context.Background()); err == nil {
		t.Fatal("relay reported success while the sink was down")
	}
	pending := func() int64 {
		var n int64
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM elitea_runtime.security_audit_outbox WHERE exported_at IS NULL`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if pending() != 3 {
		t.Fatalf("pending = %d; the relay acknowledged undelivered rows", pending())
	}

	metrics := NewMetrics()
	if err := (Monitor{DB: pool, Metrics: metrics, Security: true}).Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	_ = metrics.WriteText(&text)
	if !strings.Contains(text.String(), `elitea_audit_export_backlog{stream="security_audit"} 3`) {
		t.Fatalf("backlog metric:\n%s", text.String())
	}

	sink.setDown(false)
	stats, err := relay.Drain(context.Background())
	if err != nil || stats.Exported != 3 || pending() != 0 {
		t.Fatalf("after restore: %+v err=%v pending=%d", stats, err, pending())
	}
	if refs := sink.refs(); len(refs) != 3 || refs[0][:15] != "security_audit:" {
		t.Fatalf("delivered = %v", refs)
	}
}

func TestAuditEventsBacklogCountsFromTheSlowestSink(t *testing.T) {
	pool := newExportPool(t)
	metrics := NewMetrics()
	monitor := Monitor{DB: pool, Metrics: metrics}
	backlog := func() string {
		if err := monitor.Observe(context.Background()); err != nil {
			t.Fatal(err)
		}
		var text strings.Builder
		_ = metrics.WriteText(&text)
		for _, line := range strings.Split(text.String(), "\n") {
			if strings.HasPrefix(line, `elitea_audit_export_backlog{stream="admin_audit"}`) {
				return strings.TrimSpace(strings.TrimPrefix(line, `elitea_audit_export_backlog{stream="admin_audit"}`))
			}
		}
		return ""
	}
	insertAudit(t, pool, 3, time.Hour)
	if got := backlog(); got != "0" {
		t.Fatalf("backlog before the relay made its positions = %s, want 0", got)
	}
	fast, slow := &flakySink{name: "otlp"}, &flakySink{name: "webhook", down: true}
	relay := newRelay(t, pool, fast, func(c *AuditEventsConfig) { c.Sinks = []Sink{fast, slow}; c.FromBeginning = true })
	_, _ = relay.Drain(context.Background())
	if got := backlog(); got != "3" {
		t.Fatalf("backlog = %s, want the slow sink's 3", got)
	}
}

func TestDeadLetterRecorderIsIdempotent(t *testing.T) {
	pool := newExportPool(t)
	events := testEvents(2, 1)
	recorder := DeadLetterRecorder{DB: pool}
	for i := 1; i <= 2; i++ {
		if err := recorder.RecordDeadLetters(context.Background(), "webhook", events, "receiver answered 400", 4+i); err != nil {
			t.Fatal(err)
		}
	}
	var n, attempts int64
	if err := pool.QueryRow(context.Background(), `SELECT count(*), max(attempts) FROM elitea_runtime.audit_export_dead_letters`).Scan(&n, &attempts); err != nil {
		t.Fatal(err)
	}
	if n != 2 || attempts != 6 {
		t.Fatalf("rows=%d attempts=%d; one row per event and sink, latest attempt count", n, attempts)
	}
}

const insertRunStepSQL = `
INSERT INTO elitea_runtime.run_steps (run_id, seq, step_id, generation, project_id, kind, actor, started_at, ended_at,
                                      inputs_digest, inputs_size, inputs_original_size, inputs_media_type, inputs_object_key,
                                      prev_hash, hash)
VALUES ('run-x', $1, gen_random_uuid(), 1, 3, 'tool_call', 'user:7', clock_timestamp(), clock_timestamp(),
        decode(repeat('ab', 32), 'hex'), 4, 4, 'application/json', '3/runs/run-x/s/inputs',
        decode(repeat('00', 32), 'hex'), decode(repeat('11', 32), 'hex'))
RETURNING id`

// The run_steps stream has its own position, exports digests and refs
// (never content), and reports its backlog under its own stream label.
func TestRunStepsRelayExportsStepRecordsOnItsOwnStream(t *testing.T) {
	pool := newExportPool(t)
	ctx := context.Background()
	var ids []int64
	for seq := 1; seq <= 4; seq++ {
		var id int64
		if err := pool.QueryRow(ctx, insertRunStepSQL, seq).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	insertAudit(t, pool, 2, 0)

	metrics := NewMetrics()
	monitor := Monitor{DB: pool, Metrics: metrics}
	sink := &flakySink{}
	cfg := AuditEventsConfig{Sinks: []Sink{sink}, BatchSize: 3, FromBeginning: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	relay, err := NewRunStepsRelay(pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.Close)
	backlog := func() string {
		if err := monitor.Observe(ctx); err != nil {
			t.Fatal(err)
		}
		var text strings.Builder
		_ = metrics.WriteText(&text)
		for _, line := range strings.Split(text.String(), "\n") {
			if strings.HasPrefix(line, `elitea_audit_export_backlog{stream="run_steps"}`) {
				return strings.TrimSpace(strings.TrimPrefix(line, `elitea_audit_export_backlog{stream="run_steps"}`))
			}
		}
		return ""
	}
	sink.setDown(true)
	_, _ = relay.Drain(ctx)
	if got := backlog(); got != "4" {
		t.Fatalf("run_steps backlog = %q, want 4", got)
	}
	sink.setDown(false)
	if n := drainUntil(t, relay, 4); n != 4 {
		t.Fatalf("drain = %d, want 4", n)
	}
	refs := sink.refs()
	if len(refs) != 4 || refs[0] != fmt.Sprintf("run_step:%d", ids[0]) || refs[3] != fmt.Sprintf("run_step:%d", ids[3]) {
		t.Fatalf("delivered = %v (the admin_audit rows belong to another stream)", refs)
	}
	var stream string
	if err := pool.QueryRow(ctx, `SELECT stream FROM elitea_runtime.audit_export_cursors WHERE last_id = $1`, ids[3]).Scan(&stream); err != nil || stream != StreamRunSteps {
		t.Fatalf("cursor stream = %q %v", stream, err)
	}
	if got := backlog(); got != "0" {
		t.Fatalf("run_steps backlog after the drain = %q", got)
	}
}
