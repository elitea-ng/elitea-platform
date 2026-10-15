package repos

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

// TestPostgresWorkspaceBucketIsAbsentFromEveryArtifactQuery: the hidden
// workspace bucket (ADR-0034 decision 3, shared/0168) exists in
// elitea_storage.buckets with its grants and object rows, and no artifact
// repository method lists, resolves, sums or sweeps it. Each assertion has a
// positive twin on an ordinary bucket of the same project, so an empty answer
// is the filter and not an empty table.
func TestPostgresWorkspaceBucketIsAbsentFromEveryArtifactQuery(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const project int64 = 1

	var workspaceID, ordinaryID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO elitea_storage.buckets (project_id, name, display_name, bucket_type)
VALUES ($1, $2, $2, 'workspace') RETURNING id`, project, storage.WorkspaceBucketName).Scan(&workspaceID); err != nil {
		t.Fatalf("seed workspace bucket: %v", err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO elitea_storage.buckets (project_id, name, display_name, bucket_type)
VALUES ($1, 'reports', 'reports', 'local') RETURNING id`, project).Scan(&ordinaryID); err != nil {
		t.Fatalf("seed ordinary bucket: %v", err)
	}
	// A second workspace bucket in the same project is refused by the index.
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_storage.buckets (project_id, name, display_name, bucket_type)
VALUES ($1, 'elitea-workspaces-2', 'x', 'workspace')`, project); err == nil {
		t.Fatalf("a second workspace bucket per project was accepted")
	}
	for _, seed := range []struct {
		bucket int64
		bytes  int64
	}{{workspaceID, 1000}, {ordinaryID, 7}} {
		if _, err := pool.Exec(ctx, `
INSERT INTO elitea_storage.objects (bucket_id, key, byte_length) VALUES ($1, 'k', $2)`, seed.bucket, seed.bytes); err != nil {
			t.Fatalf("seed object: %v", err)
		}
	}
	past := time.Now().Add(-time.Hour)
	grants := map[int64]string{
		workspaceID: "0b6f9a52-1c2d-4e5f-8a9b-0c1d2e3f4a5b",
		ordinaryID:  "1b6f9a52-1c2d-4e5f-8a9b-0c1d2e3f4a5b",
	}
	for bucket, id := range grants {
		if _, err := pool.Exec(ctx, `
INSERT INTO elitea_storage.transfer_grants (id, project_id, bucket_id, key, method, max_bytes, expires_at)
VALUES ($1::uuid, $2, $3, 'k', 'PUT', 10, $4)`, id, project, bucket, past); err != nil {
			t.Fatalf("seed grant: %v", err)
		}
	}

	buckets, err := NewArtifactBucketsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := buckets.ListBuckets(ctx, project)
	if err != nil || len(listed) != 1 || listed[0].Name != "reports" {
		t.Fatalf("ListBuckets = %+v, %v; want only the ordinary bucket", listed, err)
	}
	if _, err := buckets.GetBucket(ctx, project, storage.WorkspaceBucketName); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetBucket(workspace) = %v, want ErrNotFound", err)
	}
	if _, err := buckets.GetBucketByID(ctx, workspaceID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetBucketByID(workspace) = %v, want ErrNotFound", err)
	}
	if _, err := buckets.GetBucketByID(ctx, ordinaryID); err != nil {
		t.Fatalf("GetBucketByID(ordinary) = %v", err)
	}
	if _, err := buckets.CreateBucket(ctx, NewBucketInput{
		ProjectID: project, Name: storage.WorkspaceBucketName, DisplayName: "x", BucketType: "local",
	}); !errors.Is(err, storage.ErrInvalidKey) {
		t.Fatalf("CreateBucket(reserved name) = %v, want ErrInvalidKey (never ErrAlreadyExists)", err)
	}
	if _, err := buckets.CreateBucket(ctx, NewBucketInput{
		ProjectID: project, Name: "other", DisplayName: "x", BucketType: storage.WorkspaceBucketType,
	}); !errors.Is(err, storage.ErrInvalidKey) {
		t.Fatalf("CreateBucket(workspace type) = %v, want ErrInvalidKey", err)
	}

	objects, err := NewArtifactObjectsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	if total, err := objects.SumProjectBytes(ctx, project); err != nil || total != 7 {
		t.Fatalf("SumProjectBytes = %d, %v; want 7 (the workspace bucket is outside the artifact quota)", total, err)
	}

	transfer, err := NewArtifactTransferGrantsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transfer.GetTransferGrant(ctx, grants[workspaceID], project); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetTransferGrant(workspace) = %v, want ErrNotFound", err)
	}
	if _, err := transfer.GetTransferGrantByID(ctx, grants[workspaceID]); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetTransferGrantByID(workspace) = %v, want ErrNotFound", err)
	}
	if _, err := transfer.GetTransferGrant(ctx, grants[ordinaryID], project); err != nil {
		t.Fatalf("GetTransferGrant(ordinary) = %v", err)
	}
	claimed, err := transfer.ClaimExpiredTransferGrants(ctx, time.Now(), 10)
	if err != nil || len(claimed) != 1 || claimed[0].ID != grants[ordinaryID] {
		t.Fatalf("ClaimExpiredTransferGrants = %+v, %v; want only the ordinary grant", claimed, err)
	}

	// The worker's runtime artifact plane: the actor is a project admin's
	// equal here (no access list row), and the bucket is still absent.
	runtime, err := NewCurrentRuntimeArtifactRepository(pool, newRuntimeArtifactTestStore())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runtime.ListRuntimeArtifacts(ctx, project, 4501, storage.WorkspaceBucketName, "", true, 10); !errors.Is(err, storage.ErrContentNotFound) {
		t.Fatalf("runtime list(workspace) = %v, want ErrContentNotFound", err)
	}
	if _, err := runtime.ReadRuntimeArtifact(ctx, project, 4501, storage.WorkspaceBucketName, "k", 10); !errors.Is(err, storage.ErrContentNotFound) {
		t.Fatalf("runtime read(workspace) = %v, want ErrContentNotFound", err)
	}
	if _, err := runtime.WriteRuntimeArtifact(ctx, project, 4501, storage.WorkspaceBucketName, "k", []byte("x")); !errors.Is(err, storage.ErrContentNotFound) {
		t.Fatalf("runtime write(workspace) = %v, want ErrContentNotFound", err)
	}
	if err := runtime.DeleteRuntimeArtifact(ctx, project, 4501, storage.WorkspaceBucketName, "k"); !errors.Is(err, storage.ErrContentNotFound) {
		t.Fatalf("runtime delete(workspace) = %v, want ErrContentNotFound", err)
	}
	if _, _, err := runtime.ListRuntimeArtifacts(ctx, project, 4501, "reports", "", true, 10); err != nil {
		t.Fatalf("runtime list(ordinary) = %v", err)
	}
}
