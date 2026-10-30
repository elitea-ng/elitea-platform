package requestlog

// verdicts_postgres_integration_test.go runs the verdict INSERT and the prune
// against a REAL PostgreSQL, with the SHIPPED DDL (shared migration 0214). A
// unit test with a fake execer cannot report a wrong column name or a value a
// CHECK constraint refuses.
//
// It runs only when ELITEA_TEST_DATABASE_URL names a database. With
// ELITEA_REQUIRE_GATEWAY_POSTGRES_TEST set (CI), a missing database fails it.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/guardrail"
)

func TestPostgresGuardrailVerdictsInsertAndPrune(t *testing.T) {
	url := os.Getenv("ELITEA_TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("ELITEA_REQUIRE_GATEWAY_POSTGRES_TEST") != "" {
			t.Fatal("ELITEA_REQUIRE_GATEWAY_POSTGRES_TEST is set, so this Postgres test may not skip")
		}
		t.Skip("set ELITEA_TEST_DATABASE_URL to run the guardrail verdict integration test")
	}
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	path := filepath.Join("..", "..", "..", "elitea-main", "migrations", "shared", "0214_guardrail_pipeline.sql")
	ddl, err := os.ReadFile(path) //nolint:gosec // a fixed in-repo path
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(ddl)); err != nil {
		t.Fatalf("apply 0214: %v", err)
	}
	execution := "exec-verdict-it-" + time.Now().Format("150405.000000000")
	t.Cleanup(func() {
		_, _ = pool.Exec(t.Context(), `DELETE FROM gateway.guardrail_verdicts WHERE execution_id = $1`, execution)
	})

	store := NewStore(pool)
	old := time.Now().Add(-48 * time.Hour).UTC()
	records := []Record{{
		OccurredAt: old, ProjectID: "3", UserID: "8", Route: "/llm/v1/chat/completions", ExecutionID: execution,
		Guardrails: []guardrail.Verdict{
			{Stage: "output", Row: "no-secrets", Detector: "secrets", DetectorType: "secrets", Action: "mask",
				Outcome: "masked", FailMode: "closed", Labels: []string{"github-token"}, Findings: 1, LatencyMS: 3, Runs: 2},
			{Stage: "input", Row: "adapter", Detector: "http", DetectorType: "http", Action: "flag",
				Outcome: "error_open", FailMode: "open", Labels: []string{"detector_error"}, LatencyMS: 50, Runs: 1},
		},
	}}
	if err := store.writeVerdicts(ctx, records); err != nil {
		t.Fatalf("write: %v", err)
	}
	var count int
	var labels []string
	if err := pool.QueryRow(ctx, `SELECT count(*), max(labels::text)::text[] FROM gateway.guardrail_verdicts
		WHERE execution_id = $1 AND project_id = 3 AND user_id = 8`, execution).Scan(&count, &labels); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("stored %d verdicts; want 2", count)
	}
	if _, err := store.pruneVerdicts(ctx, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM gateway.guardrail_verdicts WHERE execution_id = $1`, execution).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("%d verdicts survived the prune", count)
	}
}
