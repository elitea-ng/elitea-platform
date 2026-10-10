package projectprovisioning_test

// Project delete drops the project's PgVector database and role (#1211).
//
// Every case reads pg_database / pg_roles back; none trusts a step status.
//
//   TestDeprovisionDropsTheVectorStoreDatabaseAndRole
//       an explicit delete leaves neither project_<id> nor project_<id>_user.
//   TestCreateFailureRollbackKeepsTheVectorStoreDatabaseAndRole
//       the rollback of a failed create does NOT drop them: only the explicit
//       delete is allowed to destroy a database.
//   TestDeprovisionToleratesAVectorStoreThatIsAlreadyGone
//       a missing database or role is a no-op, not an error.
//   TestDeprovisionRefusesWhileAnIndexRunIsActive
//       a non-terminal index run refuses the delete before anything changes.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

func newVectorProvisioner(t *testing.T, pool *pgxpool.Pool) *projectprovisioning.Provisioner {
	t.Helper()
	return projectprovisioning.New(
		pool,
		migrate.New(pool, platformmigrations.Files),
		nil,
		projectprovisioning.WithVectorStore(newProjectVectorStoreForTest(t, pool)),
		projectprovisioning.WithProjectVault(v2secrets.NewHandler(pool)),
	)
}

// vectorStoreResidue reports whether the project's database and role exist on
// the shared test server (they live beside the per-test database, not in it).
func vectorStoreResidue(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID int64) (database, role bool) {
	t.Helper()
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_database WHERE datname = $1),
       EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = $2)`,
		fmt.Sprintf("project_%d", projectID), fmt.Sprintf("project_%d_user", projectID),
	).Scan(&database, &role); err != nil {
		t.Fatalf("read pg_database / pg_roles: %v", err)
	}
	return database, role
}

func provisionIndexableProject(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string) (*projectprovisioning.Provisioner, int64) {
	t.Helper()
	seedPublicPgvectorBootstrap(ctx, t, pool)
	provisioner := newVectorProvisioner(t, pool)
	result, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: name, OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("provision: %v (steps=%+v)", err, result.Steps)
	}
	dropProvisionedVectorStore(t, result.ProjectID)
	database, role := vectorStoreResidue(ctx, t, pool, result.ProjectID)
	if !database || !role {
		t.Fatalf("premise: provisioning left database=%v role=%v, want both", database, role)
	}
	return provisioner, result.ProjectID
}

func TestDeprovisionDropsTheVectorStoreDatabaseAndRole(t *testing.T) {
	skipWithoutVectorExtension(t)
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	provisioner, projectID := provisionIndexableProject(ctx, t, pool, "Dropped Vector Store")

	result, err := provisioner.Deprovision(ctx, projectID)
	if err != nil {
		t.Fatalf("deprovision: %v (steps=%+v)", err, result.RollbackSteps)
	}
	assertStepCompensated(t, result.RollbackSteps, projectprovisioning.StepProjectPgvector)

	if database, role := vectorStoreResidue(ctx, t, pool, projectID); database || role {
		t.Fatalf("after delete: database exists=%v role exists=%v, want both dropped", database, role)
	}
}

func TestCreateFailureRollbackKeepsTheVectorStoreDatabaseAndRole(t *testing.T) {
	skipWithoutVectorExtension(t)
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	seedPublicPgvectorBootstrap(ctx, t, pool)
	provisioner := newVectorProvisioner(t, pool)
	// project_admin runs after project_pgvector and fails on an unknown address.
	result, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name:        "Rolled Back Keeps Databases",
		OwnerID:     1,
		AdminEmails: []string{"nobody@example.test"},
		AdminRoles:  []string{"admin"},
		Limits:      projectprovisioning.DefaultLimits(),
	})
	if !errors.Is(err, projectprovisioning.ErrUnknownAdminEmail) {
		t.Fatalf("err = %v, want ErrUnknownAdminEmail", err)
	}
	if result.ProjectID == 0 {
		t.Fatal("the failure reported no project id")
	}
	dropProvisionedVectorStore(t, result.ProjectID)
	assertStepSucceeded(t, result.Steps, projectprovisioning.StepProjectPgvector)
	assertStepCompensated(t, result.RollbackSteps, projectprovisioning.StepProjectPgvector)

	database, role := vectorStoreResidue(ctx, t, pool, result.ProjectID)
	if !database || !role {
		t.Fatalf("the create-failure rollback dropped data: database exists=%v role exists=%v, want both kept", database, role)
	}
}

func TestDeprovisionToleratesAVectorStoreThatIsAlreadyGone(t *testing.T) {
	skipWithoutVectorExtension(t)
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	provisioner, projectID := provisionIndexableProject(ctx, t, pool, "Already Dropped")
	// Drop the role and database out from under the project: a delete retried
	// after a partial one, or an operator cleanup.
	for _, statement := range []string{
		fmt.Sprintf(`DROP DATABASE %s WITH (FORCE)`, pgx.Identifier{fmt.Sprintf("project_%d", projectID)}.Sanitize()),
		fmt.Sprintf(`DROP ROLE %s`, pgx.Identifier{fmt.Sprintf("project_%d_user", projectID)}.Sanitize()),
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("pre-drop %q: %v", statement, err)
		}
	}

	result, err := provisioner.Deprovision(ctx, projectID)
	if err != nil {
		t.Fatalf("deprovision with a vector store already gone: %v (steps=%+v)", err, result.RollbackSteps)
	}
	assertStepCompensated(t, result.RollbackSteps, projectprovisioning.StepProjectPgvector)
}

func TestDeprovisionRefusesWhileAnIndexRunIsActive(t *testing.T) {
	skipWithoutVectorExtension(t)
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	provisioner, projectID := provisionIndexableProject(ctx, t, pool, "Busy Indexer")
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.input_bundles
    (input_bundle_id, immutable_version, resource_project_id, media_type,
     manifest_digest, manifest_size, manifest_bytes, created_by)
VALUES ('bundle-1211', 'admission:bundle-1211', $1, 'application/x-protobuf',
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
    'exec-1211', 1, 'cmd-1211', ($1::integer)::text, $1::integer, $1::integer, '7', '7', 'index.ingest.v1', 'v1', 'bundle-1211',
    decode(repeat('61', 32), 'hex'), 'scope-1211', 'key-1211', 'RUNNING', 'RUNNING')`, projectID); err != nil {
		t.Fatalf("seed running index job: %v", err)
	}

	if _, err := provisioner.Deprovision(ctx, projectID); !errors.Is(err, projectprovisioning.ErrIndexRunsActive) {
		t.Fatalf("deprovision err = %v, want ErrIndexRunsActive", err)
	}
	// Nothing changed: the project row and the vector store are still there.
	var projectRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM centry.project WHERE id = $1`, projectID).Scan(&projectRows); err != nil {
		t.Fatal(err)
	}
	if database, role := vectorStoreResidue(ctx, t, pool, projectID); projectRows != 1 || !database || !role {
		t.Fatalf("a refused delete changed state: project rows=%d database=%v role=%v", projectRows, database, role)
	}

	// Once the run is terminal the same delete goes through and drops both.
	if _, err := pool.Exec(ctx,
		`UPDATE elitea_runtime.execution_jobs SET state = 'SUCCEEDED' WHERE execution_id = 'exec-1211'`); err != nil {
		t.Fatalf("settle job: %v", err)
	}
	if result, err := provisioner.Deprovision(ctx, projectID); err != nil {
		t.Fatalf("deprovision after the run settled: %v (steps=%+v)", err, result.RollbackSteps)
	}
	if database, role := vectorStoreResidue(ctx, t, pool, projectID); database || role {
		t.Fatalf("after delete: database=%v role=%v, want both dropped", database, role)
	}
}
