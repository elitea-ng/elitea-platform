package projectprovisioning_test

// The row-first delete and its cleanup journal (#1211).
//
//   TestRowDeleteFailureLeavesTheProjectUnchangedAndUsable
//       the deciding transaction fails at the row delete: the project, its
//       resources and its admission are untouched, and no journal row exists.
//   TestVectorStoreProbeFailureFailsTheDecision
//       a probe error is not guessed around: nothing changes, retry later.
//   TestProjectionJobsRefuseTheDelete
//       a job that PROJECTS into the project counts like one that uses it.
//   TestAdmissionRacingTheDecision
//       the FOR UPDATE decision against a concurrent admission, both orders.
//   TestSkipActiveWorkCheckDeletesAProjectWithAStrayJob
//       the ensurer's repair option skips the count and nothing else.
//   TestCleanupFailureLeavesAJournalRowTheReconcilerFinishes
//       a failed cleanup step is reported, recorded with its backoff, and the
//       real reconciler finishes it.
//   TestConcurrentDeletesOnASmallPool
//       two deletes of one project on a pool of two connections: one 200, one
//       404, no deadlock.
//   TestClaimStaleDeletions
//       the reconciler's claim: grace, backoff, lease, SKIP LOCKED, batch.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

const refuseProjectDeleteSQL = `
CREATE FUNCTION refuse_project_delete() RETURNS trigger LANGUAGE plpgsql AS
$$ BEGIN RAISE EXCEPTION 'project delete refused by test'; END $$;
CREATE TRIGGER refuse_project_delete BEFORE DELETE ON centry.project
    FOR EACH ROW EXECUTE FUNCTION refuse_project_delete()`

func journalRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID int64) int {
	t.Helper()
	return countRows(ctx, t, pool, `SELECT count(*) FROM centry.project_deletions WHERE project_id = $1`, projectID)
}

type journalEntry struct {
	Cleanup   map[string]any
	Attempts  int
	LastError *string
	Completed bool
	Leased    bool
	BackedOff bool
}

func readJournal(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID int64) journalEntry {
	t.Helper()
	var (
		entry journalEntry
		raw   []byte
	)
	if err := pool.QueryRow(ctx, `
SELECT cleanup, attempts, last_error, completed_at IS NOT NULL,
       claimed_until IS NOT NULL AND claimed_until > now(), next_attempt_at > now()
FROM centry.project_deletions WHERE project_id = $1`, projectID,
	).Scan(&raw, &entry.Attempts, &entry.LastError, &entry.Completed, &entry.Leased, &entry.BackedOff); err != nil {
		t.Fatalf("read the cleanup journal of project %d: %v", projectID, err)
	}
	if err := json.Unmarshal(raw, &entry.Cleanup); err != nil {
		t.Fatalf("decode the cleanup journal %s: %v", raw, err)
	}
	return entry
}

func (e journalEntry) done(step string) bool {
	done, _ := e.Cleanup["done"].(map[string]any)
	value, _ := done[step].(bool)
	return value
}

// admitOne is one admission as the kernel does it: an input bundle and an
// execution job for the project, in one transaction. It returns the database's
// answer.
func admitOne(ctx context.Context, pool *pgxpool.Pool, projectID int64, tag string) error {
	return pgx.BeginFunc(ctx, pool, func(transaction pgx.Tx) error {
		if _, err := transaction.Exec(ctx, `
INSERT INTO elitea_runtime.input_bundles
    (input_bundle_id, immutable_version, resource_project_id, media_type,
     manifest_digest, manifest_size, manifest_bytes, created_by)
VALUES ($2, 'admission:' || $2, $1, 'application/x-protobuf',
        decode(repeat('61', 32), 'hex'), 1, decode('00', 'hex'), 'actor-1')`, projectID, "bundle-"+tag); err != nil {
			return err
		}
		_, err := transaction.Exec(ctx, `
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id, resource_project_id,
    projection_project_id, actor_id, principal_ref, capability_id,
    capability_version, input_bundle_id, request_digest, idempotency_scope,
    idempotency_key, state, desired_state
) VALUES (
    $2, 1, 'cmd-' || $2, ($1::integer)::text, $1::integer, $1::integer, '7', '7', $3, 'v1', $4,
    decode(repeat('61', 32), 'hex'), 'scope-' || $2, 'key-' || $2, 'PENDING', 'RUNNING')`,
			projectID, "exec-"+tag, execution.IndexIngestCapability, "bundle-"+tag)
		return err
	})
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

func projectRowCount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID int64) int {
	t.Helper()
	return countRows(ctx, t, pool, `SELECT count(*) FROM centry.project WHERE id = $1`, projectID)
}

// THE INVARIANT THE REDESIGN RESTORES. The row delete fails inside the deciding
// transaction: the delete answers ErrProjectNotRemoved, and the project is
// exactly as it was — row, vault, buckets, system user, roles, configuration,
// vector store — with no journal row, and new work is still admitted for it.
func TestRowDeleteFailureLeavesTheProjectUnchangedAndUsable(t *testing.T) {
	skipWithoutVectorExtension(t)
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	seedPublicPgvectorBootstrap(ctx, t, pool)
	provisioner := newVectorPurgingProvisioner(t, pool)
	created, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Row Stays", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	projectID := created.ProjectID
	dropProvisionedVectorStore(t, projectID)
	if _, err := pool.Exec(ctx, `INSERT INTO elitea_storage.buckets (project_id, name, bucket_type) VALUES ($1, 'docs', 'local')`, projectID); err != nil {
		t.Fatal(err)
	}
	before := snapshotProject(ctx, t, pool, projectID)
	if _, err := pool.Exec(ctx, refuseProjectDeleteSQL); err != nil {
		t.Fatal(err)
	}

	result, err := provisioner.Deprovision(ctx, projectID)
	if !errors.Is(err, projectprovisioning.ErrProjectNotRemoved) {
		t.Fatalf("deprovision err = %v, want ErrProjectNotRemoved", err)
	}
	if !stepFailed(result.RollbackSteps, projectprovisioning.StepProjectModel) || len(result.RollbackSteps) != 1 {
		t.Errorf("steps = %+v, want only project_model, failed", result.RollbackSteps)
	}
	if projectRowCount(ctx, t, pool, projectID) != 1 {
		t.Fatal("the project row is gone after a failed decision")
	}
	assertProjectIntact(ctx, t, pool, projectID, before)
	if database, role := vectorStoreResidue(ctx, t, pool, projectID); !database || !role {
		t.Fatalf("the surviving project lost its vectors: database=%v role=%v", database, role)
	}
	if rows := journalRows(ctx, t, pool, projectID); rows != 0 {
		t.Fatalf("a failed decision left %d journal row(s)", rows)
	}
	// Usable: work is still admitted for it.
	if err := admitOne(ctx, pool, projectID, "after-failed-delete"); err != nil {
		t.Fatalf("admission after the failed delete: %v", err)
	}

	// Once the blocker is gone the delete goes through (after the work settles).
	for _, statement := range []string{
		`DROP TRIGGER refuse_project_delete ON centry.project`,
		fmt.Sprintf(`UPDATE elitea_runtime.execution_jobs SET state = 'SUCCEEDED' WHERE resource_project_id = %d`, projectID),
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if result, err := provisioner.Deprovision(ctx, projectID); err != nil {
		t.Fatalf("delete after the blocker went: %v (steps=%+v)", err, result.RollbackSteps)
	}
	if database, role := vectorStoreResidue(ctx, t, pool, projectID); database || role {
		t.Fatalf("after delete: database=%v role=%v", database, role)
	}
	if err := admitOne(ctx, pool, projectID, "after-delete"); !isForeignKeyViolation(err) {
		t.Fatalf("admission for a deleted project = %v, want a foreign-key violation", err)
	}
	if entry := readJournal(ctx, t, pool, projectID); !entry.Completed || entry.Attempts != 1 || entry.LastError != nil {
		t.Fatalf("journal = %+v, want completed after one clean run", entry)
	}
}

// probeFailingStore is the real store whose "does the project have a vector
// store?" read fails.
type probeFailingStore struct {
	projectprovisioning.ProjectVectorStore
}

func (probeFailingStore) ProjectHasVectorStore(context.Context, projectprovisioning.Querier, int64) (bool, error) {
	return false, errors.New("transient read failure")
}

// A transient probe failure must not be guessed around: "had one" would report
// a drop that never had to run, "had none" would leak a database. The decision
// fails, nothing changes, and a retry is safe.
func TestVectorStoreProbeFailureFailsTheDecision(t *testing.T) {
	skipWithoutVectorExtension(t)
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	seedPublicPgvectorBootstrap(ctx, t, pool)
	realStore := newProjectVectorStoreForTest(t, pool)
	provisioner := projectprovisioning.New(pool, migrate.New(pool, platformmigrations.Files), nil,
		projectprovisioning.WithVectorStore(realStore),
		projectprovisioning.WithProjectVault(v2secrets.NewHandler(pool)))
	created, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Probe Fails", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	projectID := created.ProjectID
	dropProvisionedVectorStore(t, projectID)

	failing := projectprovisioning.New(pool, migrate.New(pool, platformmigrations.Files), nil,
		projectprovisioning.WithVectorStore(probeFailingStore{realStore}),
		projectprovisioning.WithProjectVault(v2secrets.NewHandler(pool)))
	if _, err := failing.Deprovision(ctx, projectID); !errors.Is(err, projectprovisioning.ErrProjectNotRemoved) {
		t.Fatalf("err = %v, want ErrProjectNotRemoved", err)
	}
	if projectRowCount(ctx, t, pool, projectID) != 1 || journalRows(ctx, t, pool, projectID) != 0 {
		t.Fatal("a failed probe changed the project or wrote a journal row")
	}
	if _, err := provisioner.Deprovision(ctx, projectID); err != nil {
		t.Fatalf("retry with a working probe: %v", err)
	}
	if entry := readJournal(ctx, t, pool, projectID); entry.Cleanup["had_vector_store"] != true {
		t.Fatalf("journal = %+v, want had_vector_store recorded", entry.Cleanup)
	}
}

// A job that uses ANOTHER project's resources and projects its results into
// this one counts like one of its own.
func TestProjectionJobsRefuseTheDelete(t *testing.T) {
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
	if projectRowCount(ctx, t, pool, projectID) != 1 || journalRows(ctx, t, pool, projectID) != 0 {
		t.Fatal("a refused delete changed the project or wrote a journal row")
	}
}

// The decision's FOR UPDATE against a concurrent admission, in both orders.
// Admission inserts rows with foreign keys to the project, which take a KEY
// SHARE lock on its row.
func TestAdmissionRacingTheDecision(t *testing.T) {
	skipWithoutVectorExtension(t)
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	t.Run("admission waits for the decision and fails on the foreign key", func(t *testing.T) {
		seedPublicPgvectorBootstrap(ctx, t, pool)
		store := &blockingProbeStore{
			ProjectVectorStore: newProjectVectorStoreForTest(t, pool),
			entered:            make(chan struct{}),
			release:            make(chan struct{}),
		}
		provisioner := projectprovisioning.New(pool, migrate.New(pool, platformmigrations.Files), nil,
			projectprovisioning.WithVectorStore(store),
			projectprovisioning.WithProjectVault(v2secrets.NewHandler(pool)))
		created, err := provisioner.Provision(ctx, projectprovisioning.Request{
			Name: "Admission Loses", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
		})
		if err != nil {
			t.Fatal(err)
		}
		projectID := created.ProjectID
		dropProvisionedVectorStore(t, projectID)
		store.armed.Store(true)

		deleted := make(chan error, 1)
		go func() {
			_, err := provisioner.Deprovision(ctx, projectID)
			deleted <- err
		}()
		// The decision holds the row FOR UPDATE and is parked inside its
		// transaction (in the vector-store probe).
		select {
		case <-store.entered:
		case <-time.After(60 * time.Second):
			t.Fatal("the decision never reached the probe")
		}
		admitted := make(chan error, 1)
		go func() { admitted <- admitOne(ctx, pool, projectID, "racing") }()
		select {
		case err := <-admitted:
			t.Fatalf("admission did not wait for the decision's row lock: %v", err)
		case <-time.After(500 * time.Millisecond):
		}
		close(store.release)
		if err := <-deleted; err != nil {
			t.Fatalf("delete: %v", err)
		}
		if err := <-admitted; !isForeignKeyViolation(err) {
			t.Fatalf("admission that raced the decision = %v, want a foreign-key violation", err)
		}
	})

	t.Run("the decision waits for an admission and counts it", func(t *testing.T) {
		provisioner, projectID := provisionIndexableProject(ctx, t, pool, "Decision Loses")
		admission, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = admission.Rollback(context.WithoutCancel(ctx)) }()
		if _, err := admission.Exec(ctx, `
INSERT INTO elitea_runtime.input_bundles
    (input_bundle_id, immutable_version, resource_project_id, media_type,
     manifest_digest, manifest_size, manifest_bytes, created_by)
VALUES ('bundle-decision-loses', 'admission:x', $1, 'application/x-protobuf',
        decode(repeat('61', 32), 'hex'), 1, decode('00', 'hex'), 'actor-1')`, projectID); err != nil {
			t.Fatal(err)
		}
		if _, err := admission.Exec(ctx, `
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id, resource_project_id,
    projection_project_id, actor_id, principal_ref, capability_id,
    capability_version, input_bundle_id, request_digest, idempotency_scope,
    idempotency_key, state, desired_state
) VALUES (
    'exec-decision-loses', 1, 'cmd-decision-loses', ($1::integer)::text, $1::integer, $1::integer, '7', '7', $2, 'v1',
    'bundle-decision-loses', decode(repeat('61', 32), 'hex'), 'scope-decision-loses', 'key-decision-loses', 'PENDING', 'RUNNING')`,
			projectID, execution.IndexIngestCapability); err != nil {
			t.Fatal(err)
		}

		done := make(chan error, 1)
		go func() {
			_, err := provisioner.Deprovision(ctx, projectID)
			done <- err
		}()
		select {
		case err := <-done:
			t.Fatalf("the decision did not wait for the uncommitted admission: %v", err)
		case <-time.After(500 * time.Millisecond):
		}
		if err := admission.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, projectprovisioning.ErrProjectWorkActive) {
			t.Fatalf("deprovision err = %v, want ErrProjectWorkActive", err)
		}
		if projectRowCount(ctx, t, pool, projectID) != 1 || journalRows(ctx, t, pool, projectID) != 0 {
			t.Fatal("the refused decision changed the project or wrote a journal row")
		}
	})
}

// blockingProbeStore parks the decision transaction in its vector-store probe
// (once armed) until released.
type blockingProbeStore struct {
	projectprovisioning.ProjectVectorStore
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingProbeStore) ProjectHasVectorStore(ctx context.Context, query projectprovisioning.Querier, projectID int64) (bool, error) {
	if s.armed.Load() {
		s.once.Do(func() { close(s.entered) })
		select {
		case <-s.release:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return s.ProjectVectorStore.ProjectHasVectorStore(ctx, query, projectID)
}

// SkipActiveWorkCheck is the ensurer's repair of a project whose creation never
// finished. It skips the count, nothing else.
func TestSkipActiveWorkCheckDeletesAProjectWithAStrayJob(t *testing.T) {
	skipWithoutVectorExtension(t)
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	provisioner, projectID := provisionIndexableProject(ctx, t, pool, "Repair Skips The Count")
	seedActiveJob(ctx, t, pool, projectID, "exec-skip-count", execution.IndexIngestCapability)

	if _, err := provisioner.Deprovision(ctx, projectID); !errors.Is(err, projectprovisioning.ErrProjectWorkActive) {
		t.Fatalf("premise: err = %v, want ErrProjectWorkActive", err)
	}
	if result, err := provisioner.Deprovision(ctx, projectID, projectprovisioning.SkipActiveWorkCheck()); err != nil {
		t.Fatalf("delete with the count skipped: %v (steps=%+v)", err, result.RollbackSteps)
	}
	if projectRowCount(ctx, t, pool, projectID) != 0 {
		t.Fatal("the project row survived")
	}
	if got := countRows(ctx, t, pool, `SELECT count(*) FROM elitea_runtime.execution_jobs WHERE resource_project_id = $1`, projectID); got != 0 {
		t.Fatalf("the stray job survived the delete: %d", got)
	}
	if database, role := vectorStoreResidue(ctx, t, pool, projectID); database || role {
		t.Fatalf("database=%v role=%v, want both dropped", database, role)
	}
}

// toggleDropStore is the real store with a drop that fails while failing is set.
type toggleDropStore struct {
	projectprovisioning.ProjectVectorStore
	mu      sync.Mutex
	failing bool
}

func (s *toggleDropStore) DropProjectVectorStore(ctx context.Context, projectID int64, hadStore bool) (string, error) {
	s.mu.Lock()
	failing := s.failing
	s.mu.Unlock()
	if failing {
		return fmt.Sprintf("project_%d", projectID), errors.New("pg unreachable")
	}
	return s.ProjectVectorStore.DropProjectVectorStore(ctx, projectID, hadStore)
}

func (s *toggleDropStore) set(failing bool) {
	s.mu.Lock()
	s.failing = failing
	s.mu.Unlock()
}

// A cleanup step that fails after the row is gone: the delete reports it by
// name, the journal row keeps it (with the steps that did finish, its attempt
// count and its backoff), and the real reconciler finishes it once the grace and
// the backoff have passed — and not before.
func TestCleanupFailureLeavesAJournalRowTheReconcilerFinishes(t *testing.T) {
	skipWithoutVectorExtension(t)
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	seedPublicPgvectorBootstrap(ctx, t, pool)
	store := &toggleDropStore{ProjectVectorStore: newProjectVectorStoreForTest(t, pool), failing: true}
	provisioner := projectprovisioning.New(pool, migrate.New(pool, platformmigrations.Files), nil,
		projectprovisioning.WithVectorStore(store),
		projectprovisioning.WithProjectVault(v2secrets.NewHandler(pool)))
	created, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Journal Finishes", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	projectID := created.ProjectID
	dropProvisionedVectorStore(t, projectID)
	database := fmt.Sprintf("project_%d", projectID)

	result, err := provisioner.Deprovision(ctx, projectID)
	if !errors.Is(err, projectprovisioning.ErrVectorStoreNotDropped) || !strings.Contains(err.Error(), database) {
		t.Fatalf("err = %v, want ErrVectorStoreNotDropped naming %s", err, database)
	}
	if errors.Is(err, projectprovisioning.ErrTenantSchemaNotRemoved) || errors.Is(err, projectprovisioning.ErrCleanupIncomplete) {
		t.Fatalf("err = %v names leftovers that were cleaned", err)
	}
	if result.VectorDatabase != database || !stepFailed(result.RollbackSteps, projectprovisioning.StepProjectPgvectorDrop) {
		t.Fatalf("result = %+v, want the drop failed and %s named", result, database)
	}
	if projectRowCount(ctx, t, pool, projectID) != 0 {
		t.Fatal("the project row survived the decision")
	}

	entry := readJournal(ctx, t, pool, projectID)
	if entry.Completed || entry.Attempts != 1 || entry.LastError == nil || !strings.Contains(*entry.LastError, projectprovisioning.StepProjectPgvectorDrop) {
		t.Fatalf("journal = %+v, want one failed run naming the drop", entry)
	}
	if entry.Leased || !entry.BackedOff {
		t.Fatalf("journal = %+v, want the lease released and a backoff set", entry)
	}
	for _, step := range []string{projectprovisioning.StepProjectSchema, projectprovisioning.StepProjectSecrets, projectprovisioning.StepSystemUser} {
		if !entry.done(step) {
			t.Errorf("step %s is not recorded done: %+v", step, entry.Cleanup)
		}
	}
	if entry.done(projectprovisioning.StepProjectPgvectorDrop) {
		t.Error("the failed drop is recorded done")
	}

	reconciler, err := runtimecomposition.NewProjectDeletionReconciler(provisioner,
		runtimecomposition.ProjectDeletionReconcilerConfig{GracePeriod: time.Minute, BatchSize: 10},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	store.set(false)

	// Too fresh and still backing off: the reconciler leaves it alone.
	if worked, err := reconciler.RunOnce(ctx); err != nil || worked != 0 {
		t.Fatalf("pass on a fresh row: worked=%d err=%v, want nothing", worked, err)
	}
	// Past the grace but still backing off: still left alone.
	if _, err := pool.Exec(ctx, `UPDATE centry.project_deletions SET created_at = now() - interval '1 hour' WHERE project_id = $1`, projectID); err != nil {
		t.Fatal(err)
	}
	if worked, err := reconciler.RunOnce(ctx); err != nil || worked != 0 {
		t.Fatalf("pass during the backoff: worked=%d err=%v, want nothing", worked, err)
	}
	// Past both: finished.
	if _, err := pool.Exec(ctx, `UPDATE centry.project_deletions SET next_attempt_at = now() - interval '1 second' WHERE project_id = $1`, projectID); err != nil {
		t.Fatal(err)
	}
	if worked, err := reconciler.RunOnce(ctx); err != nil || worked != 1 {
		t.Fatalf("pass after the backoff: worked=%d err=%v, want 1", worked, err)
	}
	if reconciler.Finished() != 1 || reconciler.Failed() != 0 {
		t.Fatalf("finished=%d failed=%d, want 1 and 0", reconciler.Finished(), reconciler.Failed())
	}
	entry = readJournal(ctx, t, pool, projectID)
	if !entry.Completed || entry.Attempts != 2 || entry.LastError != nil || entry.Leased {
		t.Fatalf("journal = %+v, want completed on the second run", entry)
	}
	if database, role := vectorStoreResidue(ctx, t, pool, projectID); database || role {
		t.Fatalf("after the reconciler: database=%v role=%v, want both dropped", database, role)
	}
	// A complete row is never claimed again.
	if worked, err := reconciler.RunOnce(ctx); err != nil || worked != 0 {
		t.Fatalf("pass after completion: worked=%d err=%v", worked, err)
	}
}

// Two deletes of one project on a pool of TWO connections. Both queue on the
// project row lock (held here by a session outside that pool), each holding one
// of the two connections. When the lock goes, one delete commits and needs
// connections for its cleanup; the other wakes, finds no row and gives its
// connection back. One 200, one 404, and no deadlock: the delete holds no
// session lock or extra connection while it waits.
func TestConcurrentDeletesOnASmallPool(t *testing.T) {
	skipWithoutVectorExtension(t)
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	_, projectID := provisionIndexableProject(ctx, t, pool, "Two Deletes Small Pool")

	config := pool.Config().Copy()
	config.MaxConns = 2
	config.MinConns = 0
	small, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer small.Close()
	provisioner := projectprovisioning.New(small, migrate.New(small, platformmigrations.Files), nil,
		projectprovisioning.WithVectorStore(newProjectVectorStoreForTest(t, small)),
		projectprovisioning.WithProjectVault(v2secrets.NewHandler(small)))

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := holder.Exec(ctx, `SELECT 1 FROM centry.project WHERE id = $1 FOR UPDATE`, projectID); err != nil {
		t.Fatal(err)
	}

	deleteCtx, deleteCancel := context.WithTimeout(ctx, 90*time.Second)
	defer deleteCancel()
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := provisioner.Deprovision(deleteCtx, projectID)
			errs <- err
		}()
	}
	// Both are parked on the row lock, each on one of the pool's connections.
	deadline := time.Now().Add(30 * time.Second)
	for small.Stat().AcquiredConns() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the two deletes did not both reach the row lock: acquired=%d", small.Stat().AcquiredConns())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	var got []string
	for range 2 {
		select {
		case err := <-errs:
			switch {
			case err == nil:
				got = append(got, "ok")
			case errors.Is(err, projectprovisioning.ErrProjectNotFound):
				got = append(got, "not found")
			default:
				t.Fatalf("a delete failed: %v", err)
			}
		case <-time.After(120 * time.Second):
			t.Fatal("the deletes deadlocked on a pool of two connections")
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"not found", "ok"}) {
		t.Fatalf("results = %v, want one ok and one not found", got)
	}
	if database, role := vectorStoreResidue(ctx, t, pool, projectID); database || role {
		t.Fatalf("database=%v role=%v, want both dropped", database, role)
	}
	if entry := readJournal(ctx, t, pool, projectID); !entry.Completed || entry.Attempts != 1 {
		t.Fatalf("journal = %+v, want one complete run (no duplicated cleanup)", entry)
	}
}

// The claim the reconciler makes. Rows (all incomplete unless said):
//
//	101 old, due            → claimed
//	102 old, backing off    → not yet
//	103 fresh               → inside the grace
//	104 old, due, leased    → another run owns it
//	105 old, due, complete  → done
//	106 old, due, row-locked by another transaction → SKIP LOCKED
//	107 old, due            → claimed, but beyond a batch of one
func TestClaimStaleDeletions(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	provisioner := newTestProvisioner(t, pool, migrate.New(pool, platformmigrations.Files))
	if _, err := pool.Exec(ctx, `
INSERT INTO centry.project_deletions (project_id, cleanup, created_at, next_attempt_at, claimed_until, completed_at) VALUES
  (101, '{"done":{}}', now() - interval '1 hour', now() - interval '3 minutes', NULL, NULL),
  (102, '{"done":{}}', now() - interval '1 hour', now() + interval '1 hour', NULL, NULL),
  (103, '{"done":{}}', now(),                     now() - interval '1 minute', NULL, NULL),
  (104, '{"done":{}}', now() - interval '1 hour', now() - interval '1 minute', now() + interval '1 hour', NULL),
  (105, '{"done":{}}', now() - interval '1 hour', now() - interval '1 minute', NULL, now()),
  (106, '{"done":{}}', now() - interval '1 hour', now() - interval '1 minute', NULL, NULL),
  (107, '{"done":{}}', now() - interval '1 hour', now() - interval '2 minutes', NULL, NULL)`); err != nil {
		t.Fatal(err)
	}
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := holder.Exec(ctx, `SELECT 1 FROM centry.project_deletions WHERE project_id = 106 FOR UPDATE`); err != nil {
		t.Fatal(err)
	}

	// A batch of one takes the most overdue row.
	ids, err := provisioner.ClaimStaleDeletions(ctx, 5*time.Minute, 1)
	if err != nil || !slices.Equal(ids, []int64{101}) {
		t.Fatalf("batch of one = %v, %v; want [101]", ids, err)
	}
	// A larger batch does not wait for the locked row, and does not take 101
	// again: the first claim leased it.
	claimed := make(chan []int64, 1)
	go func() {
		ids, err := provisioner.ClaimStaleDeletions(ctx, 5*time.Minute, 10)
		if err != nil {
			t.Error(err)
		}
		claimed <- ids
	}()
	select {
	case ids = <-claimed:
	case <-time.After(10 * time.Second):
		t.Fatal("the claim waited for a row another transaction holds")
	}
	if !slices.Equal(ids, []int64{107}) {
		t.Fatalf("second claim = %v, want [107]", ids)
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if ids, err := provisioner.ClaimStaleDeletions(ctx, 5*time.Minute, 10); err != nil || !slices.Equal(ids, []int64{106}) {
		t.Fatalf("claim after the lock went = %v, %v; want [106]", ids, err)
	}
	// A claimed row with no journal content to resume is reported, not hidden.
	if _, err := provisioner.ResumeDeletion(ctx, 999); !errors.Is(err, projectprovisioning.ErrProjectNotFound) {
		t.Fatalf("resume of an unknown id = %v, want ErrProjectNotFound", err)
	}
}
