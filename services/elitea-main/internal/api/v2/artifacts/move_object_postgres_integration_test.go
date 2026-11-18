package artifacts_test

import (
	"net/http"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// The rename over a REAL migrated database: the metadata rows (which carry
// the project's quota accounting) move with the bytes, not just the store key.
func TestMoveObjectMovesThePostgresMetadataRow(t *testing.T) {
	pool := newSDKArtifactPool(t)
	seedPostgresBucket(t, pool, 1, "reports")
	handler, store := newPostgresArtifactHandler(t, pool)
	store.seedContent("1", "reports", "a/old.pdf", []byte("pdf-bytes"), "application/pdf")

	objects, err := repos.NewArtifactObjectsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	buckets, err := repos.NewArtifactBucketsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	bucket, err := buckets.GetBucket(t.Context(), 1, "reports")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := objects.UpsertObject(t.Context(), newObjectInput(bucket.ID, "a/old.pdf", 9)); err != nil {
		t.Fatal(err)
	}

	rr := postMove(moveRouter(handler), `{"from":"a/old.pdf","to":"a/new.pdf"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	rows, err := objects.ListObjects(t.Context(), bucket.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Key != "a/new.pdf" || rows[0].ByteLength != 9 {
		t.Fatalf("metadata rows = %+v, want exactly a/new.pdf (9 bytes)", rows)
	}
}
