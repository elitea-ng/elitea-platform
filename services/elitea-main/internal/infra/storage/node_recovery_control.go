package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"

	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/noderecovery"
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

func (s *ContentServer) PostNodeRecoveryControl(w http.ResponseWriter, r *http.Request) {
	if s.nodeRecovery == nil || r.ContentLength != 0 || len(r.TransferEncoding) != 0 || r.URL.RawQuery != "" {
		http.Error(w, "Invalid node recovery control request", http.StatusBadRequest)
		return
	}
	claim, err := parseExecutionClaim(r)
	if err != nil {
		http.Error(w, "Node recovery claim denied", http.StatusForbidden)
		return
	}
	if !s.takeNodeRecoverySlot(w) {
		return
	}
	defer func() { <-s.requests }()
	outcome, err := s.nodeRecovery.PollNodeRecovery(r.Context(), claim)
	if err != nil {
		writeNodeRecoveryControlError(w, err)
		return
	}
	writeNodeRecoveryJSON(w, outcome)
}

func (s *ContentServer) PostNodeRecoveryAck(w http.ResponseWriter, r *http.Request) {
	if s.nodeRecovery == nil || r.URL.RawQuery != "" {
		http.NotFound(w, r)
		return
	}
	claim, err := parseExecutionClaim(r)
	if err != nil {
		http.Error(w, "Node recovery claim denied", http.StatusForbidden)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8192))
	if err != nil {
		http.Error(w, "Invalid node recovery acknowledgement", http.StatusBadRequest)
		return
	}
	var ack NodeRecoveryAck
	if domain.DecodeStrictNullableObject(raw, &ack, []string{"request_id", "activation_id", "expected_revision", "receipt_sha256", "applied_revision", "continuation_receipt", "terminal_stop_reason", "failure_route_continuation"}, []string{"continuation_receipt", "terminal_stop_reason", "failure_route_continuation"}) != nil || !ack.Validate() {
		http.Error(w, "Invalid node recovery acknowledgement", http.StatusBadRequest)
		return
	}
	if !s.takeNodeRecoverySlot(w) {
		return
	}
	defer func() { <-s.requests }()
	outcome, err := s.nodeRecovery.AcknowledgeNodeRecovery(r.Context(), claim, ack)
	if err != nil {
		writeNodeRecoveryControlError(w, err)
		return
	}
	writeNodeRecoveryJSON(w, outcome)
}

func (s *ContentServer) takeNodeRecoverySlot(w http.ResponseWriter) bool {
	select {
	case s.requests <- struct{}{}:
		return true
	default:
		http.Error(w, "Node recovery control is busy", http.StatusServiceUnavailable)
		return false
	}
}

func writeNodeRecoveryControlError(w http.ResponseWriter, err error) {
	status := http.StatusServiceUnavailable
	if errors.Is(err, ErrContentUnauthorized) || errors.Is(err, ErrContentRejected) {
		status = http.StatusConflict
	}
	http.Error(w, "Node recovery control is unavailable", status)
}

func writeNodeRecoveryJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(value)
}
