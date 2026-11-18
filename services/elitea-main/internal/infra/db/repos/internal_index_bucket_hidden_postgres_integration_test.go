package repos

import (
	"context"
	"testing"
)

// #2167: the platform's own index result bucket (a system bucket under its
// reserved name) is answered as absent by every bucket gate, as the workspace
// bucket is, while the same name held by a person-made bucket type, and the
// ordinary and attachment buckets, are not hidden.
func TestPostgresInternalIndexBucketIsHiddenByTheBucketGate(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const project int64 = 1
	for _, bucket := range []struct{ name, kind string }{
		{"index-artifacts", "system"},
		{"chat-attachments", "system"},
		{"reports", "local"},
	} {
		if _, err := pool.Exec(ctx, `
INSERT INTO elitea_storage.buckets (project_id, name, display_name, bucket_type)
VALUES ($1, $2, $2, $3)`, project, bucket.name, bucket.kind); err != nil {
			t.Fatalf("seed %s: %v", bucket.name, err)
		}
	}
	// The same name in another project, held by a person (local type): not hidden.
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_storage.buckets (project_id, name, display_name, bucket_type)
VALUES (2, 'index-artifacts', 'index-artifacts', 'local')`); err != nil {
		t.Fatalf("seed person-made bucket: %v", err)
	}

	buckets, err := NewArtifactBucketsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	permissions, err := NewArtifactBucketPermissionsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		project int64
		name    string
		hidden  bool
	}{
		{project, "index-artifacts", true},
		{project, "chat-attachments", false},
		{project, "reports", false},
		{2, "index-artifacts", false},
		{project, "missing", false},
	} {
		isHidden, err := buckets.IsWorkspaceBucket(ctx, tc.project, tc.name)
		if err != nil || isHidden != tc.hidden {
			t.Errorf("IsWorkspaceBucket(%d, %s) = %v, %v; want %v", tc.project, tc.name, isHidden, err, tc.hidden)
		}
		gate, err := permissions.BucketGate(ctx, tc.project, 0, tc.name)
		if err != nil || gate.Workspace != tc.hidden {
			t.Errorf("BucketGate(%d, %s).Workspace = %v, %v; want %v", tc.project, tc.name, gate.Workspace, err, tc.hidden)
		}
	}
	// The platform's own writers still resolve it: hiding is a person-facing gate.
	if _, err := buckets.GetBucket(ctx, project, "index-artifacts"); err != nil {
		t.Fatalf("GetBucket(index-artifacts) for the index writer = %v", err)
	}
}
