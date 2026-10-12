package repos

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

type InputBundlesRepository struct {
	store sqlExecutor
}

func NewInputBundlesRepository(pool *pgxpool.Pool) (*InputBundlesRepository, error) {
	store, err := newPostgresSharedStore(pool)
	if err != nil {
		return nil, err
	}
	return &InputBundlesRepository{store: store}, nil
}

// ResolveClaimInput returns only the bounded immutable manifest. Content bytes
// remain behind the separately authorized input-content endpoint.
func (r *InputBundlesRepository) ResolveClaimInput(
	ctx context.Context,
	fence runtimedomain.Fence,
	reference *runtimev1.ExecutionInputBundleReferenceV1,
) (*runtimev1.ExecutionInputBundleV1, error) {
	if err := fence.Validate(); err != nil {
		return nil, err
	}
	if reference == nil || reference.GetInputBundleId() == "" || reference.GetImmutableVersion() == "" || reference.GetMediaType() != executiondomain.InputBundleManifestMediaType || reference.GetByteLength() == 0 || !validProtoDigest(reference.GetDigest()) {
		return nil, executiondomain.ErrInvalidInputBundle
	}

	var bundleID, version, mediaType string
	var manifest, digest []byte
	var manifestSize int64
	err := r.store.QueryRow(ctx, `
SELECT b.input_bundle_id,
       b.immutable_version,
       b.media_type,
       b.manifest_bytes,
       b.manifest_digest,
       b.manifest_size
FROM elitea_runtime.execution_claims AS c
JOIN elitea_runtime.execution_jobs AS j
  ON j.execution_id = c.execution_id AND j.generation = c.generation
JOIN elitea_runtime.input_bundles AS b
  ON b.input_bundle_id = j.input_bundle_id
WHERE c.execution_id = $1
  AND c.generation = $2
  AND j.command_id = $3
  AND c.workload_identity = $4
  AND c.workload_session_id = $5
  AND c.producer_id = $6
  AND c.claim_attempt = $7
  AND c.lease_epoch = $8
  AND c.fence_token = $9
  AND c.released_at IS NULL
  AND c.lease_expires_at > clock_timestamp()
  AND ((j.desired_state = 'RUNNING' AND c.recovery_mode <> 'NODE_RECOVERY') OR (
      j.desired_state IN ('SUSPENDED', 'RUNNING') AND c.recovery_mode = 'NODE_RECOVERY'
      AND j.capability_id IN ('agent.execute.application.v1', 'agent.execute.adhoc.v1')
  ))
  AND b.input_bundle_id = $10`,
		fence.ExecutionID,
		int64(fence.Generation),
		fence.CommandID,
		fence.WorkloadIdentity,
		fence.WorkloadSessionID,
		fence.ProducerID,
		int64(fence.ClaimAttempt),
		int64(fence.LeaseEpoch),
		fence.Token[:],
		reference.GetInputBundleId(),
	).Scan(&bundleID, &version, &mediaType, &manifest, &digest, &manifestSize)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, runtimedomain.ErrStaleFence
	}
	if err != nil {
		return nil, fmt.Errorf("resolve claimed input bundle: %w", err)
	}
	if manifestSize <= 0 || uint64(manifestSize) != reference.GetByteLength() || int64(len(manifest)) != manifestSize || bundleID != reference.GetInputBundleId() || version != reference.GetImmutableVersion() || mediaType != reference.GetMediaType() || len(digest) != len(runtimedomain.Digest{}) || subtle.ConstantTimeCompare(digest, reference.GetDigest().GetValue()) != 1 || runtimedomain.SHA256(manifest) != runtimedomain.Digest(digestArray(digest)) {
		return nil, executiondomain.ErrInvalidInputBundle
	}

	var decoded runtimev1.ExecutionInputBundleV1
	if err := (proto.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(manifest, &decoded); err != nil {
		return nil, fmt.Errorf("%w: stored immutable input manifest is not decodable", executiondomain.ErrInvalidInputBundle)
	}
	return &decoded, nil
}

func insertInputBundle(ctx context.Context, tx sqlExecutor, resourceProjectID int64, actorID string, bundle executiondomain.InputBundle, createdAt time.Time) error {
	if err := bundle.Validate(); err != nil {
		return err
	}
	if resourceProjectID <= 0 || actorID == "" || createdAt.IsZero() {
		return errors.New("input bundle owner is invalid")
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.input_bundles (
    input_bundle_id, immutable_version, media_type, resource_project_id,
    manifest_digest, manifest_size, manifest_bytes, created_by, created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		bundle.ID,
		bundle.Version,
		bundle.MediaType,
		resourceProjectID,
		bundle.Digest[:],
		int64(len(bundle.Manifest)),
		bundle.Manifest,
		actorID,
		createdAt,
	); err != nil {
		return fmt.Errorf("insert input bundle: %w", err)
	}
	for _, entry := range bundle.Entries {
		if _, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.input_bundle_entries (
    input_bundle_id, entry_id, entry_version, semantic_role, media_type,
    content_digest, content_size, content_reference, classification,
    required_grant_audience, content_bytes
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			bundle.ID,
			entry.ID,
			entry.Version,
			entry.SemanticRole,
			entry.MediaType,
			entry.ContentDigest[:],
			entry.ContentLength,
			entry.ContentID,
			entry.Classification,
			entry.RequiredGrantAudience,
			entry.Content,
		); err != nil {
			return fmt.Errorf("insert input bundle entry: %w", err)
		}
	}
	return nil
}

func validProtoDigest(digest *runtimev1.DigestV1) bool {
	return digest != nil && digest.GetAlgorithm() == runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256 && len(digest.GetValue()) == len(runtimedomain.Digest{})
}

func digestArray(value []byte) [32]byte {
	var digest [32]byte
	copy(digest[:], value)
	return digest
}
