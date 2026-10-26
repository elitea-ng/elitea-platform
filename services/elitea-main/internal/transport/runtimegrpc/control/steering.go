package control

import (
	"context"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

// SteeringClaimer hands a live claim the steering messages queued against its
// turn (agentexecution.SteeringService.Claim). Nil means the deployment does
// not serve steering, and the RPC answers UNSUPPORTED_CAPABILITY; the worker
// then stops asking for the rest of the turn.
type SteeringClaimer interface {
	Claim(ctx context.Context, fence runtimedomain.Fence) ([]agentexecutionapp.ClaimedSteeringMessage, error)
}

// WithSteeringMessages lets the worker claim steering messages at its safe
// points (client contract 1.26).
func (s *Server) WithSteeringMessages(steering SteeringClaimer) *Server {
	s.steering = steering
	return s
}

// ClaimSteeringMessages is the worker's half of steering during execution.
// The peer is authenticated and the fence verified live; the claimer locks the
// same claim again inside its transaction, so a stale or released claim takes
// nothing. Only an ordinary agent claim may ask: a node recovery claim or
// another capability is refused as a stale fence.
func (s *Server) ClaimSteeringMessages(ctx context.Context, request *runtimev1.ClaimSteeringMessagesRequestV1) (*runtimev1.ClaimSteeringMessagesResponseV1, error) {
	if request == nil || request.GetIdentity() == nil || request.GetFence() == nil || hasUnknownFields(request.ProtoReflect()) {
		return steeringRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The steering claim request is malformed.", false), nil
	}
	if s.steering == nil {
		return steeringRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY, "Steering is not served by this deployment.", false), nil
	}
	workloadIdentity, err := s.authorizer.AuthorizeWorkload(ctx, request.GetFence().GetWorkloadSessionId(), request.GetFence().GetProducerId())
	if err != nil || workloadIdentity == "" {
		return steeringRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHENTICATION_FAILED, "The workload session is not accepted.", false), nil
	}
	fence, err := fenceDomain(request.GetIdentity(), request.GetFence(), workloadIdentity)
	if err != nil {
		return steeringRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The execution fence is malformed.", false), nil
	}
	if err := s.claims.VerifyActive(ctx, fence); err != nil {
		code, message, retryable := safeRuntimeError(err)
		return steeringRejection(code, message, retryable), nil
	}
	messages, err := s.steering.Claim(ctx, fence)
	if err != nil {
		code, message, retryable := safeRuntimeError(err)
		if code == runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL {
			// The queue keeps the messages; the next safe point asks again.
			code, message, retryable = runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE, "The steering queue is temporarily unavailable.", true
		}
		return steeringRejection(code, message, retryable), nil
	}
	response := &runtimev1.ClaimSteeringMessagesResponseV1{
		Messages: make([]*runtimev1.SteeringMessageV1, 0, len(messages)),
	}
	for _, message := range messages {
		response.Messages = append(response.Messages, &runtimev1.SteeringMessageV1{
			SteeringId:         message.ID,
			Content:            message.Content,
			QueuedAtUnixMillis: message.QueuedAt.UTC().UnixMilli(),
		})
	}
	return response, nil
}

func steeringRejection(code runtimev1.RuntimeErrorCodeV1, message string, retryable bool) *runtimev1.ClaimSteeringMessagesResponseV1 {
	return &runtimev1.ClaimSteeringMessagesResponseV1{Rejection: runtimeError(code, message, retryable)}
}
