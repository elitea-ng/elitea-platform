package auditretention

// The sweep against a real PostgreSQL.
//
// `timestamp < $1` is the DATABASE's comparison, not this package's, and a fake
// store repeating it would only prove that the unit test agrees with itself.
// The boundary case in particular — a row stamped exactly at the cutoff — is
// decided by the `<`, and it is the row an operator is most likely to be
// looking for on the day the sweep first runs.
//
// Runs when ELITEA_TEST_DATABASE_URL is set; skips otherwise. Each test builds
// its own throwaway database, so a failing assertion never leaves rows behind
// and two tests cannot see each other's.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// bootstrapDDLPath points across module boundaries on purpose. centry.audit_events
// is created by elitea-main's bootstrap migration and swept by this service, so
// pointing at the real file means a column rename there fails this test instead
// of silently diverging. Retyping the DDL here is how a test comes to pass
// against a shape no deployment has.
const bootstrapDDLPath = "../../../elitea-main/internal/infra/db/migrations/001_initial.sql"

var (
	auditTableDDLRe = regexp.MustCompile(`(?is)CREATE TABLE IF NOT EXISTS centry\.audit_events \(.*?\n\);`)
	auditIndexDDLRe = regexp.MustCompile(`(?im)^CREATE INDEX IF NOT EXISTS ix_audit_events_timestamp .*;$`)
)

func auditEventsDDL(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(bootstrapDDLPath))
	if err != nil {
		t.Fatalf("read %s: %v", bootstrapDDLPath, err)
	}
	table := auditTableDDLRe.FindString(string(raw))
	// "not found" must FAIL, never quietly skip the table: a bootstrap file
	// that stopped creating centry.audit_events is the thing worth screaming
	// about, and a test that shrugged at it would go green on an empty schema.
	if table == "" {
		t.Fatalf("no CREATE TABLE ... centry.audit_events statement found in %s", bootstrapDDLPath)
	}
	index := auditIndexDDLRe.FindString(string(raw))
	if index == "" {
		t.Fatalf("no ix_audit_events_timestamp index found in %s; the sweep's batch statement "+
			"orders and filters on that column and would fall back to a sequential scan", bootstrapDDLPath)
	}
	return []string{table, index}
}

// newAuditPool builds a throwaway database holding the REAL centry.audit_events
// definition. Only those statements are applied; the rest of 001_initial.sql is
// elitea-main's concern and pulls in a schema graph this module has no business
// creating.
func newAuditPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("ELITEA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set ELITEA_TEST_DATABASE_URL to run the audit retention integration tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()

	databaseName := fmt.Sprintf("elitea_audit_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{databaseName}.Sanitize()); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse ELITEA_TEST_DATABASE_URL: %v", err)
	}
	cfg.ConnConfig.Database = databaseName
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open isolated pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dropCancel()
		dropPool, dropErr := pgxpool.New(dropCtx, databaseURL)
		if dropErr != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(dropCtx,
			"DROP DATABASE IF EXISTS "+pgx.Identifier{databaseName}.Sanitize()+" WITH (FORCE)")
	})

	if _, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS centry"); err != nil {
		t.Fatalf("create centry schema: %v", err)
	}
	for _, stmt := range auditEventsDDL(t) {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("apply centry.audit_events DDL: %v\n%s", err, stmt)
		}
	}
	return pool
}

// insertAuditRow writes one row and returns its id, so an assertion can name the
// exact row that survived or went.
func insertAuditRow(t *testing.T, pool *pgxpool.Pool, at time.Time, action, traceID string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO centry.audit_events (timestamp, event_type, action, trace_id, is_error)
		 VALUES ($1, 'api', $2, $3, false) RETURNING id`, at, action, traceID).Scan(&id)
	if err != nil {
		t.Fatalf("insert audit row %q: %v", action, err)
	}
	return id
}

// survivingActions reads the table back the way an assertion should: by row, not
// by count.
func survivingActions(t *testing.T, pool *pgxpool.Pool) map[string]bool {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT action FROM centry.audit_events`)
	if err != nil {
		t.Fatalf("read audit rows back: %v", err)
	}
	defer rows.Close()
	surviving := map[string]bool{}
	for rows.Next() {
		var action string
		if err := rows.Scan(&action); err != nil {
			t.Fatalf("scan action: %v", err)
		}
		surviving[action] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate audit rows: %v", err)
	}
	return surviving
}

// TestRowsOlderThanTheWindowGoAndNewerRowsStay — the whole point of the package,
// with the boundary stated exactly.
//
// Three rows around the edge: one a second older than the cutoff, one AT the
// cutoff, one a second newer. `timestamp < cutoff` keeps the row that sits on
// the boundary, so an operator asking for "the last 90 days" gets the row
// stamped exactly 90 days ago.
func TestRowsOlderThanTheWindowGoAndNewerRowsStay(t *testing.T) {
	t.Parallel()

	pool := newAuditPool(t)
	now := time.Now().UTC()
	window := 90 * 24 * time.Hour
	cutoff := now.Add(-window)

	insertAuditRow(t, pool, cutoff.Add(-time.Second), "one second past the window", "boundary")
	insertAuditRow(t, pool, cutoff, "exactly on the boundary", "boundary")
	insertAuditRow(t, pool, cutoff.Add(time.Second), "one second inside the window", "boundary")
	insertAuditRow(t, pool, now.Add(-time.Hour), "an hour old", "recent")
	insertAuditRow(t, pool, now.Add(-365*24*time.Hour), "a year old", "ancient")

	sweeper := newTestSweeper(t, pool, openGate, Config{
		RetentionDays: 90, BatchSize: 100, MaxBatchesPerPass: 10,
	}, now)

	stats, err := sweeper.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if stats.Deleted != 2 {
		t.Errorf("deleted = %d, want 2 (the year-old row and the one a second past the window)", stats.Deleted)
	}
	if !stats.Drained {
		t.Error("the pass did not report that it drained the eligible rows")
	}

	surviving := survivingActions(t, pool)
	for _, action := range []string{"exactly on the boundary", "one second inside the window", "an hour old"} {
		if !surviving[action] {
			t.Errorf("the sweep removed %q, which is inside the window", action)
		}
	}
	for _, action := range []string{"one second past the window", "a year old"} {
		if surviving[action] {
			t.Errorf("the sweep left %q, which is outside the window", action)
		}
	}
	if len(surviving) != 3 {
		t.Errorf("%d rows survived, want 3: %v", len(surviving), surviving)
	}
}

// TestTheE2ESeedersFixtureRowsSurvive.
//
// Journey 29 reads four rows the E2E seeder writes into this table
// (apps/elitea-web/scripts/e2e-stack.sh, "audit trail fixture"). They are
// stamped inside the current day so the browser's default "Today" window
// contains them, so any sane retention window contains them too — but "so it
// should" is the reasoning that ships defects, and a sweeper that removed them
// would break journey 29 in a way that reads as a flake.
//
// The window here is the FLOOR, the shortest one this package will ever act on.
// If the fixture rows survive that, they survive every configuration an
// operator can set.
func TestTheE2ESeedersFixtureRowsSurvive(t *testing.T) {
	t.Parallel()

	pool := newAuditPool(t)
	now := time.Now().UTC()

	// The seeder's own shape: a base no earlier than the start of the current
	// day, and four rows running forward from it.
	base := now.Add(-20 * time.Minute)
	if startOfDay := now.Truncate(24 * time.Hour); base.Before(startOfDay) {
		base = startOfDay
	}
	fixtures := map[string]time.Duration{
		"POST /chat/e2e":  0,
		"completion/e2e":  time.Minute,
		"search/e2e":      2 * time.Minute,
		"GET /agents/e2e": 10 * time.Minute,
	}
	for action, offset := range fixtures {
		traceID := "e2e-trace-alpha"
		if action == "GET /agents/e2e" {
			traceID = "e2e-trace-beta"
		}
		insertAuditRow(t, pool, base.Add(offset), action, traceID)
	}
	insertAuditRow(t, pool, now.Add(-8*24*time.Hour), "eight days old", "old")

	sweeper := newTestSweeper(t, pool, openGate, Config{
		RetentionDays: MinimumRetentionDays, BatchSize: 100, MaxBatchesPerPass: 10,
	}, now)

	stats, err := sweeper.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if stats.Deleted != 1 {
		t.Errorf("deleted = %d, want 1 (the eight-day-old row only)", stats.Deleted)
	}

	surviving := survivingActions(t, pool)
	for action := range fixtures {
		if !surviving[action] {
			t.Errorf("the sweep removed journey 29's fixture row %q at the shortest window this package allows", action)
		}
	}
	if surviving["eight days old"] {
		t.Error("the sweep left a row eight days past a seven-day window")
	}
}

// TestAPassBatchesRatherThanDeletingInOneStatement.
//
// The batch ceiling has to be visible in what the DATABASE did, not only in the
// returned counts: a batch size that reached the LIMIT clause as a no-op would
// leave every assertion in the unit test green while one statement took a lock
// on the whole table.
func TestAPassBatchesRatherThanDeletingInOneStatement(t *testing.T) {
	t.Parallel()

	pool := newAuditPool(t)
	now := time.Now().UTC()
	for i := range 10 {
		insertAuditRow(t, pool, now.Add(-time.Duration(400+i)*24*time.Hour),
			fmt.Sprintf("expired row %d", i), "bulk")
	}
	insertAuditRow(t, pool, now.Add(-time.Hour), "kept", "recent")

	sweeper := newTestSweeper(t, pool, openGate, Config{
		RetentionDays: 90, BatchSize: 3, MaxBatchesPerPass: 2,
	}, now)

	// First pass: bounded at 2 × 3 = 6 rows, and it must NOT claim to have
	// drained the table.
	first, err := sweeper.Sweep(context.Background())
	if err != nil {
		t.Fatalf("first Sweep: %v", err)
	}
	if first.Deleted != 6 || first.Batches != 2 || first.Drained {
		t.Errorf("first pass = %+v, want 6 rows in 2 batches, not drained", first)
	}

	// Second pass: the remaining four, then a short batch ends it.
	second, err := sweeper.Sweep(context.Background())
	if err != nil {
		t.Fatalf("second Sweep: %v", err)
	}
	if second.Deleted != 4 || !second.Drained {
		t.Errorf("second pass = %+v, want the remaining 4 rows and a drained pass", second)
	}

	surviving := survivingActions(t, pool)
	if len(surviving) != 1 || !surviving["kept"] {
		t.Errorf("after two passes the table holds %v, want only the recent row", surviving)
	}
}

// TestAMaintenanceWindowLeavesTheTableAlone, against the real table.
//
// The unit test proves no statement is issued. This proves the rows are still
// there afterwards, and that the pass after the window removes exactly what the
// suppressed pass would have — the recoverable half of the gate.
func TestAMaintenanceWindowLeavesTheTableAlone(t *testing.T) {
	t.Parallel()

	pool := newAuditPool(t)
	now := time.Now().UTC()
	insertAuditRow(t, pool, now.Add(-400*24*time.Hour), "expired", "old")
	insertAuditRow(t, pool, now.Add(-time.Hour), "kept", "recent")

	var closed bool
	sweeper := newTestSweeper(t, pool, func(context.Context) bool { return closed }, Config{
		RetentionDays: 90, BatchSize: 100, MaxBatchesPerPass: 10,
	}, now)

	closed = true
	suppressed, err := sweeper.Sweep(context.Background())
	if err != nil {
		t.Fatalf("suppressed Sweep: %v", err)
	}
	if !suppressed.Suppressed || suppressed.Deleted != 0 {
		t.Errorf("suppressed pass = %+v, want a suppressed pass that deleted nothing", suppressed)
	}
	if surviving := survivingActions(t, pool); !surviving["expired"] {
		t.Error("a pass inside a maintenance window removed an expired row")
	}

	closed = false
	after, err := sweeper.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep after the window: %v", err)
	}
	if after.Deleted != 1 {
		t.Errorf("the pass after the window deleted %d row(s), want the 1 the suppressed pass left", after.Deleted)
	}
	surviving := survivingActions(t, pool)
	if len(surviving) != 1 || !surviving["kept"] {
		t.Errorf("after the window the table holds %v, want only the recent row", surviving)
	}
}
