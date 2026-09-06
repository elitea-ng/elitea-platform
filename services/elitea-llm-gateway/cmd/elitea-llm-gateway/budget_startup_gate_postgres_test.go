package main

// budget_startup_gate_postgres_test.go — issue #304, the probe against a REAL
// PostgreSQL and the SHIPPED DDL.
//
// The fake querier in budget_startup_gate_test.go proves the control flow. It
// cannot report a syntax error, a wrong column name, or a predicate that reads
// the schema differently from the way the gateway's own snapshot reads it. The
// three statements here had never been parsed by Postgres, and one of them
// (budgetTableProbeSQL) exists only because Postgres resolves relations at
// parse time — a fact no in-process fake can demonstrate.
//
// It runs only when ELITEA_TEST_DATABASE_URL names a database:
//
//	ELITEA_TEST_DATABASE_URL='postgres://elitea:elitea@127.0.0.1:15433/elitea?sslmode=disable' \
//	  GOWORK=off go test -run TestPostgres ./cmd/elitea-llm-gateway/

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const budgetProbeDatabaseURLEnv = "ELITEA_TEST_DATABASE_URL"

// budgetProbeMigration creates both tables the probe reads. It is READ from the
// repository, so a column this code needs and the migration does not create
// fails the test.
const budgetProbeMigration = "0067_gateway_budget_schema.sql"

func openBudgetProbePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv(budgetProbeDatabaseURLEnv)
	if url == "" {
		t.Skipf("set %s to run the budget-probe integration test", budgetProbeDatabaseURLEnv)
	}
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	path := filepath.Join("..", "..", "..", "elitea-main", "migrations", "shared", budgetProbeMigration)
	sql, rerr := os.ReadFile(path) //nolint:gosec // a fixed in-repo path
	if rerr != nil {
		t.Skipf("cannot read the shipped migration %s: %v", budgetProbeMigration, rerr)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("apply %s: %v", budgetProbeMigration, err)
	}
	return pool
}

// TestPostgresBudgetProbeReadsTheShippedSchema executes every probe statement
// against the real tables, and proves the predicate counts a CEILING and not a
// row.
//
// The distinction is the whole value of the probe. Every project that opens a
// budget screen can get a gateway.project_budget row, and almost all of them
// are unlimited. A probe that counted rows would refuse to start on nearly
// every deployment, which is how a gate gets turned off again.
func TestPostgresBudgetProbeReadsTheShippedSchema(t *testing.T) {
	pool := openBudgetProbePool(t)
	ctx := t.Context()

	// A project id far outside the seeded range, unique per run, so a parallel
	// run and a shared development database cannot interfere.
	projectID := 900_000 + int(time.Now().UnixNano()%90_000)
	userID := projectID
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM gateway.user_budget WHERE project_id = $1`, projectID)
		_, _ = pool.Exec(bg, `DELETE FROM gateway.project_budget WHERE project_id = $1`, projectID)
	})

	// The probe must parse and answer on the untouched database first.
	base := probeAuthoredBudgetsWith(ctx, pool)
	if base.err != nil {
		t.Fatalf("the probe cannot read the shipped schema: %v", base.err)
	}
	if base.found {
		// The probe asks a question about the WHOLE database, so a database
		// that already holds a ceiling would answer "found" for every case
		// below, negative ones included. Skip rather than pass on an answer
		// that measures nothing.
		t.Skip("this database already holds an enforcing budget row, so the negative half of the probe " +
			"cannot be measured here; point ELITEA_TEST_DATABASE_URL at an isolated database")
	}

	// An UNLIMITED row is not a ceiling.
	if _, err := pool.Exec(ctx,
		`INSERT INTO gateway.project_budget (project_id, hard_limit_usd, is_unlimited, enabled)
		 VALUES ($1, 100, true, true)`, projectID); err != nil {
		t.Fatalf("seed an unlimited project budget: %v", err)
	}
	if got := probeAuthoredBudgetsWith(ctx, pool); got.err != nil || got.found {
		t.Fatalf("an unlimited project_budget row was read as an authored ceiling (found=%v, err=%v). "+
			"Every project that opens a budget screen gets such a row", got.found, got.err)
	}

	// A DISABLED row with an amount is not a ceiling either.
	if _, err := pool.Exec(ctx,
		`UPDATE gateway.project_budget SET is_unlimited = false, enabled = false WHERE project_id = $1`,
		projectID); err != nil {
		t.Fatalf("disable the project budget: %v", err)
	}
	if got := probeAuthoredBudgetsWith(ctx, pool); got.err != nil || got.found {
		t.Fatalf("a disabled project_budget row was read as an authored ceiling (found=%v, err=%v)", got.found, got.err)
	}

	// An enforcing row IS a ceiling.
	if _, err := pool.Exec(ctx,
		`UPDATE gateway.project_budget SET enabled = true WHERE project_id = $1`, projectID); err != nil {
		t.Fatalf("enable the project budget: %v", err)
	}
	if got := probeAuthoredBudgetsWith(ctx, pool); got.err != nil || !got.found {
		t.Fatalf("an enforcing project_budget row was not read as an authored ceiling (found=%v, err=%v)", got.found, got.err)
	}

	// A member ceiling alone is evidence. Remove the project row first, so the
	// answer can only come from gateway.user_budget.
	if _, err := pool.Exec(ctx, `DELETE FROM gateway.project_budget WHERE project_id = $1`, projectID); err != nil {
		t.Fatalf("remove the project budget: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO gateway.user_budget (project_id, user_id, hard_limit_usd, enabled)
		 VALUES ($1, $2, 25, false)`, projectID, userID); err != nil {
		t.Fatalf("seed a disabled member cap: %v", err)
	}
	if got := probeAuthoredBudgetsWith(ctx, pool); got.err != nil || got.found {
		t.Fatalf("a disabled user_budget row was read as an authored ceiling (found=%v, err=%v)", got.found, got.err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE gateway.user_budget SET enabled = true WHERE project_id = $1`, projectID); err != nil {
		t.Fatalf("enable the member cap: %v", err)
	}
	if got := probeAuthoredBudgetsWith(ctx, pool); got.err != nil || !got.found {
		t.Fatalf("an enforcing user_budget row was not read as an authored ceiling (found=%v, err=%v)", got.found, got.err)
	}
}
