package migrate

// A database the legacy runtime built, upgraded by the production path
// (Bootstrap, then the shared and tenant histories), against a real
// PostgreSQL. Runs when ELITEA_TEST_DATABASE_URL is set; skips otherwise.
//
// The legacy shape is the one measured on the real-model dump
// (product-real-models-main-d5e966074): centry.audit_events without the five
// token and cost columns, and tenant schemas whose elitea_tools has no
// owner_id, env_vars or uuid and which have no index_meta, social_likes,
// social_pins or social_feedbacks.
// The fixture builds it from the legacy-era schema by removing exactly those,
// which is what the dump's DDL is.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/dbtest/legacyshape"
	bootstrapschema "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

const legacyUpgradeDatabaseURLEnv = "ELITEA_TEST_DATABASE_URL"

var legacyUpgradeDBSeq atomic.Uint64

// newLegacyUpgradeDatabase creates an empty database and returns a pool on it.
func newLegacyUpgradeDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := testpg.DSN(os.Getenv(legacyUpgradeDatabaseURLEnv))
	if databaseURL == "" {
		t.Skipf("set %s to run the legacy-built upgrade tests", legacyUpgradeDatabaseURLEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()
	databaseName := fmt.Sprintf("elitea_legacyupgrade_%d_%d_%d", os.Getpid(), time.Now().UnixNano(), legacyUpgradeDBSeq.Add(1))
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{databaseName}.Sanitize()); err != nil {
		t.Fatalf("create database: %v", err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", legacyUpgradeDatabaseURLEnv, err)
	}
	config.ConnConfig.Database = databaseName
	config.MaxConns = 6
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 2*time.Minute)
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

// newLegacyBuiltDatabase is a database in the legacy-built shape, not yet
// upgraded.
func newLegacyBuiltDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := newLegacyUpgradeDatabase(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, bootstrapschema.Initial); err != nil {
		t.Fatalf("apply the legacy-era schema: %v", err)
	}
	if _, err := pool.Exec(ctx, legacyshape.BuiltSQL); err != nil {
		t.Fatalf("build the legacy-built shape: %v", err)
	}
	return pool
}

func columnType(t *testing.T, pool *pgxpool.Pool, schema, table, column string) string {
	t.Helper()
	var typ *string
	if err := pool.QueryRow(context.Background(), `
		SELECT format_type(a.atttypid, a.atttypmod)
		  FROM pg_attribute a
		 WHERE a.attrelid = to_regclass($1) AND a.attname = $2 AND a.attnum > 0 AND NOT a.attisdropped`,
		pgx.Identifier{schema, table}.Sanitize(), column,
	).Scan(&typ); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ""
		}
		t.Fatalf("read %s.%s.%s: %v", schema, table, column, err)
	}
	return *typ
}

func TestALegacyBuiltDatabaseUpgradesToHeadAndItsAuditChainVerifies(t *testing.T) {
	pool := newLegacyBuiltDatabase(t)
	ctx := context.Background()
	runner := New(pool, platformmigrations.Files)

	// Without the adoption step this is where the upgrade stopped: shared 0206
	// builds the canonical row over columns the table lacks, and the whole
	// batch rolls back.
	err := runner.ApplyShared(ctx)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42P01" || !strings.Contains(err.Error(), "0206_audit_events_chain") {
		t.Fatalf("ApplyShared before adoption = %v, want 0206 failing with 42P01", err)
	}

	applied, err := Bootstrap(ctx, pool, bootstrapschema.Initial)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if applied {
		t.Fatal("Bootstrap applied the legacy-era schema to a database that carries it")
	}
	if err := runner.ApplyShared(ctx); err != nil {
		t.Fatalf("ApplyShared after adoption: %v", err)
	}
	if err := runner.ApplyTenant(ctx, 1); err != nil {
		t.Fatalf("ApplyTenant(1): %v", err)
	}

	for column, want := range map[string]string{
		"input_tokens":  "integer",
		"output_tokens": "integer",
		"llm_cost":      "numeric(18,8)",
		"token_source":  "character varying(16)",
		"cost_source":   "character varying(64)",
	} {
		if got := columnType(t, pool, "centry", "audit_events", column); got != want {
			t.Errorf("centry.audit_events.%s = %q, want %q", column, got, want)
		}
	}

	// A row Main writes after the upgrade, with the token and cost columns
	// set, is chained; the two legacy rows are counted as written before the
	// chain existed. The verifier runs as the admin route runs it.
	if _, err := pool.Exec(ctx, `
		INSERT INTO centry.audit_events
		    (timestamp, event_type, action, duration_ms, is_error, user_email, project_id,
		     input_tokens, output_tokens, llm_cost, token_source, cost_source)
		VALUES (now(), 'llm', 'after the upgrade', 3.25, false, 'three@example.com', 1,
		        120, 45, 0.00123456, 'provider', 'price_table')`); err != nil {
		t.Fatalf("insert a row after the upgrade: %v", err)
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	report, err := audit.VerifyChain(ctx, tx, 0, 0)
	_ = tx.Rollback(ctx)
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if report.Break != nil || report.Checked != 1 || report.Unchained != 2 {
		t.Fatalf("VerifyChain = checked %d, unchained %d, break %+v; want 1, 2, none",
			report.Checked, report.Unchained, report.Break)
	}

	// The tenant half: the columns and tables Main reads, as
	// create_tenant_schema declares them, with the legacy rows kept.
	for column, want := range map[string]string{
		"owner_id": "integer",
		"env_vars": "jsonb",
		"uuid":     "uuid",
	} {
		if got := columnType(t, pool, "p_1", "elitea_tools", column); got != want {
			t.Errorf("p_1.elitea_tools.%s = %q, want %q", column, got, want)
		}
	}
	var rows, owned, distinctUUIDs, emptyEnv int
	var ownerNullable string
	if err := pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE owner_id = 1),
		       count(DISTINCT uuid),
		       count(*) FILTER (WHERE env_vars = '{}'::jsonb),
		       (SELECT is_nullable FROM information_schema.columns
		         WHERE table_schema = 'p_1' AND table_name = 'elitea_tools' AND column_name = 'owner_id')
		  FROM p_1.elitea_tools`).Scan(&rows, &owned, &distinctUUIDs, &emptyEnv, &ownerNullable); err != nil {
		t.Fatalf("read the upgraded toolkits: %v", err)
	}
	if rows != 2 || owned != 2 || distinctUUIDs != 2 || emptyEnv != 2 || ownerNullable != "NO" {
		t.Fatalf("toolkits = %d rows, %d owned by project 1, %d distinct uuids, %d empty env_vars, owner_id nullable %s; want 2, 2, 2, 2, NO",
			rows, owned, distinctUUIDs, emptyEnv, ownerNullable)
	}
	var settings string
	if err := pool.QueryRow(ctx, `SELECT settings::text FROM p_1.elitea_tools WHERE name = 'legacy toolkit one'`).Scan(&settings); err != nil || settings != `{"a": 1}` {
		t.Fatalf("legacy toolkit settings = %q, %v; want the legacy value", settings, err)
	}
	for _, table := range []string{"index_meta", "social_likes"} {
		var present bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "p_1."+table).Scan(&present); err != nil || !present {
			t.Errorf("p_1.%s present = %v, %v; want present", table, present, err)
		}
	}
	// The tables Main does not read per tenant stay as the legacy runtime
	// left them: absent.
	var pinsPresent bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('p_1.social_pins') IS NOT NULL`).Scan(&pinsPresent); err != nil || pinsPresent {
		t.Errorf("p_1.social_pins present = %v, %v; want absent", pinsPresent, err)
	}
	// The index pages' write path works on the new table.
	if _, err := pool.Exec(ctx, `
		INSERT INTO p_1.index_meta (toolkit_id, name, created_at)
		SELECT id, 'first index', now() AT TIME ZONE 'UTC' FROM p_1.elitea_tools WHERE name = 'legacy toolkit one'`); err != nil {
		t.Fatalf("write index metadata on the upgraded schema: %v", err)
	}
}

func TestBootstrapTakesNoLockOnAnAlreadyMigratedAuditTable(t *testing.T) {
	pool := newLegacyUpgradeDatabase(t)
	ctx := context.Background()
	if applied, err := Bootstrap(ctx, pool, bootstrapschema.Initial); err != nil || !applied {
		t.Fatalf("Bootstrap on an empty database = %v, %v; want applied", applied, err)
	}
	runner := New(pool, platformmigrations.Files)
	if err := runner.ApplyShared(ctx); err != nil {
		t.Fatalf("ApplyShared: %v", err)
	}
	var ledgerBefore int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.schema_migrations`).Scan(&ledgerBefore); err != nil {
		t.Fatal(err)
	}

	// A writer holds ROW EXCLUSIVE on the audit table, as every audit insert
	// does. An ALTER TABLE would need ACCESS EXCLUSIVE and wait behind it, so
	// Bootstrap finishing inside the deadline proves it altered nothing.
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, `LOCK TABLE centry.audit_events IN ROW EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	bootCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	applied, err := Bootstrap(bootCtx, pool, bootstrapschema.Initial)
	if err != nil || applied {
		t.Fatalf("Bootstrap on a migrated database = %v, %v; want no-op without waiting on the audit table", applied, err)
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	var ledgerAfter int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.schema_migrations`).Scan(&ledgerAfter); err != nil {
		t.Fatal(err)
	}
	if ledgerAfter != ledgerBefore {
		t.Fatalf("ledger rows %d -> %d; Bootstrap must not touch the ledger", ledgerBefore, ledgerAfter)
	}
}

// countingHandler counts the adoption log records.
type countingHandler struct {
	mu      sync.Mutex
	adopted map[string]int
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *countingHandler) WithGroup(string) slog.Handler            { return h }
func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	if !strings.Contains(r.Message, "legacy-built table") {
		return nil
	}
	var table, column string
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "table":
			table = a.Value.String()
		case "column":
			column = a.Value.String()
		}
		return true
	})
	h.mu.Lock()
	h.adopted[table+"."+column]++
	h.mu.Unlock()
	return nil
}

func TestConcurrentBootstrapsAdoptALegacyBuiltDatabaseOnce(t *testing.T) {
	pool := newLegacyBuiltDatabase(t)
	handler := &countingHandler{adopted: map[string]int{}}
	previous := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(previous) })

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = Bootstrap(ctx, pool, bootstrapschema.Initial)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Bootstrap %d: %v", i, err)
		}
	}
	if len(handler.adopted) != len(legacyAdoptionColumns) {
		t.Fatalf("adopted %v; want each of the %d columns once", handler.adopted, len(legacyAdoptionColumns))
	}
	for key, n := range handler.adopted {
		if n != 1 {
			t.Fatalf("%s adopted %d times; want once", key, n)
		}
	}
	if err := New(pool, platformmigrations.Files).ApplyShared(ctx); err != nil {
		t.Fatalf("ApplyShared after the concurrent adoptions: %v", err)
	}
}
