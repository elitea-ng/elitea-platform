package execution

import (
	"context"
	"fmt"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

// ModelCheckpointAuthorizationRepository records execution authority only.
// Rust owns checkpoint contents, validation, and restoration.
type ModelCheckpointAuthorizationRepository interface {
	AuthorizeAgentModelCheckpoint(context.Context, runtimedomain.Fence, runtimedomain.Digest) (AuthorizeInvocationDisposition, error)
}

func (s *ClaimService) AuthorizeAgentModelCheckpoint(ctx context.Context, fence runtimedomain.Fence, digest runtimedomain.Digest) (AuthorizeInvocationDisposition, error) {
	if err := fence.Validate(); err != nil {
		return "", err
	}
	if digest.IsZero() {
		return "", ErrInvalidClaim
	}
	repository, ok := s.repository.(ModelCheckpointAuthorizationRepository)
	if !ok {
		return "", ErrClaimDependencyUnavailable
	}
	disposition, err := repository.AuthorizeAgentModelCheckpoint(ctx, fence, digest)
	if err != nil {
		return "", fmt.Errorf("authorize model checkpoint: %w", err)
	}
	if !disposition.valid() {
		return "", ErrInvalidClaim
	}
	return disposition, nil
}
