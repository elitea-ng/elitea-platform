package securityaudit

// The outbox against a real PostgreSQL built from the ledgered migration
// corpus (dbtest template, copied per test). Runs when
// ELITEA_TEST_DATABASE_URL is set; skips otherwise.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/dbtest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

const (
	outboxDatabaseURLEnv   = "ELITEA_TEST_DATABASE_URL"
	outboxBootstrapSchema  = "../infra/db/migrations/001_initial.sql"
	outboxIntegrationLimit = 5 * time.Minute
)

var (
	outboxTemplate string
	outboxDBSeq    atomic.Uint64
)

func TestMain(m *testing.M) {
	databaseURL := testpg.DSN(os.Getenv(outboxDatabaseURLEnv))
	if databaseURL == "" {
		os.Exit(m.Run())
	}
	bootstrap, err := os.ReadFile(outboxBootstrapSchema)
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
		fmt.Fprintf(os.Stderr, "build securityaudit template: %v\n", err)
		os.Exit(1)
	}
	outboxTemplate = templateName
	os.Exit(m.Run())
}

func newOutboxPool(t *testing.T, maxConns int32) *pgxpool.Pool {
	t.Helper()
	databaseURL := testpg.DSN(os.Getenv(outboxDatabaseURLEnv))
	if databaseURL == "" {
		t.Skipf("set %s to run the security audit outbox integration tests", outboxDatabaseURLEnv)
	}
	if outboxTemplate == "" {
		t.Fatal("TestMain did not build the securityaudit template")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()
	databaseName := fmt.Sprintf("elitea_secaudit_%d_%d_%d", os.Getpid(), time.Now().UnixNano(), outboxDBSeq.Add(1))
	if err := dbtest.CreateFromTemplate(ctx, adminPool, outboxTemplate, databaseName); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", outboxDatabaseURLEnv, err)
	}
	config.ConnConfig.Database = databaseName
	config.MaxConns = maxConns
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

func integrationAppender(t *testing.T) *PostgresAppender {
	t.Helper()
	appender, err := NewPostgresAppender("elitea-main/integration")
	if err != nil {
		t.Fatal(err)
	}
	return appender
}

func appendOne(ctx context.Context, pool *pgxpool.Pool, appender *PostgresAppender, tenant int64, action string) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		event := sampleEvent()
		event.ID, event.Time = "", time.Time{}
		event.TenantProjectID, event.Action = tenant, action
		return appender.Append(ctx, tx, event)
	})
}

func countRows(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestIntegrationRolledBackAppendLeavesNoRow(t *testing.T) {
	pool := newOutboxPool(t, 4)
	ctx := context.Background()
	appender := integrationAppender(t)

	rollback := errors.New("the governed change failed")
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := appender.Append(ctx, tx, sampleEvent()); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("err = %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM elitea_runtime.security_audit_outbox`); n != 0 {
		t.Fatalf("%d outbox rows after rollback", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM elitea_runtime.security_audit_heads`); n != 0 {
		t.Fatalf("%d head rows after rollback", n)
	}
	// The next append starts the chain at 1, not 2.
	if err := appendOne(ctx, pool, appender, 42, "after.rollback"); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, pool, `SELECT max(partition_seq) FROM elitea_runtime.security_audit_outbox`); n != 1 {
		t.Fatalf("partition_seq = %d after a rollback, want 1", n)
	}
}

func TestIntegrationTenThousandConcurrentEventsLoseNothing(t *testing.T) {
	pool := newOutboxPool(t, 24)
	ctx, cancel := context.WithTimeout(context.Background(), outboxIntegrationLimit)
	defer cancel()
	appender := integrationAppender(t)

	const (
		writers   = 20
		perWriter = 500
		total     = writers * perWriter
	)
	tenants := []int64{0, 1, 2, 3}
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	started := time.Now()
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if err := appendOne(ctx, pool, appender, tenants[(w+i)%len(tenants)], "load.test"); err != nil {
					errs <- fmt.Errorf("writer %d event %d: %w", w, i, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	t.Logf("appended %d events with %d writers in %s", total, writers, time.Since(started))

	relay, err := NewRelay(pool, RelayConfig{BacklogAlertThreshold: total * 2, BatchSize: 1000, Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := relay.Drain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Stored != total || stats.Backlog != 0 {
		t.Fatalf("drain %+v, want %d stored and no backlog", stats, total)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM elitea_identity.security_audit_events`); n != total {
		t.Fatalf("durable store holds %d events, want %d", n, total)
	}
	var checked int64
	for _, tenant := range tenants {
		report, err := Verify(ctx, pool, SourceEvents, tenant, 1)
		if err != nil {
			t.Fatal(err)
		}
		if report.Break != nil {
			t.Fatalf("tenant %d: %v", tenant, report.Break)
		}
		checked += report.Checked
	}
	if checked != total {
		t.Fatalf("verified %d events, want %d", checked, total)
	}
}

func TestIntegrationDurableStoreIsAppendOnly(t *testing.T) {
	pool := newOutboxPool(t, 4)
	ctx := context.Background()
	if err := appendOne(ctx, pool, integrationAppender(t), 7, "append.only"); err != nil {
		t.Fatal(err)
	}
	relay, err := NewRelay(pool, RelayConfig{BacklogAlertThreshold: 10, Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := relay.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE elitea_identity.security_audit_events SET event_type = 'ai.elitea.security.v1.probe'`,
		`UPDATE elitea_identity.security_audit_events SET event = jsonb_set(event, '{data,effect}', '"allow"')`,
		`DELETE FROM elitea_identity.security_audit_events`,
		`TRUNCATE elitea_identity.security_audit_events`,
	} {
		if _, err := pool.Exec(ctx, statement); err == nil {
			t.Errorf("%s succeeded on the append-only store", statement)
		}
	}
	if n := countRows(t, pool, `SELECT count(*) FROM elitea_identity.security_audit_events`); n != 1 {
		t.Fatalf("%d rows left", n)
	}
	report, err := Verify(ctx, pool, SourceEvents, 7, 1)
	if err != nil || report.Break != nil || report.Checked != 1 {
		t.Fatalf("verify after refused edits: %+v, %v", report, err)
	}
}

// flakyExporter fails while down is set and records what it accepted.
type flakyExporter struct {
	mu       sync.Mutex
	down     bool
	attempts int
	accepted []Record
}

func (f *flakyExporter) Name() string { return "flaky" }

func (f *flakyExporter) Export(_ context.Context, records []Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.down {
		return errors.New("collector unreachable")
	}
	f.accepted = append(f.accepted, records...)
	return nil
}

func TestIntegrationExporterOutageLosesNothing(t *testing.T) {
	pool := newOutboxPool(t, 4)
	ctx := context.Background()
	appender := integrationAppender(t)
	exporter := &flakyExporter{down: true}
	relay, err := NewRelay(pool, RelayConfig{BacklogAlertThreshold: 20, BatchSize: 16, Exporter: exporter, Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}

	const events = 50
	for i := 0; i < events; i++ {
		if err := appendOne(ctx, pool, appender, int64(i%3), "outage"); err != nil {
			t.Fatal(err)
		}
	}
	// Several passes while the exporter is down.
	for pass := 0; pass < 3; pass++ {
		stats, err := relay.Drain(ctx)
		if err == nil {
			t.Fatal("a pass with the exporter down reported success")
		}
		if stats.Exported != 0 || stats.Backlog != events {
			t.Fatalf("pass %d: %+v", pass, stats)
		}
	}
	if !relay.Alerting() {
		t.Error("a backlog of 50 over a threshold of 20 does not alert")
	}
	// The durable store does not wait for the exporter.
	if n := countRows(t, pool, `SELECT count(*) FROM elitea_identity.security_audit_events`); n != events {
		t.Fatalf("durable store holds %d events while the exporter is down, want %d", n, events)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM elitea_runtime.security_audit_outbox WHERE exported_at IS NULL`); n != events {
		t.Fatalf("%d rows unexported, want %d", n, events)
	}

	exporter.mu.Lock()
	exporter.down = false
	exporter.mu.Unlock()
	stats, err := relay.Drain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Exported != events || stats.Backlog != 0 || relay.Alerting() {
		t.Fatalf("after recovery: %+v alerting=%t", stats, relay.Alerting())
	}
	seen := map[string]bool{}
	var last int64
	for _, record := range exporter.accepted {
		if seen[record.EventID] {
			t.Fatalf("event %s exported twice", record.EventID)
		}
		seen[record.EventID] = true
		if record.Seq <= last {
			t.Fatalf("export out of seq order: %d after %d", record.Seq, last)
		}
		last = record.Seq
	}
	if len(seen) != events {
		t.Fatalf("exported %d distinct events, want %d", len(seen), events)
	}
}

func TestIntegrationVerifyReportsTheEditedOutboxSeq(t *testing.T) {
	pool := newOutboxPool(t, 4)
	ctx := context.Background()
	appender := integrationAppender(t)
	for i := 0; i < 12; i++ {
		if err := appendOne(ctx, pool, appender, 5, "tamper"); err != nil {
			t.Fatal(err)
		}
	}
	report, err := Verify(ctx, pool, SourceOutbox, 5, 1)
	if err != nil || report.Break != nil || report.Checked != 12 {
		t.Fatalf("intact chain: %+v, %v", report, err)
	}
	// The durable store is empty (no relay ran): every row is still in the
	// outbox, so the tail check finds nothing missing.
	if report, err := Verify(ctx, pool, SourceEvents, 5, 1); err != nil || report.Break != nil {
		t.Fatalf("events before the relay: %+v, %v", report, err)
	}

	var seq int64
	if err := pool.QueryRow(ctx, `UPDATE elitea_runtime.security_audit_outbox
		   SET event = jsonb_set(event, '{data,effect}', '"allow"')
		 WHERE partition_key = '5' AND partition_seq = 8 RETURNING seq`).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	report, err = Verify(ctx, pool, SourceOutbox, 5, 1)
	if err != nil {
		t.Fatal(err)
	}
	if report.Break == nil || report.Break.PartitionSeq != 8 || report.Break.Seq != seq {
		t.Fatalf("break = %+v, want partition_seq 8 (seq %d)", report.Break, seq)
	}
	// From 9 the chain is anchored on row 8's stored hash and holds.
	if report, err := Verify(ctx, pool, SourceOutbox, 5, 9); err != nil || report.Break != nil || report.Checked != 4 {
		t.Fatalf("from 9: %+v, %v", report, err)
	}
}

func TestIntegrationPrerequisiteChecksTheInstalledOutbox(t *testing.T) {
	pool := newOutboxPool(t, 4)
	ctx := context.Background()
	prerequisite, err := NewOutboxPrerequisite(pool)
	if err != nil {
		t.Fatal(err)
	}
	revision := profile.PolicyRevision{ID: "2026-10-r1", Document: profile.PolicyDocument{AuditBacklogAlertThreshold: 100}}
	if err := prerequisite.Check(ctx, revision); err != nil {
		t.Fatalf("migrated database refused: %v", err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE elitea_identity.security_audit_events DISABLE TRIGGER security_audit_events_append_only`); err != nil {
		t.Fatal(err)
	}
	if err := prerequisite.Check(ctx, revision); !errors.Is(err, ErrOutboxMissing) {
		t.Fatalf("disabled trigger: err = %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM elitea_runtime.schema_migrations WHERE target_kind = 'shared' AND version = $1`, OutboxMigrationVersion); err != nil {
		t.Fatal(err)
	}
	if err := prerequisite.Check(ctx, revision); !errors.Is(err, ErrOutboxMissing) {
		t.Fatalf("unapplied migration: err = %v", err)
	}
}
