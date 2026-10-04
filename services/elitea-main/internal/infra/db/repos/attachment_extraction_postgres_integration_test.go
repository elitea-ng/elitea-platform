package repos

import (
	"context"
	"errors"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/extract"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

// TestAttachmentExtractionSidecarRoundTripsAndGoesStale runs the sidecar
// store against the real schema (shared/0137): a miss returns the object's
// version, a save under that version is read back whole, a re-upload under
// the same key makes the row stale, a save under a stale version is a no-op,
// a refusal is filed with its reason, and the row goes with its object.
func TestAttachmentExtractionSidecarRoundTripsAndGoesStale(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()

	const (
		project      = int64(90311)
		bucketName   = "chat-attachments"
		conversation = "6a6b1ad4-2b30-4a54-9b7f-2d05a0d3f6c1"
	)
	key := conversation + "/report.pdf"

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
	if _, err := objects.UpsertObject(ctx, NewObjectInput{
		BucketID: bucket.ID, Key: key, ByteLength: 2048, MediaType: "application/pdf",
	}); err != nil {
		t.Fatalf("record attachment object: %v", err)
	}

	repository, err := NewCurrentAttachmentObjectRepository(pool, &attachmentObjectTestStore{key: key})
	if err != nil {
		t.Fatalf("NewCurrentAttachmentObjectRepository: %v", err)
	}

	// A miss still names the version.
	_, version, found, err := repository.LoadAttachmentExtraction(ctx, project, bucketName, key, extract.Version)
	if err != nil || found {
		t.Fatalf("first load = found %v, err %v; want a miss", found, err)
	}
	if version.ObjectID <= 0 || version.ByteLength != 2048 || version.MediaType != "application/pdf" {
		t.Fatalf("miss version = %+v", version)
	}

	document := extract.Document{
		Format: extract.FormatPDF,
		Text:   "page one\n\npage two",
		Units: []extract.Unit{
			{Kind: extract.UnitPage, Label: "1", Start: 0, End: 8, HasText: true},
			{Kind: extract.UnitPage, Label: "2", Start: 10, End: 18, HasText: true},
		},
		UnitCount:        3,
		LowTextUnits:     []int{2},
		Partial:          true,
		PartialBy:        extract.PartialPageLimit,
		TokenEstimate:    5,
		ExtractorVersion: extract.Version,
	}
	if err := repository.SaveAttachmentExtraction(ctx, version, nil, storage.AttachmentExtraction{Document: document}); err != nil {
		t.Fatalf("SaveAttachmentExtraction: %v", err)
	}
	loaded, _, found, err := repository.LoadAttachmentExtraction(ctx, project, bucketName, key, extract.Version)
	if err != nil || !found {
		t.Fatalf("second load = found %v, err %v; want a hit", found, err)
	}
	got := loaded.Document
	if loaded.Refused || got.Text != document.Text || got.Format != document.Format ||
		len(got.Units) != 2 || got.Units[1] != document.Units[1] || got.UnitCount != 3 ||
		len(got.LowTextUnits) != 1 || got.LowTextUnits[0] != 2 || !got.Partial ||
		got.PartialBy != extract.PartialPageLimit || got.TokenEstimate != 5 {
		t.Fatalf("round trip lost data: %+v", loaded)
	}

	// Another extractor version does not answer.
	if _, _, found, err := repository.LoadAttachmentExtraction(ctx, project, bucketName, key, "elitea-extract/0"); err != nil || found {
		t.Fatalf("other extractor version = found %v, err %v; want a miss", found, err)
	}
	// Another project cannot reach the row at all.
	if _, _, _, err := repository.LoadAttachmentExtraction(ctx, project+1, bucketName, key, extract.Version); !errors.Is(err, storage.ErrContentNotFound) {
		t.Fatalf("other project err = %v, want ErrContentNotFound", err)
	}

	// A re-upload under the same key makes the filed text stale…
	if _, err := objects.UpsertObject(ctx, NewObjectInput{
		BucketID: bucket.ID, Key: key, ByteLength: 4096, MediaType: "application/pdf",
	}); err != nil {
		t.Fatalf("re-upload attachment object: %v", err)
	}
	_, fresh, found, err := repository.LoadAttachmentExtraction(ctx, project, bucketName, key, extract.Version)
	if err != nil || found {
		t.Fatalf("load after re-upload = found %v, err %v; want a miss", found, err)
	}
	// …and a save under the OLD version never answers for the new object.
	if err := repository.SaveAttachmentExtraction(ctx, version, nil, storage.AttachmentExtraction{Document: document}); err != nil {
		t.Fatalf("stale save: %v", err)
	}
	if _, _, found, _ := repository.LoadAttachmentExtraction(ctx, project, bucketName, key, extract.Version); found {
		t.Fatal("a save under a stale version answered for the new object")
	}

	// A refusal is filed with its reason.
	if err := repository.SaveAttachmentExtraction(ctx, fresh, nil, storage.AttachmentExtraction{
		Refused: true, Reason: extract.ReasonEncrypted,
	}); err != nil {
		t.Fatalf("save refusal: %v", err)
	}
	refusal, _, found, err := repository.LoadAttachmentExtraction(ctx, project, bucketName, key, extract.Version)
	if err != nil || !found || !refusal.Refused || refusal.Reason != extract.ReasonEncrypted {
		t.Fatalf("refusal round trip = %+v found %v err %v", refusal, found, err)
	}
	if refusal.ExtractedAt.IsZero() {
		t.Fatal("a filed row carries when it was made")
	}
	// An extraction for an OLDER version never replaces the row filed for
	// the current one.
	if err := repository.SaveAttachmentExtraction(ctx, version, nil, storage.AttachmentExtraction{Document: document}); err != nil {
		t.Fatalf("late stale save: %v", err)
	}
	if again, _, found, _ := repository.LoadAttachmentExtraction(ctx, project, bucketName, key, extract.Version); !found || again.Reason != extract.ReasonEncrypted {
		t.Fatalf("a late save for an older version replaced the current row: %+v found %v", again, found)
	}

	// The row goes with its object.
	if err := objects.DeleteObjects(ctx, bucket.ID, []string{key}); err != nil {
		t.Fatalf("delete object: %v", err)
	}
	var remaining int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM elitea_storage.attachment_extractions WHERE object_id = $1`, fresh.ObjectID,
	).Scan(&remaining); err != nil {
		t.Fatalf("count sidecar rows: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("%d sidecar rows outlived their object", remaining)
	}
}
