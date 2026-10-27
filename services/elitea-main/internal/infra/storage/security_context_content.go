package storage

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"

	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
)

// SecurityContextSource opens a governed execution's security context under
// the caller's live claim (ADR-0039, #1115). The content authorizer
// implements it; a backend without it never serves the namespace.
type SecurityContextSource interface {
	OpenSecurityContext(ctx context.Context, claim ContentClaim) ([]byte, runtimedomain.Digest, error)
}

// OpenSecurityContext returns the context bytes and stored digest of the
// claim's execution, when the claim is live and the content id and version
// name exactly that context. It is the GetInput authorization of the
// elitea.security-context/ namespace: claim -> job -> context row, with the
// same live-claim and workload-session predicates as AuthorizeContent.
func (r *PostgresContentRepository) OpenSecurityContext(ctx context.Context, claim ContentClaim) ([]byte, runtimedomain.Digest, error) {
	workloadID, err := workloadIdentity(claim.PeerCertificate)
	if err != nil {
		return nil, runtimedomain.Digest{}, ErrContentUnauthorized
	}
	var data, digest []byte
	err = r.store.QueryRow(ctx, `
SELECT sc.context_bytes, sc.digest
FROM elitea_runtime.execution_claims AS c
JOIN elitea_runtime.execution_jobs AS j
  ON j.execution_id = c.execution_id AND j.generation = c.generation
JOIN elitea_runtime.workload_sessions AS ws
  ON ws.workload_session_id = c.workload_session_id
 AND ws.workload_identity = c.workload_identity
 AND ws.producer_id = c.producer_id
JOIN elitea_runtime.execution_security_contexts AS sc
  ON sc.execution_id = c.execution_id
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
  AND j.desired_state IN ('RUNNING', 'SUSPENDED')
  AND sc.content_id = $6`,
		claim.ClaimID,
		claim.ExecutionID,
		claim.Generation,
		workloadID,
		claim.FenceToken,
		claim.ContentID,
	).Scan(&data, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, runtimedomain.Digest{}, ErrContentUnauthorized
	}
	if err != nil {
		return nil, runtimedomain.Digest{}, fmt.Errorf("open execution security context: %w", err)
	}
	var stored runtimedomain.Digest
	if len(digest) != len(stored) {
		return nil, runtimedomain.Digest{}, errors.New("open execution security context: invalid stored digest")
	}
	copy(stored[:], digest)
	if claim.ImmutableVersion != stored.String() {
		return nil, runtimedomain.Digest{}, ErrContentUnauthorized
	}
	return data, stored, nil
}

func isSecurityContextContentID(contentID string) bool {
	return strings.HasPrefix(contentID, securitycontext.ContentIDPrefix)
}

// getSecurityContext serves the security context namespace of GetInput. The
// bytes are sent as stored, never materialized: the worker verifies them
// against the digest its signed command carries.
func (s *ContentServer) getSecurityContext(ctx context.Context, claim ContentClaim, stream grpc.ServerStreamingServer[runtimev1.GetInputResponseV1]) error {
	source, ok := s.authorizer.(SecurityContextSource)
	if !ok || claim.ContentID != securitycontext.ContentIDPrefix+claim.ExecutionID {
		return contentStatus(codes.PermissionDenied)
	}
	data, stored, err := source.OpenSecurityContext(ctx, claim)
	if errors.Is(err, ErrContentUnauthorized) {
		return contentStatus(codes.PermissionDenied)
	}
	if err != nil {
		s.logInputFailure(ctx, claim, "security_context_open", codes.Internal, err)
		return contentStatus(codes.Internal)
	}
	digest := sha256.Sum256(data)
	if len(data) == 0 || len(data) > securitycontext.MaxBytes || subtle.ConstantTimeCompare(digest[:], stored[:]) != 1 {
		s.logInputFailure(ctx, claim, "security_context_digest", codes.Internal, nil)
		return contentStatus(codes.Internal)
	}
	if err := stream.Send(&runtimev1.GetInputResponseV1{Part: &runtimev1.GetInputResponseV1_Header{Header: &runtimev1.InputContentHeaderV1{
		MediaType:              securitycontext.MediaType,
		Length:                 uint64(len(data)),
		Sha256:                 digest[:],
		SourceSha256:           digest[:],
		SourceLength:           uint64(len(data)),
		SourceImmutableVersion: claim.ImmutableVersion,
	}}}); err != nil {
		return err
	}
	return stream.Send(&runtimev1.GetInputResponseV1{Part: &runtimev1.GetInputResponseV1_Chunk{Chunk: data}})
}
