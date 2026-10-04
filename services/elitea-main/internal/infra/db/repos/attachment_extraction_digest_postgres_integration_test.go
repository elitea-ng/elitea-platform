package repos

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/extract"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

// TestAttachmentExtractionDigestFindsAByteIdenticalReupload pins the digest
// lookup (shared/0138): the chat client uploads a file again with every
// message, the upsert moves updated_at, and the filed extraction must still
// answer for the same bytes. It also pins the scope: another project, or a
// bucket of the same name that is not the system bucket, reaches nothing.
func TestAttachmentExtractionDigestFindsAByteIdenticalReupload(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()

	const (
		project      = int64(90312)
		otherProject = int64(90313)
		bucketName   = "chat-attachments"
		conversation = "7b7c2be5-3c41-4b65-8c8f-3e16b1e4a7d2"
	)
	key := conversation + "/deck.pdf"

	buckets, err := NewArtifactBucketsRepository(pool)
	if err != nil {
		t.Fatalf("NewArtifactBucketsRepository: %v", err)
	}
	objects, err := NewArtifactObjectsRepository(pool)
	if err != nil {
		t.Fatalf("NewArtifactObjectsRepository: %v", err)
	}
	bucket, err := buckets.CreateBucket(ctx, NewBucketInput{
		ProjectID: project, Name: bucketName, DisplayName: bucketName, BucketType: "system",
	})
	if err != nil {
		t.Fatalf("create system bucket: %v", err)
	}
	// The same bucket name and key in another project, as a USER bucket.
	otherBucket, err := buckets.CreateBucket(ctx, NewBucketInput{
		ProjectID: otherProject, Name: bucketName, DisplayName: bucketName, BucketType: "local",
	})
	if err != nil {
		t.Fatalf("create user bucket: %v", err)
	}
	for _, id := range []int64{bucket.ID, otherBucket.ID} {
		if _, err := objects.UpsertObject(ctx, NewObjectInput{
			BucketID: id, Key: key, ByteLength: 4, MediaType: "application/pdf",
		}); err != nil {
			t.Fatalf("record attachment object: %v", err)
		}
	}
	repository, err := NewCurrentAttachmentObjectRepository(pool, &attachmentObjectTestStore{key: key})
	if err != nil {
		t.Fatalf("NewCurrentAttachmentObjectRepository: %v", err)
	}

	_, first, _, err := repository.LoadAttachmentExtraction(ctx, project, bucketName, key, extract.Version)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	digest := sha256.Sum256([]byte("same"))
	document := extract.Document{
		Format: extract.FormatText, Text: "same", UnitCount: 1,
		Units:            []extract.Unit{{Kind: extract.UnitPart, Label: "1", Start: 0, End: 4, HasText: true}},
		TokenEstimate:    1,
		ExtractorVersion: extract.Version,
	}
	if err := repository.SaveAttachmentExtraction(ctx, first, digest[:], storage.AttachmentExtraction{Document: document}); err != nil {
		t.Fatalf("save: %v", err)
	}

	// The scope gates hold for the sidecar exactly as for the bytes.
	if _, _, _, err := repository.LoadAttachmentExtraction(ctx, otherProject, bucketName, key, extract.Version); !errors.Is(err, storage.ErrContentNotFound) {
		t.Fatalf("a non-system bucket of the same name err = %v, want ErrContentNotFound", err)
	}
	if _, _, _, err := repository.LoadAttachmentExtraction(ctx, otherProject+1, bucketName, key, extract.Version); !errors.Is(err, storage.ErrContentNotFound) {
		t.Fatalf("another project err = %v, want ErrContentNotFound", err)
	}

	// A byte-identical re-upload moves updated_at: the fast lookup misses…
	time.Sleep(2 * time.Millisecond)
	if _, err := objects.UpsertObject(ctx, NewObjectInput{
		BucketID: bucket.ID, Key: key, ByteLength: 4, MediaType: "application/pdf",
	}); err != nil {
		t.Fatalf("re-upload: %v", err)
	}
	_, second, found, err := repository.LoadAttachmentExtraction(ctx, project, bucketName, key, extract.Version)
	if err != nil || found || second.UpdatedAt == first.UpdatedAt {
		t.Fatalf("load after re-upload = found %v err %v (%d vs %d); want a miss on a new version",
			found, err, second.UpdatedAt, first.UpdatedAt)
	}
	// …different bytes are still a miss…
	otherDigest := sha256.Sum256([]byte("diff"))
	if _, found, err := repository.LoadAttachmentExtractionByDigest(ctx, second, extract.Version, otherDigest[:]); err != nil || found {
		t.Fatalf("other bytes = found %v err %v; want a miss", found, err)
	}
	// …a version that is no longer the object's is a miss even with the
	// right digest…
	if _, found, err := repository.LoadAttachmentExtractionByDigest(ctx, first, extract.Version, digest[:]); err != nil || found {
		t.Fatalf("stale version = found %v err %v; want a miss", found, err)
	}
	// …and the same bytes hit and are filed under the new version.
	hit, found, err := repository.LoadAttachmentExtractionByDigest(ctx, second, extract.Version, digest[:])
	if err != nil || !found || hit.Document.Text != "same" {
		t.Fatalf("digest lookup = %+v found %v err %v", hit, found, err)
	}
	again, _, found, err := repository.LoadAttachmentExtraction(ctx, project, bucketName, key, extract.Version)
	if err != nil || !found || again.Document.Text != "same" {
		t.Fatalf("the digest hit was not filed under the new version: found %v err %v", found, err)
	}
	// A digest that is not SHA-256 is refused before it is stored.
	if err := repository.SaveAttachmentExtraction(ctx, second, []byte{1, 2}, storage.AttachmentExtraction{Document: document}); err == nil {
		t.Fatal("a short digest was stored")
	}
}
