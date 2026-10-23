package storage

import (
	"context"
	"errors"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// Private claim-fenced decision RPCs of the per-interrupt HITL ledger
// (libs/proto/contracts/fanout-interrupt-decisions-v1.md v1.1 §7, §7.1) on the
// RuntimeContentService of the content listener (mTLS, #1359):
//
//	FetchExecutionInterrupts       (scope only)
//	AcknowledgeExecutionInterrupt  (scope + fanout-interrupt-ack-request JSON)
//
// The store returns the canonical response bytes; this layer only bounds the
// request, maps typed refusals to fixed status codes and never echoes ledger
// data in an error.

// MaxExecutionInterruptAckBytes bounds the raw ACK body (domain
// MaxAckBodyBytes; the contract §7.1 bound).
const MaxExecutionInterruptAckBytes = 1024

// Typed refusals of the decision RPCs. The status each one maps to is part of
// the contract (§7.1): the Worker branches on the status only.
var (
	// ErrInterruptClaimRefused: the claim is not the live ordinary RUNNING
	// claim (PermissionDenied, a control stop for the Worker).
	ErrInterruptClaimRefused = errors.New("execution interrupt claim refused")
	// ErrInterruptRequestInvalid: the request is malformed (InvalidArgument).
	ErrInterruptRequestInvalid = errors.New("execution interrupt request invalid")
	// ErrInterruptNotFound: the ACK names no card of the claim's response (NotFound).
	ErrInterruptNotFound = errors.New("execution interrupt not found")
	// ErrInterruptConflict: the ACK conflicts with the ledger (AlreadyExists).
	ErrInterruptConflict = errors.New("execution interrupt conflict")
)

// ExecutionInterruptControlStore serves the two private RPCs. Both run the
// shared live-claim authority under the job lock with the database clock.
type ExecutionInterruptControlStore interface {
	FetchExecutionInterrupts(context.Context, ContentClaim) ([]byte, error)
	AckExecutionInterrupt(context.Context, ContentClaim, []byte) ([]byte, error)
}

// WithExecutionInterrupts serves the decision RPCs. Without a store they
// answer Unimplemented.
func (s *ContentServer) WithExecutionInterrupts(control ExecutionInterruptControlStore) *ContentServer {
	if s != nil {
		s.executionInterrupts = control
	}
	return s
}

// FetchExecutionInterrupts answers the decided cards of the claimed
// execution's ledger (contract v1.1 §7) as the store's canonical JSON.
func (s *ContentServer) FetchExecutionInterrupts(ctx context.Context, request *runtimev1.FetchExecutionInterruptsRequestV1) (*runtimev1.FetchExecutionInterruptsResponseV1, error) {
	if s.executionInterrupts == nil {
		return nil, contentStatus(codes.Unimplemented)
	}
	claim, err := parseExecutionClaim(ctx, request.GetScope())
	if err != nil {
		return nil, grpcstatus.Error(codes.PermissionDenied, "Execution interrupt claim denied")
	}
	release, err := takeNodeRecoverySlot(s.requests)
	if err != nil {
		return nil, err
	}
	defer release()
	body, err := s.executionInterrupts.FetchExecutionInterrupts(ctx, claim)
	if err != nil {
		return nil, executionInterruptStatus(err)
	}
	return &runtimev1.FetchExecutionInterruptsResponseV1{FetchJson: body}, nil
}

// AcknowledgeExecutionInterrupt records the Worker's acknowledgement of one
// applied decision (contract v1.1 §7.1); the ACK is bounded before the store
// reads it.
func (s *ContentServer) AcknowledgeExecutionInterrupt(ctx context.Context, request *runtimev1.AcknowledgeExecutionInterruptRequestV1) (*runtimev1.AcknowledgeExecutionInterruptResponseV1, error) {
	if s.executionInterrupts == nil {
		return nil, contentStatus(codes.Unimplemented)
	}
	claim, err := parseExecutionClaim(ctx, request.GetScope())
	if err != nil {
		return nil, grpcstatus.Error(codes.PermissionDenied, "Execution interrupt claim denied")
	}
	raw := request.GetAckJson()
	if len(raw) == 0 || len(raw) > MaxExecutionInterruptAckBytes {
		return nil, grpcstatus.Error(codes.InvalidArgument, "Invalid execution interrupt acknowledgement")
	}
	release, err := takeNodeRecoverySlot(s.requests)
	if err != nil {
		return nil, err
	}
	defer release()
	body, err := s.executionInterrupts.AckExecutionInterrupt(ctx, claim, raw)
	if err != nil {
		return nil, executionInterruptStatus(err)
	}
	return &runtimev1.AcknowledgeExecutionInterruptResponseV1{OutcomeJson: body}, nil
}

// executionInterruptStatus maps a typed refusal to its fixed status code
// (contract §7.1). The Worker branches on the code only; no ledger data is
// echoed.
func executionInterruptStatus(err error) error {
	switch {
	case errors.Is(err, ErrInterruptClaimRefused):
		return grpcstatus.Error(codes.PermissionDenied, "Execution interrupt claim denied")
	case errors.Is(err, ErrInterruptRequestInvalid):
		return grpcstatus.Error(codes.InvalidArgument, "Invalid execution interrupt request")
	case errors.Is(err, ErrInterruptNotFound):
		return grpcstatus.Error(codes.NotFound, "Execution interrupt not found")
	case errors.Is(err, ErrInterruptConflict):
		return grpcstatus.Error(codes.AlreadyExists, "Execution interrupt conflicts with the ledger")
	default:
		return grpcstatus.Error(codes.Unavailable, "Execution interrupt control is unavailable")
	}
}
