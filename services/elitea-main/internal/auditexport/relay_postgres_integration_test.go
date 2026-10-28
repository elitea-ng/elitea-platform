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
	mu        sync.Mutex
	down      bool
	delivered []string
}

func (f *flakySink) Name() string { return "test" }
func (f *flakySink) Send(_ context.Context, events []Event) error {
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

func insertAudit(t *testing.T, pool *pgxpool.Pool, n int, age time.Duration) []int64 {
	t.Helper()
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		var id int64
		err := pool.QueryRow(context.Background(), `
INSERT INTO centry.audit_events (timestamp, user_id, event_type, action, http_method, http_route, status_code, duration_ms, is_error)
VALUES (clock_timestamp() - make_interval(secs => $1), 7, 'api', 'GET /x', 'GET', '/x', 200, 1.5, false)
RETURNING id`, age.Seconds()).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func cursor(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var last int64
	if err := pool.QueryRow(context.Background(),
		`SELECT last_id FROM elitea_runtime.audit_export_cursors WHERE stream = $1`, StreamAdminAudit).Scan(&last); err != nil {
		t.Fatal(err)
	}
	return last
}

func newRelay(t *testing.T, pool *pgxpool.Pool, sink Sink, tweak func(*AuditEventsConfig)) *AuditEventsRelay {
	t.Helper()
	cfg := AuditEventsConfig{Sink: sink, BatchSize: 3, SettleDelay: -1, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if tweak != nil {
		tweak(&cfg)
	}
	relay, err := NewAuditEventsRelay(pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return relay
}

func TestAuditEventsRelayStartsAtNewestRowAndDeliversNewOnes(t *testing.T) {
	pool := newExportPool(t)
	insertAudit(t, pool, 5, time.Hour) // history before export was enabled
	sink := &flakySink{}
	relay := newRelay(t, pool, sink, nil)

	if n, err := relay.Drain(context.Background()); err != nil || n != 0 {
		t.Fatalf("history must not be replayed by default: n=%d err=%v", n, err)
	}
	fresh := insertAudit(t, pool, 7, 0)
	n, err := relay.Drain(context.Background())
	if err != nil || n != 7 {
		t.Fatalf("drain = %d, %v; want 7 in batches of 3", n, err)
	}
	if got := sink.refs(); len(got) != 7 || got[0] != fmt.Sprintf("admin_audit:%d", fresh[0]) {
		t.Fatalf("delivered = %v", got)
	}
	if cursor(t, pool) != fresh[6] {
		t.Fatalf("cursor = %d, want %d", cursor(t, pool), fresh[6])
	}
	if n, _ := relay.Drain(context.Background()); n != 0 {
		t.Fatal("exported the same rows twice")
	}
}

func TestAuditEventsRelayFromBeginningReplaysHistory(t *testing.T) {
	pool := newExportPool(t)
	insertAudit(t, pool, 4, time.Hour)
	sink := &flakySink{}
	relay := newRelay(t, pool, sink, func(c *AuditEventsConfig) { c.FromBeginning = true })
	if n, err := relay.Drain(context.Background()); err != nil || n != 4 {
		t.Fatalf("drain = %d, %v", n, err)
	}
}

func TestAuditEventsRelayLosesNothingAcrossAReceiverOutage(t *testing.T) {
	pool := newExportPool(t)
	sink := &flakySink{}
	relay := newRelay(t, pool, sink, nil)
	_, _ = relay.Drain(context.Background()) // creates the cursor at the table's end
	ids := insertAudit(t, pool, 8, 0)

	sink.setDown(true)
	for i := 0; i < 3; i++ {
		if _, err := relay.Drain(context.Background()); err == nil {
			t.Fatal("drain reported success while the receiver was down")
		}
	}
	if cursor(t, pool) >= ids[0] {
		t.Fatal("the cursor advanced past undelivered rows")
	}
	more := insertAudit(t, pool, 2, 0)

	sink.setDown(false)
	if n, err := relay.Drain(context.Background()); err != nil || n != 10 {
		t.Fatalf("after restore: n=%d err=%v; want all 10", n, err)
	}
	seen := map[string]bool{}
	for _, ref := range sink.refs() {
		seen[ref] = true
	}
	for _, id := range append(ids, more...) {
		if !seen[fmt.Sprintf("admin_audit:%d", id)] {
			t.Fatalf("row %d was lost", id)
		}
	}
}

func TestAuditEventsRelayHoldsBackRowsThatHaveNotSettled(t *testing.T) {
	pool := newExportPool(t)
	sink := &flakySink{}
	relay := newRelay(t, pool, sink, func(c *AuditEventsConfig) { c.SettleDelay = time.Minute })
	_, _ = relay.Drain(context.Background())
	old := insertAudit(t, pool, 1, 2*time.Minute)
	_ = insertAudit(t, pool, 1, 0)         // too fresh: a lower id may still be uncommitted
	_ = insertAudit(t, pool, 1, time.Hour) // older timestamp behind a fresh row: must not jump the queue
	n, err := relay.Drain(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v; only the settled prefix is exported", n, err)
	}
	if got := sink.refs(); len(got) != 1 || got[0] != fmt.Sprintf("admin_audit:%d", old[0]) {
		t.Fatalf("delivered = %v", got)
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
	_, _ = a.Drain(context.Background())
	counts := map[string]int{}
	for _, ref := range sink.refs() {
		counts[ref]++
	}
	for _, id := range ids {
		if counts[fmt.Sprintf("admin_audit:%d", id)] != 1 {
			t.Fatalf("row %d delivered %d times", id, counts[fmt.Sprintf("admin_audit:%d", id)])
		}
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
		BacklogAlertThreshold: 100, Exporter: SecurityExporter{Sink: sink}, BatchSize: 2,
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
