package control

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	indexregistryapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexregistry"
)

// IndexRemover runs Main's index delete flow for a worker's live claim
// (indexregistry.ClaimRemover). Nil in ServerConfig means the deployment does
// not serve `remove_index` through the worker (the Python indexing runtime),
// and the RPC answers UNSUPPORTED_CAPABILITY.
type IndexRemover interface {
	RemoveIndex(
		ctx context.Context,
		claim indexregistryapp.ClaimRef,
		request indexregistryapp.ClaimRemovalRequest,
	) (indexregistryapp.RemovalDisposition, error)
}

const (
	// maxRemoveIndexNameBytes bounds the index name and id the worker names;
	// the registry's own bound is the same. Both are required: the worker
	// names an index from the frozen catalog, which carries its id.
	maxRemoveIndexNameBytes = 4 << 10
)

// RemoveIndex is the `remove_index` tool's control-plane half (ADR-0030 I1,
// ADR-0031): the worker never deletes an index's vectors itself. The peer is
// authenticated, the fence verified live, and the claim handed to the remover,
// which resolves the run's principal, checks its permission and runs the same
// delete the REST route runs. The project is the claim's, never the request's.
func (s *Server) RemoveIndex(ctx context.Context, request *runtimev1.RemoveIndexRequestV1) (*runtimev1.RemoveIndexResponseV1, error) {
	if request == nil || request.GetIdentity() == nil || request.GetFence() == nil || hasUnknownFields(request.ProtoReflect()) ||
		!boundedRemoveIndexText(request.GetClaimId(), s.config.MaxStringBytes) ||
		request.GetToolkitId() <= 0 ||
		!boundedRemoveIndexText(request.GetIndexName(), maxRemoveIndexNameBytes) ||
		!boundedRemoveIndexText(request.GetIndexId(), maxRemoveIndexNameBytes) {
		return removeIndexRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The index removal request is malformed.", false), nil
	}
	if s.config.IndexRemovals == nil {
		return removeIndexRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY, "Index removal is not served by this deployment.", false), nil
	}
	workloadIdentity, err := s.authorizer.AuthorizeWorkload(ctx, request.GetFence().GetWorkloadSessionId(), request.GetFence().GetProducerId())
	if err != nil || workloadIdentity == "" {
		return removeIndexRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHENTICATION_FAILED, "The workload session is not accepted.", false), nil
	}
	fence, err := fenceDomain(request.GetIdentity(), request.GetFence(), workloadIdentity)
	if err != nil {
		return removeIndexRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The execution fence is malformed.", false), nil
	}
	if err := s.claims.VerifyActive(ctx, fence); err != nil {
		code, message, retryable := safeRuntimeError(err)
		return removeIndexRejection(code, message, retryable), nil
	}
	disposition, err := s.config.IndexRemovals.RemoveIndex(ctx, indexregistryapp.ClaimRef{
		ExecutionID:      fence.ExecutionID,
		Generation:       fence.Generation,
		ClaimID:          request.GetClaimId(),
		FenceToken:       append([]byte(nil), fence.Token[:]...),
		WorkloadIdentity: fence.WorkloadIdentity,
	}, indexregistryapp.ClaimRemovalRequest{
		ToolkitID: request.GetToolkitId(),
		IndexName: request.GetIndexName(),
		IndexID:   request.GetIndexId(),
	})
	if err != nil {
		switch {
		case errors.Is(err, indexregistryapp.ErrClaimUnauthorized):
			return removeIndexRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHORIZATION_FAILED, "The claim is not authorized to remove an index.", false), nil
		case errors.Is(err, indexregistryapp.ErrInvalidClaimRemovalRequest):
			return removeIndexRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The index removal request is malformed.", false), nil
		case errors.Is(err, indexregistryapp.ErrClaimRemovalUnavailable):
			return removeIndexRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE, "The index registry is temporarily unavailable.", true), nil
		}
		code, message, retryable := safeRuntimeError(err)
		return removeIndexRejection(code, message, retryable), nil
	}
	wire, ok := removeIndexDispositionProto(disposition)
	if !ok {
		return removeIndexRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL, "The index removal disposition is unavailable.", false), nil
	}
	return &runtimev1.RemoveIndexResponseV1{Disposition: wire}, nil
}

func removeIndexDispositionProto(disposition indexregistryapp.RemovalDisposition) (runtimev1.RemoveIndexDispositionV1, bool) {
	switch disposition {
	case indexregistryapp.RemovalRemoved:
		return runtimev1.RemoveIndexDispositionV1_REMOVE_INDEX_DISPOSITION_V1_REMOVED, true
	case indexregistryapp.RemovalNotFound:
		return runtimev1.RemoveIndexDispositionV1_REMOVE_INDEX_DISPOSITION_V1_NOT_FOUND, true
	case indexregistryapp.RemovalForbidden:
		return runtimev1.RemoveIndexDispositionV1_REMOVE_INDEX_DISPOSITION_V1_FORBIDDEN, true
	case indexregistryapp.RemovalActiveRun:
		return runtimev1.RemoveIndexDispositionV1_REMOVE_INDEX_DISPOSITION_V1_ACTIVE_RUN, true
	}
	return runtimev1.RemoveIndexDispositionV1_REMOVE_INDEX_DISPOSITION_V1_UNSPECIFIED, false
}

func removeIndexRejection(code runtimev1.RuntimeErrorCodeV1, message string, retryable bool) *runtimev1.RemoveIndexResponseV1 {
	return &runtimev1.RemoveIndexResponseV1{Rejection: runtimeError(code, message, retryable)}
}

func boundedRemoveIndexText(value string, limit int) bool {
	return value != "" && len(value) <= limit && utf8.ValidString(value) &&
		!strings.ContainsAny(value, "\x00\r\n")
}
