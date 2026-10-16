package analytics

// The one-snapshot property the cost breakdown's two reads depend on.
//
// `Costs` runs an aggregate and a row listing as two separate statements. Under
// READ COMMITTED each statement takes its OWN snapshot — even inside a
// transaction — and the write-back consumer commits into that table
// continuously, so without the REPEATABLE READ transaction the two can describe
// different databases: `by_scope[...].rows` says 2 while `periods` holds 1 row
// and `periods_truncated` is false, which is exactly the completeness signal
// those fields exist to give.
//
// This is an INTERNAL test because the property lives at the seam the handler
// uses (scopeTotals and periods both taking one `querier`), and because the
// interleaving has to be deterministic: the concurrent write happens BETWEEN
// the two reads by construction, rather than by racing a live request and
// hoping. Rewriting the handler to two pool.Query calls — the exact
// simplification this guards against — passes every other test in this package
// and fails here.

import (
	"context"
	"fmt"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	infradb "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db"
)

func TestCostReadsShareOneSnapshotAcrossTheAggregateAndTheListing(t *testing.T) {
	pool := newSnapshotPool(t)
	ctx := context.Background()

	const projectID = 71
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	insertAccumulator(t, pool, projectID, "project", "71", start, end, "3.00000000")

	// The same transaction the handler opens.
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	totals, err := scopeTotals(ctx, tx, projectID, start, end)
	if err != nil {
		t.Fatalf("scopeTotals: %v", err)
	}
	if len(totals) != 1 || totals[0].Rows != 1 || totals[0].TotalCost.String() != "3.00000000" {
		t.Fatalf("totals = %+v, want one project row of 3.00000000", totals)
	}

	// A committed write from OUTSIDE the transaction, landing between the two
	// reads — what budgetwriteback does continuously.
	insertAccumulator(t, pool, projectID, "user", "71:9", start, end, "1.00000000")

	periods, truncated, err := periods(ctx, tx, projectID, start, end)
	if err != nil {
		t.Fatalf("periods: %v", err)
	}
	if truncated {
		t.Fatal("truncated = true on a two-row window")
	}
	// THE assertion: the listing is a subset of exactly the rows the totals
	// covered. Seeing the new row here would mean the two halves of one
	// response describe different databases.
	if len(periods) != 1 {
		t.Fatalf("periods holds %d rows, want 1 — the listing must not see a write "+
			"committed after the aggregate's snapshot was taken", len(periods))
	}
	if periods[0].Scope != "project" {
		t.Fatalf("periods[0].scope = %q, want project", periods[0].Scope)
	}

	// And the row IS there for the next request: the isolation is a snapshot,
	// not a filter that would hide the write forever.
	fresh, err := scopeTotals(ctx, pool, projectID, start, end)
	if err != nil {
		t.Fatalf("scopeTotals outside the transaction: %v", err)
	}
	if len(fresh) != 2 {
		t.Fatalf("a read outside the transaction sees %d scopes, want both", len(fresh))
	}
}

func insertAccumulator(
	t *testing.T, pool *pgxpool.Pool, projectID int, scope, scopeID string,
	start, end time.Time, usd string,
) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO gateway.llm_budget_accumulators
    (project_id, scope, scope_id, period_start, period_end, accumulated_cost)
VALUES ($1, $2, $3, $4, $5, $6::numeric)`, projectID, scope, scopeID, start, end, usd)
	if err != nil {
		t.Fatalf("insert accumulator: %v", err)
	}
}

func newSnapshotPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", environment, err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open PostgreSQL admin pool: %v", err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	databaseName := fmt.Sprintf("elitea_costsnap_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated PostgreSQL integration database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	// More than one connection, so the transaction under test and the
	// concurrent writer cannot be serialised onto the same connection by the
	// pool — which would make the interleave untestable.
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		if _, dropErr := adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); dropErr != nil {
			t.Errorf("drop database after pool open failure: %v", dropErr)
		}
		adminPool.Close()
		t.Fatalf("open isolated PostgreSQL integration database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		// 120 s, not the old 20 s to 30 s. This DROP queues behind the
		// CREATE DATABASE calls of every package that `go test ./...` runs at
		// the same time, so the wait is server load and not a hang. Two full
		// runs failed here with "drop isolated ... database: timeout" (#409).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated PostgreSQL integration database: %v", err)
		}
		adminPool.Close()
	})

	gatewaySQL, err := infradb.GatewayMigrationSQL()
	if err != nil {
		t.Fatalf("load gateway migrations: %v", err)
	}
	if _, err := pool.Exec(ctx, gatewaySQL); err != nil {
		t.Fatalf("apply gateway migrations: %v", err)
	}
	return pool
}
