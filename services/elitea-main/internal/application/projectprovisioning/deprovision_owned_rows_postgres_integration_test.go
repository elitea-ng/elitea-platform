package projectprovisioning_test

// C1, found during the 2026-10 regression cleanup: deleting a project left
// its artifact bucket rows (three of them not even soft-deleted), the S3
// objects under p/<id>/, and rows in public.pipeline_runs, centry.social_pins,
// gateway.llm_budget_accumulators and elitea_runtime.tool_call_records. None
// of those tables carries a foreign key to centry.project, so nothing blocked
// the delete and nothing cleared them.
//
// The test seeds one row per table for the doomed project AND for a
// bystander id, deletes the project, and counts. The bystander rows prove
// the statements are scoped to the project rather than truncating.
//
// Two rows deliberately stay (review of #1026). A live bucket whose object
// purge failed is the only handle on its bytes, so it is kept for a retry.
// The budget accumulators are the spend history, like usage and audit rows.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/artifactbootstrap"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

// accumulatorRows counts the budget accumulator rows of $1. They must
// survive a project delete.
const accumulatorRows = `SELECT count(*) FROM gateway.llm_budget_accumulators WHERE project_id = $1`

// purgeStore is the slice of storage.ObjectStore that TeardownProjectBuckets
// calls: List and DeleteBatch. Any other call panics on the nil embedded
// interface. DeleteBatch fails for every bucket named in failing.
type purgeStore struct {
	storage.ObjectStore
	mu      sync.Mutex
	objects map[string]map[string]bool // bucket -> keys
	failing map[string]bool
}

func newPurgeStore() *purgeStore {
	return &purgeStore{objects: map[string]map[string]bool{}, failing: map[string]bool{}}
}

func (s *purgeStore) seed(bucket, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.objects[bucket] == nil {
		s.objects[bucket] = map[string]bool{}
	}
	s.objects[bucket][key] = true
}

func (s *purgeStore) setFailing(bucket string, failing bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing[bucket] = failing
}

func (s *purgeStore) count(bucket string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.objects[bucket])
}

func (s *purgeStore) List(_ context.Context, q storage.ListQuery) (storage.ListPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	page := storage.ListPage{}
	for key := range s.objects[q.Bucket.Bucket()] {
		page.Objects = append(page.Objects, storage.ObjectInfo{Key: key})
	}
	return page, nil
}

func (s *purgeStore) DeleteBatch(_ context.Context, refs []storage.ObjectRef) (storage.BatchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := storage.BatchResult{}
	for _, ref := range refs {
		if s.failing[ref.Bucket()] {
			return result, errors.New("simulated object store timeout")
		}
		delete(s.objects[ref.Bucket()], ref.Key())
		result.Deleted = append(result.Deleted, ref.Key())
	}
	return result, nil
}

type bucketRepos struct {
	*repos.ArtifactBucketsRepository
	*repos.ArtifactObjectsRepository
}

// newPurgingProvisioner wires the real artifact bootstrapper, over the real
// bucket repositories, to store.
func newPurgingProvisioner(t *testing.T, pool *pgxpool.Pool, store storage.ObjectStore) *projectprovisioning.Provisioner {
	t.Helper()
	buckets, err := repos.NewArtifactBucketsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := repos.NewArtifactObjectsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	return projectprovisioning.New(pool, migrate.New(pool, platformmigrations.Files), nil,
		projectprovisioning.WithProjectVault(v2secrets.NewHandler(pool)),
		projectprovisioning.WithArtifactBuckets(
			artifactbootstrap.NewBootstrapper(bucketRepos{buckets, objects}, store)))
}

func countRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, query, args...).Scan(&count); err != nil {
		t.Fatalf("count: %v\n%s", err, query)
	}
	return count
}

func stepFailed(steps []projectprovisioning.StepStatus, name string) bool {
	for _, status := range steps {
		if status.Step == name {
			return status.OK != nil && !*status.OK
		}
	}
	return false
}

// ownedRowTables names every table C1 lists, plus the storage tables that
// belong to a bucket or a project. Each query counts the rows of $1.
var ownedRowTables = map[string]string{
	"elitea_storage.buckets":                `SELECT count(*) FROM elitea_storage.buckets WHERE project_id = $1`,
	"elitea_storage.objects":                `SELECT count(*) FROM elitea_storage.objects o JOIN elitea_storage.buckets b ON b.id = o.bucket_id WHERE b.project_id = $1`,
	"elitea_storage.bucket_permissions":     `SELECT count(*) FROM elitea_storage.bucket_permissions WHERE project_id = $1`,
	"elitea_storage.project_storage_policy": `SELECT count(*) FROM elitea_storage.project_storage_policy WHERE project_id = $1`,
	"elitea_storage.attachment_chunks":      `SELECT count(*) FROM elitea_storage.attachment_chunks WHERE project_id = $1`,
	"public.pipeline_runs":                  `SELECT count(*) FROM public.pipeline_runs WHERE project_id = ($1::bigint)::text`,
	"centry.social_pins":                    `SELECT count(*) FROM centry.social_pins WHERE project_id = $1`,
	"elitea_runtime.tool_call_records":      `SELECT count(*) FROM elitea_runtime.tool_call_records WHERE project_id = $1`,
}

func seedProjectOwnedRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID int64) {
	t.Helper()
	statements := []string{
		// A live user bucket, a soft-deleted one, and an object row.
		`INSERT INTO elitea_storage.buckets (project_id, name, bucket_type) VALUES ($1, 'user-bucket', 'local')`,
		`INSERT INTO elitea_storage.buckets (project_id, name, bucket_type, deleted_at) VALUES ($1, 'gone-bucket', 'local', now())`,
		`INSERT INTO elitea_storage.objects (bucket_id, key, byte_length)
		 SELECT id, 'a.txt', 1 FROM elitea_storage.buckets WHERE project_id = $1 AND name = 'user-bucket'`,
		`INSERT INTO elitea_storage.bucket_permissions (project_id, user_id, bucket, permissions) VALUES ($1, 1, 'user-bucket', ARRAY['read'])`,
		`INSERT INTO elitea_storage.project_storage_policy (project_id) VALUES ($1)`,
		`INSERT INTO elitea_storage.attachment_chunks (project_id, conversation_id, file_id, chunk_index, total_chunks, file_name, bytes)
		 VALUES ($1, 'c', 'f', 0, 1, 'f.txt', '\x00')`,
		`INSERT INTO public.pipeline_runs (execution_id, project_id, application_id, version_id, conversation_uuid, origin)
		 VALUES ('exec-' || ($1::bigint)::text, ($1::bigint)::text, 1, 1, gen_random_uuid()::text, 'manual')`,
		`INSERT INTO centry.social_pins (entity, user_id, project_id, entity_id) VALUES ('application', 1, $1, 7)`,
		`INSERT INTO gateway.llm_budget_accumulators (project_id, scope, scope_id, period_start, period_end)
		 VALUES ($1::bigint, 'project', ($1::bigint)::text, date_trunc('month', now()), date_trunc('month', now()) + interval '1 month')`,
		`INSERT INTO elitea_runtime.tool_call_records (project_id, source, source_ref, tool_name, started_at)
		 VALUES ($1::bigint, 'agent_turn', 'ref-' || ($1::bigint)::text, 'search', now())`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement, projectID); err != nil {
			t.Fatalf("seed project %d: %v\n%s", projectID, err, statement)
		}
	}
}

func countOwnedRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID int64) map[string]int {
	t.Helper()
	counts := make(map[string]int, len(ownedRowTables))
	for table, query := range ownedRowTables {
		var count int
		if err := pool.QueryRow(ctx, query, projectID).Scan(&count); err != nil {
			t.Fatalf("count %s for project %d: %v", table, projectID, err)
		}
		counts[table] = count
	}
	return counts
}

func TestDeprovisionRemovesTheProjectOwnedRowsThatHaveNoForeignKey(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	store := newPurgeStore()
	provisioner := newPurgingProvisioner(t, pool, store)
	created, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Owned Rows", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	projectID := created.ProjectID
	bystander := projectID + 100000

	seedProjectOwnedRows(ctx, t, pool, projectID)
	seedProjectOwnedRows(ctx, t, pool, bystander)
	store.seed("user-bucket", "a.txt")
	for table, rows := range countOwnedRows(ctx, t, pool, projectID) {
		if rows == 0 {
			t.Fatalf("the seed wrote no row in %s, so the delete proves nothing there", table)
		}
	}

	if result, err := provisioner.Deprovision(ctx, projectID); err != nil {
		t.Fatalf("deprovision: %v (steps=%+v)", err, result.RollbackSteps)
	}

	for table, rows := range countOwnedRows(ctx, t, pool, projectID) {
		if rows != 0 {
			t.Errorf("%s kept %d row(s) of the deleted project %d", table, rows, projectID)
		}
	}
	for table, rows := range countOwnedRows(ctx, t, pool, bystander) {
		if rows == 0 {
			t.Errorf("%s lost the bystander project's rows: the delete is not scoped", table)
		}
	}
	if store.count("user-bucket") != 0 {
		t.Errorf("the object store kept %d object(s) of the deleted project", store.count("user-bucket"))
	}
	// The spend history outlives the project, like usage and audit rows.
	if got := countRows(ctx, t, pool, accumulatorRows, projectID); got != 1 {
		t.Errorf("budget accumulator rows of the deleted project = %d, want the 1 kept as history", got)
	}
}

// A bucket whose object purge fails keeps its row and its object rows, so a
// retry can still find and purge its bytes. The delete reports the residue
// instead of answering success.
func TestDeprovisionKeepsABucketWhosePurgeFailed(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	store := newPurgeStore()
	provisioner := newPurgingProvisioner(t, pool, store)
	created, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Purge Fails", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	projectID := created.ProjectID
	for _, statement := range []string{
		`INSERT INTO elitea_storage.buckets (project_id, name, bucket_type) VALUES ($1, 'docs', 'local')`,
		`INSERT INTO elitea_storage.objects (bucket_id, key, byte_length)
		 SELECT id, 'a.txt', 1 FROM elitea_storage.buckets WHERE project_id = $1 AND name = 'docs'`,
	} {
		if _, err := pool.Exec(ctx, statement, projectID); err != nil {
			t.Fatal(err)
		}
	}
	store.seed("docs", "a.txt")
	store.setFailing("docs", true)

	result, err := provisioner.Deprovision(ctx, projectID)
	if !errors.Is(err, projectprovisioning.ErrArtifactsNotRemoved) {
		t.Fatalf("deprovision with a failed purge = %v, want ErrArtifactsNotRemoved", err)
	}
	if !stepFailed(result.RollbackSteps, projectprovisioning.StepArtifactBuckets) {
		t.Errorf("artifact_buckets step is not reported failed: %+v", result.RollbackSteps)
	}
	if got := countRows(ctx, t, pool, `SELECT count(*) FROM centry.project WHERE id = $1`, projectID); got != 0 {
		t.Fatalf("project row survived: %d", got)
	}
	liveDocs := `SELECT count(*) FROM elitea_storage.buckets WHERE project_id = $1 AND name = 'docs' AND deleted_at IS NULL`
	if got := countRows(ctx, t, pool, liveDocs, projectID); got != 1 {
		t.Fatalf("live 'docs' bucket rows = %d, want the 1 kept for a retry", got)
	}
	docsObjects := `SELECT count(*) FROM elitea_storage.objects o JOIN elitea_storage.buckets b ON b.id = o.bucket_id
		WHERE b.project_id = $1 AND b.name = 'docs'`
	if got := countRows(ctx, t, pool, docsObjects, projectID); got != 1 {
		t.Fatalf("'docs' object rows = %d, want 1", got)
	}
	// The system buckets purged cleanly, so their rows went.
	if got := countRows(ctx, t, pool, `SELECT count(*) FROM elitea_storage.buckets WHERE project_id = $1 AND name <> 'docs'`, projectID); got != 0 {
		t.Errorf("purged bucket rows left = %d, want 0", got)
	}

	// The store recovers. The project row is gone, so another delete is a 404;
	// the cleanup journal kept the purge and its next run finishes it (#1211).
	store.setFailing("docs", false)
	if _, err := provisioner.Deprovision(ctx, projectID); !errors.Is(err, projectprovisioning.ErrProjectNotFound) {
		t.Fatalf("second delete = %v, want ErrProjectNotFound", err)
	}
	if completed, err := provisioner.ResumeDeletion(ctx, projectID); err != nil || !completed {
		t.Fatalf("resume the journal: completed=%v err=%v", completed, err)
	}
	if got := countRows(ctx, t, pool, `SELECT count(*) FROM elitea_storage.buckets WHERE project_id = $1`, projectID); got != 0 {
		t.Errorf("bucket rows after the retry = %d, want 0", got)
	}
	if store.count("docs") != 0 {
		t.Errorf("object store kept %d object(s) after the retry", store.count("docs"))
	}
	var completed bool
	if err := pool.QueryRow(ctx,
		`SELECT completed_at IS NOT NULL FROM centry.project_deletions WHERE project_id = $1`, projectID).Scan(&completed); err != nil || !completed {
		t.Errorf("the journal row is not complete after the resumed purge: %v, %v", completed, err)
	}
}

// Without an object store nothing can purge the bytes. The live bucket row
// stays and the delete does not report success.
func TestDeprovisionWithoutAnObjectStoreKeepsLiveBuckets(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	provisioner := newTestProvisioner(t, pool, migrate.New(pool, platformmigrations.Files))
	created, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "No Object Store", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	projectID := created.ProjectID
	if _, err := pool.Exec(ctx, `INSERT INTO elitea_storage.buckets (project_id, name, bucket_type) VALUES ($1, 'docs', 'local')`, projectID); err != nil {
		t.Fatal(err)
	}

	result, err := provisioner.Deprovision(ctx, projectID)
	if !errors.Is(err, projectprovisioning.ErrArtifactsNotRemoved) {
		t.Fatalf("deprovision without an object store = %v, want ErrArtifactsNotRemoved", err)
	}
	if !stepFailed(result.RollbackSteps, projectprovisioning.StepArtifactBuckets) {
		t.Errorf("artifact_buckets step is not reported failed: %+v", result.RollbackSteps)
	}
	if got := countRows(ctx, t, pool, `SELECT count(*) FROM elitea_storage.buckets WHERE project_id = $1 AND deleted_at IS NULL`, projectID); got != 1 {
		t.Errorf("live bucket rows = %d, want the 1 kept", got)
	}
}
