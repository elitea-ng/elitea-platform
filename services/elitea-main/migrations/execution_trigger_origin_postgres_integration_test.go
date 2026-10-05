package migrations_test

// shared/0140: execution_jobs.trigger_origin (legacy issues 6802 and 6881).
//
// The backfill cases run the ledgered corpus, write rows the way a database
// that predates 0140 holds them (every row `manual`), and then run the
// migration's own SQL again. Re-running the real file is the point: a copy of
// the SQL here would measure the copy.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"os"
	"regexp"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const triggerOriginMigration = "shared/0140_execution_trigger_origin.sql"

func applyTriggerOriginMigration(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	body, err := os.ReadFile(triggerOriginMigration)
	if err != nil {
		t.Fatalf("read %s: %v", triggerOriginMigration, err)
	}
	ctx, cancel := testContext()
	defer cancel()
	if _, err := pool.Exec(ctx, string(body)); err != nil {
		t.Fatalf("apply %s: %v", triggerOriginMigration, err)
	}
}

// seedOriginJob writes one execution_jobs row with the column default, the
// shape every row had before 0140.
func seedOriginJob(t *testing.T, pool *pgxpool.Pool, executionID, capabilityID string) {
	t.Helper()
	ctx, cancel := testContext()
	defer cancel()
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.input_bundles
    (input_bundle_id, immutable_version, resource_project_id, media_type,
     manifest_digest, manifest_size, manifest_bytes, created_by)
VALUES ($1, 'admission:'||$1, 1, 'application/x-protobuf',
        decode(repeat('61', 32), 'hex'), 1, decode('00', 'hex'), 'actor-1')`,
		"bundle-"+executionID); err != nil {
		t.Fatalf("seed input bundle: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id, resource_project_id,
    projection_project_id, actor_id, principal_ref, capability_id,
    capability_version, input_bundle_id, request_digest, idempotency_scope,
    idempotency_key, state, desired_state
) VALUES (
    $1, 1, 'cmd-'||$1, '1', 1, 1, '7', '7', $2, 'v1', $3,
    decode(repeat('61', 32), 'hex'), 'scope-'||$1, 'key-'||$1, 'SUCCEEDED', 'RUNNING'
)`, executionID, capabilityID, "bundle-"+executionID); err != nil {
		t.Fatalf("seed execution job %s: %v", executionID, err)
	}
}

func originOf(t *testing.T, pool *pgxpool.Pool, executionID string) string {
	t.Helper()
	ctx, cancel := testContext()
	defer cancel()
	var origin string
	if err := pool.QueryRow(ctx,
		`SELECT trigger_origin FROM elitea_runtime.execution_jobs WHERE execution_id = $1`,
		executionID).Scan(&origin); err != nil {
		t.Fatalf("read trigger_origin of %s: %v", executionID, err)
	}
	return origin
}

// A new row is `manual` unless its writer says otherwise, and the CHECK
// refuses a value the analytics reads would not recognise.
func TestTriggerOriginDefaultsToManualAndRefusesAnUnknownValue(t *testing.T) {
	pool := newMigratedPool(t)
	seedOriginJob(t, pool, "exec-default", "agent.execute.application.v1")
	if got := originOf(t, pool, "exec-default"); got != "manual" {
		t.Fatalf("default trigger_origin = %q, want manual", got)
	}

	ctx, cancel := testContext()
	defer cancel()
	if _, err := pool.Exec(ctx,
		`UPDATE elitea_runtime.execution_jobs SET trigger_origin = 'cron' WHERE execution_id = 'exec-default'`); err == nil {
		t.Fatal("the CHECK accepted an unknown trigger origin")
	}
	for _, origin := range []string{"manual", "api", "schedule", "webhook", "index"} {
		if _, err := pool.Exec(ctx,
			`UPDATE elitea_runtime.execution_jobs SET trigger_origin = $1 WHERE execution_id = 'exec-default'`,
			origin); err != nil {
			t.Fatalf("the CHECK refused %q: %v", origin, err)
		}
	}
}

// The two recoverable origins are backfilled: every index ingest, and every
// unattended pipeline run public.pipeline_runs tracked. Everything else stays
// manual, which is what it was counted as before.
func TestTriggerOriginBackfillsIndexIngestsAndTrackedPipelineRuns(t *testing.T) {
	pool := newMigratedPool(t)

	seedOriginJob(t, pool, "exec-index", "index.ingest.v1")
	seedOriginJob(t, pool, "exec-scheduled", "agent.execute.application.v1")
	seedOriginJob(t, pool, "exec-webhook", "agent.execute.application.v1")
	seedOriginJob(t, pool, "exec-chat", "agent.execute.application.v1")

	ctx, cancel := testContext()
	defer cancel()
	if _, err := pool.Exec(ctx, `
INSERT INTO public.pipeline_runs
    (execution_id, project_id, application_id, version_id, conversation_uuid, origin)
VALUES ('exec-scheduled', '1', 5, 6, gen_random_uuid()::text, 'Schedule'),
       ('exec-webhook',   '1', 5, 6, gen_random_uuid()::text, 'Webhook')`); err != nil {
		t.Fatalf("seed pipeline runs: %v", err)
	}
	// The corpus already ran 0140 over an empty table, so every seeded row
	// holds the default. Re-running the file is the upgrade of a populated
	// database.
	for _, id := range []string{"exec-index", "exec-scheduled", "exec-webhook", "exec-chat"} {
		if got := originOf(t, pool, id); got != "manual" {
			t.Fatalf("%s before the backfill = %q, want manual", id, got)
		}
	}

	applyTriggerOriginMigration(t, pool)

	want := map[string]string{
		"exec-index":     "index",
		"exec-scheduled": "schedule",
		"exec-webhook":   "webhook",
		"exec-chat":      "manual",
	}
	for id, origin := range want {
		if got := originOf(t, pool, id); got != origin {
			t.Errorf("%s after the backfill = %q, want %q", id, got, origin)
		}
	}

	// Idempotent: a second run changes nothing and does not fail on the
	// constraint it already created.
	applyTriggerOriginMigration(t, pool)
	for id, origin := range want {
		if got := originOf(t, pool, id); got != origin {
			t.Errorf("%s after a second run = %q, want %q", id, got, origin)
		}
	}
}

// The ledgered runner applies every pending file in ONE transaction, so an
// upgrade runs this file while 0139's ALTER holds ACCESS EXCLUSIVE on the
// request log and this file's ALTER holds it on execution_jobs. An index
// build or a validating CHECK here would keep both locks for a full scan.
// Static, so it runs without a database.
func TestTriggerOriginMigrationHoldsNoLockForAFullScan(t *testing.T) {
	body, err := os.ReadFile(triggerOriginMigration)
	if err != nil {
		t.Fatalf("read %s: %v", triggerOriginMigration, err)
	}
	sql := stripSQLComments(string(body))
	if regexp.MustCompile(`(?i)\bCREATE\s+(UNIQUE\s+)?INDEX\b`).MatchString(sql) {
		t.Fatal("0140 builds an index inside the migration transaction")
	}
	if !regexp.MustCompile(`(?is)ADD\s+CONSTRAINT\s+execution_jobs_trigger_origin.*?NOT\s+VALID`).MatchString(sql) {
		t.Fatal("0140 adds the trigger_origin CHECK without NOT VALID, which scans execution_jobs under the lock")
	}
}

// The CHECK is enforced for new rows although it is NOT VALID.
func TestTriggerOriginCheckIsNotValidatedButEnforced(t *testing.T) {
	pool := newMigratedPool(t)
	ctx, cancel := testContext()
	defer cancel()
	var validated bool
	if err := pool.QueryRow(ctx, `
SELECT convalidated FROM pg_constraint
WHERE conname = 'execution_jobs_trigger_origin'
  AND conrelid = 'elitea_runtime.execution_jobs'::regclass`).Scan(&validated); err != nil {
		t.Fatalf("read constraint: %v", err)
	}
	if validated {
		t.Fatal("the trigger_origin CHECK was validated, so the migration scanned execution_jobs")
	}
	var indexed bool
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass('gateway.idx_llm_request_logs_project_execution') IS NOT NULL`).Scan(&indexed); err != nil {
		t.Fatalf("probe index: %v", err)
	}
	if indexed {
		t.Fatal("0140 built an index on gateway.llm_request_logs")
	}
}

func stripSQLComments(sql string) string {
	return regexp.MustCompile(`(?m)--.*$`).ReplaceAllString(sql, "")
}
