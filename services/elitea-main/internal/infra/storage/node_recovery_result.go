package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"

	domain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/noderecovery"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

type NodeRecoveryResultStore interface {
	ReadNodeRecoveryResult(context.Context, ContentClaim, string, string) ([]byte, error)
}

func (s *ContentServer) WithNodeRecoveryResults(store NodeRecoveryResultStore) *ContentServer {
	if s != nil {
		s.nodeRecoveryResults = store
	}
	return s
}

// ReadNodeRecoveryResult serves one recovered result whose version is its
// SHA-256.
func (s *ContentServer) ReadNodeRecoveryResult(ctx context.Context, request *runtimev1.ReadNodeRecoveryResultRequestV1) (*runtimev1.ReadNodeRecoveryResultResponseV1, error) {
	if s.nodeRecoveryResults == nil {
		return nil, contentStatus(codes.Unimplemented)
	}
	contentID, version := request.GetContentId(), request.GetVersion()
	if !domain.ValidID(contentID) || !domain.ValidID(version) {
		return nil, grpcstatus.Error(codes.InvalidArgument, "Invalid node recovery result request")
	}
	claim, err := parseExecutionClaim(ctx, request.GetScope())
	if err != nil {
		return nil, grpcstatus.Error(codes.PermissionDenied, "Node recovery claim denied")
	}
	release, err := takeNodeRecoverySlot(s.requests)
	if err != nil {
		return nil, err
	}
	defer release()
	data, err := s.nodeRecoveryResults.ReadNodeRecoveryResult(ctx, claim, contentID, version)
	if err != nil {
		return nil, nodeRecoveryControlStatus(err)
	}
	if len(data) == 0 || len(data) > 1024*1024 {
		return nil, nodeRecoveryControlStatus(ErrContentRejected)
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != version {
		return nil, nodeRecoveryControlStatus(ErrContentRejected)
	}
	return &runtimev1.ReadNodeRecoveryResultResponseV1{ResultJson: data}, nil
}
