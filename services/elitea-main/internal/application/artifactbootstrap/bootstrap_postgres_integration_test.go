package artifactbootstrap_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	artifactbootstrap "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/artifactbootstrap"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

const postgresIntegrationDatabaseURL = "ELITEA_TEST_DATABASE_URL"

// bootstrapRepoAdapter satisfies artifactbootstrap.Repository by embedding
// both S6 repositories — they share no method names, so Go's method
// promotion does the rest. Same pattern as internal/api/router.go's
// artifactRepoAdapter.
type bootstrapRepoAdapter struct {
	*repos.ArtifactBucketsRepository
	*repos.ArtifactObjectsRepository
}

func newPostgresIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv(postgresIntegrationDatabaseURL)
	if databaseURL == "" {
		t.Skipf("set %s to run the artifact bootstrap PostgreSQL integration tests", postgresIntegrationDatabaseURL)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", postgresIntegrationDatabaseURL, err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open PostgreSQL admin pool: %v", err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	databaseName := fmt.Sprintf("elitea_artifactbootstrap_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated PostgreSQL integration database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 6
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)")
		adminPool.Close()
		t.Fatalf("open isolated PostgreSQL integration database: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)")
		adminPool.Close()
		t.Fatalf("ping isolated PostgreSQL integration database: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		// 120 s, not the old 20 s to 30 s. This DROP queues behind the
		// CREATE DATABASE calls of every package that `go test ./...` runs at
		// the same time, so the wait is server load and not a hang. Two full
		// runs failed here with "drop isolated ... database: timeout" (#409).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated PostgreSQL integration database: %v", err)
		}
		adminPool.Close()
	})
	return pool
}

// applySharedMigrations applies every embedded shared migration (including
// migrations/shared/0057_artifact_storage.sql) — elitea_storage.buckets/
// objects are shared-scope tables, not per-tenant p_N schema, so no
// ApplyTenant call is needed here. A handful of earlier shared migrations
// (0030, 0031, 0037, 0041, 0046, 0047) FK-reference centry.project(id) —
// that schema/table is owned and created by something outside elitea-main
// entirely (see docs/plans/storage-migration-plan.md S13's own point about
// centry.project having no CREATE TABLE anywhere in this service), so a
// bare ApplyShared fails on a real empty database; stub the minimum shape
// those FKs need first, matching the same stub other packages'
// integration tests already use for the same reason.
func applySharedMigrations(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
CREATE SCHEMA centry;
CREATE TABLE centry.project (
    id INTEGER PRIMARY KEY,
    create_success BOOLEAN NOT NULL DEFAULT TRUE,
    suspended BOOLEAN NOT NULL DEFAULT FALSE
);`); err != nil {
		t.Fatalf("stub centry.project: %v", err)
	}
	runner := migrate.New(pool, platformmigrations.Files)
	if err := runner.ApplyShared(ctx); err != nil {
		t.Fatalf("apply embedded shared migrations: %v", err)
	}
}

func newTestBootstrapper(t *testing.T) (*artifactbootstrap.Bootstrapper, *bootstrapRepoAdapter, *fakeObjectStore) {
	t.Helper()
	pool := newPostgresIntegrationPool(t)
	applySharedMigrations(t, pool)

	bucketsRepo, err := repos.NewArtifactBucketsRepository(pool)
	if err != nil {
		t.Fatalf("NewArtifactBucketsRepository: %v", err)
	}
	objectsRepo, err := repos.NewArtifactObjectsRepository(pool)
	if err != nil {
		t.Fatalf("NewArtifactObjectsRepository: %v", err)
	}
	repo := &bootstrapRepoAdapter{ArtifactBucketsRepository: bucketsRepo, ArtifactObjectsRepository: objectsRepo}
	store := newFakeObjectStore()
	return artifactbootstrap.NewBootstrapper(repo, store), repo, store
}

func TestArtifactBootstrapCreatesReportsAndTasksIdempotently(t *testing.T) {
	b, repo, _ := newTestBootstrapper(t)
	const projectID = "9001"

	if err := b.BootstrapProjectBuckets(t.Context(), projectID); err != nil {
		t.Fatalf("first BootstrapProjectBuckets: %v", err)
	}
	if err := b.BootstrapProjectBuckets(t.Context(), projectID); err != nil {
		t.Fatalf("second BootstrapProjectBuckets (must be a no-op, not an error): %v", err)
	}

	rows, err := repo.ListBuckets(t.Context(), 9001)
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected exactly 2 buckets after two Bootstrap calls, got %d: %+v", len(rows), rows)
	}
	byName := map[string]repos.BucketRow{}
	for _, row := range rows {
		byName[row.Name] = row
	}
	for _, name := range []string{"reports", "tasks"} {
		row, ok := byName[name]
		if !ok {
			t.Errorf("expected a %q bucket, got %+v", name, rows)
			continue
		}
		if row.BucketType != "system" {
			t.Errorf("%q bucket_type = %q, want %q", name, row.BucketType, "system")
		}
	}
}

func TestArtifactBootstrapRejectsInvalidProjectID(t *testing.T) {
	b, _, _ := newTestBootstrapper(t)

	// "007", "+9001", and the 19-digit int64 max are all values
	// strconv.ParseInt alone would accept, but storage.NewBucketRef's
	// projectIDPattern rejects (no leading zero, no sign, 18-digit cap).
	// parseProjectID must reject them too — the regression this covers: it
	// used to accept them, let Bootstrap create real bucket rows for them,
	// and then Teardown could never reach those same rows again because it
	// builds its storage.NewBucketRef from the identical (rejected) string.
	invalid := []string{"", "0", "-1", "not-a-number", "1.5", "007", "+9001", "9223372036854775807"}
	for _, id := range invalid {
		if err := b.BootstrapProjectBuckets(t.Context(), id); err == nil {
			t.Errorf("BootstrapProjectBuckets(%q): expected an error, got nil", id)
		}
	}
}

func TestArtifactTeardownRemovesBothBucketsAndLeavesNoObjects(t *testing.T) {
	b, repo, store := newTestBootstrapper(t)
	const projectID = "9002"

	if err := b.BootstrapProjectBuckets(t.Context(), projectID); err != nil {
		t.Fatalf("BootstrapProjectBuckets: %v", err)
	}
	reportsBucket, err := repo.GetBucket(t.Context(), 9002, "reports")
	if err != nil {
		t.Fatalf("GetBucket(reports): %v", err)
	}

	// Seed both a physical object and its metadata row, proving Teardown
	// purges both — not just the physical bytes, which is the gap S12 found
	// (and deliberately deferred) in the unrelated DeleteBucket cascade.
	store.seed(projectID, "reports", "a.png")
	if _, err := repo.UpsertObject(t.Context(), repos.NewObjectInput{
		BucketID: reportsBucket.ID, Key: "a.png", ByteLength: 10, MediaType: "image/png",
	}); err != nil {
		t.Fatalf("seed UpsertObject: %v", err)
	}

	if err := b.TeardownProjectBuckets(t.Context(), projectID); err != nil {
		t.Fatalf("TeardownProjectBuckets: %v", err)
	}

	for _, name := range []string{"reports", "tasks"} {
		if _, err := repo.GetBucket(t.Context(), 9002, name); !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("GetBucket(%q) after teardown: err = %v, want ErrNotFound (soft-deleted)", name, err)
		}
	}
	if store.objectCount() != 0 {
		t.Errorf("expected the physical store to be empty after teardown, got %d objects", store.objectCount())
	}
	metadataRows, err := repo.ListObjects(t.Context(), reportsBucket.ID, "")
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(metadataRows) != 0 {
		t.Errorf("expected no object metadata rows for the torn-down reports bucket, got %d", len(metadataRows))
	}
}

func TestArtifactTeardownIsIdempotentForMissingBuckets(t *testing.T) {
	b, _, _ := newTestBootstrapper(t)

	// Never bootstrapped — both buckets already "don't exist."
	if err := b.TeardownProjectBuckets(t.Context(), "9003"); err != nil {
		t.Fatalf("TeardownProjectBuckets on a project with no buckets: expected nil (no-op), got %v", err)
	}

	// Bootstrap, tear down once, tear down again — the second call must
	// also be a no-op.
	if err := b.BootstrapProjectBuckets(t.Context(), "9004"); err != nil {
		t.Fatalf("BootstrapProjectBuckets: %v", err)
	}
	if err := b.TeardownProjectBuckets(t.Context(), "9004"); err != nil {
		t.Fatalf("first TeardownProjectBuckets: %v", err)
	}
	if err := b.TeardownProjectBuckets(t.Context(), "9004"); err != nil {
		t.Fatalf("second TeardownProjectBuckets (must be a no-op, not an error): %v", err)
	}
}

// TestArtifactTeardownPreservesMetadataConsistencyOnPartialDeleteFailure
// covers the regression purgeObjects originally had: on a partial
// DeleteBatch failure it returned before calling repo.DeleteObjects at all,
// permanently orphaning the metadata row for the object that DID delete
// (that row would count toward the project's quota forever, and a retry
// can never rediscover a key List no longer returns).
func TestArtifactTeardownPreservesMetadataConsistencyOnPartialDeleteFailure(t *testing.T) {
	b, repo, store := newTestBootstrapper(t)
	const projectID = "9005"

	if err := b.BootstrapProjectBuckets(t.Context(), projectID); err != nil {
		t.Fatalf("BootstrapProjectBuckets: %v", err)
	}
	reportsBucket, err := repo.GetBucket(t.Context(), 9005, "reports")
	if err != nil {
		t.Fatalf("GetBucket(reports): %v", err)
	}

	store.seed(projectID, "reports", "ok.png")
	store.seed(projectID, "reports", "stuck.png")
	for _, key := range []string{"ok.png", "stuck.png"} {
		if _, err := repo.UpsertObject(t.Context(), repos.NewObjectInput{
			BucketID: reportsBucket.ID, Key: key, ByteLength: 10, MediaType: "image/png",
		}); err != nil {
			t.Fatalf("seed UpsertObject(%s): %v", key, err)
		}
	}
	store.failKeys = map[string]bool{"stuck.png": true}

	if err := b.TeardownProjectBuckets(t.Context(), projectID); err == nil {
		t.Fatal("TeardownProjectBuckets: expected an error from the simulated partial delete failure, got nil")
	}

	if store.objectCount() != 1 {
		t.Errorf("expected only the failed-to-delete stuck.png to remain physically, got %d objects", store.objectCount())
	}
	metadataRows, err := repo.ListObjects(t.Context(), reportsBucket.ID, "")
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(metadataRows) != 1 || metadataRows[0].Key != "stuck.png" {
		t.Fatalf("expected exactly the still-undeleted stuck.png metadata row to remain (ok.png's row must be cleaned up), got %+v", metadataRows)
	}

	// The bucket must remain active (not soft-deleted) since teardown did
	// not fully complete — a caller can retry once the transient failure
	// clears.
	if _, err := repo.GetBucket(t.Context(), 9005, "reports"); err != nil {
		t.Fatalf("GetBucket(reports) after partial failure: expected the bucket to remain active, got %v", err)
	}
}

// TestArtifactTeardownPurgesObjectsAcrossMultiplePages forces
// fakeObjectStore.List to truncate, exercising the continuation-token loop
// in purgeObjects that a single-page fake would otherwise leave completely
// uncovered.
func TestArtifactTeardownPurgesObjectsAcrossMultiplePages(t *testing.T) {
	b, repo, store := newTestBootstrapper(t)
	const projectID = "9006"

	if err := b.BootstrapProjectBuckets(t.Context(), projectID); err != nil {
		t.Fatalf("BootstrapProjectBuckets: %v", err)
	}
	reportsBucket, err := repo.GetBucket(t.Context(), 9006, "reports")
	if err != nil {
		t.Fatalf("GetBucket(reports): %v", err)
	}

	keys := []string{"a.png", "b.png", "c.png", "d.png", "e.png"}
	for _, key := range keys {
		store.seed(projectID, "reports", key)
		if _, err := repo.UpsertObject(t.Context(), repos.NewObjectInput{
			BucketID: reportsBucket.ID, Key: key, ByteLength: 10, MediaType: "image/png",
		}); err != nil {
			t.Fatalf("seed UpsertObject(%s): %v", key, err)
		}
	}
	store.pageLimit = 2 // forces at least 3 List() calls to exhaust 5 objects

	if err := b.TeardownProjectBuckets(t.Context(), projectID); err != nil {
		t.Fatalf("TeardownProjectBuckets: %v", err)
	}

	if store.objectCount() != 0 {
		t.Errorf("expected the physical store to be empty after a multi-page teardown, got %d objects", store.objectCount())
	}
	metadataRows, err := repo.ListObjects(t.Context(), reportsBucket.ID, "")
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(metadataRows) != 0 {
		t.Errorf("expected no object metadata rows after a multi-page teardown, got %d: %+v", len(metadataRows), metadataRows)
	}
}

// fakeObjectStore is a minimal in-memory storage.ObjectStore double. Only
// Put/List/DeleteBatch/Stat/Delete see real use in this package's tests;
// the rest exist to satisfy the interface and return storage.ErrNotSupported.
//
// failKeys and pageLimit are test-only knobs, unset (nil/0) by default:
// failKeys makes DeleteBatch report specific keys as failed instead of
// deleting them, so tests can force a partial failure the way a real
// backend occasionally does; pageLimit forces List to paginate the way a
// real backend does regardless of the caller's own MaxKeys, since
// purgeObjects (matching handler.go's deleteAllObjects) never sets one.
type fakeObjectStore struct {
	mu        sync.Mutex
	objects   map[string]storage.ObjectInfo
	data      map[string][]byte
	failKeys  map[string]bool
	pageLimit int
}

func newFakeObjectStore() *fakeObjectStore {
	return &fakeObjectStore{objects: make(map[string]storage.ObjectInfo), data: make(map[string][]byte)}
}

func (s *fakeObjectStore) objectCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.objects)
}

// seed adds an object directly, bypassing Put, for test fixture setup.
func (s *fakeObjectStore) seed(projectID, bucket, key string) {
	ref, err := storage.NewObjectRef(projectID, bucket, key)
	if err != nil {
		panic(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[ref.StorageKey("")] = storage.ObjectInfo{Key: key, Size: 10, LastModified: time.Now()}
}

func (s *fakeObjectStore) Put(_ context.Context, ref storage.ObjectRef, body io.Reader, _ storage.PutOptions) (storage.ObjectInfo, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	info := storage.ObjectInfo{Key: ref.Key(), Size: int64(len(data)), LastModified: time.Now()}
	s.mu.Lock()
	defer s.mu.Unlock()
	storageKey := ref.StorageKey("")
	s.objects[storageKey] = info
	s.data[storageKey] = data
	return info, nil
}

func (s *fakeObjectStore) Get(_ context.Context, ref storage.ObjectRef, _ *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	storageKey := ref.StorageKey("")
	info, ok := s.objects[storageKey]
	if !ok {
		return nil, storage.ObjectInfo{}, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(s.data[storageKey])), info, nil
}

func (s *fakeObjectStore) Stat(_ context.Context, ref storage.ObjectRef) (storage.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, ok := s.objects[ref.StorageKey("")]
	if !ok {
		return storage.ObjectInfo{}, storage.ErrNotFound
	}
	return info, nil
}

func (s *fakeObjectStore) Delete(_ context.Context, ref storage.ObjectRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	storageKey := ref.StorageKey("")
	delete(s.objects, storageKey)
	delete(s.data, storageKey)
	return nil
}

func (s *fakeObjectStore) DeleteBatch(_ context.Context, refs []storage.ObjectRef) (storage.BatchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := storage.BatchResult{}
	for _, ref := range refs {
		if s.failKeys[ref.Key()] {
			result.Failed = append(result.Failed, storage.BatchError{Key: ref.Key(), Err: errors.New("simulated delete failure")})
			continue
		}
		storageKey := ref.StorageKey("")
		delete(s.objects, storageKey)
		delete(s.data, storageKey)
		result.Deleted = append(result.Deleted, ref.Key())
	}
	return result, nil
}

// List paginates over the matching storage keys in sorted order, honoring
// pageLimit as a forced per-call cap (real backends impose their own —
// purgeObjects itself never requests one via MaxKeys) and treating
// ContinuationToken as "resume strictly after this storage key", matching
// what NextContinuationToken returns below.
func (s *fakeObjectStore) List(_ context.Context, q storage.ListQuery) (storage.ListPage, error) {
	basePrefix := q.Bucket.BucketPrefix("")
	s.mu.Lock()
	defer s.mu.Unlock()

	var keys []string
	for storageKey := range s.objects {
		if strings.HasPrefix(storageKey, basePrefix) {
			keys = append(keys, storageKey)
		}
	}
	sort.Strings(keys)

	if q.ContinuationToken != "" {
		start := sort.SearchStrings(keys, q.ContinuationToken)
		if start < len(keys) && keys[start] == q.ContinuationToken {
			start++
		}
		keys = keys[start:]
	}

	limit := len(keys)
	if s.pageLimit > 0 && s.pageLimit < limit {
		limit = s.pageLimit
	}

	page := storage.ListPage{}
	for _, storageKey := range keys[:limit] {
		page.Objects = append(page.Objects, s.objects[storageKey])
	}
	if limit < len(keys) {
		page.IsTruncated = true
		page.NextContinuationToken = keys[limit-1]
	}
	return page, nil
}

func (s *fakeObjectStore) PresignGet(context.Context, storage.ObjectRef, time.Duration) (string, error) {
	return "", storage.ErrNotSupported
}

func (s *fakeObjectStore) PresignPut(context.Context, storage.ObjectRef, time.Duration, storage.PutOptions) (string, error) {
	return "", storage.ErrNotSupported
}

func (s *fakeObjectStore) StartMultipart(context.Context, storage.ObjectRef, storage.PutOptions) (storage.UploadID, error) {
	return "", storage.ErrNotSupported
}

func (s *fakeObjectStore) PresignPart(context.Context, storage.ObjectRef, storage.UploadID, int32, time.Duration) (string, error) {
	return "", storage.ErrNotSupported
}

func (s *fakeObjectStore) CompleteMultipart(context.Context, storage.ObjectRef, storage.UploadID, []storage.Part) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, storage.ErrNotSupported
}

func (s *fakeObjectStore) AbortMultipart(context.Context, storage.ObjectRef, storage.UploadID) error {
	return storage.ErrNotSupported
}

func (s *fakeObjectStore) Capabilities() storage.Capabilities { return storage.Capabilities{} }

var _ storage.ObjectStore = (*fakeObjectStore)(nil)
var _ artifactbootstrap.Repository = (*bootstrapRepoAdapter)(nil)

// TestArtifactTeardownRemovesUserBucketsToo pins C1. Teardown used to touch
// "reports" and "tasks" only, so a deleted project kept every bucket a user
// created, live, with its objects under p/<id>/. A bystander project's
// bucket proves the teardown is scoped.
func TestArtifactTeardownRemovesUserBucketsToo(t *testing.T) {
	b, repo, store := newTestBootstrapper(t)
	const projectID = "9010"
	const bystanderID = "9011"

	if err := b.BootstrapProjectBuckets(t.Context(), projectID); err != nil {
		t.Fatalf("BootstrapProjectBuckets: %v", err)
	}
	for _, seed := range []struct {
		project string
		id      int64
	}{{projectID, 9010}, {bystanderID, 9011}} {
		bucket, err := repo.CreateBucket(t.Context(), repos.NewBucketInput{
			ProjectID: seed.id, Name: "user-files", DisplayName: "User files", BucketType: "local",
		})
		if err != nil {
			t.Fatalf("CreateBucket(%s): %v", seed.project, err)
		}
		store.seed(seed.project, "user-files", "notes.txt")
		if _, err := repo.UpsertObject(t.Context(), repos.NewObjectInput{
			BucketID: bucket.ID, Key: "notes.txt", ByteLength: 10, MediaType: "text/plain",
		}); err != nil {
			t.Fatalf("UpsertObject(%s): %v", seed.project, err)
		}
	}

	if err := b.TeardownProjectBuckets(t.Context(), projectID); err != nil {
		t.Fatalf("TeardownProjectBuckets: %v", err)
	}

	live, err := repo.ListBuckets(t.Context(), 9010)
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("live buckets after teardown = %d, want 0 (%+v)", len(live), live)
	}
	if store.objectCount() != 1 {
		t.Errorf("objects left in the store = %d, want only the bystander's 1", store.objectCount())
	}
	bystander, err := repo.ListBuckets(t.Context(), 9011)
	if err != nil || len(bystander) != 1 {
		t.Fatalf("bystander buckets = %v (err %v), want its 1 bucket untouched", bystander, err)
	}
}
