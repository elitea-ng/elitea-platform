package storage

import (
	"context"
	"encoding/json"
	"errors"
	"unicode/utf8"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/grpc/codes"
)

const ToolkitDiscoveryArtifactMediaType = "application/vnd.elitea.toolkit-available-tools.v1+json"
const MaxToolkitDiscoveryArtifactBytes = 1024 * 1024

// ToolkitDiscoveryArtifactStore keeps immutable results outside command transport.
// Every operation validates the live claim and its admitted settings entry.
type ToolkitDiscoveryArtifactStore interface {
	PutToolkitDiscoveryArtifact(context.Context, ContentClaim, []byte) (*runtimev1.ToolkitAvailableToolsArtifactReferenceV1, error)
	GetToolkitDiscoveryArtifact(context.Context, ContentClaim) ([]byte, error)
}

// WithToolkitDiscoveryArtifacts adds a bounded result route before serving requests.
func (s *ContentServer) WithToolkitDiscoveryArtifacts(store ToolkitDiscoveryArtifactStore) *ContentServer {
	s.toolkitArtifacts = store
	return s
}

// PutToolkitDiscoveryResult stores the immutable available-tools result of
// one admitted toolkit input.
func (s *ContentServer) PutToolkitDiscoveryResult(ctx context.Context, request *runtimev1.PutToolkitDiscoveryResultRequestV1) (*runtimev1.PutToolkitDiscoveryResultResponseV1, error) {
	if s.toolkitArtifacts == nil {
		return nil, contentStatus(codes.Unimplemented)
	}
	release, err := acquireContentSlot(s.requests)
	if err != nil {
		return nil, err
	}
	defer release()
	claim, err := parseContentClaim(ctx, request.GetScope(), request.GetContentId(), request.GetImmutableVersion())
	if err != nil {
		return nil, contentStatus(codes.Unauthenticated)
	}
	// Authorize before looking at the document. The write repeats this check
	// under database locks to fence cancellation and claim replacement.
	authorization, err := s.authorizer.AuthorizeContent(ctx, claim)
	if err != nil || authorization.CapabilityID != "toolkit.available_tools.v1" || authorization.SemanticRole != "toolkit.available_tools.settings" {
		return nil, contentStatus(codes.PermissionDenied)
	}
	body := request.GetResultJson()
	if len(body) == 0 || len(body) > MaxToolkitDiscoveryArtifactBytes {
		return nil, contentStatus(codes.ResourceExhausted)
	}
	if !utf8.Valid(body) || !json.Valid(body) {
		return nil, contentStatus(codes.InvalidArgument)
	}
	ref, err := s.toolkitArtifacts.PutToolkitDiscoveryArtifact(ctx, claim, body)
	if err != nil {
		return nil, toolkitArtifactStatus(err)
	}
	return &runtimev1.PutToolkitDiscoveryResultResponseV1{Reference: ref}, nil
}

// GetToolkitDiscoveryResult reads that result back under the live claim.
func (s *ContentServer) GetToolkitDiscoveryResult(ctx context.Context, request *runtimev1.GetToolkitDiscoveryResultRequestV1) (*runtimev1.GetToolkitDiscoveryResultResponseV1, error) {
	if s.toolkitArtifacts == nil {
		return nil, contentStatus(codes.Unimplemented)
	}
	release, err := acquireContentSlot(s.requests)
	if err != nil {
		return nil, err
	}
	defer release()
	claim, err := parseContentClaim(ctx, request.GetScope(), request.GetContentId(), request.GetImmutableVersion())
	if err != nil {
		return nil, contentStatus(codes.Unauthenticated)
	}
	body, err := s.toolkitArtifacts.GetToolkitDiscoveryArtifact(ctx, claim)
	if err != nil {
		return nil, toolkitArtifactStatus(err)
	}
	return &runtimev1.GetToolkitDiscoveryResultResponseV1{ResultJson: body}, nil
}

func toolkitArtifactStatus(err error) error {
	switch {
	case errors.Is(err, ErrContentNotFound):
		return contentStatus(codes.NotFound)
	case errors.Is(err, ErrContentRejected), errors.Is(err, ErrSystemBucketNameTaken):
		return contentStatus(codes.Aborted)
	case errors.Is(err, ErrContentUnauthorized):
		return contentStatus(codes.PermissionDenied)
	}
	return contentStatus(codes.Unavailable)
}
