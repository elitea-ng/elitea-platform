package repos

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

// newArtifactTransferGrantsTestRepos builds both S6's bucket repository and
// S15's grant repository against the SAME pool — a grant row's bucket_id
// foreign-keys into elitea_storage.buckets (ON DELETE CASCADE), so a
// meaningful grant test always needs a real bucket row from the same
// database first.
func newArtifactTransferGrantsTestRepos(t *testing.T) (*ArtifactBucketsRepository, *ArtifactTransferGrantsRepository) {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	buckets, err := NewArtifactBucketsRepository(pool)
	if err != nil {
		t.Fatalf("NewArtifactBucketsRepository: %v", err)
	}
	grants, err := NewArtifactTransferGrantsRepository(pool)
	if err != nil {
		t.Fatalf("NewArtifactTransferGrantsRepository: %v", err)
	}
	return buckets, grants
}

func TestArtifactGrantRepositoryCreatesAndFetchesAgainstRealPostgres(t *testing.T) {
	buckets, grants := newArtifactTransferGrantsTestRepos(t)
	ctx := context.Background()
	const projectID = int64(9201)

	bucket := mustCreateArtifactBucket(t, buckets, projectID, "reports")
	digestAlg := "sha256"
	digest := []byte{0xde, 0xad, 0xbe, 0xef}
	expiresAt := time.Now().Add(15 * time.Minute).Truncate(time.Millisecond)

	created, err := grants.CreateTransferGrant(ctx, NewTransferGrantInput{
		ID: "11111111-1111-4111-8111-111111111111", ProjectID: projectID, BucketID: bucket.ID,
		Key: "11111111-1111-4111-8111-111111111111", Method: "PUT", ContentType: "image/png",
		MaxBytes: 4096, DigestAlg: &digestAlg, Digest: digest, ExpiresAt: expiresAt,
	})
	if err != nil {
		t.Fatalf("CreateTransferGrant: %v", err)
	}
	if created.ID != "11111111-1111-4111-8111-111111111111" || created.ProjectID != projectID ||
		created.BucketID != bucket.ID || created.Method != "PUT" || created.ContentType != "image/png" ||
		created.MaxBytes != 4096 || created.DigestAlg == nil || *created.DigestAlg != "sha256" ||
		string(created.Digest) != string(digest) || created.ConsumedAt != nil {
		t.Fatalf("created = %+v", created)
	}
	if !created.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("created.ExpiresAt = %v, want %v", created.ExpiresAt, expiresAt)
	}

	fetched, err := grants.GetTransferGrant(ctx, created.ID, projectID)
	if err != nil {
		t.Fatalf("GetTransferGrant: %v", err)
	}
	if fetched.ID != created.ID || fetched.Key != created.Key || !fetched.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("fetched = %+v", fetched)
	}
}

// TestArtifactGrantRepositoryUploadIDRoundTripsAndGetByIDIsUnscopedAgainstRealPostgres
// proves S16's schema addition end to end: a grant created with a non-nil
// UploadID round-trips it through both CreateTransferGrant and
// GetTransferGrant, and GetTransferGrantByID resolves the same row without
// the project_id filter GetTransferGrant applies — the property S16's
// ownership check (requireOwnedMultipartGrant, internal/api/v2/artifacts)
// depends on.
func TestArtifactGrantRepositoryUploadIDRoundTripsAndGetByIDIsUnscopedAgainstRealPostgres(t *testing.T) {
	buckets, grants := newArtifactTransferGrantsTestRepos(t)
	ctx := context.Background()
	const projectID = int64(9205)

	bucket := mustCreateArtifactBucket(t, buckets, projectID, "reports")
	uploadID := "s3-multipart-upload-abc123"
	created, err := grants.CreateTransferGrant(ctx, NewTransferGrantInput{
		ID: "44444444-4444-4444-8444-444444444444", ProjectID: projectID, BucketID: bucket.ID,
		Key: "44444444-4444-4444-8444-444444444444", Method: "PUT", ContentType: "application/octet-stream",
		MaxBytes: 200 << 20, UploadID: &uploadID, ExpiresAt: time.Now().Add(15 * time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateTransferGrant: %v", err)
	}
	if created.UploadID == nil || *created.UploadID != uploadID {
		t.Fatalf("created.UploadID = %v, want %q", created.UploadID, uploadID)
	}

	fetched, err := grants.GetTransferGrant(ctx, created.ID, projectID)
	if err != nil {
		t.Fatalf("GetTransferGrant: %v", err)
	}
	if fetched.UploadID == nil || *fetched.UploadID != uploadID {
		t.Fatalf("fetched.UploadID = %v, want %q", fetched.UploadID, uploadID)
	}

	// GetTransferGrantByID resolves the same row regardless of which
	// project asks — the caller (requireOwnedMultipartGrant), not the
	// query, is responsible for the ownership decision.
	byID, err := grants.GetTransferGrantByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetTransferGrantByID: %v", err)
	}
	if byID.ProjectID != projectID || byID.UploadID == nil || *byID.UploadID != uploadID {
		t.Fatalf("byID = %+v, want ProjectID=%d UploadID=%q", byID, projectID, uploadID)
	}

	if _, err := grants.GetTransferGrantByID(ctx, "00000000-0000-4000-8000-000000000000"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetTransferGrantByID for an unknown id: err = %v, want ErrNotFound", err)
	}
}

func TestArtifactGrantRepositoryGetIsScopedToProjectIDAgainstRealPostgres(t *testing.T) {
	buckets, grants := newArtifactTransferGrantsTestRepos(t)
	ctx := context.Background()
	const projectID = int64(9202)
	const otherProjectID = int64(9203)

	bucket := mustCreateArtifactBucket(t, buckets, projectID, "reports")
	created, err := grants.CreateTransferGrant(ctx, NewTransferGrantInput{
		ID: "22222222-2222-4222-8222-222222222222", ProjectID: projectID, BucketID: bucket.ID,
		Key: "22222222-2222-4222-8222-222222222222", Method: "PUT", ContentType: "image/png",
		MaxBytes: 4096, ExpiresAt: time.Now().Add(15 * time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateTransferGrant: %v", err)
	}

	if _, err := grants.GetTransferGrant(ctx, created.ID, otherProjectID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetTransferGrant under the wrong project: err = %v, want ErrNotFound", err)
	}
	if _, err := grants.GetTransferGrant(ctx, created.ID, projectID); err != nil {
		t.Fatalf("GetTransferGrant under the correct project: %v", err)
	}
}

// TestArtifactGrantRepositoryMarkConsumedEnforcesSingleUseAgainstRealPostgres
// proves the plan's "PUT grants are single-use" acceptance criterion at the
// repository layer, against a real database — not just the in-memory fake
// this same behavior is also unit-tested against (fake_repo_test.go).
// The staging sweep lists exactly the consumed, expired presigned grants
// whose key is id + ".upload". A consumed grant with any other key (the
// toolkit discovery inventory records live objects that way), an unexpired
// one and an unconsumed one are not its business. Clearing resets the key
// and drops the row from the list, and the row itself stays.
func TestArtifactGrantRepositoryListsAndClearsExpiredStagingAgainstRealPostgres(t *testing.T) {
	buckets, grants := newArtifactTransferGrantsTestRepos(t)
	ctx := context.Background()
	const projectID = int64(9206)
	bucket := mustCreateArtifactBucket(t, buckets, projectID, "reports")
	past := time.Now().Add(-time.Hour)
	create := func(id, key string, expiresAt time.Time, consume bool) {
		t.Helper()
		if _, err := grants.CreateTransferGrant(ctx, NewTransferGrantInput{
			ID: id, ProjectID: projectID, BucketID: bucket.ID, Key: key, Method: "PUT",
			ContentType: "text/plain", MaxBytes: 64, ExpiresAt: expiresAt,
		}); err != nil {
			t.Fatalf("CreateTransferGrant %s: %v", id, err)
		}
		if consume {
			if err := grants.MarkTransferGrantConsumed(ctx, id); err != nil {
				t.Fatalf("MarkTransferGrantConsumed %s: %v", id, err)
			}
		}
	}
	const (
		staged     = "55555555-5555-4555-8555-555555555555"
		inventory  = "66666666-6666-4666-8666-666666666666"
		live       = "77777777-7777-4777-8777-777777777777"
		unconsumed = "88888888-8888-4888-8888-888888888888"
	)
	create(staged, staged+GrantStagingSuffix, past, true)
	create(inventory, "discovery/toolkit/1/"+inventory, past, true)
	create(live, live+GrantStagingSuffix, time.Now().Add(time.Hour), true)
	create(unconsumed, unconsumed+GrantStagingSuffix, past, false)

	rows, err := grants.ListExpiredGrantStaging(ctx, time.Now(), 100)
	if err != nil {
		t.Fatalf("ListExpiredGrantStaging: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != staged || rows[0].Key != staged+GrantStagingSuffix ||
		rows[0].ProjectID != projectID || rows[0].BucketID != bucket.ID {
		t.Fatalf("rows = %+v, want only %s", rows, staged)
	}

	if err := grants.ClearGrantStaging(ctx, staged, staged+GrantStagingSuffix); err != nil {
		t.Fatalf("ClearGrantStaging: %v", err)
	}
	if rows, err := grants.ListExpiredGrantStaging(ctx, time.Now(), 100); err != nil || len(rows) != 0 {
		t.Fatalf("after clear: rows = %+v, err = %v", rows, err)
	}
	fetched, err := grants.GetTransferGrant(ctx, staged, projectID)
	if err != nil {
		t.Fatalf("GetTransferGrant after clear: %v", err)
	}
	if fetched.Key != staged || fetched.ConsumedAt == nil {
		t.Fatalf("cleared row = %+v, want key %s and still consumed", fetched, staged)
	}
}

func TestArtifactGrantRepositoryMarkConsumedEnforcesSingleUseAgainstRealPostgres(t *testing.T) {
	buckets, grants := newArtifactTransferGrantsTestRepos(t)
	ctx := context.Background()
	const projectID = int64(9204)

	bucket := mustCreateArtifactBucket(t, buckets, projectID, "reports")
	created, err := grants.CreateTransferGrant(ctx, NewTransferGrantInput{
		ID: "33333333-3333-4333-8333-333333333333", ProjectID: projectID, BucketID: bucket.ID,
		Key: "33333333-3333-4333-8333-333333333333", Method: "PUT", ContentType: "image/png",
		MaxBytes: 4096, ExpiresAt: time.Now().Add(15 * time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateTransferGrant: %v", err)
	}

	if err := grants.MarkTransferGrantConsumed(ctx, created.ID); err != nil {
		t.Fatalf("first MarkTransferGrantConsumed: %v", err)
	}
	fetched, err := grants.GetTransferGrant(ctx, created.ID, projectID)
	if err != nil {
		t.Fatalf("GetTransferGrant after consuming: %v", err)
	}
	if fetched.ConsumedAt == nil {
		t.Fatal("expected ConsumedAt to be set after MarkTransferGrantConsumed")
	}

	if err := grants.MarkTransferGrantConsumed(ctx, created.ID); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("second MarkTransferGrantConsumed: err = %v, want ErrAlreadyExists", err)
	}
}

// Concurrent single-shot commits race for ClaimTransferGrant: exactly one
// gets a stamp, and only that stamp releases the grant for a retry.
func TestArtifactGrantRepositoryClaimHasOneWinnerAndReleasesOnlyItsOwnClaimAgainstRealPostgres(t *testing.T) {
	buckets, grants := newArtifactTransferGrantsTestRepos(t)
	ctx := context.Background()
	const projectID = int64(9211)

	bucket := mustCreateArtifactBucket(t, buckets, projectID, "reports")
	const grantID = "21111111-1111-4111-8111-111111111111"
	if _, err := grants.CreateTransferGrant(ctx, NewTransferGrantInput{
		ID: grantID, ProjectID: projectID, BucketID: bucket.ID, Key: grantID + GrantStagingSuffix,
		Method: "PUT", ContentType: "text/plain", MaxBytes: 64, ExpiresAt: time.Now().Add(15 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateTransferGrant: %v", err)
	}

	const racers = 8
	stamps := make(chan time.Time, racers)
	errs := make(chan error, racers)
	var wg sync.WaitGroup
	for range racers {
		wg.Go(func() {
			at, err := grants.ClaimTransferGrant(ctx, grantID)
			if err != nil {
				errs <- err
				return
			}
			stamps <- at
		})
	}
	wg.Wait()
	close(stamps)
	close(errs)
	if len(stamps) != 1 {
		t.Fatalf("claims won = %d, want 1", len(stamps))
	}
	for err := range errs {
		if !errors.Is(err, storage.ErrAlreadyExists) {
			t.Fatalf("losing claim: %v, want ErrAlreadyExists", err)
		}
	}
	claimedAt := <-stamps

	if released, err := grants.ReleaseTransferGrantClaim(ctx, grantID, claimedAt.Add(time.Microsecond)); err != nil || released {
		t.Fatalf("release with a foreign stamp: released=%v err=%v, want false", released, err)
	}
	if err := grants.MarkTransferGrantConsumed(ctx, grantID); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("mark while claimed: %v, want ErrAlreadyExists", err)
	}
	if released, err := grants.ReleaseTransferGrantClaim(ctx, grantID, claimedAt); err != nil || !released {
		t.Fatalf("release own claim: released=%v err=%v, want true", released, err)
	}
	row, err := grants.GetTransferGrant(ctx, grantID, projectID)
	if err != nil {
		t.Fatalf("GetTransferGrant: %v", err)
	}
	if row.ConsumedAt != nil {
		t.Fatalf("consumed_at = %v after release, want NULL", row.ConsumedAt)
	}
	if _, err := grants.ClaimTransferGrant(ctx, grantID); err != nil {
		t.Fatalf("claim after release: %v", err)
	}
}
