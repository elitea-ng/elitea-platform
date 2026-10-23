package repos

import (
	"context"
	"crypto/sha256"
	"testing"
	"testing/fstest"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSharedMigration0047BackfillsLatestWorkerSequenceAndAddsReplayIndexes(t *testing.T) {
	pool := newPostgresIntegrationPool(t)
	seedSharedMigrationMinimums(t, pool)
	applySharedMigrationsUpTo(t, pool, 46)
	seedHistoricalReplayGapFixture(t, pool)
	applySharedMigrationsUpTo(t, pool, 47)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var sequence int64
	var eventID string
	var retainedEvents int64
	if err := pool.QueryRow(ctx, `
SELECT last_node_sequence, last_node_event_id, retained_progress_events
FROM elitea_runtime.execution_replay_state
WHERE execution_id = $1 AND generation = 1`,
		"execution-gap-sequence",
	).Scan(&sequence, &eventID, &retainedEvents); err != nil {
		t.Fatal(err)
	}
	if sequence != 3 || eventID != "command-gap-sequence:3" || retainedEvents != 3 {
		t.Fatalf("0047 did not backfill the latest worker sequence: sequence=%d event_id=%q retained=%d", sequence, eventID, retainedEvents)
	}

	for _, indexName := range []string{
		"elitea_runtime.execution_replay_progress_project_execution_generation_idx",
		"elitea_runtime.execution_replay_state_last_node_event_idx",
	} {
		var present bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, indexName).Scan(&present); err != nil {
			t.Fatalf("lookup replay index %s: %v", indexName, err)
		}
		if !present {
			t.Fatalf("replay index %s was not created", indexName)
		}
	}
}

func TestSharedMigration0050BackfillsConservativeInvocationState(t *testing.T) {
	pool := newPostgresIntegrationPool(t)
	seedSharedMigrationMinimums(t, pool)
	applySharedMigrationsUpTo(t, pool, 46)
	seedHistoricalReplayGapFixture(t, pool)
	applySharedMigrationsUpTo(t, pool, 50)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var state string
	if err := pool.QueryRow(ctx, `
SELECT invocation_state
FROM elitea_runtime.execution_jobs
WHERE execution_id = 'execution-gap-sequence' AND generation = 1`,
	).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "MAY_HAVE_STARTED" {
		t.Fatalf("0050 did not conservatively fence historical RUNNING work: %q", state)
	}
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs
SET invocation_state = 'UNSAFE'
WHERE execution_id = 'execution-gap-sequence' AND generation = 1`,
	); err == nil {
		t.Fatal("0050 accepted an unknown invocation state")
	}
}

func seedSharedMigrationMinimums(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
CREATE SCHEMA centry;
CREATE TABLE centry.project (
    id INTEGER PRIMARY KEY,
    create_success BOOLEAN NOT NULL,
    suspended BOOLEAN NOT NULL
);
INSERT INTO centry.project (id, create_success, suspended)
VALUES (1, TRUE, FALSE)`); err != nil {
		t.Fatalf("seed minimum shared migration fixtures: %v", err)
	}
}

func applySharedMigrationsUpTo(t *testing.T, pool *pgxpool.Pool, version int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	runner := migrate.New(pool, sharedMigrationHistoryUpTo(t, version))
	if err := runner.ApplyShared(ctx); err != nil {
		t.Fatalf("apply shared migrations through %d: %v", version, err)
	}
}

func sharedMigrationHistoryUpTo(t *testing.T, version int64) fstest.MapFS {
	t.Helper()
	manifest, err := migrate.LoadManifest(platformmigrations.Files, migrate.ScopeShared)
	if err != nil {
		t.Fatalf("load shared migration manifest: %v", err)
	}
	files := make(fstest.MapFS)
	for _, migration := range manifest {
		if migration.Version > version {
			break
		}
		files[migration.Path] = &fstest.MapFile{Data: append([]byte(nil), migration.SQL...)}
	}
	return files
}

func seedHistoricalReplayGapFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bundleDigest := migrationDigestBytes("gap-bundle")
	requestDigest := migrationDigestBytes("gap-request")
	progressOne := []byte(`{"sequence":1}`)
	progressThree := []byte(`{"sequence":3}`)
	malformedNewest := []byte(`{"sequence":"malformed-newest"}`)
	progressOneDigest := migrationDigestBytes(string(progressOne))
	progressThreeDigest := migrationDigestBytes(string(progressThree))
	malformedNewestDigest := migrationDigestBytes(string(malformedNewest))

	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.input_bundles (
    input_bundle_id, immutable_version, media_type, resource_project_id,
    manifest_digest, manifest_size, manifest_bytes, created_by
) VALUES (
    'bundle-gap-sequence', '1', 'application/x-protobuf', 1,
    $1, 10, '0123456789'::bytea, 'test'
)`, bundleDigest); err != nil {
		t.Fatalf("seed historical replay input bundle: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id, resource_project_id,
    projection_project_id, actor_id, principal_ref, capability_id,
    capability_version, input_bundle_id, request_digest, idempotency_scope,
    idempotency_key, configuration_revision_id, configuration_type,
    catalog_revision, catalog_digest, schema_id, schema_revision,
    schema_digest, settings_entry_id, state, desired_state
) VALUES (
    'execution-gap-sequence', 1, 'command-gap-sequence', '1', 1, 1, '1',
    'principal-gap-sequence', 'index.ingest.v1', '1', 'bundle-gap-sequence',
    $1, 'tests', 'gap-sequence', NULL, NULL, NULL, NULL, NULL, NULL, NULL,
    NULL, 'RUNNING', 'RUNNING'
)`, requestDigest); err != nil {
		t.Fatalf("seed historical replay execution: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.execution_replay_events (
    event_id, execution_id, generation, projection_project_id,
    event_type, event_bytes, event_digest
) VALUES
    ('command-gap-sequence:1', 'execution-gap-sequence', 1, 1,
     'execution.node_event', $1, $2),
    ('command-gap-sequence:3', 'execution-gap-sequence', 1, 1,
     'execution.node_event', $3, $4),
    ('command-gap-sequence:not-a-number', 'execution-gap-sequence', 1, 1,
     'execution.node_event', $5, $6)`,
		progressOne,
		progressOneDigest,
		progressThree,
		progressThreeDigest,
		malformedNewest,
		malformedNewestDigest,
	); err != nil {
		t.Fatalf("seed historical replay events: %v", err)
	}

	var cursorCount int
	if err := pool.QueryRow(ctx, `
SELECT count(*)
FROM elitea_runtime.execution_replay_events
WHERE execution_id = 'execution-gap-sequence'`).Scan(&cursorCount); err != nil {
		t.Fatal(err)
	}
	if cursorCount != 3 {
		t.Fatalf("unexpected historical replay fixture row count: %d", cursorCount)
	}
}

func migrationDigestBytes(value string) []byte {
	digest := sha256String(value)
	return digest[:]
}

func sha256String(value string) [32]byte {
	return sha256.Sum256([]byte(value))
}
