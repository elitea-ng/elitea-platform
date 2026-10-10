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
//   TestDeprovisionRefusesWhileProjectWorkIsActive
//       non-terminal work of ANY capability refuses the delete before anything changes.
//   TestDeprovisionKeepsTheVectorStoreWhenTheRowDeleteFails
//       the drop runs only after the project row is proved gone; the failed
//       delete leaves a tombstone, admission is refused, and a retry completes.
//   TestAdmissionRacingTheFenceIsRefusedOrCounted
//       the FOR UPDATE fence versus a concurrent admission, both orders.
//   TestDeprovisionDropSurvivesACancelledRequest
//       the post-commit drop is detached from the request context.
//   TestDeprovisionReportsAVectorStoreItCannotReach / ...SkipsAProjectThatNeverHadOne
//       the missing-bootstrap rule.
//   TestDeprovisionReportsEveryCleanupFailure
//       a failed vector drop, a surviving bucket: both are named, neither hides the other.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/artifactbootstrap"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	vectorstoreapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/vectorstore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
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

// newVectorPurgingProvisioner is newVectorProvisioner plus an artifact bucket
// bootstrapper over an in-memory object store.
func newVectorPurgingProvisioner(t *testing.T, pool *pgxpool.Pool) *projectprovisioning.Provisioner {
	t.Helper()
	buckets, err := repos.NewArtifactBucketsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := repos.NewArtifactObjectsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	return projectprovisioning.New(
		pool,
		migrate.New(pool, platformmigrations.Files),
		nil,
		projectprovisioning.WithVectorStore(newProjectVectorStoreForTest(t, pool)),
		projectprovisioning.WithProjectVault(v2secrets.NewHandler(pool)),
		projectprovisioning.WithArtifactBuckets(
			artifactbootstrap.NewBootstrapper(bucketRepos{buckets, objects}, newPurgeStore())),
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
	skipWithoutVectorExtension(t)
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// A provisioner that can purge buckets, so the live bucket below is a real
	// resource the walk would remove (and a successful delete can finish).
	seedPublicPgvectorBootstrap(ctx, t, pool)
	provisioner := newVectorPurgingProvisioner(t, pool)
	created, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Busy Project", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	projectID := created.ProjectID
	dropProvisionedVectorStore(t, projectID)
	// An agent execution, not an index run: it uses the project's PgVector
	// database for its checkpoints all the same.
	seedActiveJob(ctx, t, pool, projectID, "exec-1211-agent", execution.AgentApplicationCapability)
	seedActiveJob(ctx, t, pool, projectID, "exec-1211-index", execution.IndexIngestCapability)
	if _, err := pool.Exec(ctx, `INSERT INTO elitea_storage.buckets (project_id, name, bucket_type) VALUES ($1, 'docs', 'local')`, projectID); err != nil {
		t.Fatal(err)
	}
	resourcesBefore := snapshotProject(ctx, t, pool, projectID)

	if _, err := provisioner.Deprovision(ctx, projectID); !errors.Is(err, projectprovisioning.ErrProjectWorkActive) {
		t.Fatalf("deprovision err = %v, want ErrProjectWorkActive", err)
	}
	// Nothing changed (the 409 is TRUE): the row, the vector store, and every
	// resource the walk would have removed are still there, and the project is
	// not tombstoned.
	var projectRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM centry.project WHERE id = $1`, projectID).Scan(&projectRows); err != nil {
		t.Fatal(err)
	}
	if database, role := vectorStoreResidue(ctx, t, pool, projectID); projectRows != 1 || !database || !role {
		t.Fatalf("a refused delete changed state: project rows=%d database=%v role=%v", projectRows, database, role)
	}
	assertProjectIntact(ctx, t, pool, projectID, resourcesBefore)
	if deleting := projectTombstoned(ctx, t, pool, projectID); deleting {
		t.Fatal("a refused delete left the project tombstoned")
	}

	// One terminal job is not enough: the other is still running.
	if _, err := pool.Exec(ctx,
		`UPDATE elitea_runtime.execution_jobs SET state = 'SUCCEEDED' WHERE execution_id = 'exec-1211-index'`); err != nil {
		t.Fatalf("settle job: %v", err)
	}
	if _, err := provisioner.Deprovision(ctx, projectID); !errors.Is(err, projectprovisioning.ErrProjectWorkActive) {
		t.Fatalf("deprovision with only the agent job active: %v, want ErrProjectWorkActive", err)
	}

	// Once all work is terminal the same delete goes through and drops both.
	if _, err := pool.Exec(ctx,
		`UPDATE elitea_runtime.execution_jobs SET state = 'FAILED' WHERE execution_id = 'exec-1211-agent'`); err != nil {
		t.Fatalf("settle job: %v", err)
	}
	if result, err := provisioner.Deprovision(ctx, projectID); err != nil {
		t.Fatalf("deprovision after the work settled: %v (steps=%+v)", err, result.RollbackSteps)
	}
	if database, role := vectorStoreResidue(ctx, t, pool, projectID); database || role {
		t.Fatalf("after delete: database=%v role=%v, want both dropped", database, role)
	}
}

// projectSnapshot is what a delete that changed nothing leaves: the rows the
// walk's steps remove, counted, so "nothing changed" is read from the database
// and not from a status.
type projectSnapshot struct {
	vaultKeys, vaultData, liveBuckets, systemUsers, projectRoles, userRoles, configurations int
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
		&snap.vaultKeys:      {`SELECT count(*) FROM centry.secrets_key WHERE id = $1`, []any{vault}},
		&snap.vaultData:      {`SELECT count(*) FROM centry.secrets_data WHERE id = $1`, []any{vault}},
		&snap.liveBuckets:    {`SELECT count(*) FROM elitea_storage.buckets WHERE project_id = $1 AND deleted_at IS NULL`, []any{projectID}},
		&snap.systemUsers:    {`SELECT count(*) FROM public.auth_core__user WHERE email = $1`, []any{email}},
		&snap.projectRoles:   {`SELECT count(*) FROM public.auth_core__project_role WHERE project_id = $1`, []any{projectID}},
		&snap.userRoles:      {`SELECT count(*) FROM public.auth_core__project_user_role WHERE project_id = $1`, []any{projectID}},
		&snap.configurations: {fmt.Sprintf(`SELECT count(*) FROM p_%d.configuration WHERE elitea_title = 'elitea-pgvector'`, projectID), nil},
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
		before.projectRoles == 0 || before.userRoles == 0 || before.configurations == 0 {
		t.Fatalf("premise: the snapshot has an empty resource, so it proves nothing: %+v", before)
	}
	if after := snapshotProject(ctx, t, pool, projectID); after != before {
		t.Fatalf("a refused delete changed the project's resources:\nbefore %+v\nafter  %+v", before, after)
	}
}

func projectTombstoned(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID int64) bool {
	t.Helper()
	var deleting bool
	if err := pool.QueryRow(ctx,
		`SELECT deleting_at IS NOT NULL FROM centry.project WHERE id = $1`, projectID).Scan(&deleting); err != nil {
		t.Fatalf("read the tombstone: %v", err)
	}
	return deleting
}

// admitJobs tries every admission INSERT the kernel has against a project:
// the index-ingest query, the agent-execution query, and the generic one the
// seed helper stands for. Each returns the error the database gave.
func admitJobs(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID int64, tag string) map[string]error {
	t.Helper()
	bundle := "bundle-" + tag
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.input_bundles
    (input_bundle_id, immutable_version, resource_project_id, media_type,
     manifest_digest, manifest_size, manifest_bytes, created_by)
VALUES ($2, 'admission:' || $2, $1, 'application/x-protobuf',
        decode(repeat('61', 32), 'hex'), 1, decode('00', 'hex'), 'actor-1')`, projectID, bundle); err != nil {
		t.Fatalf("seed input bundle: %v", err)
	}
	queries := sqlcgen.New(pool)
	digest := make([]byte, 32)
	admitted := pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	out := map[string]error{}
	_, out["index.ingest"] = queries.InsertIndexIngestExecutionJob(ctx, sqlcgen.InsertIndexIngestExecutionJobParams{
		ExecutionID: "exec-" + tag + "-index", Generation: 1, CommandID: "cmd-" + tag + "-index",
		TenantID: "1", ResourceProjectID: int32(projectID), ProjectionProjectID: int32(projectID),
		ActorID: "7", PrincipalRef: "7", CapabilityVersion: "v1",
		InputBundleID: bundle, RequestDigest: digest,
		IdempotencyScope: "scope-" + tag + "-index", IdempotencyKey: "key-" + tag + "-index",
		State: "PENDING", AdmittedAt: admitted,
	})
	_, out["agent"] = queries.InsertAgentExecutionJob(ctx, sqlcgen.InsertAgentExecutionJobParams{
		ExecutionID: "exec-" + tag + "-agent", Generation: 1, CommandID: "cmd-" + tag + "-agent",
		TenantID: "1", ResourceProjectID: int32(projectID), ProjectionProjectID: int32(projectID),
		ActorID: "7", PrincipalRef: "7", CapabilityID: execution.AgentApplicationCapability, CapabilityVersion: "v1",
		InputBundleID: bundle, RequestDigest: digest,
		IdempotencyScope: "scope-" + tag + "-agent", IdempotencyKey: "key-" + tag + "-agent",
		State: "PENDING", AdmittedAt: admitted,
	})
	_, out["generic"] = pool.Exec(ctx, `
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id, resource_project_id,
    projection_project_id, actor_id, principal_ref, capability_id,
    capability_version, input_bundle_id, request_digest, idempotency_scope,
    idempotency_key, state, desired_state
) VALUES (
    $2, 1, 'cmd-' || $2, ($1::integer)::text, $1::integer, $1::integer, '7', '7', $3, 'v1', $4,
    decode(repeat('61', 32), 'hex'), 'scope-' || $2, 'key-' || $2, 'PENDING', 'RUNNING')`,
		projectID, "exec-"+tag+"-generic", execution.IndexIngestCapability, bundle)
	return out
}

func isProjectDeletingRefusal(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "55000" && strings.Contains(pgErr.Message, "is being deleted")
}

// A failed step leaves the project tombstoned, admission is then refused on
// every path, and a retried delete skips the fence and completes.
func TestDeprovisionTombstonesTheProjectAndARetryCompletes(t *testing.T) {
	skipWithoutVectorExtension(t)
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	provisioner, projectID := provisionIndexableProject(ctx, t, pool, "Tombstoned Then Retried")
	// Admission works before the delete starts.
	for path, err := range admitJobs(ctx, t, pool, projectID, "before") {
		if err != nil {
			t.Fatalf("premise: %s admission failed before the delete: %v", path, err)
		}
	}
	for _, statement := range []string{
		`UPDATE elitea_runtime.execution_jobs SET state = 'SUCCEEDED' WHERE resource_project_id = ` + fmt.Sprint(projectID),
		`CREATE FUNCTION refuse_project_delete() RETURNS trigger LANGUAGE plpgsql AS
		 $$ BEGIN RAISE EXCEPTION 'project delete refused by test'; END $$`,
		`CREATE TRIGGER refuse_project_delete BEFORE DELETE ON centry.project
		 FOR EACH ROW EXECUTE FUNCTION refuse_project_delete()`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}

	// The row delete fails after the fence passed: the project is tombstoned.
	if _, err := provisioner.Deprovision(ctx, projectID); !errors.Is(err, projectprovisioning.ErrProjectNotRemoved) ||
		errors.Is(err, projectprovisioning.ErrProjectWorkActive) {
		t.Fatalf("deprovision err = %v, want ErrProjectNotRemoved and not ErrProjectWorkActive", err)
	}
	if !projectTombstoned(ctx, t, pool, projectID) {
		t.Fatal("a delete that failed after the fence did not leave the tombstone")
	}
	for path, err := range admitJobs(ctx, t, pool, projectID, "after") {
		if !isProjectDeletingRefusal(err) {
			t.Errorf("%s admission for a tombstoned project: %v, want the 55000 'is being deleted' refusal", path, err)
		}
	}
	// The vector store was not touched: the row is still there.
	if database, role := vectorStoreResidue(ctx, t, pool, projectID); !database || !role {
		t.Fatalf("a failed row delete lost the vectors: database=%v role=%v", database, role)
	}

	// The retry finds the tombstone, skips the fence, and finishes the walk.
	if _, err := pool.Exec(ctx, `DROP TRIGGER refuse_project_delete ON centry.project`); err != nil {
		t.Fatal(err)
	}
	result, err := provisioner.Deprovision(ctx, projectID)
	if err != nil {
		t.Fatalf("retried deprovision: %v (steps=%+v)", err, result.RollbackSteps)
	}
	var projectRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM centry.project WHERE id = $1`, projectID).Scan(&projectRows); err != nil {
		t.Fatal(err)
	}
	if database, role := vectorStoreResidue(ctx, t, pool, projectID); projectRows != 0 || database || role {
		t.Fatalf("after the retry: project rows=%d database=%v role=%v, want all gone", projectRows, database, role)
	}
}

// The FOR UPDATE fence against a concurrent admission, in both orders.
func TestAdmissionRacingTheFenceIsRefusedOrCounted(t *testing.T) {
	skipWithoutVectorExtension(t)
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	t.Run("admission waits for the fence and is refused", func(t *testing.T) {
		_, projectID := provisionIndexableProject(ctx, t, pool, "Admission Loses")
		fence, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = fence.Rollback(context.WithoutCancel(ctx)) }()
		if _, err := fence.Exec(ctx, `SELECT 1 FROM centry.project WHERE id = $1 FOR UPDATE`, projectID); err != nil {
			t.Fatal(err)
		}

		done := make(chan map[string]error, 1)
		go func() { done <- admitJobs(ctx, t, pool, projectID, "racing") }()
		select {
		case <-done:
			t.Fatal("admission did not wait for the fence's row lock")
		case <-time.After(500 * time.Millisecond):
		}
		if _, err := fence.Exec(ctx, `UPDATE centry.project SET deleting_at = now() WHERE id = $1`, projectID); err != nil {
			t.Fatal(err)
		}
		if err := fence.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		for path, err := range <-done {
			if !isProjectDeletingRefusal(err) {
				t.Errorf("%s admission that raced the fence: %v, want the 'is being deleted' refusal", path, err)
			}
		}
	})

	t.Run("the fence waits for an admission and counts it", func(t *testing.T) {
		provisioner, projectID := provisionIndexableProject(ctx, t, pool, "Fence Loses")
		admission, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = admission.Rollback(context.WithoutCancel(ctx)) }()
		bundle := "bundle-fence-loses"
		if _, err := admission.Exec(ctx, `
INSERT INTO elitea_runtime.input_bundles
    (input_bundle_id, immutable_version, resource_project_id, media_type,
     manifest_digest, manifest_size, manifest_bytes, created_by)
VALUES ($2, 'admission:' || $2, $1, 'application/x-protobuf',
        decode(repeat('61', 32), 'hex'), 1, decode('00', 'hex'), 'actor-1')`, projectID, bundle); err != nil {
			t.Fatal(err)
		}
		if _, err := admission.Exec(ctx, `
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id, resource_project_id,
    projection_project_id, actor_id, principal_ref, capability_id,
    capability_version, input_bundle_id, request_digest, idempotency_scope,
    idempotency_key, state, desired_state
) VALUES (
    'exec-fence-loses', 1, 'cmd-fence-loses', ($1::integer)::text, $1::integer, $1::integer, '7', '7', $2, 'v1', $3,
    decode(repeat('61', 32), 'hex'), 'scope-fence-loses', 'key-fence-loses', 'PENDING', 'RUNNING')`,
			projectID, execution.IndexIngestCapability, bundle); err != nil {
			t.Fatal(err)
		}

		done := make(chan error, 1)
		go func() {
			_, err := provisioner.Deprovision(ctx, projectID)
			done <- err
		}()
		select {
		case err := <-done:
			t.Fatalf("the fence did not wait for the uncommitted admission: %v", err)
		case <-time.After(500 * time.Millisecond):
		}
		if err := admission.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, projectprovisioning.ErrProjectWorkActive) {
			t.Fatalf("deprovision err = %v, want ErrProjectWorkActive", err)
		}
		if projectTombstoned(ctx, t, pool, projectID) {
			t.Fatal("the refused fence left the project tombstoned")
		}
	})
}

// cancelDuringDropStore cancels the REQUEST context from inside the drop and
// then runs the real drop on the context it was given.
type cancelDuringDropStore struct {
	projectprovisioning.ProjectVectorStore
	cancelRequest context.CancelFunc
	sawDeadline   bool
	ctxErrAfter   error
}

func (s *cancelDuringDropStore) DropProjectVectorStore(ctx context.Context, projectID int64, hadStore bool) (string, error) {
	deadline, ok := ctx.Deadline()
	s.sawDeadline = ok && time.Until(deadline) <= 2*time.Minute
	s.cancelRequest()
	s.ctxErrAfter = ctx.Err()
	return s.ProjectVectorStore.DropProjectVectorStore(ctx, projectID, hadStore)
}

func TestDeprovisionDropSurvivesACancelledRequest(t *testing.T) {
	skipWithoutVectorExtension(t)
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	seedPublicPgvectorBootstrap(ctx, t, pool)
	store := &cancelDuringDropStore{ProjectVectorStore: newProjectVectorStoreForTest(t, pool)}
	provisioner := projectprovisioning.New(
		pool,
		migrate.New(pool, platformmigrations.Files),
		nil,
		projectprovisioning.WithVectorStore(store),
		projectprovisioning.WithProjectVault(v2secrets.NewHandler(pool)),
	)
	created, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Hung Up Client", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	projectID := created.ProjectID
	dropProvisionedVectorStore(t, projectID)

	requestCtx, hangUp := context.WithCancel(ctx)
	store.cancelRequest = hangUp
	result, err := provisioner.Deprovision(requestCtx, projectID)
	if err != nil {
		t.Fatalf("deprovision: %v (steps=%+v)", err, result.RollbackSteps)
	}
	if requestCtx.Err() == nil {
		t.Fatal("premise: the request context was not cancelled during the drop")
	}
	if store.ctxErrAfter != nil {
		t.Fatalf("the drop's context was cancelled with the request: %v", store.ctxErrAfter)
	}
	if !store.sawDeadline {
		t.Fatal("the drop context has no bounded deadline of about 2 minutes")
	}
	if database, role := vectorStoreResidue(ctx, t, pool, projectID); database || role {
		t.Fatalf("the cancelled request left the drop half done: database=%v role=%v", database, role)
	}
}

// A project that HAD a vector store cannot be dropped once the bootstrap is
// gone: that is a failure naming the database, not a quiet success.
func TestDeprovisionReportsAVectorStoreItCannotReach(t *testing.T) {
	skipWithoutVectorExtension(t)
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	provisioner, projectID := provisionIndexableProject(ctx, t, pool, "Bootstrap Removed")
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		`DELETE FROM %s.configuration WHERE elitea_title = $1`,
		pgx.Identifier{fmt.Sprintf("p_%d", referenceProjectID)}.Sanitize()),
		vectorstoreapp.DefaultProjectPgvectorTitle); err != nil {
		t.Fatalf("remove the public bootstrap: %v", err)
	}

	result, err := provisioner.Deprovision(ctx, projectID)
	if !errors.Is(err, projectprovisioning.ErrVectorStoreNotDropped) {
		t.Fatalf("err = %v, want ErrVectorStoreNotDropped", err)
	}
	want := fmt.Sprintf("project_%d", projectID)
	if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "PgVector bootstrap not configured") {
		t.Errorf("err = %q, want it to name %s and the missing bootstrap", err, want)
	}
	if result.VectorDatabase != want {
		t.Errorf("VectorDatabase = %q, want %q", result.VectorDatabase, want)
	}
	if !stepFailed(result.RollbackSteps, projectprovisioning.StepProjectPgvectorDrop) {
		t.Errorf("the drop step is not failed: %+v", result.RollbackSteps)
	}
	if database, _ := vectorStoreResidue(ctx, t, pool, projectID); !database {
		t.Fatal("premise: the database should still exist, it was unreachable")
	}
}

// A project that never had a vector store reports the drop as skipped.
func TestDeprovisionSkipsAProjectThatNeverHadAVectorStore(t *testing.T) {
	skipWithoutVectorExtension(t)
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// No public bootstrap is seeded, so provisioning creates no vector store.
	provisioner := newVectorProvisioner(t, pool)
	created, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Never Indexed", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	result, err := provisioner.Deprovision(ctx, created.ProjectID)
	if err != nil {
		t.Fatalf("deprovision: %v (steps=%+v)", err, result.RollbackSteps)
	}
	for _, status := range result.RollbackSteps {
		if status.Step != projectprovisioning.StepProjectPgvectorDrop {
			continue
		}
		if status.OK == nil || !*status.OK || status.Msg != "skipped: no vector store" {
			t.Fatalf("drop step = %+v, want ok with 'skipped: no vector store'", status)
		}
		return
	}
	t.Fatalf("no %s step was reported: %+v", projectprovisioning.StepProjectPgvectorDrop, result.RollbackSteps)
}

func TestDeprovisionKeepsTheVectorStoreWhenTheRowDeleteFails(t *testing.T) {
	skipWithoutVectorExtension(t)
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	provisioner, projectID := provisionIndexableProject(ctx, t, pool, "Row Delete Fails")
	if _, err := pool.Exec(ctx, `
CREATE FUNCTION refuse_project_delete() RETURNS trigger LANGUAGE plpgsql AS
$$ BEGIN RAISE EXCEPTION 'project delete refused by test'; END $$;
CREATE TRIGGER refuse_project_delete BEFORE DELETE ON centry.project
    FOR EACH ROW EXECUTE FUNCTION refuse_project_delete()`); err != nil {
		t.Fatalf("install trigger: %v", err)
	}

	if _, err := provisioner.Deprovision(ctx, projectID); !errors.Is(err, projectprovisioning.ErrProjectNotRemoved) {
		t.Fatalf("deprovision err = %v, want ErrProjectNotRemoved", err)
	}
	var projectRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM centry.project WHERE id = $1`, projectID).Scan(&projectRows); err != nil {
		t.Fatal(err)
	}
	database, role := vectorStoreResidue(ctx, t, pool, projectID)
	if projectRows != 1 || !database || !role {
		t.Fatalf("the surviving project lost its vectors: rows=%d database=%v role=%v", projectRows, database, role)
	}
}

// failingDropStore is the real store with a drop that always fails.
type failingDropStore struct {
	projectprovisioning.ProjectVectorStore
}

func (failingDropStore) DropProjectVectorStore(_ context.Context, projectID int64, _ bool) (string, error) {
	return fmt.Sprintf("project_%d", projectID), errors.New("pg unreachable")
}

func TestDeprovisionReportsEveryCleanupFailure(t *testing.T) {
	skipWithoutVectorExtension(t)
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	seedPublicPgvectorBootstrap(ctx, t, pool)
	provisioner := projectprovisioning.New(
		pool,
		migrate.New(pool, platformmigrations.Files),
		nil,
		projectprovisioning.WithVectorStore(failingDropStore{newProjectVectorStoreForTest(t, pool)}),
		projectprovisioning.WithProjectVault(v2secrets.NewHandler(pool)),
	)
	created, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Two Failures", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	projectID := created.ProjectID
	dropProvisionedVectorStore(t, projectID)
	// A live bucket and no object store: the artifact purge cannot run.
	if _, err := pool.Exec(ctx, `INSERT INTO elitea_storage.buckets (project_id, name, bucket_type) VALUES ($1, 'docs', 'local')`, projectID); err != nil {
		t.Fatal(err)
	}

	result, err := provisioner.Deprovision(ctx, projectID)
	if !errors.Is(err, projectprovisioning.ErrArtifactsNotRemoved) || !errors.Is(err, projectprovisioning.ErrVectorStoreNotDropped) {
		t.Fatalf("err = %v, want both ErrArtifactsNotRemoved and ErrVectorStoreNotDropped", err)
	}
	if result.VectorDatabase != fmt.Sprintf("project_%d", projectID) {
		t.Errorf("VectorDatabase = %q", result.VectorDatabase)
	}
	if !stepFailed(result.RollbackSteps, projectprovisioning.StepArtifactBuckets) ||
		!stepFailed(result.RollbackSteps, projectprovisioning.StepProjectPgvectorDrop) {
		t.Errorf("the steps do not list both failures: %+v", result.RollbackSteps)
	}
}
