package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"

	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/noderecovery"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

type NodeRecoveryControl struct {
	Schema       string          `json:"schema"`
	ExecutionID  string          `json:"execution_id"`
	Generation   uint64          `json:"generation"`
	DesiredState string          `json:"desired_state"`
	Receipt      json.RawMessage `json:"receipt"`
	Action       json.RawMessage `json:"action"`
}

type NodeRecoveryAck struct {
	RequestID                string                    `json:"request_id"`
	ActivationID             string                    `json:"activation_id"`
	ExpectedRevision         uint64                    `json:"expected_revision"`
	ReceiptSHA256            string                    `json:"receipt_sha256"`
	AppliedRevision          uint64                    `json:"applied_revision"`
	ContinuationReceipt      json.RawMessage           `json:"continuation_receipt"`
	TerminalStopReason       *string                   `json:"terminal_stop_reason"`
	FailureRouteContinuation *NodeRecoveryFailureRoute `json:"failure_route_continuation"`
}

type NodeRecoveryFailedDescriptor struct {
	ActivationID string `json:"activation_id"`
	Attempt      uint16 `json:"attempt"`
	FailureClass string `json:"failure_class"`
	StopReason   string `json:"stop_reason"`
}
type NodeRecoveryFailureRoute struct {
	Schema  string                       `json:"schema"`
	RouteID string                       `json:"route_id"`
	Failed  NodeRecoveryFailedDescriptor `json:"failed"`
}

func (a NodeRecoveryAck) Validate() bool {
	if a.FailureRouteContinuation != nil {
		f := a.FailureRouteContinuation
		if f.Schema != "elitea.pipeline.node-recovery-failure-route.v1" || !domain.ValidID(f.RouteID) || f.Failed.ActivationID != a.ActivationID || f.Failed.Attempt < 1 || f.Failed.Attempt > 16 || a.TerminalStopReason != nil || len(a.ContinuationReceipt) > 0 && !bytes.Equal(bytes.TrimSpace(a.ContinuationReceipt), []byte("null")) {
			return false
		}
		switch f.Failed.StopReason {
		case "attempts_exhausted", "elapsed_limit", "not_retryable", "retry_disabled":
		default:
			return false
		}
	}

	if a.TerminalStopReason != nil {
		switch *a.TerminalStopReason {
		case "attempts_exhausted", "elapsed_limit", "not_retryable", "retry_disabled":
		default:
			return false
		}
		if len(a.ContinuationReceipt) > 0 && !bytes.Equal(bytes.TrimSpace(a.ContinuationReceipt), []byte("null")) {
			return false
		}
	}
	if len(a.ContinuationReceipt) > 0 && !bytes.Equal(bytes.TrimSpace(a.ContinuationReceipt), []byte("null")) {
		r, err := domain.DecodeReceipt(a.ContinuationReceipt)
		if err != nil || r.ActivationID != a.ActivationID || r.JournalRevision != a.AppliedRevision {
			return false
		}
	}
	return domain.ValidID(a.RequestID) && domain.ValidID(a.ActivationID) && domain.ValidID(a.ReceiptSHA256) && a.ExpectedRevision > 0 && a.ExpectedRevision < math.MaxInt64 && a.AppliedRevision == a.ExpectedRevision+1
}

type NodeRecoveryResumption struct {
	Schema                   string                    `json:"schema"`
	ExecutionID              string                    `json:"execution_id"`
	Generation               uint64                    `json:"generation"`
	RequestID                string                    `json:"request_id"`
	ActivationID             string                    `json:"activation_id"`
	JournalRevision          uint64                    `json:"journal_revision"`
	InputBundleID            string                    `json:"input_bundle_id"`
	InputManifestSHA256      string                    `json:"input_manifest_sha256"`
	ReceiptSHA256            string                    `json:"receipt_sha256"`
	ClaimID                  string                    `json:"claim_id"`
	FailureRouteContinuation *NodeRecoveryFailureRoute `json:"failure_route_continuation"`
}

type NodeRecoveryTerminalSettlement struct {
	Schema          string `json:"schema"`
	ExecutionID     string `json:"execution_id"`
	Generation      uint64 `json:"generation"`
	RequestID       string `json:"request_id"`
	ActivationID    string `json:"activation_id"`
	JournalRevision uint64 `json:"journal_revision"`
	ReceiptSHA256   string `json:"receipt_sha256"`
	ClaimID         string `json:"claim_id"`
	StopReason      string `json:"stop_reason"`
}

type NodeRecoveryAckOutcome struct {
	Schema                       string                          `json:"schema"`
	ExecutionID                  string                          `json:"execution_id"`
	Generation                   uint64                          `json:"generation"`
	RequestID                    string                          `json:"request_id"`
	AppliedRevision              uint64                          `json:"applied_revision"`
	Replay                       bool                            `json:"replay"`
	RecoveryResumeAuthorized     bool                            `json:"recovery_resume_authorized"`
	Resumption                   *NodeRecoveryResumption         `json:"resumption"`
	TerminalSettlementAuthorized bool                            `json:"terminal_settlement_authorized"`
	TerminalAuthorization        *NodeRecoveryTerminalSettlement `json:"terminal_authorization"`
}

type NodeRecoveryControlStore interface {
	PollNodeRecovery(context.Context, ContentClaim) (NodeRecoveryControl, error)
	AcknowledgeNodeRecovery(context.Context, ContentClaim, NodeRecoveryAck) (NodeRecoveryAckOutcome, error)
}

func (s *ContentServer) WithNodeRecoveryControl(control NodeRecoveryControlStore) *ContentServer {
	if s != nil {
		s.nodeRecovery = control
	}
	return s
}

// PollNodeRecovery answers the claimed execution's node recovery control.
func (s *ContentServer) PollNodeRecovery(ctx context.Context, request *runtimev1.PollNodeRecoveryRequestV1) (*runtimev1.PollNodeRecoveryResponseV1, error) {
	if s.nodeRecovery == nil {
		return nil, contentStatus(codes.Unimplemented)
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
	outcome, err := s.nodeRecovery.PollNodeRecovery(ctx, claim)
	if err != nil {
		return nil, nodeRecoveryControlStatus(err)
	}
	encoded, err := nodeRecoveryJSON(outcome)
	if err != nil {
		return nil, err
	}
	return &runtimev1.PollNodeRecoveryResponseV1{ControlJson: encoded}, nil
}

// AcknowledgeNodeRecovery records the worker's acknowledgement of one
// recovery control.
func (s *ContentServer) AcknowledgeNodeRecovery(ctx context.Context, request *runtimev1.AcknowledgeNodeRecoveryRequestV1) (*runtimev1.AcknowledgeNodeRecoveryResponseV1, error) {
	if s.nodeRecovery == nil {
		return nil, contentStatus(codes.Unimplemented)
	}
	claim, err := parseExecutionClaim(ctx, request.GetScope())
	if err != nil {
		return nil, grpcstatus.Error(codes.PermissionDenied, "Node recovery claim denied")
	}
	raw := request.GetAckJson()
	invalid := grpcstatus.Error(codes.InvalidArgument, "Invalid node recovery acknowledgement")
	if len(raw) > 8192 {
		return nil, invalid
	}
	var ack NodeRecoveryAck
	if domain.DecodeStrictNullableObject(raw, &ack, []string{"request_id", "activation_id", "expected_revision", "receipt_sha256", "applied_revision", "continuation_receipt", "terminal_stop_reason", "failure_route_continuation"}, []string{"continuation_receipt", "terminal_stop_reason", "failure_route_continuation"}) != nil || !ack.Validate() {
		return nil, invalid
	}
	release, err := takeNodeRecoverySlot(s.requests)
	if err != nil {
		return nil, err
	}
	defer release()
	outcome, err := s.nodeRecovery.AcknowledgeNodeRecovery(ctx, claim, ack)
	if err != nil {
		return nil, nodeRecoveryControlStatus(err)
	}
	encoded, err := nodeRecoveryJSON(outcome)
	if err != nil {
		return nil, err
	}
	return &runtimev1.AcknowledgeNodeRecoveryResponseV1{OutcomeJson: encoded}, nil
}

func takeNodeRecoverySlot(slots chan struct{}) (func(), error) {
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	default:
		return nil, grpcstatus.Error(codes.Unavailable, "Node recovery control is busy")
	}
}

// nodeRecoveryControlStatus: a refused claim or a rejected state is a
// conflict with the durable recovery state (Aborted); anything else is
// retryable.
func nodeRecoveryControlStatus(err error) error {
	if errors.Is(err, ErrContentUnauthorized) || errors.Is(err, ErrContentRejected) {
		return grpcstatus.Error(codes.Aborted, "Node recovery control is unavailable")
	}
	return grpcstatus.Error(codes.Unavailable, "Node recovery control is unavailable")
}

func nodeRecoveryJSON(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, grpcstatus.Error(codes.Unavailable, "Node recovery control is unavailable")
	}
	return encoded, nil
}
