package projectprovisioning_test

// Review round 3 of the project delete (#1211).
//
//   TestRetryAfterTheConfigRowIsGoneReportsTheLeak / ...RecordsNoStoreForAProjectThatNeverHadOne
//       the fence persists whether the project had a vector store on the
//       tombstone, and a retry reads it instead of re-deriving it.
//   TestConcurrentDeletesOfOneProjectAreSerialized / ...RefusedWhileAnotherSessionHoldsTheLock
//       the per-project delete lock.
//   TestProjectionJobsFenceTheDelete
//       a job that PROJECTS into the project counts like one that uses it.
//   TestSkipActiveWorkCheckStillTombstones
//       the ensurer's repair option skips the count and nothing else.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	vectorstoreapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/vectorstore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

const refuseProjectDeleteSQL = `
CREATE FUNCTION refuse_project_delete() RETURNS trigger LANGUAGE plpgsql AS
$$ BEGIN RAISE EXCEPTION 'project delete refused by test'; END $$;
CREATE TRIGGER refuse_project_delete BEFORE DELETE ON centry.project
    FOR EACH ROW EXECUTE FUNCTION refuse_project_delete()`

func readDeletionState(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID int64) map[string]any {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(ctx,
		`SELECT deleting_state FROM centry.project WHERE id = $1`, projectID).Scan(&raw); err != nil {
		t.Fatalf("read deleting_state: %v", err)
	}
	if raw == nil {
		t.Fatal("the tombstone carries no deleting_state")
	}
	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("deleting_state %q: %v", raw, err)
	}
	return state
}

// THE REGRESSION TEST FOR deprovision.go:274. The first attempt removes the
// configuration row (the step walk) and then fails to remove the project row.
// The retry used to ask "does the project have a vector store?" of a
// configuration that was already gone, hear "no", and report the drop as
// skipped while the database was still there.
func TestRetryAfterTheConfigRowIsGoneReportsTheLeak(t *testing.T) {
	skipWithoutVectorExtension(t)
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	provisioner, projectID := provisionIndexableProject(ctx, t, pool, "Retry After Config Gone")
	if _, err := pool.Exec(ctx, refuseProjectDeleteSQL); err != nil {
		t.Fatalf("install trigger: %v", err)
	}
	if _, err := provisioner.Deprovision(ctx, projectID); !errors.Is(err, projectprovisioning.ErrProjectNotRemoved) {
		t.Fatalf("first deprovision err = %v, want ErrProjectNotRemoved", err)
	}

	// The fence recorded the state, with the tombstone.
	state := readDeletionState(ctx, t, pool, projectID)
	if state["had_vector_store"] != true || state["vector_database"] != fmt.Sprintf("project_%d", projectID) {
		t.Fatalf("deleting_state = %v, want had_vector_store=true and the database name", state)
	}
	// Premise: the first attempt really did remove the evidence a re-derivation
	// would read, and the bootstrap is gone, so the drop cannot run.
	var configRows int
	if err := pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT count(*) FROM p_%d.configuration WHERE project_id = $1::integer AND section = 'vectorstorage'`, projectID),
		projectID).Scan(&configRows); err != nil || configRows != 0 {
		t.Fatalf("premise: the walk left %d vector-store configuration rows (err %v)", configRows, err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		`DELETE FROM %s.configuration WHERE elitea_title = $1`,
		pgx.Identifier{fmt.Sprintf("p_%d", referenceProjectID)}.Sanitize()),
		vectorstoreapp.DefaultProjectPgvectorTitle); err != nil {
		t.Fatalf("remove the public bootstrap: %v", err)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER refuse_project_delete ON centry.project`); err != nil {
		t.Fatal(err)
	}

	result, err := provisioner.Deprovision(ctx, projectID)
	if !errors.Is(err, projectprovisioning.ErrVectorStoreNotDropped) {
		t.Fatalf("retry err = %v, want ErrVectorStoreNotDropped (the leak), not a skip", err)
	}
	if !stepFailed(result.RollbackSteps, projectprovisioning.StepProjectPgvectorDrop) {
		t.Errorf("the drop step is not failed: %+v", result.RollbackSteps)
	}
	if want := fmt.Sprintf("project_%d", projectID); result.VectorDatabase != want {
		t.Errorf("VectorDatabase = %q, want %q", result.VectorDatabase, want)
	}
	if database, _ := vectorStoreResidue(ctx, t, pool, projectID); !database {
		t.Fatal("premise: the database should still exist, it was unreachable")
	}
}

// The other half: a project that never had a store records that, and its retry
// is a skip.
func TestFenceRecordsNoStoreForAProjectThatNeverHadOne(t *testing.T) {
	skipWithoutVectorExtension(t)
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	provisioner := newVectorProvisioner(t, pool)
	created, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Never Had One", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, refuseProjectDeleteSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := provisioner.Deprovision(ctx, created.ProjectID); !errors.Is(err, projectprovisioning.ErrProjectNotRemoved) {
		t.Fatalf("first deprovision err = %v, want ErrProjectNotRemoved", err)
	}
	if state := readDeletionState(ctx, t, pool, created.ProjectID); state["had_vector_store"] != false {
		t.Fatalf("deleting_state = %v, want had_vector_store=false", state)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER refuse_project_delete ON centry.project`); err != nil {
		t.Fatal(err)
	}
	result, err := provisioner.Deprovision(ctx, created.ProjectID)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	for _, status := range result.RollbackSteps {
		if status.Step == projectprovisioning.StepProjectPgvectorDrop && status.Msg != "skipped: no vector store" {
			t.Fatalf("drop step = %+v, want the skip", status)
		}
	}
}

// blockingDropStore parks the PgVector drop until released, so a delete can be
// held part way through while a second one arrives.
type blockingDropStore struct {
	projectprovisioning.ProjectVectorStore
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingDropStore) DropProjectVectorStore(ctx context.Context, projectID int64, hadStore bool) (string, error) {
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.release:
	case <-ctx.Done():
	}
	return s.ProjectVectorStore.DropProjectVectorStore(ctx, projectID, hadStore)
}

func TestConcurrentDeletesOfOneProjectAreSerialized(t *testing.T) {
	skipWithoutVectorExtension(t)
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	seedPublicPgvectorBootstrap(ctx, t, pool)
	store := &blockingDropStore{
		ProjectVectorStore: newProjectVectorStoreForTest(t, pool),
		entered:            make(chan struct{}),
		release:            make(chan struct{}),
	}
	provisioner := projectprovisioning.New(
		pool, migrate.New(pool, platformmigrations.Files), nil,
		projectprovisioning.WithVectorStore(store),
		projectprovisioning.WithProjectVault(v2secrets.NewHandler(pool)),
	)
	created, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Two Deletes", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	projectID := created.ProjectID
	dropProvisionedVectorStore(t, projectID)

	first := make(chan error, 1)
	go func() {
		_, err := provisioner.Deprovision(ctx, projectID)
		first <- err
	}()
	select {
	case <-store.entered:
	case <-time.After(60 * time.Second):
		t.Fatal("the first delete never reached the drop")
	}

	// The first delete is mid-flight and holds the lock: the second is turned
	// away at once, and does not wait.
	started := time.Now()
	_, err = provisioner.Deprovision(ctx, projectID)
	if !errors.Is(err, projectprovisioning.ErrProjectDeletionInProgress) {
		t.Fatalf("second delete err = %v, want ErrProjectDeletionInProgress", err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("the second delete waited %v instead of being refused", elapsed)
	}

	close(store.release)
	if err := <-first; err != nil {
		t.Fatalf("first delete: %v", err)
	}
	// The lock is released with the delete: a later call is an ordinary
	// not-found, not another "in progress".
	if _, err := provisioner.Deprovision(ctx, projectID); !errors.Is(err, projectprovisioning.ErrProjectNotFound) {
		t.Fatalf("delete after completion err = %v, want ErrProjectNotFound", err)
	}
}

// A delete held by ANOTHER SESSION (another replica) is refused before it
// changes anything, and its refusal leaves no tombstone.
func TestDeleteRefusedWhileAnotherSessionHoldsTheLock(t *testing.T) {
	skipWithoutVectorExtension(t)
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	provisioner, projectID := provisionIndexableProject(ctx, t, pool, "Held Elsewhere")
	holder, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	// "PDEL", the delete lock's namespace.
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock($1, $2)`, int32(0x5044454c), int32(projectID)); err != nil {
		t.Fatal(err)
	}

	if _, err := provisioner.Deprovision(ctx, projectID); !errors.Is(err, projectprovisioning.ErrProjectDeletionInProgress) {
		t.Fatalf("err = %v, want ErrProjectDeletionInProgress", err)
	}
	if projectTombstoned(ctx, t, pool, projectID) {
		t.Fatal("a refused delete tombstoned the project")
	}
	if database, role := vectorStoreResidue(ctx, t, pool, projectID); !database || !role {
		t.Fatalf("a refused delete touched the vector store: database=%v role=%v", database, role)
	}

	if _, err := holder.Exec(ctx, `SELECT pg_advisory_unlock($1, $2)`, int32(0x5044454c), int32(projectID)); err != nil {
		t.Fatal(err)
	}
	if _, err := provisioner.Deprovision(ctx, projectID); err != nil {
		t.Fatalf("delete after the lock was released: %v", err)
	}
}

// THE REGRESSION TEST FOR deprovision.go:736. The job below uses ANOTHER
// project's resources and projects its results into this one; the admission
// trigger guards that side too, so the fence has to count it.
func TestProjectionJobsFenceTheDelete(t *testing.T) {
	skipWithoutVectorExtension(t)
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	provisioner, projectID := provisionIndexableProject(ctx, t, pool, "Projected Into")
	const bundle = "bundle-projection"
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.input_bundles
    (input_bundle_id, immutable_version, resource_project_id, media_type,
     manifest_digest, manifest_size, manifest_bytes, created_by)
VALUES ($2, 'admission:' || $2, $1, 'application/x-protobuf',
        decode(repeat('61', 32), 'hex'), 1, decode('00', 'hex'), 'actor-1')`, referenceProjectID, bundle); err != nil {
		t.Fatalf("seed input bundle: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id, resource_project_id,
    projection_project_id, actor_id, principal_ref, capability_id,
    capability_version, input_bundle_id, request_digest, idempotency_scope,
    idempotency_key, state, desired_state
) VALUES (
    'exec-projection', 1, 'cmd-projection', '1', $1::integer, $2::integer, '7', '7',
    $4, 'v1', $3, decode(repeat('61', 32), 'hex'), 'scope-projection',
    'key-projection', 'RUNNING', 'RUNNING')`, referenceProjectID, projectID, bundle, execution.IndexIngestCapability); err != nil {
		t.Fatalf("seed a job that projects into the project: %v", err)
	}

	if _, err := provisioner.Deprovision(ctx, projectID); !errors.Is(err, projectprovisioning.ErrProjectWorkActive) {
		t.Fatalf("err = %v, want ErrProjectWorkActive for a job projecting into the project", err)
	}
	if projectTombstoned(ctx, t, pool, projectID) {
		t.Fatal("a refused delete tombstoned the project")
	}
}

// SkipActiveWorkCheck is the ensurer's repair of a project whose creation never
// finished. It skips the count, nothing else: the tombstone is still set, so
// new work is still refused, and the delete runs to the end.
func TestSkipActiveWorkCheckStillTombstones(t *testing.T) {
	skipWithoutVectorExtension(t)
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	provisioner, projectID := provisionIndexableProject(ctx, t, pool, "Repair Skips The Count")
	seedActiveJob(ctx, t, pool, projectID, "exec-skip-count", execution.IndexIngestCapability)

	// Without the option the stray job refuses the delete (the explicit DELETE).
	if _, err := provisioner.Deprovision(ctx, projectID); !errors.Is(err, projectprovisioning.ErrProjectWorkActive) {
		t.Fatalf("premise: err = %v, want ErrProjectWorkActive", err)
	}

	// With it, the delete passes the fence and is tombstoned, whatever happens next.
	if _, err := pool.Exec(ctx, refuseProjectDeleteSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := provisioner.Deprovision(ctx, projectID, projectprovisioning.SkipActiveWorkCheck()); !errors.Is(err, projectprovisioning.ErrProjectNotRemoved) ||
		errors.Is(err, projectprovisioning.ErrProjectWorkActive) {
		t.Fatalf("err = %v, want ErrProjectNotRemoved and not ErrProjectWorkActive", err)
	}
	if !projectTombstoned(ctx, t, pool, projectID) {
		t.Fatal("the delete that skipped the count did not tombstone the project")
	}
	for path, err := range admitJobs(ctx, t, pool, projectID, "skip") {
		if !isProjectDeletingRefusal(err) {
			t.Errorf("%s admission after the skipped-count tombstone: %v, want the 55000 refusal", path, err)
		}
	}

	// And it finishes.
	if _, err := pool.Exec(ctx, `DROP TRIGGER refuse_project_delete ON centry.project`); err != nil {
		t.Fatal(err)
	}
	if _, err := provisioner.Deprovision(ctx, projectID, projectprovisioning.SkipActiveWorkCheck()); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if database, role := vectorStoreResidue(ctx, t, pool, projectID); database || role {
		t.Fatalf("after the retry: database=%v role=%v, want both dropped", database, role)
	}
}
