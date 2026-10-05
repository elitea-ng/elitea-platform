package repos

// The two admission writers of execution_jobs.trigger_origin (shared 0140),
// against the migrated corpus. Requires ELITEA_TEST_DATABASE_URL.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

// The agent admission writes the origin its caller named, and `manual` when
// the caller named none (the chat composer).
func TestPostgresAgentAdmissionPersistsTheTriggerOrigin(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	policy := testAgentDispatchPolicy()
	// Four admissions stay outstanding; the default test limit is two.
	policy.MaxOutstanding = 8
	repository, err := NewAgentExecutionJobsRepository(pool, policy, 1)
	if err != nil {
		t.Fatal(err)
	}

	for index, origin := range []executiondomain.TriggerOrigin{
		"", executiondomain.TriggerOriginAPI, executiondomain.TriggerOriginSchedule,
		executiondomain.TriggerOriginWebhook,
	} {
		admission := postgresAgentCapacityAdmission(870 + index)
		admission.Record.Job.TriggerOrigin = origin
		outcome, err := repository.AdmitAgentExecution(ctx, admission)
		if err != nil || !outcome.Created {
			t.Fatalf("admit with origin %q: outcome=%+v err=%v", origin, outcome, err)
		}
		var stored string
		if err := pool.QueryRow(ctx,
			`SELECT trigger_origin FROM elitea_runtime.execution_jobs WHERE execution_id = $1`,
			outcome.ExecutionID).Scan(&stored); err != nil {
			t.Fatalf("read trigger_origin: %v", err)
		}
		if stored != origin.Stored() {
			t.Errorf("origin %q stored as %q, want %q", origin, stored, origin.Stored())
		}
	}
}

// Every index ingest is stamped `index` by the statement itself, whoever
// started it.
func TestPostgresIndexIngestJobIsStampedIndex(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.input_bundles
    (input_bundle_id, immutable_version, resource_project_id, media_type,
     manifest_digest, manifest_size, manifest_bytes, created_by)
VALUES ('bundle-index-origin', 'admission:bundle-index-origin', 1, 'application/x-protobuf',
        decode(repeat('61', 32), 'hex'), 1, decode('00', 'hex'), 'actor-1')`); err != nil {
		t.Fatalf("seed input bundle: %v", err)
	}
	digest := make([]byte, 32)
	executionID, err := sqlcgen.New(pool).InsertIndexIngestExecutionJob(ctx, sqlcgen.InsertIndexIngestExecutionJobParams{
		ExecutionID: "exec-index-origin", Generation: 1, CommandID: "cmd-index-origin",
		TenantID: "1", ResourceProjectID: 1, ProjectionProjectID: 1,
		ActorID: "7", PrincipalRef: "7", CapabilityVersion: "v1",
		InputBundleID: "bundle-index-origin", RequestDigest: digest,
		IdempotencyScope: "scope-index-origin", IdempotencyKey: "key-index-origin",
		State:      "PENDING",
		AdmittedAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
	})
	if err != nil {
		t.Fatalf("insert index ingest job: %v", err)
	}
	var stored string
	if err := pool.QueryRow(ctx,
		`SELECT trigger_origin FROM elitea_runtime.execution_jobs WHERE execution_id = $1`,
		executionID).Scan(&stored); err != nil {
		t.Fatalf("read trigger_origin: %v", err)
	}
	if stored != string(executiondomain.TriggerOriginIndex) {
		t.Fatalf("index ingest trigger_origin = %q, want index", stored)
	}
}
