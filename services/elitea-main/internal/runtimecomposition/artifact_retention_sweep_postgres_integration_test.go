package runtimecomposition

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	schedulingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/scheduling"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

const artifactRetentionPostgresIntegrationDatabaseURL = "ELITEA_TEST_DATABASE_URL"

func newArtifactRetentionPostgresPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := testpg.DSN(os.Getenv(artifactRetentionPostgresIntegrationDatabaseURL))
	if databaseURL == "" {
		t.Skipf("set %s to run the artifact retention sweep PostgreSQL integration tests", artifactRetentionPostgresIntegrationDatabaseURL)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", artifactRetentionPostgresIntegrationDatabaseURL, err)
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

	databaseName := fmt.Sprintf("elitea_artifact_retention_it_%d_%d", os.Getpid(), time.Now().UnixNano())
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

// applyArtifactRetentionSharedMigrations applies every embedded shared
// migration plus the minimum externally-owned-table stubs elitea-main
// itself never creates: centry.project (id, owner_id, create_success,
// suspended — see internal/application/artifactbootstrap's S13 integration
// test for the create_success/suspended half of this) and
// centry.notifications (see internal/db/schema/notifications_baseline.sql,
// which documents that table as owned by the platform schema lifecycle,
// not a runtime migration).
func applyArtifactRetentionSharedMigrations(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
CREATE SCHEMA centry;
CREATE TABLE centry.project (
    id INTEGER PRIMARY KEY,
    owner_id INTEGER NOT NULL,
    create_success BOOLEAN NOT NULL DEFAULT TRUE,
    suspended BOOLEAN NOT NULL DEFAULT FALSE
);
CREATE TABLE centry.notifications (
    id serial PRIMARY KEY,
    uuid uuid NOT NULL UNIQUE,
    is_seen boolean NOT NULL,
    project_id integer NOT NULL,
    user_id integer NOT NULL,
    meta jsonb NOT NULL,
    event_type varchar NOT NULL,
    created_at timestamp NOT NULL DEFAULT now(),
    updated_at timestamp
);`); err != nil {
		t.Fatalf("stub centry.project/centry.notifications: %v", err)
	}
	runner := migrate.New(pool, platformmigrations.Files)
	if err := runner.ApplyShared(ctx); err != nil {
		t.Fatalf("apply embedded shared migrations: %v", err)
	}
}

func newArtifactRetentionPostgresSweep(t *testing.T) (*artifactRetentionSweep, *repos.ArtifactBucketsRepository, *repos.ArtifactObjectsRepository, *pgxpool.Pool, *artifactRetentionIntegrationFakeStore) {
	t.Helper()
	pool := newArtifactRetentionPostgresPool(t)
	applyArtifactRetentionSharedMigrations(t, pool)

	buckets, err := repos.NewArtifactBucketsRepository(pool)
	if err != nil {
		t.Fatalf("NewArtifactBucketsRepository: %v", err)
	}
	objects, err := repos.NewArtifactObjectsRepository(pool)
	if err != nil {
		t.Fatalf("NewArtifactObjectsRepository: %v", err)
	}
	notifications, err := repos.NewArtifactRetentionNotificationRepository(pool)
	if err != nil {
		t.Fatalf("NewArtifactRetentionNotificationRepository: %v", err)
	}
	grants, err := repos.NewArtifactTransferGrantsRepository(pool)
	if err != nil {
		t.Fatalf("NewArtifactTransferGrantsRepository: %v", err)
	}
	store := newArtifactRetentionIntegrationFakeStore()
	sweep, err := newArtifactRetentionSweep(objects, buckets, notifications, grants, store)
	if err != nil {
		t.Fatalf("newArtifactRetentionSweep: %v", err)
	}
	return sweep, buckets, objects, pool, store
}

func TestArtifactRetentionSweepDeletesExpiredObjectAndMetadataAgainstRealPostgres(t *testing.T) {
	sweep, buckets, objects, _, store := newArtifactRetentionPostgresSweep(t)
	const projectID = int64(9101)

	bucket, err := buckets.CreateBucket(t.Context(), repos.NewBucketInput{
		ProjectID: projectID, Name: "reports", DisplayName: "reports", BucketType: "local",
	})
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	past := time.Now().Add(-time.Hour)
	if _, err := objects.UpsertObject(t.Context(), repos.NewObjectInput{
		BucketID: bucket.ID, Key: "old.png", ByteLength: 10, MediaType: "image/png", ExpiresAt: &past,
	}); err != nil {
		t.Fatalf("seed UpsertObject: %v", err)
	}
	store.seed(fmt.Sprintf("p/%d/b/reports/o/old.png", projectID))

	outcome, err := sweep.Execute(t.Context(), artifactRetentionOccurrence(time.Now()))
	if err != nil || outcome != schedulingapp.OutcomeLocalCompleted {
		t.Fatalf("outcome=%q error=%v", outcome, err)
	}

	remaining, err := objects.ListObjects(t.Context(), bucket.ID, "")
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("expected no object metadata rows after the sweep, got %d: %+v", len(remaining), remaining)
	}
	if store.exists(fmt.Sprintf("p/%d/b/reports/o/old.png", projectID)) {
		t.Error("expected the expired object's physical bytes to be deleted")
	}
}

func TestArtifactRetentionSweepNotifiesAndMarksBucketAgainstRealPostgres(t *testing.T) {
	sweep, buckets, _, pool, _ := newArtifactRetentionPostgresSweep(t)
	const projectID = int64(9102)
	const ownerUserID = int64(4242)

	if _, err := pool.Exec(t.Context(),
		"INSERT INTO centry.project (id, owner_id) VALUES ($1, $2)", projectID, ownerUserID,
	); err != nil {
		t.Fatalf("seed centry.project: %v", err)
	}

	bucket, err := buckets.CreateBucket(t.Context(), repos.NewBucketInput{
		ProjectID: projectID, Name: "reports", DisplayName: "reports", BucketType: "local",
		RetentionDays: int32ptr(1),
		ExpiresAt:     timeptr(time.Now().Add(2 * time.Hour)), // within the 24h notify window
	})
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	outcome, err := sweep.Execute(t.Context(), artifactRetentionOccurrence(time.Now()))
	if err != nil || outcome != schedulingapp.OutcomeLocalCompleted {
		t.Fatalf("outcome=%q error=%v", outcome, err)
	}

	updated, err := buckets.GetBucket(t.Context(), projectID, "reports")
	if err != nil {
		t.Fatalf("GetBucket: %v", err)
	}
	if updated.NotifiedAt == nil {
		t.Fatal("expected NotifiedAt to be set after the sweep notified this bucket")
	}

	var (
		gotProjectID, gotUserID int64
		gotEventType            string
		gotMeta                 []byte
	)
	row := pool.QueryRow(t.Context(),
		"SELECT project_id, user_id, event_type, meta FROM centry.notifications WHERE project_id = $1", projectID,
	)
	if err := row.Scan(&gotProjectID, &gotUserID, &gotEventType, &gotMeta); err != nil {
		t.Fatalf("query centry.notifications: %v", err)
	}
	if gotProjectID != projectID || gotUserID != ownerUserID || gotEventType != "artifact_bucket_expiring" {
		t.Fatalf("notification row = (project=%d user=%d event=%q), want (project=%d user=%d event=artifact_bucket_expiring)",
			gotProjectID, gotUserID, gotEventType, projectID, ownerUserID)
	}
	var meta map[string]any
	if err := json.Unmarshal(gotMeta, &meta); err != nil {
		t.Fatalf("unmarshal notification meta: %v", err)
	}
	if int64(meta["bucket_id"].(float64)) != bucket.ID {
		t.Errorf("notification meta bucket_id = %v, want %d", meta["bucket_id"], bucket.ID)
	}

	// A second tick must not insert a duplicate notification — notified_at
	// is now set, so ListBucketsNeedingExpiryNotice excludes this bucket.
	if _, err := sweep.Execute(t.Context(), artifactRetentionOccurrence(time.Now())); err != nil {
		t.Fatalf("second Execute: %v", err)
	}
	var count int
	if err := pool.QueryRow(t.Context(),
		"SELECT COUNT(*) FROM centry.notifications WHERE project_id = $1", projectID,
	).Scan(&count); err != nil {
		t.Fatalf("count centry.notifications: %v", err)
	}
	if count != 1 {
		t.Errorf("expected exactly 1 notification after two ticks, got %d", count)
	}
}

func int32ptr(v int32) *int32        { return &v }
func timeptr(t time.Time) *time.Time { return &t }

// artifactRetentionIntegrationFakeStore is a minimal ObjectStore double for
// this file's tests — only DeleteBatch sees real use.
type artifactRetentionIntegrationFakeStore struct {
	mu     sync.Mutex
	byKeys map[string]bool
}

func newArtifactRetentionIntegrationFakeStore() *artifactRetentionIntegrationFakeStore {
	return &artifactRetentionIntegrationFakeStore{byKeys: map[string]bool{}}
}

func (s *artifactRetentionIntegrationFakeStore) seed(storageKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byKeys[storageKey] = true
}

func (s *artifactRetentionIntegrationFakeStore) exists(storageKey string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byKeys[storageKey]
}

func (s *artifactRetentionIntegrationFakeStore) DeleteBatch(_ context.Context, refs []storage.ObjectRef) (storage.BatchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := storage.BatchResult{}
	for _, ref := range refs {
		delete(s.byKeys, ref.StorageKey(""))
		result.Deleted = append(result.Deleted, ref.Key())
	}
	return result, nil
}

func (s *artifactRetentionIntegrationFakeStore) Put(context.Context, storage.ObjectRef, io.Reader, storage.PutOptions) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, storage.ErrNotSupported
}

func (s *artifactRetentionIntegrationFakeStore) Get(context.Context, storage.ObjectRef, *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	return nil, storage.ObjectInfo{}, storage.ErrNotSupported
}

func (s *artifactRetentionIntegrationFakeStore) Stat(context.Context, storage.ObjectRef) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, storage.ErrNotSupported
}

func (s *artifactRetentionIntegrationFakeStore) Delete(context.Context, storage.ObjectRef) error {
	return storage.ErrNotSupported
}

func (s *artifactRetentionIntegrationFakeStore) List(context.Context, storage.ListQuery) (storage.ListPage, error) {
	return storage.ListPage{}, storage.ErrNotSupported
}

func (s *artifactRetentionIntegrationFakeStore) PresignGet(context.Context, storage.ObjectRef, time.Duration) (string, error) {
	return "", storage.ErrNotSupported
}

func (s *artifactRetentionIntegrationFakeStore) PresignPut(context.Context, storage.ObjectRef, time.Duration, storage.PutOptions) (string, error) {
	return "", storage.ErrNotSupported
}

func (s *artifactRetentionIntegrationFakeStore) StartMultipart(context.Context, storage.ObjectRef, storage.PutOptions) (storage.UploadID, error) {
	return "", storage.ErrNotSupported
}

func (s *artifactRetentionIntegrationFakeStore) PresignPart(context.Context, storage.ObjectRef, storage.UploadID, int32, time.Duration) (string, error) {
	return "", storage.ErrNotSupported
}

func (s *artifactRetentionIntegrationFakeStore) CompleteMultipart(context.Context, storage.ObjectRef, storage.UploadID, []storage.Part) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, storage.ErrNotSupported
}

func (s *artifactRetentionIntegrationFakeStore) AbortMultipart(context.Context, storage.ObjectRef, storage.UploadID) error {
	return storage.ErrNotSupported
}

func (s *artifactRetentionIntegrationFakeStore) Capabilities() storage.Capabilities {
	return storage.Capabilities{}
}

var _ storage.ObjectStore = (*artifactRetentionIntegrationFakeStore)(nil)
