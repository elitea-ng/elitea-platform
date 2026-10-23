package repos

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestArtifactGrantRepositoryCreatesAndRoundTripsFields(t *testing.T) {
	digestAlg := "sha256"
	digest := []byte{1, 2, 3}
	expiresAt := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	queries := &artifactTransferGrantQueriesStub{
		createRow: sqlcgen.CreateArtifactTransferGrantRow{
			ID: "grant-1", ProjectID: 7, BucketID: 42, Key: "grant-1",
			Method: "PUT", ContentType: "image/png", MaxBytes: 1024,
			DigestAlg: &digestAlg, Digest: digest,
			ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
		},
	}
	repository, err := newArtifactTransferGrantsRepository(queries)
	if err != nil {
		t.Fatal(err)
	}

	row, err := repository.CreateTransferGrant(context.Background(), NewTransferGrantInput{
		ID: "grant-1", ProjectID: 7, BucketID: 42, Key: "grant-1",
		Method: "PUT", ContentType: "image/png", MaxBytes: 1024,
		DigestAlg: &digestAlg, Digest: digest, ExpiresAt: expiresAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if row.ID != "grant-1" || row.ProjectID != 7 || row.BucketID != 42 || row.Method != "PUT" ||
		row.ContentType != "image/png" || row.MaxBytes != 1024 || *row.DigestAlg != "sha256" ||
		string(row.Digest) != string(digest) || !row.ExpiresAt.Equal(expiresAt) || row.ConsumedAt != nil {
		t.Fatalf("row = %+v", row)
	}
	if queries.createArg.ExpiresAt.Time != expiresAt {
		t.Fatalf("create arg expires_at = %v, want %v", queries.createArg.ExpiresAt.Time, expiresAt)
	}
}

func TestArtifactGrantRepositoryGetMapsNoRowsToNotFound(t *testing.T) {
	queries := &artifactTransferGrantQueriesStub{getErr: pgx.ErrNoRows}
	repository, err := newArtifactTransferGrantsRepository(queries)
	if err != nil {
		t.Fatal(err)
	}
	_, err = repository.GetTransferGrant(context.Background(), "missing", 7)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestArtifactGrantRepositoryGetScopesByProjectID(t *testing.T) {
	queries := &artifactTransferGrantQueriesStub{
		getRow: sqlcgen.GetArtifactTransferGrantRow{ID: "grant-1", ProjectID: 7, Method: "PUT"},
	}
	repository, err := newArtifactTransferGrantsRepository(queries)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.GetTransferGrant(context.Background(), "grant-1", 7); err != nil {
		t.Fatal(err)
	}
	if queries.getArg.ID != "grant-1" || queries.getArg.ProjectID != 7 {
		t.Fatalf("get arg = %+v", queries.getArg)
	}
}

func TestArtifactGrantRepositoryGetByIDIsNotScopedByProject(t *testing.T) {
	queries := &artifactTransferGrantQueriesStub{
		getByIDRow: sqlcgen.GetArtifactTransferGrantByIDRow{ID: "grant-1", ProjectID: 7, Method: "PUT"},
	}
	repository, err := newArtifactTransferGrantsRepository(queries)
	if err != nil {
		t.Fatal(err)
	}
	row, err := repository.GetTransferGrantByID(context.Background(), "grant-1")
	if err != nil {
		t.Fatal(err)
	}
	if row.ProjectID != 7 {
		t.Fatalf("row.ProjectID = %d, want 7 — the caller, not the query, is responsible for comparing this against the request's own projectID", row.ProjectID)
	}
	if queries.getByIDArg != "grant-1" {
		t.Fatalf("get-by-id arg = %q, want grant-1", queries.getByIDArg)
	}
}

func TestArtifactGrantRepositoryGetByIDMapsNoRowsToNotFound(t *testing.T) {
	queries := &artifactTransferGrantQueriesStub{getByIDErr: pgx.ErrNoRows}
	repository, err := newArtifactTransferGrantsRepository(queries)
	if err != nil {
		t.Fatal(err)
	}
	_, err = repository.GetTransferGrantByID(context.Background(), "missing")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

// TestArtifactGrantRepositoryMarkConsumedRejectsSecondCallWithAlreadyExists
// proves the plan's "a second commit on the same grant returns 409"
// acceptance criterion at the repository layer: 0 rows affected — whether
// from an already-consumed grant or (in production) one that no longer
// exists — maps to storage.ErrAlreadyExists, which artifacts/grants.go maps
// to 409.
func TestArtifactGrantRepositoryMarkConsumedRejectsSecondCallWithAlreadyExists(t *testing.T) {
	queries := &artifactTransferGrantQueriesStub{markRows: 0}
	repository, err := newArtifactTransferGrantsRepository(queries)
	if err != nil {
		t.Fatal(err)
	}
	err = repository.MarkTransferGrantConsumed(context.Background(), "grant-1")
	if !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("error = %v, want ErrAlreadyExists", err)
	}
}

func TestArtifactGrantRepositoryMarkConsumedSucceedsOnFirstCall(t *testing.T) {
	queries := &artifactTransferGrantQueriesStub{markRows: 1}
	repository, err := newArtifactTransferGrantsRepository(queries)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.MarkTransferGrantConsumed(context.Background(), "grant-1"); err != nil {
		t.Fatal(err)
	}
}

type artifactTransferGrantQueriesStub struct {
	createRow sqlcgen.CreateArtifactTransferGrantRow
	createArg sqlcgen.CreateArtifactTransferGrantParams
	createErr error

	getRow sqlcgen.GetArtifactTransferGrantRow
	getArg sqlcgen.GetArtifactTransferGrantParams
	getErr error

	getByIDRow sqlcgen.GetArtifactTransferGrantByIDRow
	getByIDArg string
	getByIDErr error

	markRows int64
	markErr  error

	claimRows []sqlcgen.ClaimExpiredArtifactTransferGrantsRow
	claimArg  sqlcgen.ClaimExpiredArtifactTransferGrantsParams
	claimErr  error
}

func (stub *artifactTransferGrantQueriesStub) CreateArtifactTransferGrant(
	_ context.Context, arg sqlcgen.CreateArtifactTransferGrantParams,
) (sqlcgen.CreateArtifactTransferGrantRow, error) {
	stub.createArg = arg
	return stub.createRow, stub.createErr
}

func (stub *artifactTransferGrantQueriesStub) GetArtifactTransferGrant(
	_ context.Context, arg sqlcgen.GetArtifactTransferGrantParams,
) (sqlcgen.GetArtifactTransferGrantRow, error) {
	stub.getArg = arg
	return stub.getRow, stub.getErr
}

func (stub *artifactTransferGrantQueriesStub) GetArtifactTransferGrantByID(
	_ context.Context, id string,
) (sqlcgen.GetArtifactTransferGrantByIDRow, error) {
	stub.getByIDArg = id
	return stub.getByIDRow, stub.getByIDErr
}

func (stub *artifactTransferGrantQueriesStub) ClaimArtifactTransferGrantForCommit(
	_ context.Context, _ string,
) (pgtype.Timestamptz, error) {
	return pgtype.Timestamptz{}, errors.New("not used")
}

func (stub *artifactTransferGrantQueriesStub) ReleaseArtifactTransferGrantClaim(
	_ context.Context, _ sqlcgen.ReleaseArtifactTransferGrantClaimParams,
) (int64, error) {
	return 0, errors.New("not used")
}

func (stub *artifactTransferGrantQueriesStub) MarkArtifactTransferGrantConsumed(
	_ context.Context, _ string,
) (int64, error) {
	return stub.markRows, stub.markErr
}

func (stub *artifactTransferGrantQueriesStub) ClaimExpiredArtifactTransferGrants(
	_ context.Context, arg sqlcgen.ClaimExpiredArtifactTransferGrantsParams,
) ([]sqlcgen.ClaimExpiredArtifactTransferGrantsRow, error) {
	stub.claimArg = arg
	return stub.claimRows, stub.claimErr
}

func (stub *artifactTransferGrantQueriesStub) ListExpiredArtifactGrantStaging(
	_ context.Context, _ sqlcgen.ListExpiredArtifactGrantStagingParams,
) ([]sqlcgen.ListExpiredArtifactGrantStagingRow, error) {
	return nil, nil
}

func (stub *artifactTransferGrantQueriesStub) ClearArtifactGrantStaging(
	_ context.Context, _ sqlcgen.ClearArtifactGrantStagingParams,
) error {
	return nil
}

// TestArtifactGrantRepositoryClaimsExpiredGrants covers the query that lets
// the retention sweeper reclaim the bytes behind a grant nobody committed.
// Before it existed, those bytes had no objects row, so neither the project
// quota nor the object sweep could ever see them.
func TestArtifactGrantRepositoryClaimsExpiredGrants(t *testing.T) {
	uploadID := "upload-9"
	olderThan := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	queries := &artifactTransferGrantQueriesStub{
		claimRows: []sqlcgen.ClaimExpiredArtifactTransferGrantsRow{{
			ID: "grant-1", ProjectID: 7, BucketID: 42, Key: "grant-1",
			Method: "PUT", UploadID: &uploadID,
			ExpiresAt: pgtype.Timestamptz{Time: olderThan, Valid: true},
		}},
	}
	repository, err := newArtifactTransferGrantsRepository(queries)
	if err != nil {
		t.Fatal(err)
	}

	rows, err := repository.ClaimExpiredTransferGrants(context.Background(), olderThan, 500)
	if err != nil {
		t.Fatalf("ClaimExpiredTransferGrants: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].ID != "grant-1" || rows[0].Method != "PUT" || rows[0].UploadID == nil || *rows[0].UploadID != uploadID {
		t.Errorf("row = %+v, want the grant id, method and upload id carried through", rows[0])
	}
	if !queries.claimArg.OlderThan.Time.Equal(olderThan) || queries.claimArg.RowLimit != 500 {
		t.Errorf("query arguments = %+v, want olderThan=%v limit=500", queries.claimArg, olderThan)
	}
}

func TestArtifactGrantRepositoryWrapsAClaimFailure(t *testing.T) {
	queries := &artifactTransferGrantQueriesStub{claimErr: errors.New("boom")}
	repository, err := newArtifactTransferGrantsRepository(queries)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ClaimExpiredTransferGrants(context.Background(), time.Now(), 10); err == nil {
		t.Fatal("expected the claim failure to surface")
	}
}
