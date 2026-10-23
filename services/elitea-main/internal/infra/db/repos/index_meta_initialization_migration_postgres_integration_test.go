package repos

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSharedMigration0051RecoversAndQuarantinesPreRecoveryAdmissions(t *testing.T) {
	pool := newPostgresIntegrationPool(t)
	seedSharedMigrationMinimums(t, pool)
	applySharedMigrationsUpTo(t, pool, 50)
	seedPre0051IndexInitializationFixtures(t, pool)
	applySharedMigrationsUpTo(t, pool, 51)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Run("recoverable pre-authority admission", func(t *testing.T) {
		var status, state, desiredState string
		var attemptCount int
		var nextAttemptAt, admittedAt time.Time
		var lastErrorCode, failedAt, retiredAt, retirementCode *string
		if err := pool.QueryRow(ctx, `
SELECT i.index_meta_initialization_status,
       i.index_meta_initialization_attempt_count,
       i.index_meta_initialization_next_attempt_at,
       i.index_meta_initialization_last_error_code,
       i.index_meta_initialization_failed_at::text,
       j.state,
       j.desired_state,
       j.admitted_at,
       o.retired_at::text,
       o.retirement_code
FROM elitea_runtime.index_ingest_jobs AS i
JOIN elitea_runtime.execution_jobs AS j
  ON j.execution_id = i.execution_id
 AND j.generation = i.generation
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = i.execution_id
 AND o.generation = i.generation
WHERE i.execution_id = 'migration-0051-recoverable'
  AND i.generation = 1`).Scan(
			&status,
			&attemptCount,
			&nextAttemptAt,
			&lastErrorCode,
			&failedAt,
			&state,
			&desiredState,
			&admittedAt,
			&retiredAt,
			&retirementCode,
		); err != nil {
			t.Fatal(err)
		}
		if status != "PENDING" || attemptCount != 0 ||
			!nextAttemptAt.Equal(admittedAt) || lastErrorCode != nil ||
			failedAt != nil || state != "PENDING" || desiredState != "RUNNING" ||
			retiredAt != nil || retirementCode != nil {
			t.Fatalf(
				"recoverable admission was not preserved for retry: status=%q attempts=%d next=%v admitted=%v error=%v failed=%v state=%q desired=%q retired=%v retirement=%v",
				status,
				attemptCount,
				nextAttemptAt,
				admittedAt,
				lastErrorCode,
				failedAt,
				state,
				desiredState,
				retiredAt,
				retirementCode,
			)
		}
		assertPostgresCount(t, ctx, pool, 0, `
SELECT count(*)
FROM elitea_runtime.execution_replay_events
WHERE execution_id = 'migration-0051-recoverable'`)
	})

	t.Run("unsafe pre-authority admission", func(t *testing.T) {
		var initializationStatus, initializationError, state, desiredState string
		var initializationFailedAt, settledAt, retiredAt time.Time
		var retirementCode string
		var preparedAt, publishedAt, authorityGrantedAt *string
		if err := pool.QueryRow(ctx, `
SELECT i.index_meta_initialization_status,
       i.index_meta_initialization_last_error_code,
       i.index_meta_initialization_failed_at,
       j.state,
       j.desired_state,
       j.settled_at,
       o.retired_at,
       o.retirement_code,
       o.prepared_at::text,
       o.published_at::text,
       o.authority_granted_at::text
FROM elitea_runtime.index_ingest_jobs AS i
JOIN elitea_runtime.execution_jobs AS j
  ON j.execution_id = i.execution_id
 AND j.generation = i.generation
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = i.execution_id
 AND o.generation = i.generation
WHERE i.execution_id = 'migration-0051-unsafe-pre-authority'
  AND i.generation = 1`).Scan(
			&initializationStatus,
			&initializationError,
			&initializationFailedAt,
			&state,
			&desiredState,
			&settledAt,
			&retiredAt,
			&retirementCode,
			&preparedAt,
			&publishedAt,
			&authorityGrantedAt,
		); err != nil {
			t.Fatal(err)
		}
		if initializationStatus != "QUARANTINED" ||
			initializationError != "UNSAFE_PRE_RECOVERY_ADMISSION" ||
			initializationFailedAt.IsZero() ||
			state != "QUARANTINED" || desiredState != "CANCELLED" ||
			settledAt.IsZero() || retiredAt.IsZero() ||
			retirementCode != "CANCELLED" ||
			preparedAt != nil || publishedAt != nil || authorityGrantedAt != nil {
			t.Fatalf(
				"unsafe pre-authority admission was not terminalized: initialization=%q error=%q failed=%v state=%q desired=%q settled=%v retired=%v retirement=%q prepared=%v published=%v authority=%v",
				initializationStatus,
				initializationError,
				initializationFailedAt,
				state,
				desiredState,
				settledAt,
				retiredAt,
				retirementCode,
				preparedAt,
				publishedAt,
				authorityGrantedAt,
			)
		}

		assertPostgresCount(t, ctx, pool, 1, `
SELECT count(*)
FROM elitea_runtime.execution_replay_events
WHERE execution_id = 'migration-0051-unsafe-pre-authority'
  AND generation = 1`)
		var eventID, eventType, eventJSON, eventDigest string
		if err := pool.QueryRow(ctx, `
SELECT event_id,
       event_type,
       convert_from(event_bytes, 'UTF8'),
       encode(event_digest, 'hex')
FROM elitea_runtime.execution_replay_events
WHERE execution_id = 'migration-0051-unsafe-pre-authority'
  AND generation = 1`).Scan(
			&eventID,
			&eventType,
			&eventJSON,
			&eventDigest,
		); err != nil {
			t.Fatal(err)
		}
		const canonicalFailure = `{"code":"INTERNAL","safe_message":"The runtime operation failed.","retryable":false}`
		if eventID != "index-meta-initialization-quarantine:migration-0051-unsafe-pre-authority-outbox" ||
			eventType != "execution.failed" ||
			eventJSON != canonicalFailure ||
			eventDigest != "56beddb414e183b1cece7ebc123a2e48f97a523477ee4cfce8275dd682ddc0ae" {
			t.Fatalf(
				"unexpected canonical failure replay: id=%q type=%q json=%q digest=%q",
				eventID,
				eventType,
				eventJSON,
				eventDigest,
			)
		}
	})

	t.Run("unsafe post-authority admission", func(t *testing.T) {
		var initializationStatus, initializationError, state, desiredState string
		var initializationFailedAt, preparedAt, publishedAt, authorityGrantedAt time.Time
		var settledAt, retiredAt, retirementCode *string
		if err := pool.QueryRow(ctx, `
SELECT i.index_meta_initialization_status,
       i.index_meta_initialization_last_error_code,
       i.index_meta_initialization_failed_at,
       j.state,
       j.desired_state,
       j.settled_at::text,
       o.prepared_at,
       o.published_at,
       o.authority_granted_at,
       o.retired_at::text,
       o.retirement_code
FROM elitea_runtime.index_ingest_jobs AS i
JOIN elitea_runtime.execution_jobs AS j
  ON j.execution_id = i.execution_id
 AND j.generation = i.generation
JOIN elitea_runtime.command_outbox AS o
  ON o.execution_id = i.execution_id
 AND o.generation = i.generation
WHERE i.execution_id = 'migration-0051-unsafe-post-authority'
  AND i.generation = 1`).Scan(
			&initializationStatus,
			&initializationError,
			&initializationFailedAt,
			&state,
			&desiredState,
			&settledAt,
			&preparedAt,
			&publishedAt,
			&authorityGrantedAt,
			&retiredAt,
			&retirementCode,
		); err != nil {
			t.Fatal(err)
		}
		expectedPreparedAt := time.Date(2026, time.July, 27, 8, 0, 0, 0, time.UTC)
		expectedPublishedAt := expectedPreparedAt.Add(time.Second)
		expectedAuthorityAt := expectedPublishedAt.Add(time.Second)
		if initializationStatus != "QUARANTINED" ||
			initializationError != "UNSAFE_PRE_RECOVERY_ADMISSION" ||
			initializationFailedAt.IsZero() ||
			state != "RUNNING" || desiredState != "CANCELLED" ||
			settledAt != nil ||
			!preparedAt.Equal(expectedPreparedAt) ||
			!publishedAt.Equal(expectedPublishedAt) ||
			!authorityGrantedAt.Equal(expectedAuthorityAt) ||
			retiredAt != nil || retirementCode != nil {
			t.Fatalf(
				"post-authority admission lost external-effect evidence: initialization=%q error=%q failed=%v state=%q desired=%q settled=%v prepared=%v published=%v authority=%v retired=%v retirement=%v",
				initializationStatus,
				initializationError,
				initializationFailedAt,
				state,
				desiredState,
				settledAt,
				preparedAt,
				publishedAt,
				authorityGrantedAt,
				retiredAt,
				retirementCode,
			)
		}
		assertPostgresCount(t, ctx, pool, 0, `
SELECT count(*)
FROM elitea_runtime.execution_replay_events
WHERE execution_id = 'migration-0051-unsafe-post-authority'`)
	})
}

func TestSharedMigration0051RejectsMissingAdmissionOutboxTransactionally(t *testing.T) {
	pool := newPostgresIntegrationPool(t)
	seedSharedMigrationMinimums(t, pool)
	applySharedMigrationsUpTo(t, pool, 50)
	seedPre0051MissingOutboxFixture(t, pool)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	runner := migrate.New(pool, sharedMigrationHistoryUpTo(t, 51))
	err := runner.ApplyShared(ctx)
	if err == nil ||
		!strings.Contains(
			err.Error(),
			"index metadata initialization recovery requires complete admission ownership",
		) {
		t.Fatalf("missing-outbox migration error = %v", err)
	}
	var postgresError *pgconn.PgError
	if !errors.As(err, &postgresError) || postgresError.Code != "23514" {
		t.Fatalf("missing-outbox migration SQLSTATE = %v", err)
	}

	var addedColumns int
	if err := pool.QueryRow(ctx, `
SELECT count(*)
FROM information_schema.columns
WHERE table_schema = 'elitea_runtime'
  AND table_name = 'index_ingest_jobs'
  AND column_name LIKE 'index_meta_initialization_%'`).Scan(&addedColumns); err != nil {
		t.Fatal(err)
	}
	if addedColumns != 0 {
		t.Fatalf("failed migration retained %d initialization columns", addedColumns)
	}
	assertPostgresCount(t, ctx, pool, 0, `
SELECT count(*)
FROM elitea_runtime.schema_migrations
WHERE target_kind = 'shared'
  AND target_id = 'platform'
  AND version = 51`)
}

func seedPre0051IndexInitializationFixtures(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedPre0051IndexInputBundle(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id, resource_project_id,
    projection_project_id, actor_id, principal_ref, capability_id,
    capability_version, input_bundle_id, request_digest, idempotency_scope,
    idempotency_key, state, desired_state, admitted_at, invocation_state
) VALUES
    (
        'migration-0051-recoverable', 1, 'migration-0051-recoverable-command',
        '1', 1, 1, '1', 'migration-0051-principal', 'index.ingest.v1',
        '1', 'migration-0051-bundle', decode(repeat('31', 32), 'hex'),
        'migration-0051', 'recoverable', 'PENDING', 'RUNNING',
        '2026-07-27T07:00:00Z', 'NOT_STARTED'
    ),
    (
        'migration-0051-unsafe-pre-authority', 1,
        'migration-0051-unsafe-pre-authority-command',
        '1', 1, 1, '1', 'migration-0051-principal', 'index.ingest.v1',
        '1', 'migration-0051-bundle', decode(repeat('32', 32), 'hex'),
        'migration-0051', 'unsafe-pre-authority', 'PENDING', 'RUNNING',
        '2026-07-27T07:01:00Z', 'NOT_STARTED'
    ),
    (
        'migration-0051-unsafe-post-authority', 1,
        'migration-0051-unsafe-post-authority-command',
        '1', 1, 1, '1', 'migration-0051-principal', 'index.ingest.v1',
        '1', 'migration-0051-bundle', decode(repeat('33', 32), 'hex'),
        'migration-0051', 'unsafe-post-authority', 'RUNNING', 'RUNNING',
        '2026-07-27T07:02:00Z', 'MAY_HAVE_STARTED'
    );

INSERT INTO elitea_runtime.index_ingest_jobs (
    execution_id, generation, capability_id, input_bundle_id,
    toolkit_configuration_entry_id, tool_parameters_entry_id,
    toolkit_id, index_name, initiator, index_meta_id,
    index_meta_correlation_id, index_generation
) VALUES
    (
        'migration-0051-recoverable', 1, 'index.ingest.v1',
        'migration-0051-bundle', 'toolkit-configuration', 'tool-parameters',
        51, 'recoverable', 'user', 'migration-0051-recoverable-meta',
        'migration-0051-recoverable-correlation', 1
    ),
    (
        'migration-0051-unsafe-pre-authority', 1, 'index.ingest.v1',
        'migration-0051-bundle', 'toolkit-configuration', 'tool-parameters',
        51, 'unsafe-pre-authority', 'user', NULL, NULL, 1
    ),
    (
        'migration-0051-unsafe-post-authority', 1, 'index.ingest.v1',
        'migration-0051-bundle', 'toolkit-configuration', 'tool-parameters',
        51, 'unsafe-post-authority', 'user',
        'migration-0051-unsafe-post-authority-meta',
        'migration-0051-unsafe-post-authority-correlation', 1
    );

INSERT INTO elitea_runtime.command_outbox (
    outbox_id, execution_id, generation, stream_name, resource_class,
    isolation_class, priority, deadline, limits_revision
) VALUES
    (
        'migration-0051-recoverable-outbox',
        'migration-0051-recoverable', 1, 'migration-0051-index-commands',
        'indexing', 'project', 1, '2035-01-01T00:00:00Z', 'migration-0051'
    ),
    (
        'migration-0051-unsafe-pre-authority-outbox',
        'migration-0051-unsafe-pre-authority', 1,
        'migration-0051-index-commands', 'indexing', 'project', 1,
        '2035-01-01T00:00:00Z', 'migration-0051'
    );

INSERT INTO elitea_runtime.command_outbox (
    outbox_id, execution_id, generation, stream_name, resource_class,
    isolation_class, priority, deadline, limits_revision,
    prepared_signed_envelope_bytes, prepared_signed_envelope_digest,
    prepared_signature_profile, prepared_key_id, prepared_at, published_at,
    published_envelope_digest, authority_granted_at
) VALUES (
    'migration-0051-unsafe-post-authority-outbox',
    'migration-0051-unsafe-post-authority', 1,
    'migration-0051-index-commands', 'indexing', 'project', 1,
    '2035-01-01T00:00:00Z', 'migration-0051',
    convert_to('migration-0051-signed-envelope', 'UTF8'),
    decode(repeat('34', 32), 'hex'), 1, 'migration-0051-key',
    '2026-07-27T08:00:00Z', '2026-07-27T08:00:01Z',
    decode(repeat('34', 32), 'hex'), '2026-07-27T08:00:02Z'
)`); err != nil {
		t.Fatalf("seed pre-0051 initialization fixtures: %v", err)
	}
}

func seedPre0051MissingOutboxFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedPre0051IndexInputBundle(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id, resource_project_id,
    projection_project_id, actor_id, principal_ref, capability_id,
    capability_version, input_bundle_id, request_digest, idempotency_scope,
    idempotency_key, state, desired_state, admitted_at, invocation_state
) VALUES (
    'migration-0051-missing-outbox', 1, 'migration-0051-missing-outbox-command',
    '1', 1, 1, '1', 'migration-0051-principal', 'index.ingest.v1',
    '1', 'migration-0051-bundle', decode(repeat('35', 32), 'hex'),
    'migration-0051', 'missing-outbox', 'PENDING', 'RUNNING',
    '2026-07-27T07:03:00Z', 'NOT_STARTED'
);

INSERT INTO elitea_runtime.index_ingest_jobs (
    execution_id, generation, capability_id, input_bundle_id,
    toolkit_configuration_entry_id, tool_parameters_entry_id,
    toolkit_id, index_name, initiator, index_meta_id,
    index_meta_correlation_id, index_generation
) VALUES (
    'migration-0051-missing-outbox', 1, 'index.ingest.v1',
    'migration-0051-bundle', 'toolkit-configuration', 'tool-parameters',
    51, 'missing-outbox', 'user', 'migration-0051-missing-outbox-meta',
    'migration-0051-missing-outbox-correlation', 1
)`); err != nil {
		t.Fatalf("seed pre-0051 missing-outbox fixture: %v", err)
	}
}

func seedPre0051IndexInputBundle(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.input_bundles (
    input_bundle_id, immutable_version, media_type, resource_project_id,
    manifest_digest, manifest_size, manifest_bytes, created_by
) VALUES (
    'migration-0051-bundle', '1', 'application/x-protobuf', 1,
    decode(repeat('11', 32), 'hex'), 2, decode('0001', 'hex'), 'migration-test'
);

INSERT INTO elitea_runtime.input_bundle_entries (
    input_bundle_id, entry_id, entry_version, semantic_role, media_type,
    content_digest, content_size, content_reference, classification,
    required_grant_audience, content_bytes
) VALUES
    (
        'migration-0051-bundle', 'toolkit-configuration', '1',
        'toolkit_configuration', 'application/json',
        decode(repeat('21', 32), 'hex'), 2, 'migration:toolkit',
        'confidential', 'index.ingest.v1', convert_to('{}', 'UTF8')
    ),
    (
        'migration-0051-bundle', 'tool-parameters', '1',
        'tool_parameters', 'application/json',
        decode(repeat('22', 32), 'hex'), 2, 'migration:parameters',
        'confidential', 'index.ingest.v1', convert_to('{}', 'UTF8')
    )`); err != nil {
		t.Fatalf("seed pre-0051 input bundle: %v", err)
	}
}
