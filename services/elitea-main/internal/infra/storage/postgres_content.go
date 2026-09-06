package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"fmt"
	"io"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/workloadidentity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const inputReadGrantAudience = "elitea.runtime.input.read.v1"

// PostgresContentRepository is the bounded first-slice content backend. It is
// suitable for small tenant-confidential configuration payloads. Large files
// use the separately scoped artifact service and are never admitted here.
type PostgresContentRepository struct {
	store contentQueryer
}

type contentQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func NewPostgresContentRepository(pool *pgxpool.Pool) (*PostgresContentRepository, error) {
	if pool == nil {
		return nil, errors.New("content database pool is required")
	}
	return newPostgresContentRepository(pool)
}

func newPostgresContentRepository(store contentQueryer) (*PostgresContentRepository, error) {
	if store == nil {
		return nil, errors.New("content database store is required")
	}
	return &PostgresContentRepository{store: store}, nil
}

func (r *PostgresContentRepository) AuthorizeContent(
	ctx context.Context,
	claim ContentClaim,
) (ContentAuthorization, error) {
	workloadID, err := workloadIdentity(claim.PeerCertificate)
	if err != nil {
		return ContentAuthorization{}, ErrContentUnauthorized
	}

	var authorization ContentAuthorization
	var digest []byte
	err = r.store.QueryRow(ctx, `
SELECT j.resource_project_id::text,
       j.actor_id,
       j.input_bundle_id,
       j.capability_id,
       e.semantic_role,
       e.media_type,
       e.content_digest,
       e.content_size
FROM elitea_runtime.execution_claims AS c
JOIN elitea_runtime.execution_jobs AS j
  ON j.execution_id = c.execution_id AND j.generation = c.generation
JOIN elitea_runtime.workload_sessions AS ws
  ON ws.workload_session_id = c.workload_session_id
 AND ws.workload_identity = c.workload_identity
 AND ws.producer_id = c.producer_id
JOIN elitea_runtime.input_bundle_entries AS e
  ON e.input_bundle_id = j.input_bundle_id
WHERE c.claim_id = $1
  AND c.execution_id = $2
  AND c.generation = $3
  AND c.workload_identity = $4
  AND c.fence_token = $5
  AND c.released_at IS NULL
  AND c.lease_expires_at > clock_timestamp()
  AND ws.issued_at <= clock_timestamp()
  AND ws.expires_at > clock_timestamp()
  AND ws.revoked_at IS NULL
  AND j.desired_state = 'RUNNING'
  AND j.capability_id IN (
      'configuration.validate.v1',
      'index.ingest.v1',
      'agent.execute.application.v1',
      'agent.execute.adhoc.v1',
      'toolkit.execute.read.v1'
  )
  AND e.content_reference = $6
  AND e.entry_version = $7
  AND e.required_grant_audience = $8`,
		claim.ClaimID,
		claim.ExecutionID,
		claim.Generation,
		workloadID,
		claim.FenceToken,
		claim.ContentID,
		claim.ImmutableVersion,
		inputReadGrantAudience,
	).Scan(
		&authorization.ResourceProjectID,
		&authorization.ActorID,
		&authorization.InputBundleID,
		&authorization.CapabilityID,
		&authorization.SemanticRole,
		&authorization.ExpectedMediaType,
		&digest,
		&authorization.ExpectedLength,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ContentAuthorization{}, ErrContentUnauthorized
	}
	if err != nil {
		return ContentAuthorization{}, fmt.Errorf("authorize input content: %w", err)
	}
	if len(digest) != sha256.Size {
		return ContentAuthorization{}, errors.New("authorize input content: invalid stored digest")
	}
	copy(authorization.ExpectedDigest[:], digest)
	return authorization, nil
}

func (r *PostgresContentRepository) OpenContent(
	ctx context.Context,
	resourceProjectID string,
	inputBundleID string,
	contentID string,
	immutableVersion string,
) (io.ReadCloser, error) {
	var content []byte
	err := r.store.QueryRow(ctx, `
SELECT e.content_bytes
FROM elitea_runtime.input_bundle_entries AS e
JOIN elitea_runtime.input_bundles AS b
  ON b.input_bundle_id = e.input_bundle_id
WHERE b.resource_project_id::text = $1
  AND b.input_bundle_id = $2
  AND e.content_reference = $3
  AND e.entry_version = $4`,
		resourceProjectID,
		inputBundleID,
		contentID,
		immutableVersion,
	).Scan(&content)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrContentNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("open input content: %w", err)
	}
	return io.NopCloser(bytes.NewReader(content)), nil
}

func workloadIdentity(certificate *x509.Certificate) (string, error) {
	identity, err := workloadidentity.Certificate(certificate)
	if err != nil {
		return "", ErrContentUnauthorized
	}
	return identity, nil
}
