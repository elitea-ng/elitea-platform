package projectprovisioning_test

// A delete refuses while the project has work in flight (#1211).
//
//   TestDeprovisionRefusesWhileProjectWorkIsActive
//       non-terminal work of ANY capability refuses the delete before anything
//       changes; no cleanup is recorded.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
)

func seedActiveJob(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID int64, executionID, capability string) {
	t.Helper()
	bundle := "bundle-" + executionID
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.input_bundles
    (input_bundle_id, immutable_version, resource_project_id, media_type,
     manifest_digest, manifest_size, manifest_bytes, created_by)
VALUES ($2, 'admission:' || $2, $1, 'application/x-protobuf',
        decode(repeat('61', 32), 'hex'), 1, decode('00', 'hex'), 'actor-1')`, projectID, bundle); err != nil {
		t.Fatalf("seed input bundle: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id, resource_project_id,
    projection_project_id, actor_id, principal_ref, capability_id,
    capability_version, input_bundle_id, request_digest, idempotency_scope,
    idempotency_key, state, desired_state
) VALUES (
    $2, 1, 'cmd-' || $2, ($1::integer)::text, $1::integer, $1::integer, '7', '7', $3, 'v1', $4,
    decode(repeat('61', 32), 'hex'), 'scope-' || $2, 'key-' || $2, 'RUNNING', 'RUNNING')`,
		projectID, executionID, capability, bundle); err != nil {
		t.Fatalf("seed running %s job: %v", capability, err)
	}
}

func TestDeprovisionRefusesWhileProjectWorkIsActive(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// A provisioner that can purge buckets, so the live bucket below is a real
	// resource the walk would remove (and a successful delete can finish).
	provisioner := newPurgingProvisioner(t, pool, newPurgeStore())
	created, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Busy Project", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	projectID := created.ProjectID
	seedActiveJob(ctx, t, pool, projectID, "exec-1211-agent", execution.AgentApplicationCapability)
	seedActiveJob(ctx, t, pool, projectID, "exec-1211-index", execution.IndexIngestCapability)
	if _, err := pool.Exec(ctx, `INSERT INTO elitea_storage.buckets (project_id, name, bucket_type) VALUES ($1, 'docs', 'local')`, projectID); err != nil {
		t.Fatal(err)
	}
	resourcesBefore := snapshotProject(ctx, t, pool, projectID)

	if _, err := provisioner.Deprovision(ctx, projectID); !errors.Is(err, projectprovisioning.ErrProjectWorkActive) {
		t.Fatalf("deprovision err = %v, want ErrProjectWorkActive", err)
	}
	// Nothing changed (the 409 is TRUE): the row and every resource the delete
	// would have removed are still there, and no cleanup was recorded.
	if rows := projectRowCount(ctx, t, pool, projectID); rows != 1 {
		t.Fatalf("a refused delete changed state: project rows=%d", rows)
	}
	assertProjectIntact(ctx, t, pool, projectID, resourcesBefore)
	if rows := journalRows(ctx, t, pool, projectID); rows != 0 {
		t.Fatalf("a refused delete wrote %d cleanup journal row(s)", rows)
	}

	// One terminal job is not enough: the other is still running.
	if _, err := pool.Exec(ctx,
		`UPDATE elitea_runtime.execution_jobs SET state = 'SUCCEEDED' WHERE execution_id = 'exec-1211-index'`); err != nil {
		t.Fatalf("settle job: %v", err)
	}
	if _, err := provisioner.Deprovision(ctx, projectID); !errors.Is(err, projectprovisioning.ErrProjectWorkActive) {
		t.Fatalf("deprovision with only the agent job active: %v, want ErrProjectWorkActive", err)
	}

	// Once all work is terminal the same delete goes through.
	if _, err := pool.Exec(ctx,
		`UPDATE elitea_runtime.execution_jobs SET state = 'FAILED' WHERE execution_id = 'exec-1211-agent'`); err != nil {
		t.Fatalf("settle job: %v", err)
	}
	if result, err := provisioner.Deprovision(ctx, projectID); err != nil {
		t.Fatalf("deprovision after the work settled: %v (steps=%+v)", err, result.RollbackSteps)
	}
	if rows := projectRowCount(ctx, t, pool, projectID); rows != 0 {
		t.Fatalf("after delete: project rows=%d, want 0", rows)
	}
}

// projectSnapshot is what a delete that changed nothing leaves: the rows the
// walk's steps remove, counted, so "nothing changed" is read from the database
// and not from a status.
type projectSnapshot struct {
	vaultKeys, vaultData, liveBuckets, systemUsers, projectRoles, userRoles int
}

func snapshotProject(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID int64) projectSnapshot {
	t.Helper()
	var snap projectSnapshot
	vault := fmt.Sprintf("project-%d", projectID)
	email := fmt.Sprintf("system_user_%d@centry.user", projectID)
	for dest, query := range map[*int]struct {
		sql  string
		args []any
	}{
		&snap.vaultKeys:    {`SELECT count(*) FROM centry.secrets_key WHERE id = $1`, []any{vault}},
		&snap.vaultData:    {`SELECT count(*) FROM centry.secrets_data WHERE id = $1`, []any{vault}},
		&snap.liveBuckets:  {`SELECT count(*) FROM elitea_storage.buckets WHERE project_id = $1 AND deleted_at IS NULL`, []any{projectID}},
		&snap.systemUsers:  {`SELECT count(*) FROM public.auth_core__user WHERE email = $1`, []any{email}},
		&snap.projectRoles: {`SELECT count(*) FROM public.auth_core__project_role WHERE project_id = $1`, []any{projectID}},
		&snap.userRoles:    {`SELECT count(*) FROM public.auth_core__project_user_role WHERE project_id = $1`, []any{projectID}},
	} {
		if err := pool.QueryRow(ctx, query.sql, query.args...).Scan(dest); err != nil {
			t.Fatalf("snapshot %q: %v", query.sql, err)
		}
	}
	return snap
}

func assertProjectIntact(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID int64, before projectSnapshot) {
	t.Helper()
	if before.vaultKeys == 0 || before.vaultData == 0 || before.liveBuckets == 0 || before.systemUsers == 0 ||
		before.projectRoles == 0 || before.userRoles == 0 {
		t.Fatalf("premise: the snapshot has an empty resource, so it proves nothing: %+v", before)
	}
	if after := snapshotProject(ctx, t, pool, projectID); after != before {
		t.Fatalf("the project's resources changed:\nbefore %+v\nafter  %+v", before, after)
	}
}
