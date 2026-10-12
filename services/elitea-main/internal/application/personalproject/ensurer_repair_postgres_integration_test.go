package personalproject_test

// The personal-project repair over the row-first project delete (#1211).
//
//   TestEnsureRepairSkipsTheActiveWorkCountForAnUnfinishedProject
//       the repair of a project whose creation never finished is not refused by
//       a stray job row, while an explicit delete of it is.
//   TestEnsureRepairContinuesWhenAConcurrentDeleteRemovedTheRow
//       a concurrent delete that won the row lock answers the loser 404; the
//       row is gone, which is all the repair needs.

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/personalproject"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

func seedStrayJob(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID int64) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.input_bundles
    (input_bundle_id, immutable_version, resource_project_id, media_type,
     manifest_digest, manifest_size, manifest_bytes, created_by)
VALUES ('bundle-stray', 'admission:stray', $1, 'application/x-protobuf',
        decode(repeat('61', 32), 'hex'), 1, decode('00', 'hex'), 'actor-1')`, projectID); err != nil {
		t.Fatalf("seed input bundle: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id, resource_project_id,
    projection_project_id, actor_id, principal_ref, capability_id,
    capability_version, input_bundle_id, request_digest, idempotency_scope,
    idempotency_key, state, desired_state
) VALUES (
    'exec-stray', 1, 'cmd-stray', ($1::integer)::text, $1::integer, $1::integer, '7', '7', $2, 'v1',
    'bundle-stray', decode(repeat('61', 32), 'hex'), 'scope-stray', 'key-stray', 'RUNNING', 'RUNNING')`,
		projectID, execution.IndexIngestCapability); err != nil {
		t.Fatalf("seed a stray running job: %v", err)
	}
}

func TestEnsureRepairSkipsTheActiveWorkCountForAnUnfinishedProject(t *testing.T) {
	ctx := context.Background()
	pool := newPersonalProjectPool(t)
	provisioner := projectprovisioning.New(
		pool, migrate.New(pool, platformmigrations.Files), nil,
		projectprovisioning.WithProjectVault(v2secrets.NewHandler(pool)),
	)
	ensurer, err := personalproject.NewEnsurer(pool, provisioner)
	if err != nil {
		t.Fatal(err)
	}
	userID := seedUser(t, pool, "stray-job@autotest.local", "Stray Job")
	strandedID := seedUnfinishedProject(ctx, t, pool, userID)
	seedStrayJob(ctx, t, pool, strandedID)

	// Premise: an explicit delete of this project IS refused by the count.
	if _, err := provisioner.Deprovision(ctx, strandedID); !errors.Is(err, projectprovisioning.ErrProjectWorkActive) {
		t.Fatalf("premise: Deprovision err = %v, want ErrProjectWorkActive", err)
	}

	// The repair is not. It removes the unfinished project and recreates.
	repaired, err := ensurer.Ensure(ctx, userID)
	if err != nil {
		t.Fatalf("Ensure over an unfinished project with a stray job: %v", err)
	}
	if repaired == 0 || repaired == strandedID {
		t.Fatalf("Ensure returned %d for stranded project %d", repaired, strandedID)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM centry.project WHERE id = $1`, strandedID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("the unfinished project survived the repair: %d, %v", left, err)
	}
}

// concurrentDeleteProvisioner stands for a delete that lost the race: another
// delete (a second login, an operator) removed the row first, so this one finds
// no row (ErrProjectNotFound). The row really is gone.
type concurrentDeleteProvisioner struct {
	*projectprovisioning.Provisioner
}

func (p concurrentDeleteProvisioner) Deprovision(ctx context.Context, projectID int64, options ...projectprovisioning.DeprovisionOption) (projectprovisioning.Result, error) {
	if _, err := p.Provisioner.Deprovision(ctx, projectID, options...); err != nil {
		return projectprovisioning.Result{}, err
	}
	return projectprovisioning.Result{}, projectprovisioning.ErrProjectNotFound
}

func TestEnsureRepairContinuesWhenAConcurrentDeleteRemovedTheRow(t *testing.T) {
	ctx := context.Background()
	pool := newPersonalProjectPool(t)
	provisioner := projectprovisioning.New(
		pool, migrate.New(pool, platformmigrations.Files), nil,
		projectprovisioning.WithProjectVault(v2secrets.NewHandler(pool)),
	)
	ensurer, err := personalproject.NewEnsurer(pool, concurrentDeleteProvisioner{provisioner})
	if err != nil {
		t.Fatal(err)
	}
	userID := seedUser(t, pool, "lost-the-race@autotest.local", "Lost The Race")
	strandedID := seedUnfinishedProject(ctx, t, pool, userID)

	repaired, err := ensurer.Ensure(ctx, userID)
	if err != nil {
		t.Fatalf("Ensure after a concurrent delete removed the row: %v", err)
	}
	if repaired == 0 || repaired == strandedID {
		t.Fatalf("Ensure returned %d for stranded project %d", repaired, strandedID)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM centry.project WHERE id = $1`, strandedID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("the unfinished project survived: %d, %v", left, err)
	}
}
