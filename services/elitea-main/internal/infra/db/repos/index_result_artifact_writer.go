package repos

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

const (
	// indexArtifactBucketName is the fixed system bucket every result
	// artifact grant/commit lands in, per project — the same "reserved
	// system bucket, created on first use" convention S20a's attachments
	// (RequireAttachmentBucket) and S20b's icons (iconBucket) already
	// established. No retention policy: this stage keeps its own
	// settlement semantics per ADR-0014, deliberately independent of S14's
	// elitea_storage.objects-based retention sweeper — a committed result
	// artifact's lifecycle is owned by elitea_runtime.index_result_artifacts,
	// never elitea_storage.objects.
	indexArtifactBucketName = "index-artifacts"

	// indexArtifactGrantTTL mirrors S15's own defaultGrantTTL
	// (internal/api/v2/artifacts/grants.go) — the same kind of short-lived,
	// single-use transfer grant, just minted for a different producer.
	indexArtifactGrantTTL = 15 * time.Minute
)

// generateArtifactGrantID returns a random UUIDv4, the same crypto/rand
// construction internal/api/v2/artifacts/grants.go's generateGrantID and
// internal/api/v2/auth/util.go's generateUUID each already use —
// package-local here too, since neither of those is exported and importing
// the API package from repos would invert this codebase's layering.
func generateArtifactGrantID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[:4]) + "-" +
		hex.EncodeToString(b[4:6]) + "-" +
		hex.EncodeToString(b[6:8]) + "-" +
		hex.EncodeToString(b[8:10]) + "-" +
		hex.EncodeToString(b[10:]), nil
}

type indexResultArtifactWriteQueries interface {
	InsertIndexResultArtifact(context.Context, sqlcgen.InsertIndexResultArtifactParams) (sqlcgen.InsertIndexResultArtifactRow, error)
	GetIndexResultArtifactByPrimaryKey(context.Context, sqlcgen.GetIndexResultArtifactByPrimaryKeyParams) (sqlcgen.GetIndexResultArtifactByPrimaryKeyRow, error)
}

// IndexResultArtifactWriter implements outputapp.ArtifactGrantIssuer
// (ADR-0014's Artifact plane) on top of S15's own grant machinery
// (ArtifactTransferGrantsRepository, elitea_storage.transfer_grants) and
// storage.ObjectStore — sharing both, per the plan's own default framing —
// while keeping its own settlement semantics: a committed artifact is
// recorded in elitea_runtime.index_result_artifacts, the same durable
// ledger ArtifactVerifier.VerifyDurable (index_ingest_results.go) already
// reads. See docs/plans/storage-migration-plan.md S20c: this type has no
// wired caller anywhere in this codebase, by design.
type IndexResultArtifactWriter struct {
	grants  *ArtifactTransferGrantsRepository
	buckets *ArtifactBucketsRepository
	store   storage.ObjectStore
	queries indexResultArtifactWriteQueries
	policy  IndexIngestOutputPolicy
}

// NewIndexResultArtifactWriter takes the same IndexIngestOutputPolicy
// NewIndexIngestResultsRepository already validates against (LimitsRevision
// is unused here — only ArtifactMediaType/MaxArtifactBytes apply to a
// writer) so a future caller wiring both cannot accidentally configure them
// with two different, drifting artifact policies.
func NewIndexResultArtifactWriter(pool *pgxpool.Pool, store storage.ObjectStore, policy IndexIngestOutputPolicy) (*IndexResultArtifactWriter, error) {
	if pool == nil {
		return nil, errors.New("index result artifact database is required")
	}
	if store == nil {
		return nil, errors.New("index result artifact object store is required")
	}
	if err := policy.validate(); err != nil {
		return nil, err
	}
	grants, err := NewArtifactTransferGrantsRepository(pool)
	if err != nil {
		return nil, err
	}
	buckets, err := NewArtifactBucketsRepository(pool)
	if err != nil {
		return nil, err
	}
	return &IndexResultArtifactWriter{
		grants: grants, buckets: buckets, store: store, queries: sqlcgen.New(pool), policy: policy,
	}, nil
}

// requireBucket returns projectID's reserved index-artifact bucket,
// creating it on first use — mirrors attachment_store.go's
// RequireAttachmentBucket: there is no project-creation hook wired
// anywhere in this service (S13's own bootstrapper is deliberately
// unwired for the same reason), so this cannot depend on one either.
func (w *IndexResultArtifactWriter) requireBucket(ctx context.Context, projectID int64) (BucketRow, error) {
	return requireIndexArtifactBucket(ctx, w.buckets, projectID)
}

// requireIndexArtifactBucket is projectID's index-artifacts SYSTEM bucket,
// created on first use; the index result writer and the toolkit discovery
// inventory share it (review round 3), so both refuse a leftover bucket a
// person made under the name with the same ErrSystemBucketNameTaken.
func requireIndexArtifactBucket(ctx context.Context, buckets *ArtifactBucketsRepository, projectID int64) (BucketRow, error) {
	row, err := buckets.GetBucket(ctx, projectID, indexArtifactBucketName)
	if err == nil {
		return requireSystemBucket(ctx, row, projectID)
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return BucketRow{}, err
	}
	row, err = buckets.CreateBucket(ctx, NewBucketInput{
		ProjectID:   projectID,
		Name:        indexArtifactBucketName,
		DisplayName: indexArtifactBucketName,
		BucketType:  "system",
	})
	if err == nil {
		return row, nil
	}
	if errors.Is(err, storage.ErrAlreadyExists) {
		// Lost a create race against a concurrent first-grant request —
		// the bucket now exists either way.
		row, err = buckets.GetBucket(ctx, projectID, indexArtifactBucketName)
		if err != nil {
			return BucketRow{}, err
		}
		return requireSystemBucket(ctx, row, projectID)
	}
	return BucketRow{}, err
}

func (w *IndexResultArtifactWriter) CreateArtifactGrant(ctx context.Context, request outputapp.CreateArtifactGrantRequest) (outputapp.ArtifactGrant, error) {
	if err := request.Validate(); err != nil {
		return outputapp.ArtifactGrant{}, err
	}
	resourceProjectID, err := parseProjectID(request.ResourceProjectID)
	if err != nil || resourceProjectID > math.MaxInt32 {
		return outputapp.ArtifactGrant{}, outputapp.ErrInvalidIndexIngestOutput
	}
	if request.Artifact.ByteLength > math.MaxInt64 {
		return outputapp.ArtifactGrant{}, outputapp.ErrInvalidIndexIngestOutput
	}
	if request.Artifact.MediaType != w.policy.ArtifactMediaType || request.Artifact.ByteLength > w.policy.MaxArtifactBytes {
		// The plan's own text: "honouring the existing 1 MiB and
		// application/json policy." Rejected before a grant is ever minted
		// — IndexArtifactReference.Validate() alone only requires a
		// non-zero length and *some* parseable MIME type, not this
		// producer's specific admitted policy, and this bucket has no
		// retention sweeper to reclaim a policy-violating commit after the
		// fact (unlike elitea_storage.objects/S14).
		return outputapp.ArtifactGrant{}, outputapp.ErrIndexIngestArtifactMismatch
	}
	if !w.store.Capabilities().Presign {
		// No facade endpoint exists at this layer to fall back to (S15's
		// grantURL falls back to its own HTTP upload route, which this
		// unwired writer has no equivalent of) — a backend without presign
		// support (GCS) simply cannot serve this writer yet, flagged rather
		// than returned as a grant with an unusable empty URL.
		return outputapp.ArtifactGrant{}, fmt.Errorf("%w: backend does not support presigned uploads", storage.ErrNotSupported)
	}

	bucketRow, err := w.requireBucket(ctx, resourceProjectID)
	if err != nil {
		return outputapp.ArtifactGrant{}, fmt.Errorf("require index artifact bucket: %w", err)
	}

	grantID, err := generateArtifactGrantID()
	if err != nil {
		return outputapp.ArtifactGrant{}, fmt.Errorf("generate index artifact grant id: %w", err)
	}
	ref, err := storage.NewObjectRef(request.ResourceProjectID, bucketRow.Name, grantID)
	if err != nil {
		return outputapp.ArtifactGrant{}, fmt.Errorf("build index artifact object ref: %w", err)
	}

	digestAlg := "sha256"
	digest := request.Artifact.Digest
	expiresAt := time.Now().Add(indexArtifactGrantTTL)
	if _, err := w.grants.CreateTransferGrant(ctx, NewTransferGrantInput{
		ID:          grantID,
		ProjectID:   resourceProjectID,
		BucketID:    bucketRow.ID,
		Key:         grantID,
		Method:      "PUT",
		ContentType: request.Artifact.MediaType,
		MaxBytes:    int64(request.Artifact.ByteLength),
		DigestAlg:   &digestAlg,
		Digest:      digest[:],
		ExpiresAt:   expiresAt,
	}); err != nil {
		return outputapp.ArtifactGrant{}, fmt.Errorf("create index artifact transfer grant: %w", err)
	}

	url, err := w.store.PresignPut(ctx, ref, indexArtifactGrantTTL, storage.PutOptions{ContentType: request.Artifact.MediaType})
	if err != nil {
		return outputapp.ArtifactGrant{}, fmt.Errorf("presign index artifact upload: %w", err)
	}
	return outputapp.ArtifactGrant{GrantID: grantID, URL: url, ExpiresAt: expiresAt}, nil
}

func (w *IndexResultArtifactWriter) CommitArtifact(ctx context.Context, request outputapp.CommitArtifactRequest) (outputapp.DurableIndexArtifact, error) {
	if err := request.Validate(); err != nil {
		return outputapp.DurableIndexArtifact{}, err
	}
	resourceProjectID, err := parseProjectID(request.ResourceProjectID)
	if err != nil || resourceProjectID > math.MaxInt32 {
		return outputapp.DurableIndexArtifact{}, outputapp.ErrInvalidIndexIngestOutput
	}
	if request.Generation > math.MaxInt64 {
		return outputapp.DurableIndexArtifact{}, outputapp.ErrInvalidIndexIngestOutput
	}

	grant, err := w.grants.GetTransferGrant(ctx, request.GrantID, resourceProjectID)
	if err != nil {
		return outputapp.DurableIndexArtifact{}, fmt.Errorf("get index artifact transfer grant: %w", err)
	}
	if grant.Method != "PUT" {
		// Mirrors CommitTransferGrant's own grant.Method != methodPut check
		// (grants.go) — elitea_storage.transfer_grants is shared with S15's
		// general-purpose REST grant API, so without this a GET grant
		// (nothing to verify or commit) could reach the code below.
		return outputapp.DurableIndexArtifact{}, outputapp.ErrInvalidIndexIngestOutput
	}
	if grant.ConsumedAt != nil {
		return outputapp.DurableIndexArtifact{}, storage.ErrAlreadyExists
	}
	if time.Now().After(grant.ExpiresAt) {
		return outputapp.DurableIndexArtifact{}, storage.ErrPreconditionFailed
	}
	digest := request.Artifact.Digest
	if grant.ContentType != request.Artifact.MediaType || grant.MaxBytes != int64(request.Artifact.ByteLength) || !bytes.Equal(grant.Digest, digest[:]) {
		// The grant was created for a different IndexArtifactReference than
		// this commit request now claims — CreateArtifactGrant always
		// stores the reference's own content_type/max_bytes/digest on the
		// grant row, so this can only happen if the caller mixed up which
		// grant belongs to which artifact. This does NOT cross-check
		// ArtifactID/ImmutableVersion/ExecutionID/Generation/TenantID/
		// ProjectionProjectID/CommandID/Classification against what
		// CreateArtifactGrant was originally called with — see
		// CommitArtifactRequest's own doc comment for why that gap is real
		// and still open.
		return outputapp.DurableIndexArtifact{}, outputapp.ErrIndexIngestArtifactMismatch
	}

	bucketRow, err := w.buckets.GetBucketByID(ctx, grant.BucketID)
	if err != nil {
		return outputapp.DurableIndexArtifact{}, fmt.Errorf("get index artifact bucket: %w", err)
	}
	if bucketRow.Name != indexArtifactBucketName {
		// elitea_storage.transfer_grants carries no marker distinguishing a
		// grant this writer minted from one minted through S15's REST API
		// for an arbitrary bucket — without this check, a grant obtained
		// through that unrelated, general-purpose endpoint (with
		// caller-controlled content_type/max_bytes/digest) could be
		// "laundered" through CommitArtifact into a durable
		// index_result_artifacts row.
		return outputapp.DurableIndexArtifact{}, outputapp.ErrInvalidIndexIngestOutput
	}
	ref, err := storage.NewObjectRef(request.ResourceProjectID, bucketRow.Name, grant.Key)
	if err != nil {
		return outputapp.DurableIndexArtifact{}, fmt.Errorf("build index artifact object ref: %w", err)
	}

	// Same reasoning as CommitTransferGrant (grants.go): no cheap
	// provider-side call yields a SHA-256 for an object that landed via
	// presigned PUT on any backend, so this streams the full object
	// through a hasher once, which is also how the actual byte count is
	// learned. info.Size is checked first, the same cheap early exit
	// finalizeGrantCommit (grants.go) uses, before paying for a full
	// read+hash of a response body a presigned PUT URL never enforced any
	// server-side size cap on.
	body, info, err := w.store.Get(ctx, ref, nil)
	if err != nil {
		return outputapp.DurableIndexArtifact{}, fmt.Errorf("get uploaded index artifact: %w", err)
	}
	defer func() { _ = body.Close() }()

	if info.Size > grant.MaxBytes {
		_ = w.store.Delete(ctx, ref)
		return outputapp.DurableIndexArtifact{}, outputapp.ErrIndexIngestArtifactMismatch
	}

	hasher := sha256.New()
	n, err := io.Copy(hasher, body)
	if err != nil {
		return outputapp.DurableIndexArtifact{}, fmt.Errorf("read uploaded index artifact: %w", err)
	}
	if n > grant.MaxBytes {
		_ = w.store.Delete(ctx, ref)
		return outputapp.DurableIndexArtifact{}, outputapp.ErrIndexIngestArtifactMismatch
	}
	var actual runtimedomain.Digest
	copy(actual[:], hasher.Sum(nil))
	if n != int64(request.Artifact.ByteLength) || info.ContentType != request.Artifact.MediaType || actual != request.Artifact.Digest {
		_ = w.store.Delete(ctx, ref)
		return outputapp.DurableIndexArtifact{}, outputapp.ErrIndexIngestArtifactMismatch
	}

	if err := w.grants.MarkTransferGrantConsumed(ctx, request.GrantID); err != nil {
		return outputapp.DurableIndexArtifact{}, fmt.Errorf("mark index artifact grant consumed: %w", err)
	}

	verifiedAt := time.Now().UTC()
	row, err := w.queries.InsertIndexResultArtifact(ctx, sqlcgen.InsertIndexResultArtifactParams{
		ArtifactID:        request.Artifact.ArtifactID,
		ImmutableVersion:  request.Artifact.ImmutableVersion,
		ExecutionID:       request.ExecutionID,
		Generation:        int64(request.Generation),
		ResourceProjectID: int32(resourceProjectID),
		MediaType:         request.Artifact.MediaType,
		ByteLength:        n,
		Digest:            actual[:],
		Classification:    request.Artifact.Classification,
		StorageRecordID:   request.GrantID,
		BytesVerifiedAt:   toTimestamptz(&verifiedAt),
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return w.reconcileDuplicateCommit(ctx, request, resourceProjectID, actual)
		}
		return outputapp.DurableIndexArtifact{}, fmt.Errorf("insert index result artifact: %w", err)
	}
	return newDurableIndexArtifact(row.ArtifactID, row.ImmutableVersion, row.MediaType, row.ByteLength, row.Digest, row.Classification, row.StorageRecordID, row.BytesVerifiedAt)
}

// reconcileDuplicateCommit runs when InsertIndexResultArtifact hits the
// table's (artifact_id, immutable_version) primary key — the grant this
// call just consumed can only reach here if MarkTransferGrantConsumed
// above succeeded, so this is never a replay of the *same* grant (a
// second CommitArtifact call for one grant always fails earlier, at
// grant.ConsumedAt != nil). It is a *different*, fresh grant claiming to
// commit the same artifact identity — a legitimate producer retry after a
// prior grant/commit round trip whose result it never observed (e.g. a
// timeout), or a genuine identity conflict. Distinguished by comparing the
// newly-verified content against what is already durably recorded.
func (w *IndexResultArtifactWriter) reconcileDuplicateCommit(ctx context.Context, request outputapp.CommitArtifactRequest, resourceProjectID int64, actual runtimedomain.Digest) (outputapp.DurableIndexArtifact, error) {
	existing, err := w.queries.GetIndexResultArtifactByPrimaryKey(ctx, sqlcgen.GetIndexResultArtifactByPrimaryKeyParams{
		ArtifactID:       request.Artifact.ArtifactID,
		ImmutableVersion: request.Artifact.ImmutableVersion,
	})
	if err != nil {
		return outputapp.DurableIndexArtifact{}, fmt.Errorf("load conflicting index result artifact: %w", err)
	}
	if existing.ExecutionID != request.ExecutionID || existing.Generation != int64(request.Generation) ||
		existing.ResourceProjectID != int32(resourceProjectID) || existing.MediaType != request.Artifact.MediaType ||
		uint64(existing.ByteLength) != request.Artifact.ByteLength || existing.Classification != request.Artifact.Classification ||
		!bytes.Equal(existing.Digest, actual[:]) {
		return outputapp.DurableIndexArtifact{}, outputapp.ErrArtifactGrantConflict
	}
	return newDurableIndexArtifact(existing.ArtifactID, existing.ImmutableVersion, existing.MediaType, existing.ByteLength, existing.Digest, existing.Classification, existing.StorageRecordID, existing.BytesVerifiedAt)
}

// ResolveArtifact checks identity against the row's own
// execution_id/generation/resource_project_id — the only join keys
// elitea_runtime.index_result_artifacts actually has to execution_jobs —
// rather than re-validating tenant/projection/command the way
// GetDurableIndexResultArtifact's join does for VerifyDurable: this
// method's job is handing back a URL to read already-durable bytes, not
// re-attesting identity for settlement.
func (w *IndexResultArtifactWriter) ResolveArtifact(ctx context.Context, request outputapp.ArtifactVerificationRequest) (outputapp.ArtifactGrant, error) {
	if err := request.Validate(); err != nil {
		return outputapp.ArtifactGrant{}, err
	}
	if request.Generation > math.MaxInt64 {
		return outputapp.ArtifactGrant{}, outputapp.ErrInvalidIndexIngestOutput
	}
	resourceProjectID, err := parseProjectID(request.ResourceProjectID)
	if err != nil || resourceProjectID > math.MaxInt32 {
		return outputapp.ArtifactGrant{}, outputapp.ErrInvalidIndexIngestOutput
	}

	row, err := w.queries.GetIndexResultArtifactByPrimaryKey(ctx, sqlcgen.GetIndexResultArtifactByPrimaryKeyParams{
		ArtifactID:       request.Artifact.ArtifactID,
		ImmutableVersion: request.Artifact.ImmutableVersion,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return outputapp.ArtifactGrant{}, outputapp.ErrIndexIngestArtifactUnavailable
	}
	if err != nil {
		return outputapp.ArtifactGrant{}, fmt.Errorf("load index result artifact: %w", err)
	}
	if row.ExecutionID != request.ExecutionID || row.Generation != int64(request.Generation) || row.ResourceProjectID != int32(resourceProjectID) {
		return outputapp.ArtifactGrant{}, outputapp.ErrIndexIngestArtifactUnavailable
	}

	grant, err := w.grants.GetTransferGrantByID(ctx, row.StorageRecordID)
	if err != nil {
		return outputapp.ArtifactGrant{}, fmt.Errorf("resolve index artifact storage record: %w", err)
	}
	bucketRow, err := w.buckets.GetBucketByID(ctx, grant.BucketID)
	if err != nil {
		return outputapp.ArtifactGrant{}, fmt.Errorf("get index artifact bucket: %w", err)
	}
	ref, err := storage.NewObjectRef(request.ResourceProjectID, bucketRow.Name, grant.Key)
	if err != nil {
		return outputapp.ArtifactGrant{}, fmt.Errorf("build index artifact object ref: %w", err)
	}
	if !w.store.Capabilities().Presign {
		return outputapp.ArtifactGrant{}, fmt.Errorf("%w: backend does not support presigned reads", storage.ErrNotSupported)
	}
	expiresAt := time.Now().Add(indexArtifactGrantTTL)
	url, err := w.store.PresignGet(ctx, ref, indexArtifactGrantTTL)
	if err != nil {
		return outputapp.ArtifactGrant{}, fmt.Errorf("presign index artifact download: %w", err)
	}
	return outputapp.ArtifactGrant{GrantID: row.StorageRecordID, URL: url, ExpiresAt: expiresAt}, nil
}

func newDurableIndexArtifact(artifactID, immutableVersion, mediaType string, byteLength int64, digestBytes []byte, classification, storageRecordID string, bytesVerifiedAt pgtype.Timestamptz) (outputapp.DurableIndexArtifact, error) {
	digest, err := storedDigest(digestBytes)
	if err != nil {
		return outputapp.DurableIndexArtifact{}, outputapp.ErrIndexIngestArtifactMismatch
	}
	verified := outputapp.DurableIndexArtifact{
		Reference: outputapp.IndexArtifactReference{
			ArtifactID: artifactID, ImmutableVersion: immutableVersion, MediaType: mediaType,
			ByteLength: uint64(byteLength), Digest: digest, Classification: classification,
		},
		StorageRecordID: storageRecordID,
		VerifiedAt:      bytesVerifiedAt.Time.UTC(),
	}
	if err := verified.Validate(); err != nil {
		return outputapp.DurableIndexArtifact{}, err
	}
	return verified, nil
}

var _ outputapp.ArtifactGrantIssuer = (*IndexResultArtifactWriter)(nil)

// requireSystemBucket is a platform bucket only when it is a SYSTEM bucket
// (review round 3). A bucket a person made under a now-reserved name is not
// used: ErrSystemBucketNameTaken (a 409 at the routes), with a WARN naming
// the project and the bucket, since no migration frees the name.
func requireSystemBucket(ctx context.Context, row BucketRow, projectID int64) (BucketRow, error) {
	if row.BucketType == "system" {
		return row, nil
	}
	slog.WarnContext(ctx, "a platform-reserved bucket name is held by a bucket that is not a system bucket",
		"project_id", projectID, "bucket", row.Name, "bucket_type", row.BucketType)
	return BucketRow{}, storage.ErrSystemBucketNameTaken
}
