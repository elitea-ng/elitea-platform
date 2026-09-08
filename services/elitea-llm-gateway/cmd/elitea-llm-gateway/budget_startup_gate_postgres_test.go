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
// It runs in a DATABASE OF ITS OWN, created and dropped by the test. The probe
// asks a question about the whole database ("does this deployment enforce any
// budget?"), so a shared database makes its negative cases meaningless: the
// first run of this file against the standalone stack failed the moment
// internal/failmode's own integration test seeded an enforcing project_budget
// row in a parallel package. A private database is the only isolation that
// matches the question the statement asks.
//
// It runs only when ELITEA_TEST_DATABASE_URL names a server:
//
//	ELITEA_TEST_DATABASE_URL='postgres://elitea:elitea@127.0.0.1:15433/elitea?sslmode=disable' \
//	  GOWORK=off go test -run TestPostgres ./cmd/elitea-llm-gateway/

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const budgetProbeDatabaseURLEnv = "ELITEA_TEST_DATABASE_URL"

// budgetProbeMigration creates both tables the probe reads. It is READ from the
// repository, so a column this code needs and the migration does not create
// fails the test.
const budgetProbeMigration = "0067_gateway_budget_schema.sql"

// openBudgetProbePool creates a private database, applies the shipped gateway
// DDL to it, and returns a pool onto it. The database is dropped at the end of
// the test.
func openBudgetProbePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	raw := os.Getenv(budgetProbeDatabaseURLEnv)
	if raw == "" {
		t.Skipf("set %s to run the budget-probe integration test", budgetProbeDatabaseURLEnv)
	}
	base, err := url.Parse(raw)
	if err != nil || base.Scheme == "" {
		t.Skipf("%s is not a URL DSN, so this test cannot create its own database: %v",
			budgetProbeDatabaseURLEnv, err)
	}

	// Read the migration BEFORE creating anything, so a missing file skips
	// instead of leaving a database behind.
	path := filepath.Join("..", "..", "..", "elitea-main", "migrations", "shared", budgetProbeMigration)
	migration, rerr := os.ReadFile(path) //nolint:gosec // a fixed in-repo path
	if rerr != nil {
		t.Skipf("cannot read the shipped migration %s: %v", budgetProbeMigration, rerr)
	}

	ctx := t.Context()
	admin, err := openSmallPool(ctx, raw)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close()

	// A name unique per run, so a leftover from an interrupted run and a
	// parallel run cannot collide. Only letters, digits and underscores reach
	// the identifier.
	name := fmt.Sprintf("elitea_budget_probe_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+pgIdentifier(name)); err != nil {
		t.Skipf("cannot create a private database for this test (%v). "+
			"Point %s at a server where the user may CREATE DATABASE", err, budgetProbeDatabaseURLEnv)
	}
	target := *base
	target.Path = "/" + name
	pool, err := openSmallPool(ctx, target.String())
	if err != nil {
		t.Fatalf("connect to the private database: %v", err)
	}

	// ONE cleanup, in one order: close this pool, THEN drop the database.
	// Two cleanups would rely on the LIFO order of t.Cleanup, and an open
	// connection makes DROP DATABASE wait rather than fail — which reads as a
	// hung test, not as a leak.
	t.Cleanup(func() {
		pool.Close()
		// A fresh context: t.Context() is already cancelled by now, and the
		// admin pool above is closed.
		bg, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cleaner, cerr := openSmallPool(bg, raw)
		if cerr != nil {
			t.Logf("cannot drop the test database %s: %v", name, cerr)
			return
		}
		defer cleaner.Close()
		if _, derr := cleaner.Exec(bg, `DROP DATABASE IF EXISTS `+pgIdentifier(name)+` WITH (FORCE)`); derr != nil {
			t.Logf("cannot drop the test database %s: %v", name, derr)
		}
	})

	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		t.Fatalf("apply %s: %v", budgetProbeMigration, err)
	}
	return pool
}

// openSmallPool dials with ONE connection.
//
// pgxpool's default maximum is one connection per CPU, and this file opens
// three pools per test against a server that is usually the shared development
// stack. The default exhausted max_connections and failed the second test with
// "sorry, too many clients already" — a failure that says nothing about the
// code under test.
func openSmallPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 1
	cfg.MinConns = 0
	return pgxpool.NewWithConfig(ctx, cfg)
}

// pgIdentifier quotes a generated identifier. The name is built from a pid and
// a timestamp, so it carries no caller input; the quoting is here because an
// identifier concatenated into DDL must never depend on that staying true.
func pgIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
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

	const projectID = 901
	const userID = 902

	// An empty database that HAS the tables. This is the fresh-install posture,
	// and it must not refuse a startup.
	if got := probeAuthoredBudgetsWith(ctx, pool); got.err != nil || got.found {
		t.Fatalf("an empty database reported authored budgets (found=%v, err=%v)", got.found, got.err)
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

// TestPostgresBudgetProbeOnADatabaseWithNoGatewaySchema is the other half of
// the two-statement design: a database that has never run shared migration 0067
// must answer "no budgets" and NOT an error.
//
// A fresh install is exactly that database, and an error there would be read by
// budgetStartupGate as "cannot tell", which is a log line nobody needs on every
// first boot.
func TestPostgresBudgetProbeOnADatabaseWithNoGatewaySchema(t *testing.T) {
	pool := openBudgetProbePool(t)
	ctx := t.Context()

	if _, err := pool.Exec(ctx, `DROP SCHEMA gateway CASCADE`); err != nil {
		t.Fatalf("drop the gateway schema: %v", err)
	}
	got := probeAuthoredBudgetsWith(ctx, pool)
	if got.err != nil {
		t.Fatalf("a database with no gateway schema made the probe fail: %v. "+
			"The table-existence probe must run in its OWN statement, because Postgres resolves "+
			"relations at parse time", got.err)
	}
	if got.found {
		t.Fatal("a database with no gateway schema reported authored budgets")
	}
}
