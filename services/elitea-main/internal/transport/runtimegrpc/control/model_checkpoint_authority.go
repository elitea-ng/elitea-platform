package control

import (
	"context"
	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
)

func (s *Server) AuthorizeAgentModelCheckpoint(ctx context.Context, request *runtimev1.AuthorizeAgentModelCheckpointRequestV1) (*runtimev1.AuthorizeAgentModelCheckpointResponseV1, error) {
	if request == nil || request.GetIdentity() == nil || request.GetFence() == nil || hasUnknownFields(request.ProtoReflect()) {
		return modelCheckpointRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The invocation authorization request is malformed.", false), nil
	}
	workloadIdentity, err := s.authorizer.AuthorizeWorkload(ctx, request.GetFence().GetWorkloadSessionId(), request.GetFence().GetProducerId())
	if err != nil || workloadIdentity == "" {
		return modelCheckpointRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHENTICATION_FAILED, "The workload session is not accepted.", false), nil
	}
	fence, err := fenceDomain(request.GetIdentity(), request.GetFence(), workloadIdentity)
	if err != nil {
		return modelCheckpointRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The execution fence is malformed.", false), nil
	}
	digest, err := digestDomain(request.GetCheckpointDigest())
	if err != nil || digest.IsZero() {
		return modelCheckpointRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The checkpoint digest is malformed.", false), nil
	}
	controller, ok := s.claims.(executionapp.ModelCheckpointAuthorizationRepository)
	if !ok {
		return modelCheckpointRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE, "Checkpoint authorization is unavailable.", false), nil
	}
	disposition, err := controller.AuthorizeAgentModelCheckpoint(ctx, fence, digest)
	if err != nil {
		code, message, retryable := safeRuntimeError(err)
		return modelCheckpointRejection(code, message, retryable), nil
	}
	wireDisposition, err := authorizeInvocationDispositionProto(disposition)
	if err != nil {
		return modelCheckpointRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL, "The invocation authorization disposition is unavailable.", false), nil
	}
	return &runtimev1.AuthorizeAgentModelCheckpointResponseV1{Disposition: wireDisposition}, nil
}

func modelCheckpointRejection(code runtimev1.RuntimeErrorCodeV1, message string, retryable bool) *runtimev1.AuthorizeAgentModelCheckpointResponseV1 {
	rejection := authorizeInvocationRejection(code, message, retryable)
	return &runtimev1.AuthorizeAgentModelCheckpointResponseV1{Rejection: rejection.Rejection}
}
