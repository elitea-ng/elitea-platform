package migrations_test

// shared/0174 gives the gateway its metered tool price catalog and the ledger
// columns that tell a tool row from a model row (#1238). The gateway reads the
// price as exact nano-USD, and a writer that runs before the migration still
// writes model rows, so the test checks both halves.
//
// Revert shared/0174 and every subtest fails with SQLSTATE 42P01 or 42703.

import (
	"context"
	"errors"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestMigrationCorpusCreatesTheMeteredOperationCatalog(t *testing.T) {
	pool := newMigratedPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Run("the seed prices read as exact nano-USD", func(t *testing.T) {
		const read = `SELECT unit, (price_per_unit_usd * 1000000000::numeric)::bigint
			FROM gateway.metered_operation_prices WHERE provider = $1 AND operation = $2`
		for _, tc := range []struct {
			provider, operation, unit string
			nano                      int64
		}{
			{"tavily", "web_search", "credit", 8_000_000},
			{"firecrawl", "web_fetch", "page", 3_800_000},
			{"brave", "web_search", "request", 5_000_000},
			{"ddg_instant", "web_search", "request", 0},
			{"direct", "web_fetch", "page", 0},
			{"shodan", "host_lookup", "credit", 6_900_000},
		} {
			var unit string
			var nano int64
			if err := pool.QueryRow(ctx, read, tc.provider, tc.operation).Scan(&unit, &nano); err != nil {
				t.Fatalf("read %s/%s: %v", tc.provider, tc.operation, err)
			}
			if unit != tc.unit || nano != tc.nano {
				t.Errorf("%s/%s = %s %d nano-USD, want %s %d", tc.provider, tc.operation, unit, nano, tc.unit, tc.nano)
			}
		}
	})

	t.Run("the catalog refuses an unknown unit and a negative price", func(t *testing.T) {
		const insert = `INSERT INTO gateway.metered_operation_prices (provider, operation, unit, price_per_unit_usd)
			VALUES ($1, $2, $3, $4)`
		for _, tc := range []struct {
			name, unit, price string
		}{
			{"unknown unit", "token", "0.001"},
			{"negative price", "request", "-0.001"},
		} {
			_, err := pool.Exec(ctx, insert, "example", "web_search", tc.unit, tc.price)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
				t.Errorf("%s: want a 23514 check violation, got %v", tc.name, err)
			}
		}
	})

	t.Run("an old writer still inserts a model row", func(t *testing.T) {
		// The 0084 column list, without the three new columns.
		if _, err := pool.Exec(ctx, `INSERT INTO gateway.llm_usage_events
			(event_id, project_id, user_id, provider, model, cost_usd, period_start, period_end)
			VALUES ('00000000-0000-0000-0000-000000000174', 1, 1, 'openai', 'gpt-4o', 0.01, now(), now() + interval '1 month')`); err != nil {
			t.Fatalf("insert an old-shape usage event: %v", err)
		}
		var operation string
		var units *int64
		if err := pool.QueryRow(ctx, `SELECT operation, units FROM gateway.llm_usage_events
			WHERE event_id = '00000000-0000-0000-0000-000000000174'`).Scan(&operation, &units); err != nil {
			t.Fatalf("read the usage event back: %v", err)
		}
		if operation != "llm" || units != nil {
			t.Fatalf("an old-shape row reads operation=%q units=%v, want llm and NULL", operation, units)
		}
	})

	t.Run("the request log takes a tool row with its cost", func(t *testing.T) {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT count(*) = 4 FROM information_schema.columns
			WHERE table_schema = 'gateway' AND table_name = 'llm_request_logs'
			AND column_name IN ('operation', 'units', 'unit', 'cost_usd')`).Scan(&exists); err != nil {
			t.Fatalf("read the request log columns: %v", err)
		}
		if !exists {
			t.Fatal("gateway.llm_request_logs lacks one of operation, units, unit, cost_usd")
		}
	})
}

const meteredOperationsMigration = "shared/0174_gateway_metered_operations.sql"

// The ledgered runner applies every pending file in ONE transaction, so each
// ALTER of gateway.llm_usage_events and gateway.llm_request_logs holds ACCESS
// EXCLUSIVE until the upgrade commits. 0174 must not scan either table under
// that lock: no inline (validating) CHECK on the new column and no index on
// them. Static, so it runs without a database.
func TestMeteredOperationsMigrationHoldsNoLockForAFullScan(t *testing.T) {
	body, err := os.ReadFile(meteredOperationsMigration)
	if err != nil {
		t.Fatalf("read %s: %v", meteredOperationsMigration, err)
	}
	sql := stripSQLComments(string(body))
	if regexp.MustCompile(`(?i)\bCREATE\s+(UNIQUE\s+)?INDEX\b[^;]*\bON\s+gateway\.llm_(usage_events|request_logs)\b`).MatchString(sql) {
		t.Fatal("0174 builds an index on a ledger table inside the migration transaction")
	}
	if regexp.MustCompile(`(?is)ADD\s+COLUMN[^;]*\boperation\b[^;]*\bCHECK\b`).MatchString(sql) {
		t.Fatal("0174 adds the operation column with an inline CHECK, which validates every row under the lock")
	}
	for _, name := range []string{"llm_usage_events_operation", "llm_request_logs_operation"} {
		if !regexp.MustCompile(`(?is)ADD\s+CONSTRAINT\s+` + name + `.*?NOT\s+VALID`).MatchString(sql) {
			t.Fatalf("0174 adds %s without NOT VALID", name)
		}
	}
}

// The operation CHECKs are NOT VALID (no scan) and still refuse a new row
// with an unknown operation.
func TestMeteredOperationChecksAreNotValidatedButEnforced(t *testing.T) {
	pool := newMigratedPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, tc := range []struct{ constraint, table string }{
		{"llm_usage_events_operation", "gateway.llm_usage_events"},
		{"llm_request_logs_operation", "gateway.llm_request_logs"},
	} {
		var validated bool
		if err := pool.QueryRow(ctx, `SELECT convalidated FROM pg_constraint
			WHERE conname = $1 AND conrelid = $2::regclass`, tc.constraint, tc.table).Scan(&validated); err != nil {
			t.Fatalf("read %s: %v", tc.constraint, err)
		}
		if validated {
			t.Fatalf("%s was validated, so the migration scanned %s", tc.constraint, tc.table)
		}
	}
	_, err := pool.Exec(ctx, `INSERT INTO gateway.llm_usage_events
		(event_id, project_id, user_id, provider, model, cost_usd, period_start, period_end, operation)
		VALUES ('00000000-0000-0000-0000-000000001740', 1, 1, 'openai', 'gpt-4o', 0.01, now(), now() + interval '1 month', 'scan')`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("an unknown operation: want a 23514 check violation, got %v", err)
	}
	var indexed bool
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass('gateway.idx_llm_usage_events_project_operation_time') IS NOT NULL`).Scan(&indexed); err != nil {
		t.Fatalf("probe index: %v", err)
	}
	if indexed {
		t.Fatal("0174 built an index on gateway.llm_usage_events")
	}
}
