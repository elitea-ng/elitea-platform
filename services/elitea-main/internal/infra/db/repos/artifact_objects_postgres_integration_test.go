package repos

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// newArtifactObjectsTestRepo returns both repositories over one pool: object
// tests almost always need a bucket to hang objects off first.
func newArtifactObjectsTestRepo(t *testing.T) (*ArtifactBucketsRepository, *ArtifactObjectsRepository, *pgxpool.Pool) {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	buckets, err := NewArtifactBucketsRepository(pool)
	if err != nil {
		t.Fatalf("NewArtifactBucketsRepository: %v", err)
	}
	objects, err := NewArtifactObjectsRepository(pool)
	if err != nil {
		t.Fatalf("NewArtifactObjectsRepository: %v", err)
	}
	return buckets, objects, pool
}

func TestArtifactObjectsRepositoryUpsertInsertsThenUpdates(t *testing.T) {
	buckets, objects, _ := newArtifactObjectsTestRepo(t)
	ctx := context.Background()
	bucket := mustCreateArtifactBucket(t, buckets, 9101, "upsert-bucket")

	digestAlg := "sha256"
	digest := []byte{1, 2, 3, 4}
	inserted, err := objects.UpsertObject(ctx, NewObjectInput{
		BucketID:   bucket.ID,
		Key:        "reports/q1.csv",
		ByteLength: 1024,
		MediaType:  "text/csv",
		DigestAlg:  &digestAlg,
		Digest:     digest,
	})
	if err != nil {
		t.Fatalf("UpsertObject (insert): %v", err)
	}
	if inserted.Classification != "internal" || inserted.ScanState != "not_scanned" {
		t.Fatalf("UpsertObject defaults wrong: %+v", inserted)
	}
	if inserted.ByteLength != 1024 {
		t.Fatalf("UpsertObject.ByteLength = %d, want 1024", inserted.ByteLength)
	}

	updated, err := objects.UpsertObject(ctx, NewObjectInput{
		BucketID:   bucket.ID,
		Key:        "reports/q1.csv",
		ByteLength: 2048,
		MediaType:  "text/csv",
		DigestAlg:  &digestAlg,
		Digest:     digest,
	})
	if err != nil {
		t.Fatalf("UpsertObject (update): %v", err)
	}
	if updated.ID != inserted.ID {
		t.Fatalf("UpsertObject on conflict changed id: got %d, want %d", updated.ID, inserted.ID)
	}
	if updated.ByteLength != 2048 {
		t.Fatalf("UpsertObject on conflict ByteLength = %d, want 2048", updated.ByteLength)
	}
}

func TestArtifactObjectsRepositoryListFiltersByPrefix(t *testing.T) {
	buckets, objects, _ := newArtifactObjectsTestRepo(t)
	ctx := context.Background()
	bucket := mustCreateArtifactBucket(t, buckets, 9102, "list-bucket")

	for _, key := range []string{"match/one.txt", "match/two.txt", "other/three.txt"} {
		if _, err := objects.UpsertObject(ctx, NewObjectInput{BucketID: bucket.ID, Key: key, ByteLength: 1, MediaType: "text/plain"}); err != nil {
			t.Fatalf("UpsertObject(%q): %v", key, err)
		}
	}

	matched, err := objects.ListObjects(ctx, bucket.ID, "match/")
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(matched) != 2 {
		t.Fatalf("ListObjects(match/) = %+v, want 2 rows", matched)
	}

	all, err := objects.ListObjects(ctx, bucket.ID, "")
	if err != nil {
		t.Fatalf("ListObjects(\"\"): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("ListObjects(\"\") = %+v, want 3 rows", all)
	}
}

func TestArtifactObjectsRepositoryDeleteRemovesRows(t *testing.T) {
	buckets, objects, _ := newArtifactObjectsTestRepo(t)
	ctx := context.Background()
	bucket := mustCreateArtifactBucket(t, buckets, 9103, "delete-bucket")

	for _, key := range []string{"a.txt", "b.txt", "c.txt"} {
		if _, err := objects.UpsertObject(ctx, NewObjectInput{BucketID: bucket.ID, Key: key, ByteLength: 1, MediaType: "text/plain"}); err != nil {
			t.Fatalf("UpsertObject(%q): %v", key, err)
		}
	}

	if err := objects.DeleteObjects(ctx, bucket.ID, []string{"a.txt", "c.txt"}); err != nil {
		t.Fatalf("DeleteObjects: %v", err)
	}
	remaining, err := objects.ListObjects(ctx, bucket.ID, "")
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(remaining) != 1 || remaining[0].Key != "b.txt" {
		t.Fatalf("ListObjects after delete = %+v, want only b.txt", remaining)
	}

	if err := objects.DeleteObjects(ctx, bucket.ID, nil); err != nil {
		t.Fatalf("DeleteObjects(empty) err = %v, want nil", err)
	}
}

func TestArtifactSumBucketBytes(t *testing.T) {
	buckets, objects, _ := newArtifactObjectsTestRepo(t)
	ctx := context.Background()
	bucket := mustCreateArtifactBucket(t, buckets, 9104, "sum-bucket")

	for i, size := range []int64{100, 250, 4096} {
		key := []string{"a", "b", "c"}[i]
		if _, err := objects.UpsertObject(ctx, NewObjectInput{BucketID: bucket.ID, Key: key, ByteLength: size, MediaType: "application/octet-stream"}); err != nil {
			t.Fatalf("UpsertObject(%q): %v", key, err)
		}
	}

	sum, err := objects.SumBucketBytes(ctx, bucket.ID)
	if err != nil {
		t.Fatalf("SumBucketBytes: %v", err)
	}
	if sum != 100+250+4096 {
		t.Fatalf("SumBucketBytes = %d, want %d", sum, 100+250+4096)
	}

	empty, err := buckets.CreateBucket(ctx, NewBucketInput{ProjectID: 9104, Name: "empty-bucket", DisplayName: "empty", BucketType: "local"})
	if err != nil {
		t.Fatalf("CreateBucket(empty): %v", err)
	}
	sum, err = objects.SumBucketBytes(ctx, empty.ID)
	if err != nil {
		t.Fatalf("SumBucketBytes(empty): %v", err)
	}
	if sum != 0 {
		t.Fatalf("SumBucketBytes(empty) = %d, want 0", sum)
	}
}

func TestArtifactCountBucketObjectsExcludesSoftDeleted(t *testing.T) {
	buckets, objects, _ := newArtifactObjectsTestRepo(t)
	ctx := context.Background()
	bucket := mustCreateArtifactBucket(t, buckets, 9105, "count-bucket")

	for _, key := range []string{"a", "b", "c"} {
		if _, err := objects.UpsertObject(ctx, NewObjectInput{BucketID: bucket.ID, Key: key, ByteLength: 1, MediaType: "application/octet-stream"}); err != nil {
			t.Fatalf("UpsertObject(%q): %v", key, err)
		}
	}

	count, err := objects.CountBucketObjects(ctx, bucket.ID)
	if err != nil {
		t.Fatalf("CountBucketObjects: %v", err)
	}
	if count != 3 {
		t.Fatalf("CountBucketObjects before soft delete = %d, want 3", count)
	}

	// Soft-deleting the bucket doesn't cascade-delete its object rows (only a
	// hard DELETE, via ON DELETE CASCADE, does that) — CountBucketObjects
	// must still report 0 for a bucket that no longer "exists" from the
	// caller's perspective.
	if err := buckets.SoftDeleteBucket(ctx, bucket.ID); err != nil {
		t.Fatalf("SoftDeleteBucket: %v", err)
	}
	count, err = objects.CountBucketObjects(ctx, bucket.ID)
	if err != nil {
		t.Fatalf("CountBucketObjects after soft delete: %v", err)
	}
	if count != 0 {
		t.Fatalf("CountBucketObjects after soft delete = %d, want 0 (excludes soft-deleted buckets)", count)
	}
}

func TestArtifactSumProjectBytesAggregatesAcrossBuckets(t *testing.T) {
	buckets, objects, _ := newArtifactObjectsTestRepo(t)
	ctx := context.Background()
	const projectID = int64(9106)

	bucketA := mustCreateArtifactBucket(t, buckets, projectID, "bucket-a")
	bucketB := mustCreateArtifactBucket(t, buckets, projectID, "bucket-b")
	otherProjectBucket := mustCreateArtifactBucket(t, buckets, 9107, "other-project-bucket")

	if _, err := objects.UpsertObject(ctx, NewObjectInput{BucketID: bucketA.ID, Key: "x", ByteLength: 100, MediaType: "application/octet-stream"}); err != nil {
		t.Fatalf("UpsertObject(a): %v", err)
	}
	if _, err := objects.UpsertObject(ctx, NewObjectInput{BucketID: bucketB.ID, Key: "y", ByteLength: 200, MediaType: "application/octet-stream"}); err != nil {
		t.Fatalf("UpsertObject(b): %v", err)
	}
	if _, err := objects.UpsertObject(ctx, NewObjectInput{BucketID: otherProjectBucket.ID, Key: "z", ByteLength: 999999, MediaType: "application/octet-stream"}); err != nil {
		t.Fatalf("UpsertObject(other project): %v", err)
	}

	sum, err := objects.SumProjectBytes(ctx, projectID)
	if err != nil {
		t.Fatalf("SumProjectBytes: %v", err)
	}
	if sum != 300 {
		t.Fatalf("SumProjectBytes = %d, want 300 (must not include the other project's bucket)", sum)
	}

	if err := buckets.SoftDeleteBucket(ctx, bucketB.ID); err != nil {
		t.Fatalf("SoftDeleteBucket(b): %v", err)
	}
	sum, err = objects.SumProjectBytes(ctx, projectID)
	if err != nil {
		t.Fatalf("SumProjectBytes after soft delete: %v", err)
	}
	if sum != 100 {
		t.Fatalf("SumProjectBytes after soft-deleting bucket-b = %d, want 100", sum)
	}
}

func TestArtifactGetProjectStoragePolicyReturnsDefaultsWhenMissing(t *testing.T) {
	_, objects, _ := newArtifactObjectsTestRepo(t)
	policy, err := objects.GetProjectStoragePolicy(context.Background(), 9108)
	if err != nil {
		t.Fatalf("GetProjectStoragePolicy(missing) err = %v, want nil", err)
	}
	if policy.MaxObjectBytes != nil || policy.MaxTotalBytes != nil || policy.RetentionDefaultDays != nil || policy.RetentionMaxDays != nil || policy.AttachmentBucket != nil {
		t.Fatalf("GetProjectStoragePolicy(missing) = %+v, want every field nil (unlimited/default)", policy)
	}
}

func TestArtifactGetProjectStoragePolicyReturnsStoredRow(t *testing.T) {
	_, objects, pool := newArtifactObjectsTestRepo(t)
	ctx := context.Background()
	const projectID = int64(9109)

	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_storage.project_storage_policy (
    project_id, max_object_bytes, max_total_bytes, retention_default_days,
    retention_max_days, attachment_bucket
) VALUES ($1, $2, $3, $4, $5, $6)`,
		projectID, int64(150<<20), int64(10<<30), int32(365), int32(3650), "chat-attachments",
	); err != nil {
		t.Fatalf("seed project_storage_policy: %v", err)
	}

	policy, err := objects.GetProjectStoragePolicy(ctx, projectID)
	if err != nil {
		t.Fatalf("GetProjectStoragePolicy: %v", err)
	}
	if policy.MaxObjectBytes == nil || *policy.MaxObjectBytes != 150<<20 {
		t.Fatalf("GetProjectStoragePolicy.MaxObjectBytes = %v, want %d", policy.MaxObjectBytes, int64(150<<20))
	}
	if policy.AttachmentBucket == nil || *policy.AttachmentBucket != "chat-attachments" {
		t.Fatalf("GetProjectStoragePolicy.AttachmentBucket = %v, want chat-attachments", policy.AttachmentBucket)
	}
}

func TestArtifactListExpiredObjectsReturnsBoundedBatch(t *testing.T) {
	buckets, objects, _ := newArtifactObjectsTestRepo(t)
	ctx := context.Background()
	bucket := mustCreateArtifactBucket(t, buckets, 9110, "expiry-bucket")

	past := time.Now().Add(-1 * time.Hour).UTC()
	future := time.Now().Add(1 * time.Hour).UTC()
	for _, tc := range []struct {
		key       string
		expiresAt *time.Time
	}{
		{"expired-1", &past},
		{"expired-2", &past},
		{"expired-3", &past},
		{"not-expired", &future},
		{"never-expires", nil},
	} {
		if _, err := objects.UpsertObject(ctx, NewObjectInput{BucketID: bucket.ID, Key: tc.key, ByteLength: 1, MediaType: "application/octet-stream", ExpiresAt: tc.expiresAt}); err != nil {
			t.Fatalf("UpsertObject(%q): %v", tc.key, err)
		}
	}

	expired, err := objects.ListExpiredObjects(ctx, time.Now().UTC(), 2)
	if err != nil {
		t.Fatalf("ListExpiredObjects: %v", err)
	}
	if len(expired) != 2 {
		t.Fatalf("ListExpiredObjects(limit=2) = %+v, want 2 rows (bounded batch)", expired)
	}
	for _, row := range expired {
		if row.ExpiresAt == nil || !row.ExpiresAt.Before(time.Now()) {
			t.Fatalf("ListExpiredObjects returned a non-expired row: %+v", row)
		}
	}

	allExpired, err := objects.ListExpiredObjects(ctx, time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("ListExpiredObjects: %v", err)
	}
	if len(allExpired) != 3 {
		t.Fatalf("ListExpiredObjects(limit=10) = %+v, want exactly the 3 expired rows", allExpired)
	}
}

func TestArtifactDeleteObjectRowsRemovesByID(t *testing.T) {
	buckets, objects, _ := newArtifactObjectsTestRepo(t)
	ctx := context.Background()
	bucket := mustCreateArtifactBucket(t, buckets, 9111, "sweep-bucket")

	past := time.Now().Add(-1 * time.Hour).UTC()
	var ids []int64
	for _, key := range []string{"x", "y"} {
		row, err := objects.UpsertObject(ctx, NewObjectInput{BucketID: bucket.ID, Key: key, ByteLength: 1, MediaType: "application/octet-stream", ExpiresAt: &past})
		if err != nil {
			t.Fatalf("UpsertObject(%q): %v", key, err)
		}
		ids = append(ids, row.ID)
	}

	if err := objects.DeleteObjectRows(ctx, ids); err != nil {
		t.Fatalf("DeleteObjectRows: %v", err)
	}
	remaining, err := objects.ListObjects(ctx, bucket.ID, "")
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("ListObjects after DeleteObjectRows = %+v, want empty", remaining)
	}

	if err := objects.DeleteObjectRows(ctx, nil); err != nil {
		t.Fatalf("DeleteObjectRows(empty) err = %v, want nil", err)
	}
}

// #1059: concurrent reservations of one key — exactly one wins, and a
// reservation never overwrites a stored object's row.
func TestArtifactObjectsRepositoryReserveObjectKeyHasOneWinner(t *testing.T) {
	buckets, objects, _ := newArtifactObjectsTestRepo(t)
	ctx := context.Background()
	bucket := mustCreateArtifactBucket(t, buckets, 9110, "reserve-bucket")

	const callers = 16
	type outcome struct {
		reserved bool
		err      error
	}
	results := make(chan outcome, callers)
	for range callers {
		go func() {
			reserved, err := objects.ReserveObjectKey(ctx, bucket.ID, "conv/photo.jpg", "image/jpeg", nil)
			results <- outcome{reserved, err}
		}()
	}
	winners := 0
	for range callers {
		result := <-results
		if result.err != nil {
			t.Fatalf("ReserveObjectKey: %v", result.err)
		}
		if result.reserved {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("reservations won = %d, want exactly 1", winners)
	}

	if _, err := objects.UpsertObject(ctx, NewObjectInput{BucketID: bucket.ID, Key: "conv/photo.jpg", ByteLength: 42, MediaType: "image/jpeg"}); err != nil {
		t.Fatalf("UpsertObject over the placeholder: %v", err)
	}
	again, err := objects.ReserveObjectKey(ctx, bucket.ID, "conv/photo.jpg", "image/jpeg", nil)
	if err != nil || again {
		t.Fatalf("ReserveObjectKey over a stored object = %v, %v; want false, nil", again, err)
	}
	rows, err := objects.ListObjects(ctx, bucket.ID, "conv/photo.jpg")
	if err != nil || len(rows) != 1 || rows[0].ByteLength != 42 {
		t.Fatalf("stored row after a refused reservation = %+v, %v; want byte_length 42", rows, err)
	}
}
