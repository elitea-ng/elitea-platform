package migrations_test

// Shared migration 0115 — the two CHECK constraints that decide whether the
// kernel will hold a `toolkit.call_tool.v1` execution at all.
//
// # Why this runs against the real corpus and not a hand-made schema
//
// The property under test is a property of the MIGRATION. Both constraints
// enumerate capability and payload literals, and a row naming a capability
// neither arm lists is rejected with 23514 — not with a message that says
// "unknown capability", just a constraint violation from the storage layer,
// hundreds of lines away from the producer that wrote it. Hand-building the
// table would assert the test's own CREATE TABLE, which is the shape
// [[absence-reads-as-correctness]] warns about.
//
// The negative half matters as much as the positive one. A test that only
// inserted the new capability would pass against a constraint that had been
// widened to accept ANY string, so this also proves an invented capability is
// still refused.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestToolkitCallToolCapabilityIsAdmissibleAndAnInventedOneIsNot(t *testing.T) {
	pool := newMigratedPool(t)
	ctx := context.Background()
	projectID := seedToolRunProject(t, pool)
	bundleID := seedToolRunInputBundle(t, pool, projectID)

	if err := insertToolRunExecutionJob(
		ctx, pool, "execution-tool-run-1", "toolkit.call_tool.v1", projectID, bundleID,
	); err != nil {
		t.Fatalf("the kernel refused the tool-run capability its worker now serves: %v", err)
	}

	// The same row shape, one invented capability. The constraint must still
	// close: widening it to "any capability" would let a typo admit work no
	// worker will ever claim, and the execution would sit RUNNING forever.
	if err := insertToolRunExecutionJob(
		ctx, pool, "execution-tool-run-2", "toolkit.call_tool.v99", projectID, bundleID,
	); err == nil {
		t.Fatal("the capability payload constraint accepted an invented capability")
	}
}

func TestToolkitCallToolResultIsAnAdmissiblePayloadType(t *testing.T) {
	pool := newMigratedPool(t)
	ctx := context.Background()

	// The constraint is asserted directly. Building a whole admitted execution
	// plus a claim plus a fence to reach it through the projector would measure
	// the fixture, not the constraint this migration changed.
	var accepted bool
	if err := pool.QueryRow(ctx, `
SELECT 'TOOLKIT_CALL_TOOL_RESULT' IN (
    'CONFIGURATION_VALIDATION',
    'RUNTIME_FAILURE',
    'INDEX_INGEST_RESULT',
    'AGENT_EXECUTION_RESULT',
    'TOOLKIT_CALL_TOOL_RESULT'
)`).Scan(&accepted); err != nil || !accepted {
		t.Fatalf("payload-type probe failed: accepted=%v err=%v", accepted, err)
	}

	var definition string
	if err := pool.QueryRow(ctx, `
SELECT pg_get_constraintdef(c.oid)
FROM pg_constraint AS c
JOIN pg_class AS t ON t.oid = c.conrelid
JOIN pg_namespace AS n ON n.oid = t.relnamespace
WHERE n.nspname = 'elitea_runtime'
  AND t.relname = 'output_inbox'
  AND c.conname = 'output_inbox_payload_type'`).Scan(&definition); err != nil {
		t.Fatalf("read the payload-type constraint: %v", err)
	}
	if !containsToolRunPayloadType(definition) {
		t.Fatalf("output_inbox still refuses the tool-run payload type: %s", definition)
	}
}

func containsToolRunPayloadType(definition string) bool {
	return len(definition) > 0 &&
		indexOfSubstring(definition, "TOOLKIT_CALL_TOOL_RESULT") >= 0
}

func indexOfSubstring(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// seedToolRunProject creates the one project row the execution_jobs foreign
// keys require. Both project columns point at centry.project.
func seedToolRunProject(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var projectID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO centry.project (name, owner_id, create_success)
VALUES ('tool-run-migration-probe', 1, true)
RETURNING id`).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return projectID
}

func seedToolRunInputBundle(t *testing.T, pool *pgxpool.Pool, projectID int64) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const bundleID = "bundle-tool-run-probe"
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.input_bundles (
    input_bundle_id, immutable_version, media_type, resource_project_id,
    manifest_digest, manifest_size, manifest_bytes, created_by
) VALUES ($1, 'admission:'||$1, 'application/x-protobuf', $2,
    sha256('manifest'::bytea), octet_length('manifest'::bytea),
    'manifest'::bytea, 'user:1')`, bundleID, projectID); err != nil {
		t.Fatalf("seed input bundle: %v", err)
	}
	return bundleID
}

func insertToolRunExecutionJob(
	ctx context.Context,
	pool *pgxpool.Pool,
	executionID string,
	capabilityID string,
	projectID int64,
	bundleID string,
) error {
	_, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id, resource_project_id,
    projection_project_id, actor_id, principal_ref, capability_id,
    capability_version, input_bundle_id, request_digest,
    idempotency_scope, idempotency_key, state, desired_state, admitted_at
) VALUES (
    $1, 1, $1 || ':command', 'tenant-1', $2::int, $2::int, 'user:1', 'user:1',
    $3, '1', $4, sha256('request'::bytea), $5, $1,
    'PENDING', 'RUNNING', clock_timestamp()
)`,
		executionID,
		projectID,
		capabilityID,
		bundleID,
		fmt.Sprintf("tenant-1/%d/user:1", projectID),
	)
	return err
}
