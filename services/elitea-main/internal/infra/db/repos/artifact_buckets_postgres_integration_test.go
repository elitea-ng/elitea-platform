package repos

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

func newArtifactBucketsTestRepo(t *testing.T) *ArtifactBucketsRepository {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	repo, err := NewArtifactBucketsRepository(pool)
	if err != nil {
		t.Fatalf("NewArtifactBucketsRepository: %v", err)
	}
	return repo
}

func mustCreateArtifactBucket(t *testing.T, repo *ArtifactBucketsRepository, projectID int64, name string) BucketRow {
	t.Helper()
	bucket, err := repo.CreateBucket(context.Background(), NewBucketInput{
		ProjectID:   projectID,
		Name:        name,
		DisplayName: name,
		BucketType:  "local",
	})
	if err != nil {
		t.Fatalf("CreateBucket(%q): %v", name, err)
	}
	return bucket
}

func TestArtifactBucketsRepositoryCreatesAndLists(t *testing.T) {
	repo := newArtifactBucketsTestRepo(t)
	ctx := context.Background()
	const projectID = int64(9001)

	created := mustCreateArtifactBucket(t, repo, projectID, "reports")
	if created.ID == 0 {
		t.Fatal("CreateBucket returned zero ID")
	}
	if created.ProjectID != projectID || created.Name != "reports" {
		t.Fatalf("CreateBucket row = %+v, want ProjectID=%d Name=reports", created, projectID)
	}
	if created.BucketType != "local" || created.IsPinned {
		t.Fatalf("CreateBucket defaults wrong: %+v", created)
	}

	list, err := repo.ListBuckets(ctx, projectID)
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	if len(list) != 1 || list[0].Name != "reports" {
		t.Fatalf("ListBuckets = %+v, want one bucket named reports", list)
	}

	got, err := repo.GetBucket(ctx, projectID, "reports")
	if err != nil {
		t.Fatalf("GetBucket: %v", err)
	}
	if got.ID != created.ID {
		t.Fatalf("GetBucket ID = %d, want %d", got.ID, created.ID)
	}
}

func TestArtifactBucketsRepositoryRejectsDuplicateName(t *testing.T) {
	repo := newArtifactBucketsTestRepo(t)
	const projectID = int64(9002)
	mustCreateArtifactBucket(t, repo, projectID, "logs")

	_, err := repo.CreateBucket(context.Background(), NewBucketInput{
		ProjectID:   projectID,
		Name:        "logs",
		DisplayName: "logs",
		BucketType:  "local",
	})
	if !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("CreateBucket duplicate err = %v, want ErrAlreadyExists", err)
	}
}

func TestArtifactBucketsRepositoryGetReturnsNotFoundForMissing(t *testing.T) {
	repo := newArtifactBucketsTestRepo(t)
	_, err := repo.GetBucket(context.Background(), 9003, "does-not-exist")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetBucket(missing) err = %v, want ErrNotFound", err)
	}
}

func TestArtifactBucketsRepositoryUpdatesRetention(t *testing.T) {
	repo := newArtifactBucketsTestRepo(t)
	ctx := context.Background()
	bucket := mustCreateArtifactBucket(t, repo, 9004, "temp")

	days := int32(30)
	expires := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second)
	updated, err := repo.UpdateBucketRetention(ctx, bucket.ID, &days, &expires)
	if err != nil {
		t.Fatalf("UpdateBucketRetention: %v", err)
	}
	if updated.RetentionDays == nil || *updated.RetentionDays != 30 {
		t.Fatalf("UpdateBucketRetention.RetentionDays = %v, want 30", updated.RetentionDays)
	}
	if updated.ExpiresAt == nil || !updated.ExpiresAt.Equal(expires) {
		t.Fatalf("UpdateBucketRetention.ExpiresAt = %v, want %v", updated.ExpiresAt, expires)
	}

	if _, err := repo.UpdateBucketRetention(ctx, 999999999, &days, &expires); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("UpdateBucketRetention(missing) err = %v, want ErrNotFound", err)
	}
}

func TestArtifactBucketsRepositorySetsPinned(t *testing.T) {
	repo := newArtifactBucketsTestRepo(t)
	ctx := context.Background()
	bucket := mustCreateArtifactBucket(t, repo, 9005, "pinned-bucket")

	updated, err := repo.SetBucketPinned(ctx, bucket.ID, true)
	if err != nil {
		t.Fatalf("SetBucketPinned: %v", err)
	}
	if !updated.IsPinned {
		t.Fatal("SetBucketPinned(true) left IsPinned false")
	}

	updated, err = repo.SetBucketPinned(ctx, bucket.ID, false)
	if err != nil {
		t.Fatalf("SetBucketPinned: %v", err)
	}
	if updated.IsPinned {
		t.Fatal("SetBucketPinned(false) left IsPinned true")
	}
}

func TestArtifactBucketsRepositoryUpdatesTags(t *testing.T) {
	repo := newArtifactBucketsTestRepo(t)
	ctx := context.Background()
	bucket := mustCreateArtifactBucket(t, repo, 9006, "tagged-bucket")

	tags := json.RawMessage(`{"team":"platform","env":"prod"}`)
	updated, err := repo.UpdateBucketTags(ctx, bucket.ID, tags)
	if err != nil {
		t.Fatalf("UpdateBucketTags: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(updated.Tags, &got); err != nil {
		t.Fatalf("unmarshal updated tags: %v", err)
	}
	if got["team"] != "platform" || got["env"] != "prod" {
		t.Fatalf("UpdateBucketTags round-trip = %v, want team=platform env=prod", got)
	}
}

func TestArtifactBucketsRepositorySoftDeleteExcludesFromListAndGet(t *testing.T) {
	repo := newArtifactBucketsTestRepo(t)
	ctx := context.Background()
	const projectID = int64(9007)
	bucket := mustCreateArtifactBucket(t, repo, projectID, "ephemeral")

	if err := repo.SoftDeleteBucket(ctx, bucket.ID); err != nil {
		t.Fatalf("SoftDeleteBucket: %v", err)
	}

	if _, err := repo.GetBucket(ctx, projectID, "ephemeral"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetBucket after soft delete err = %v, want ErrNotFound", err)
	}
	list, err := repo.ListBuckets(ctx, projectID)
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("ListBuckets after soft delete = %+v, want empty", list)
	}
}

func TestArtifactBucketsRepositorySoftDeleteMissingReturnsNotFound(t *testing.T) {
	repo := newArtifactBucketsTestRepo(t)
	if err := repo.SoftDeleteBucket(context.Background(), 999999999); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("SoftDeleteBucket(missing) err = %v, want ErrNotFound", err)
	}
}

func TestArtifactBucketsRepositoryListsBucketsNeedingExpiryNotice(t *testing.T) {
	repo := newArtifactBucketsTestRepo(t)
	ctx := context.Background()
	const projectID = int64(9008)

	soon := mustCreateArtifactBucket(t, repo, projectID, "expiring-soon")
	days := int32(1)
	expiresSoon := time.Now().Add(12 * time.Hour).UTC().Truncate(time.Second)
	if _, err := repo.UpdateBucketRetention(ctx, soon.ID, &days, &expiresSoon); err != nil {
		t.Fatalf("UpdateBucketRetention(soon): %v", err)
	}

	far := mustCreateArtifactBucket(t, repo, projectID, "expiring-far")
	expiresFar := time.Now().Add(365 * 24 * time.Hour).UTC().Truncate(time.Second)
	if _, err := repo.UpdateBucketRetention(ctx, far.ID, &days, &expiresFar); err != nil {
		t.Fatalf("UpdateBucketRetention(far): %v", err)
	}

	due, err := repo.ListBucketsNeedingExpiryNotice(ctx, 24*time.Hour, 10)
	if err != nil {
		t.Fatalf("ListBucketsNeedingExpiryNotice: %v", err)
	}
	if len(due) != 1 || due[0].ID != soon.ID {
		t.Fatalf("ListBucketsNeedingExpiryNotice = %+v, want exactly [%d]", due, soon.ID)
	}

	if err := repo.MarkBucketNotified(ctx, soon.ID); err != nil {
		t.Fatalf("MarkBucketNotified: %v", err)
	}
	due, err = repo.ListBucketsNeedingExpiryNotice(ctx, 24*time.Hour, 10)
	if err != nil {
		t.Fatalf("ListBucketsNeedingExpiryNotice after notify: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("ListBucketsNeedingExpiryNotice after notify = %+v, want empty (deduplicated)", due)
	}
}

func TestArtifactBucketsRepositoryMarksBucketNotifiedMissingReturnsNotFound(t *testing.T) {
	repo := newArtifactBucketsTestRepo(t)
	if err := repo.MarkBucketNotified(context.Background(), 999999999); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("MarkBucketNotified(missing) err = %v, want ErrNotFound", err)
	}
}

// S18: ListProjectIDsWithBuckets backs the retention sweeper's per-project
// byte-usage gauge — it must return each project exactly once even when a
// project owns multiple buckets, must exclude a project whose only bucket is
// soft-deleted, and must exclude a project with no buckets at all.
func TestArtifactBucketsRepositoryListsProjectIDsWithBuckets(t *testing.T) {
	repo := newArtifactBucketsTestRepo(t)
	ctx := context.Background()

	mustCreateArtifactBucket(t, repo, 100, "reports")
	mustCreateArtifactBucket(t, repo, 100, "exports")
	mustCreateArtifactBucket(t, repo, 200, "reports")
	deletedOnly := mustCreateArtifactBucket(t, repo, 300, "reports")
	if err := repo.SoftDeleteBucket(ctx, deletedOnly.ID); err != nil {
		t.Fatalf("SoftDeleteBucket: %v", err)
	}

	got, err := repo.ListProjectIDsWithBuckets(ctx)
	if err != nil {
		t.Fatalf("ListProjectIDsWithBuckets: %v", err)
	}
	want := []int64{100, 200}
	if len(got) != len(want) {
		t.Fatalf("ListProjectIDsWithBuckets = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ListProjectIDsWithBuckets = %v, want %v", got, want)
		}
	}
}

// Review round 3: a leftover bucket a person made under the index-artifacts
// name is refused with ErrSystemBucketNameTaken (409 at the routes) by both
// users of the bucket, the index result writer and the discovery inventory;
// the platform's own system bucket is created and returned.
func TestIndexArtifactBucketRefusesALeftoverNonSystemBucket(t *testing.T) {
	repo := newArtifactBucketsTestRepo(t)
	ctx := context.Background()
	mustCreateArtifactBucket(t, repo, 1, indexArtifactBucketName)
	if _, err := requireIndexArtifactBucket(ctx, repo, 1); !errors.Is(err, storage.ErrSystemBucketNameTaken) {
		t.Fatalf("a leftover local bucket = %v, want ErrSystemBucketNameTaken", err)
	}
	row, err := requireIndexArtifactBucket(ctx, repo, 2)
	if err != nil || row.BucketType != "system" || row.Name != indexArtifactBucketName {
		t.Fatalf("first use = %+v, %v; want the created system bucket", row, err)
	}
	if again, err := requireIndexArtifactBucket(ctx, repo, 2); err != nil || again.ID != row.ID {
		t.Fatalf("second use = %+v, %v; want the same bucket", again, err)
	}
}
