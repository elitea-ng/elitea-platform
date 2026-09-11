package repos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/workloadidentity"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const discoveryObjectPrefix = "toolkit-discovery/"
const discoveryArtifactTimeout = 30 * time.Second

// ToolkitDiscoveryArtifactRepository reuses the existing artifact ledger and
// transfer inventory. It never issues a transferable upload grant to a worker.
type ToolkitDiscoveryArtifactRepository struct {
	pool    *pgxpool.Pool
	objects storage.ObjectStore
	buckets *ArtifactBucketsRepository
}

func NewToolkitDiscoveryArtifactRepository(pool *pgxpool.Pool, objects storage.ObjectStore) (*ToolkitDiscoveryArtifactRepository, error) {
	if pool == nil || objects == nil {
		return nil, errors.New("toolkit discovery requires artifact database and object store")
	}
	buckets, err := NewArtifactBucketsRepository(pool)
	if err != nil {
		return nil, err
	}
	return &ToolkitDiscoveryArtifactRepository{pool: pool, objects: objects, buckets: buckets}, nil
}

// Lock the admitted input together with the live claim. A second statement in
// the same READ COMMITTED transaction then sees any earlier writer's commit.
const discoveryArtifactAuthoritySQL = `
SELECT j.resource_project_id
FROM elitea_runtime.execution_claims c
JOIN elitea_runtime.execution_jobs j USING(execution_id,generation)
JOIN elitea_runtime.workload_sessions ws ON ws.workload_session_id=c.workload_session_id
 AND ws.workload_identity=c.workload_identity AND ws.producer_id=c.producer_id
JOIN elitea_runtime.input_bundle_entries e ON e.input_bundle_id=j.input_bundle_id
WHERE c.claim_id=$1 AND c.execution_id=$2 AND c.generation=$3
 AND c.workload_identity=$4 AND c.fence_token=$5 AND c.released_at IS NULL
 AND c.lease_expires_at>clock_timestamp() AND ws.issued_at<=clock_timestamp()
 AND ws.expires_at>clock_timestamp() AND ws.revoked_at IS NULL AND j.desired_state='RUNNING'
 AND j.capability_id='toolkit.available_tools.v1'
 AND e.content_reference=$6 AND e.entry_version=$7
 AND e.required_grant_audience='elitea.runtime.input.read.v1'
 AND e.semantic_role='toolkit.available_tools.settings'
FOR UPDATE OF c,j,ws`

func discoveryArtifactIdentity(claim storage.ContentClaim) string {
	encoded, _ := json.Marshal([]any{claim.ExecutionID, claim.Generation})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func lockDiscoveryArtifactClaim(ctx context.Context, tx pgx.Tx, claim storage.ContentClaim) (int64, error) {
	identity, err := workloadidentity.Certificate(claim.PeerCertificate)
	if err != nil {
		return 0, storage.ErrContentUnauthorized
	}
	args := []any{claim.ClaimID, claim.ExecutionID, claim.Generation, identity, claim.FenceToken, claim.ContentID, claim.ImmutableVersion}
	var projectID int64
	if err = tx.QueryRow(ctx, discoveryArtifactAuthoritySQL, args...).Scan(&projectID); err != nil {
		return 0, discoveryArtifactError(ctx, err)
	}
	// Expiry can pass while waiting for a preceding holder of the row locks.
	if err = tx.QueryRow(ctx, discoveryArtifactAuthoritySQL, args...).Scan(&projectID); err != nil {
		return 0, discoveryArtifactError(ctx, err)
	}
	return projectID, nil
}

func discoveryArtifactError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return storage.ErrContentUnauthorized
	}
	return storage.ErrContentUnavailable
}

type discoveryArtifactRecord struct {
	id, version, storageID string
	projectID              int64
	length                 int64
	digest                 []byte
}

func findDiscoveryArtifact(ctx context.Context, tx pgx.Tx, claim storage.ContentClaim) (discoveryArtifactRecord, error) {
	var a discoveryArtifactRecord
	err := tx.QueryRow(ctx, `SELECT artifact_id,immutable_version,storage_record_id,resource_project_id,byte_length,digest
FROM elitea_runtime.index_result_artifacts WHERE execution_id=$1 AND generation=$2 AND artifact_id=$3
 AND media_type=$4 AND classification='tenant-confidential'`, claim.ExecutionID, claim.Generation, discoveryArtifactIdentity(claim), storage.ToolkitDiscoveryArtifactMediaType).
		Scan(&a.id, &a.version, &a.storageID, &a.projectID, &a.length, &a.digest)
	return a, err
}

func (a discoveryArtifactRecord) reference() *runtimev1.ToolkitAvailableToolsArtifactReferenceV1 {
	return &runtimev1.ToolkitAvailableToolsArtifactReferenceV1{ArtifactId: a.id, ImmutableVersion: a.version,
		MediaType: storage.ToolkitDiscoveryArtifactMediaType, ByteLength: uint64(a.length), Classification: "tenant-confidential",
		Digest: &runtimev1.DigestV1{Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256, Value: bytes.Clone(a.digest)}}
}

func (r *ToolkitDiscoveryArtifactRepository) PutToolkitDiscoveryArtifact(ctx context.Context, claim storage.ContentClaim, content []byte) (*runtimev1.ToolkitAvailableToolsArtifactReferenceV1, error) {
	if len(content) == 0 || len(content) > storage.MaxToolkitDiscoveryArtifactBytes || !utf8.Valid(content) || !json.Valid(content) {
		return nil, storage.ErrContentRejected
	}
	ctx, cancel := context.WithTimeout(ctx, discoveryArtifactTimeout)
	defer cancel()
	digest := sha256.Sum256(content)
	wanted := discoveryArtifactRecord{id: discoveryArtifactIdentity(claim), version: hex.EncodeToString(digest[:]), length: int64(len(content)), digest: digest[:]}
	// Authorize and short-circuit retries before creating any object or inventory.
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, discoveryArtifactError(ctx, err)
	}
	projectID, err := lockDiscoveryArtifactClaim(ctx, tx, claim)
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	existing, lookupErr := findDiscoveryArtifact(ctx, tx, claim)
	_ = tx.Rollback(ctx)
	if lookupErr == nil {
		return matchDiscoveryArtifact(existing, wanted, projectID)
	}
	if !errors.Is(lookupErr, pgx.ErrNoRows) {
		return nil, discoveryArtifactError(ctx, lookupErr)
	}
	wanted.projectID = projectID
	bucket, err := r.buckets.GetBucket(ctx, projectID, indexArtifactBucketName)
	if errors.Is(err, storage.ErrNotFound) {
		bucket, err = r.buckets.CreateBucket(ctx, NewBucketInput{ProjectID: projectID, Name: indexArtifactBucketName, DisplayName: indexArtifactBucketName, BucketType: "system"})
		if errors.Is(err, storage.ErrAlreadyExists) {
			bucket, err = r.buckets.GetBucket(ctx, projectID, indexArtifactBucketName)
		}
	}
	if err != nil {
		return nil, discoveryArtifactError(ctx, err)
	}
	wanted.storageID, err = generateArtifactGrantID()
	if err != nil {
		return nil, storage.ErrContentUnavailable
	}
	// A unique attempt suffix makes orphan deletion independent of retry uploads.
	key := discoveryObjectPrefix + wanted.id + "/" + wanted.version + "/" + wanted.storageID
	ref, err := storage.NewObjectRef(strconv.FormatInt(projectID, 10), bucket.Name, key)
	if err != nil {
		return nil, storage.ErrContentUnavailable
	}
	// Consumed before upload: no public grant endpoint can redeem this internal
	// inventory, and the generic unconsumed-grant sweeper cannot claim it.
	_, err = r.pool.Exec(ctx, `INSERT INTO elitea_storage.transfer_grants
 (id,project_id,bucket_id,key,method,content_type,max_bytes,digest_alg,digest,expires_at,consumed_at)
 VALUES ($1,$2,$3,$4,'PUT',$5,$6,'sha256',$7,clock_timestamp()+interval '15 minutes',clock_timestamp())`,
		wanted.storageID, projectID, bucket.ID, key, storage.ToolkitDiscoveryArtifactMediaType, len(content), digest[:])
	if err != nil {
		return nil, discoveryArtifactError(ctx, err)
	}
	if _, err = r.objects.Put(ctx, ref, bytes.NewReader(content), storage.PutOptions{ContentType: storage.ToolkitDiscoveryArtifactMediaType, ContentLength: int64(len(content))}); err != nil {
		return nil, discoveryArtifactError(ctx, err)
	}
	verified, err := r.readDiscoveryObject(ctx, ref, wanted)
	if err != nil {
		return nil, err
	}
	clear(verified)
	tx, err = r.pool.Begin(ctx)
	if err != nil {
		return nil, discoveryArtifactError(ctx, err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	projectID, err = lockDiscoveryArtifactClaim(ctx, tx, claim)
	if err != nil {
		return nil, err
	}
	// Lock the durable upload inventory against orphan collection until commit.
	var grantID string
	err = tx.QueryRow(ctx, `SELECT id::text FROM elitea_storage.transfer_grants WHERE id=$1 AND project_id=$2
 AND key=$3 AND consumed_at IS NOT NULL AND expires_at>clock_timestamp() FOR UPDATE`, wanted.storageID, projectID, key).Scan(&grantID)
	if err != nil {
		return nil, discoveryArtifactError(ctx, err)
	}
	existing, err = findDiscoveryArtifact(ctx, tx, claim)
	if err == nil {
		return matchDiscoveryArtifact(existing, wanted, projectID)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, discoveryArtifactError(ctx, err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO elitea_runtime.index_result_artifacts
 (artifact_id,immutable_version,execution_id,generation,resource_project_id,media_type,byte_length,digest,
 classification,storage_record_id,metadata_created_at,bytes_verified_at)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'tenant-confidential',$9,statement_timestamp(),clock_timestamp())`,
		wanted.id, wanted.version, claim.ExecutionID, claim.Generation, projectID, storage.ToolkitDiscoveryArtifactMediaType, wanted.length, wanted.digest, wanted.storageID)
	if err != nil {
		return nil, discoveryArtifactError(ctx, err)
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, discoveryArtifactError(ctx, err)
	}
	return wanted.reference(), nil
}

func matchDiscoveryArtifact(a, wanted discoveryArtifactRecord, projectID int64) (*runtimev1.ToolkitAvailableToolsArtifactReferenceV1, error) {
	if a.projectID != projectID || a.version != wanted.version || a.length != wanted.length || !bytes.Equal(a.digest, wanted.digest) {
		return nil, storage.ErrContentRejected
	}
	return a.reference(), nil
}

func (r *ToolkitDiscoveryArtifactRepository) readDiscoveryObject(ctx context.Context, ref storage.ObjectRef, a discoveryArtifactRecord) ([]byte, error) {
	body, info, err := r.objects.Get(ctx, ref, nil)
	if err != nil {
		return nil, discoveryArtifactError(ctx, err)
	}
	defer body.Close()
	if a.length <= 0 || a.length > storage.MaxToolkitDiscoveryArtifactBytes || info.Size != a.length {
		return nil, storage.ErrContentRejected
	}
	content, err := io.ReadAll(io.LimitReader(body, storage.MaxToolkitDiscoveryArtifactBytes+1))
	if err != nil {
		clear(content)
		return nil, discoveryArtifactError(ctx, err)
	}
	digest := sha256.Sum256(content)
	if int64(len(content)) != a.length || !bytes.Equal(digest[:], a.digest) || !utf8.Valid(content) || !json.Valid(content) {
		clear(content)
		return nil, storage.ErrContentRejected
	}
	return content, nil
}

func (r *ToolkitDiscoveryArtifactRepository) openDiscoveryArtifact(ctx context.Context, a discoveryArtifactRecord) ([]byte, error) {
	var key, bucket string
	err := r.pool.QueryRow(ctx, `SELECT g.key,b.name FROM elitea_storage.transfer_grants g JOIN elitea_storage.buckets b ON b.id=g.bucket_id
 WHERE g.id::text=$1 AND g.project_id=$2 AND b.project_id=g.project_id AND b.name=$3 AND g.consumed_at IS NOT NULL
 AND g.content_type=$4 AND g.max_bytes=$5 AND g.digest=$6`, a.storageID, a.projectID, indexArtifactBucketName, storage.ToolkitDiscoveryArtifactMediaType, a.length, a.digest).Scan(&key, &bucket)
	if err != nil {
		return nil, discoveryArtifactError(ctx, err)
	}
	if key != discoveryObjectPrefix+a.id+"/"+a.version+"/"+a.storageID {
		return nil, storage.ErrContentRejected
	}
	ref, err := storage.NewObjectRef(strconv.FormatInt(a.projectID, 10), bucket, key)
	if err != nil {
		return nil, storage.ErrContentRejected
	}
	return r.readDiscoveryObject(ctx, ref, a)
}

func (r *ToolkitDiscoveryArtifactRepository) GetToolkitDiscoveryArtifact(ctx context.Context, claim storage.ContentClaim) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, discoveryArtifactTimeout)
	defer cancel()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, discoveryArtifactError(ctx, err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	projectID, err := lockDiscoveryArtifactClaim(ctx, tx, claim)
	if err != nil {
		return nil, err
	}
	a, err := findDiscoveryArtifact(ctx, tx, claim)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, storage.ErrContentNotFound
	}
	if err != nil {
		return nil, discoveryArtifactError(ctx, err)
	}
	if a.projectID != projectID {
		return nil, storage.ErrContentUnauthorized
	}
	// End the transaction before I/O; do not occupy a second pooled connection.
	_ = tx.Rollback(ctx)
	return r.openDiscoveryArtifact(ctx, a)
}

func (r *ToolkitDiscoveryArtifactRepository) ReadToolkitDiscoveryResult(ctx context.Context, projectID int64, executionID string, generation uint64, reference *runtimev1.ToolkitAvailableToolsArtifactReferenceV1) ([]byte, error) {
	if projectID <= 0 || executionID == "" || generation == 0 || generation > 1<<62 || reference == nil || reference.Digest == nil || reference.Digest.Algorithm != runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256 || reference.MediaType != storage.ToolkitDiscoveryArtifactMediaType || reference.Classification != "tenant-confidential" {
		return nil, storage.ErrContentRejected
	}
	ctx, cancel := context.WithTimeout(ctx, discoveryArtifactTimeout)
	defer cancel()
	var a discoveryArtifactRecord
	err := r.pool.QueryRow(ctx, `SELECT a.artifact_id,a.immutable_version,a.storage_record_id,a.resource_project_id,a.byte_length,a.digest
 FROM elitea_runtime.index_result_artifacts a JOIN elitea_runtime.execution_jobs j USING(execution_id,generation)
 WHERE a.execution_id=$1 AND a.generation=$2 AND a.resource_project_id=$3 AND j.resource_project_id=$3
 AND j.capability_id='toolkit.available_tools.v1' AND a.artifact_id=$4 AND a.immutable_version=$5
 AND a.media_type=$6 AND a.classification='tenant-confidential'
 AND EXISTS (SELECT 1 FROM elitea_runtime.output_inbox o WHERE o.execution_id=j.execution_id AND o.generation=j.generation
 AND o.payload_type='TOOLKIT_AVAILABLE_TOOLS_RESULT' AND o.settlement_outcome='SUCCEEDED')`, executionID, generation, projectID, reference.ArtifactId, reference.ImmutableVersion, storage.ToolkitDiscoveryArtifactMediaType).
		Scan(&a.id, &a.version, &a.storageID, &a.projectID, &a.length, &a.digest)
	if err != nil {
		return nil, discoveryArtifactError(ctx, err)
	}
	if uint64(a.length) != reference.ByteLength || !bytes.Equal(a.digest, reference.Digest.Value) {
		return nil, storage.ErrContentRejected
	}
	return r.openDiscoveryArtifact(ctx, a)
}

// SweepOrphanToolkitDiscoveryArtifacts reclaims abandoned uploads and artifacts
// whose execution was deleted. Failed deletion rolls back, retaining inventory
// for retry. An accepted ledger row always protects its object, even after TTL.
func (r *ToolkitDiscoveryArtifactRepository) SweepOrphanToolkitDiscoveryArtifacts(ctx context.Context, limit int) (int, error) {
	if limit <= 0 || limit > 128 {
		return 0, storage.ErrContentRejected
	}
	ctx, cancel := context.WithTimeout(ctx, discoveryArtifactTimeout)
	defer cancel()
	deleted := 0
	for ; deleted < limit; deleted++ {
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			return deleted, discoveryArtifactError(ctx, err)
		}
		var id, key, bucket string
		var projectID int64
		err = tx.QueryRow(ctx, `SELECT g.id::text,g.project_id,g.key,b.name FROM elitea_storage.transfer_grants g
 JOIN elitea_storage.buckets b ON b.id=g.bucket_id AND b.project_id=g.project_id
 WHERE g.consumed_at IS NOT NULL AND g.expires_at<clock_timestamp() AND g.key LIKE 'toolkit-discovery/%'
 AND b.name=$1 AND NOT EXISTS (SELECT 1 FROM elitea_runtime.index_result_artifacts a WHERE a.storage_record_id=g.id::text)
 ORDER BY g.expires_at,g.id LIMIT 1 FOR UPDATE OF g SKIP LOCKED`, indexArtifactBucketName).Scan(&id, &projectID, &key, &bucket)
		if errors.Is(err, pgx.ErrNoRows) {
			_ = tx.Rollback(ctx)
			return deleted, nil
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return deleted, discoveryArtifactError(ctx, err)
		}
		if !strings.HasPrefix(key, discoveryObjectPrefix) {
			_ = tx.Rollback(ctx)
			return deleted, storage.ErrContentRejected
		}
		ref, err := storage.NewObjectRef(strconv.FormatInt(projectID, 10), bucket, key)
		if err == nil {
			err = r.objects.Delete(ctx, ref)
		}
		if err == nil {
			_, err = tx.Exec(ctx, `DELETE FROM elitea_storage.transfer_grants WHERE id=$1`, id)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return deleted, discoveryArtifactError(ctx, err)
		}
		if err = tx.Commit(ctx); err != nil {
			return deleted, discoveryArtifactError(ctx, err)
		}
	}
	return deleted, nil
}
