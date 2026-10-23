package storage

import (
	"context"
	"errors"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// maxCodeWorkspaceRequestBytes bounds one Code workspace request document.
const maxCodeWorkspaceRequestBytes = 2 << 20

// A nil dependency leaves the RPC unimplemented during a rolling deployment.
func (s *ContentServer) WithCodeWorkspaces(workspaces *CodeWorkspaceService) *ContentServer {
	if s == nil {
		return nil
	}
	s.codeWorkspaces = workspaces
	return s
}

// ResolveCodeWorkspace resolves the claimed execution's Code workspace
// manifest from the original declaration.
func (s *ContentServer) ResolveCodeWorkspace(ctx context.Context, request *runtimev1.ResolveCodeWorkspaceRequestV1) (*runtimev1.ResolveCodeWorkspaceResponseV1, error) {
	if s.codeWorkspaces == nil {
		return nil, contentStatus(codes.Unimplemented)
	}
	release, err := acquireContentSlot(s.requests)
	if err != nil {
		return nil, err
	}
	defer release()
	body := request.GetRequestJson()
	if len(body) == 0 || len(body) > maxCodeWorkspaceRequestBytes {
		return nil, contentStatus(codes.InvalidArgument)
	}
	claim, err := parseExecutionClaim(ctx, request.GetScope())
	if err != nil {
		return nil, contentStatus(codes.Unauthenticated)
	}
	parsed, err := ParseCodeWorkspaceResolveRequest(body, s.codeWorkspaces.policy)
	if err != nil {
		return nil, codeWorkspaceStatus(err)
	}
	manifest, err := s.codeWorkspaces.Resolve(ctx, claim, parsed)
	if err != nil {
		return nil, codeWorkspaceStatus(err)
	}
	return &runtimev1.ResolveCodeWorkspaceResponseV1{ManifestJson: manifest.Bytes(), WorkspaceRoot: manifest.Digest()}, nil
}

// codeWorkspaceStatus maps a workspace failure. A refused bound carries only
// its typed constant name and operator number.
func codeWorkspaceStatus(err error) error {
	var bounds *CodeWorkspaceBoundsError
	if errors.As(err, &bounds) {
		return grpcstatus.Error(codes.FailedPrecondition, bounds.Error())
	}
	switch {
	case errors.Is(err, ErrContentUnauthorized):
		return contentStatus(codes.PermissionDenied)
	case errors.Is(err, ErrCodeWorkspaceInvalid), errors.Is(err, ErrCodeWorkspaceCapability), errors.Is(err, ErrCodeWorkspaceWritableUnavailable):
		return contentStatus(codes.FailedPrecondition)
	}
	return contentStatus(codes.Unavailable)
}
