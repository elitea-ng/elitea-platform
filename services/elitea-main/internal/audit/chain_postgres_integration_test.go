package audit

// The centry.audit_events hash chain (shared migration 0206) against a real
// PostgreSQL built from the ledgered migration corpus (dbtest template,
// copied per test). Runs when ELITEA_TEST_DATABASE_URL is set; skips
// otherwise.

import (
	"context"
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
	chainDatabaseURLEnv  = "ELITEA_TEST_DATABASE_URL"
	chainBootstrapSchema = "../infra/db/migrations/001_initial.sql"
)

var (
	chainTemplate string
	chainDBSeq    atomic.Uint64
)

func TestMain(m *testing.M) {
	databaseURL := testpg.DSN(os.Getenv(chainDatabaseURLEnv))
	if databaseURL == "" {
		os.Exit(m.Run())
	}
	bootstrap, err := os.ReadFile(chainBootstrapSchema)
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
		fmt.Fprintf(os.Stderr, "build audit template: %v\n", err)
		os.Exit(1)
	}
	chainTemplate = templateName
	os.Exit(m.Run())
}

func newChainPool(t *testing.T, maxConns int32) *pgxpool.Pool {
	t.Helper()
	databaseURL := testpg.DSN(os.Getenv(chainDatabaseURLEnv))
	if databaseURL == "" {
		t.Skipf("set %s to run the audit chain integration tests", chainDatabaseURLEnv)
	}
	if chainTemplate == "" {
		t.Fatal("TestMain did not build the audit template")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()
	databaseName := fmt.Sprintf("elitea_auditchain_%d_%d_%d", os.Getpid(), time.Now().UnixNano(), chainDBSeq.Add(1))
	if err := dbtest.CreateFromTemplate(ctx, adminPool, chainTemplate, databaseName); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", chainDatabaseURLEnv, err)
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

// verify runs VerifyChain the way the admin route does: one REPEATABLE READ,
// read-only transaction.
func verify(t *testing.T, pool *pgxpool.Pool, from, limit int64) ChainReport {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	report, err := VerifyChain(ctx, tx, from, limit)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	return report
}

func insertRow(t *testing.T, pool *pgxpool.Pool, action string, at time.Time) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `
INSERT INTO centry.audit_events (timestamp, event_type, action, duration_ms, is_error, user_email)
VALUES ($1, 'api', $2, 12.345, false, 'ä@example.com') RETURNING id`, at, action).Scan(&id); err != nil {
		t.Fatalf("insert %q: %v", action, err)
	}
	return id
}

func TestConcurrentWritersProduceAGapFreeChain(t *testing.T) {
	pool := newChainPool(t, 16)
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Four batching recorders (the request path, one per replica) and four
	// multi-row INSERT ... SELECT writers (the session relay's shape) at once.
	const (
		recorders       = 4
		eventsPerWriter = 50
		selectWriters   = 4
		rowsPerSelect   = 5
		selectsEach     = 10
	)
	var wg sync.WaitGroup
	for r := range recorders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recorder := NewPostgresRecorder(pool, quiet)
			for i := range eventsPerWriter {
				status, duration := int32(200), float64(i)/3
				recorder.Record(ctx, Event{EventType: "api", Action: fmt.Sprintf("POST /r%d/%d", r, i), StatusCode: &status, DurationMS: &duration})
			}
			if err := recorder.Close(ctx); err != nil {
				t.Errorf("recorder close: %v", err)
			}
			if recorder.Dropped() != 0 {
				t.Errorf("recorder dropped %d events", recorder.Dropped())
			}
		}()
	}
	for w := range selectWriters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range selectsEach {
				if _, err := pool.Exec(ctx, `
INSERT INTO centry.audit_events (timestamp, event_type, action, is_error, entity_type, entity_name)
SELECT clock_timestamp() - make_interval(secs => n), 'session', 'session.start', false, 'browser_session', $1
  FROM generate_series(1, $2::int) AS n ORDER BY n`, fmt.Sprintf("writer=%d batch=%d", w, i), rowsPerSelect); err != nil {
					t.Errorf("insert select: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	want := int64(recorders*eventsPerWriter + selectWriters*selectsEach*rowsPerSelect)
	var rows, distinct, lowest, highest int64
	if err := pool.QueryRow(ctx, `
SELECT count(*), count(DISTINCT partition_seq), min(partition_seq), max(partition_seq)
  FROM centry.audit_events WHERE partition_key = '0'`).Scan(&rows, &distinct, &lowest, &highest); err != nil {
		t.Fatal(err)
	}
	if rows != want || distinct != want || lowest != 1 || highest != want {
		t.Fatalf("chain positions: %d rows, %d distinct, %d..%d; want %d gap-free from 1", rows, distinct, lowest, highest, want)
	}
	report := verify(t, pool, 0, 0)
	if report.Break != nil || report.Checked != want || report.HeadSeq != want || report.Truncated {
		t.Fatalf("report = %+v (break %+v); want %d checked, no break", report, report.Break, want)
	}
}

func TestTheHashIsChainHashOverTheCanonicalRowInAnySession(t *testing.T) {
	pool := newChainPool(t, 4)
	ctx := context.Background()
	at := time.Date(2026, 10, 29, 9, 30, 15, 123456000, time.UTC)
	insertRow(t, pool, "GET /one", at)
	insertRow(t, pool, "GET /two", at.Add(time.Second))

	// The trigger hashed under the pool's session; read the canonical form
	// back under other float and time zone settings: it must not move.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET extra_float_digits = 3; SET TimeZone = 'Asia/Kolkata'`); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.Query(ctx, `
SELECT centry.audit_event_canonical(e), prev_hash, hash FROM centry.audit_events e ORDER BY partition_seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	prev := securityaudit.ZeroHash
	n := 0
	for rows.Next() {
		var (
			canonical            string
			prevHash, storedHash []byte
		)
		if err := rows.Scan(&canonical, &prevHash, &storedHash); err != nil {
			t.Fatal(err)
		}
		if string(prevHash) != string(prev[:]) {
			t.Fatalf("row %d prev_hash is not the previous row's hash", n+1)
		}
		computed := securityaudit.ChainHash(prev, []byte(canonical))
		if string(computed[:]) != string(storedHash) {
			t.Fatalf("row %d: the stored hash is not ChainHash(prev, canonical); canonical %s", n+1, canonical)
		}
		if !strings.Contains(canonical, `"2026-10-29T09:30:15.123456Z"`) && n == 0 {
			t.Fatalf("canonical timestamp is not UTC with microseconds: %s", canonical)
		}
		prev = computed
		n++
	}
	if err := rows.Err(); err != nil || n != 2 {
		t.Fatalf("read %d rows: %v", n, err)
	}
}

func TestAMutatedRowFailsVerificationAtThatRow(t *testing.T) {
	pool := newChainPool(t, 4)
	ctx := context.Background()
	now := time.Now().UTC()
	ids := make([]int64, 5)
	for i := range ids {
		ids[i] = insertRow(t, pool, fmt.Sprintf("POST /x/%d", i+1), now.Add(time.Duration(i)*time.Second))
	}
	if report := verify(t, pool, 0, 0); report.Break != nil || report.Checked != 5 {
		t.Fatalf("intact chain: %+v", report)
	}

	// A chained row is append-only.
	if _, err := pool.Exec(ctx, `UPDATE centry.audit_events SET action = 'POST /forged' WHERE id = $1`, ids[2]); err == nil ||
		!strings.Contains(err.Error(), "append-only") {
		t.Fatalf("UPDATE of a chained row: err = %v, want the append-only refusal", err)
	}
	// Someone with the table's owner rights gets past the guard.
	if _, err := pool.Exec(ctx, `
ALTER TABLE centry.audit_events DISABLE TRIGGER audit_events_append_only;
UPDATE centry.audit_events SET action = 'POST /forged' WHERE id = `+fmt.Sprint(ids[2])+`;
ALTER TABLE centry.audit_events ENABLE TRIGGER audit_events_append_only`); err != nil {
		t.Fatal(err)
	}
	report := verify(t, pool, 0, 0)
	if report.Break == nil || report.Break.PartitionSeq != 3 || report.Break.Seq != ids[2] ||
		!strings.Contains(report.Break.Reason, "hash does not match") || report.Checked != 2 {
		t.Fatalf("mutated row 3: report %+v, break %+v", report, report.Break)
	}

	// A deleted middle row is reported at the missing position, and a deleted
	// tail against the head.
	if _, err := pool.Exec(ctx, `DELETE FROM centry.audit_events WHERE id = ANY($1)`, []int64{ids[2]}); err != nil {
		t.Fatal(err)
	}
	if report := verify(t, pool, 0, 0); report.Break == nil || report.Break.PartitionSeq != 3 ||
		!strings.Contains(report.Break.Reason, "row missing") {
		t.Fatalf("deleted row 3: break %+v", report.Break)
	}
	if report := verify(t, pool, 4, 0); report.Break == nil || report.Break.PartitionSeq != 3 {
		t.Fatalf("from 4 with row 3 gone: break %+v", report.Break)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM centry.audit_events WHERE id = $1`, ids[4]); err != nil {
		t.Fatal(err)
	}
	if report := verify(t, pool, 5, 0); report.Break == nil || report.Break.PartitionSeq != 5 ||
		!strings.Contains(report.Break.Reason, "the chain head is at partition_seq 5") {
		t.Fatalf("deleted tail: break %+v", report.Break)
	}
}

// pruneThrough deletes the chain prefix through seq and moves the anchor,
// the contract the scheduler's retention sweep keeps in one statement
// (services/elitea-scheduler/internal/auditretention, which is asserted
// against PostgreSQL there with this migration applied).
func pruneThrough(t *testing.T, pool *pgxpool.Pool, seq int64, moveAnchor bool) {
	t.Helper()
	statement := `DELETE FROM centry.audit_events WHERE partition_key = '0' AND partition_seq <= $1`
	if moveAnchor {
		statement = `
WITH tip AS (
    SELECT partition_seq, hash FROM centry.audit_events WHERE partition_key = '0' AND partition_seq = $1
), anchor AS (
    UPDATE elitea_runtime.audit_events_chain_heads AS h
       SET pruned_seq = tip.partition_seq, pruned_hash = tip.hash, pruned_at = clock_timestamp()
      FROM tip WHERE h.partition_key = '0'
    RETURNING 1
)
DELETE FROM centry.audit_events WHERE partition_key = '0' AND partition_seq <= $1`
	}
	if _, err := pool.Exec(context.Background(), statement, seq); err != nil {
		t.Fatalf("prune through %d: %v", seq, err)
	}
}

func TestAPrunedChainStillVerifiesFromItsAnchor(t *testing.T) {
	pool := newChainPool(t, 4)
	ctx := context.Background()
	now := time.Now().UTC()

	// A row from before the chain: counted, never checked.
	if _, err := pool.Exec(ctx, `
ALTER TABLE centry.audit_events DISABLE TRIGGER zz_audit_events_chain;
INSERT INTO centry.audit_events (timestamp, event_type, action) VALUES (now() - interval '1 year', 'api', 'GET /before');
ALTER TABLE centry.audit_events ENABLE TRIGGER zz_audit_events_chain`); err != nil {
		t.Fatal(err)
	}
	for i := range 6 {
		insertRow(t, pool, fmt.Sprintf("POST /p/%d", i+1), now.Add(time.Duration(i)*time.Second))
	}
	pruneThrough(t, pool, 3, true)

	report := verify(t, pool, 0, 0)
	if report.Break != nil || report.From != 4 || report.Checked != 3 || report.PrunedThrough != 3 ||
		report.HeadSeq != 6 || report.Unchained != 1 {
		t.Fatalf("pruned chain: report %+v, break %+v", report, report.Break)
	}

	// Paged: two rows, then the rest from next_from.
	first := verify(t, pool, 0, 2)
	if first.Break != nil || !first.Truncated || first.NextFrom != 6 || first.Checked != 2 {
		t.Fatalf("first page: %+v", first)
	}
	if rest := verify(t, pool, first.NextFrom, 2); rest.Break != nil || rest.Truncated || rest.Checked != 1 {
		t.Fatalf("second page: %+v", rest)
	}

	// Without the anchor a prune reads as missing rows: what the anchor is for.
	pruneThrough(t, pool, 4, false)
	if report := verify(t, pool, 0, 0); report.Break == nil || report.Break.PartitionSeq != 4 {
		t.Fatalf("prune without the anchor: break %+v", report.Break)
	}
}
