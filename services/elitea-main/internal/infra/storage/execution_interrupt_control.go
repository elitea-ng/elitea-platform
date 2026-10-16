package storage

import (
	"context"
	"errors"
	"io"
	"net/http"
)

// Private claim-fenced decision routes of the per-interrupt HITL ledger
// (libs/proto/contracts/fanout-interrupt-decisions-v1.md v1.1 §7, §7.1).
//
//	POST /executions/{executionID}/generations/{generation}/interrupts/fetch  (empty body)
//	POST /executions/{executionID}/generations/{generation}/interrupts/ack    (fanout-interrupt-ack-request)
//
// The store returns the canonical response bytes; this layer only bounds the
// request, maps typed refusals to fixed status codes and never echoes ledger
// data in an error body.

// MaxExecutionInterruptAckBytes bounds the raw ACK body (domain
// MaxAckBodyBytes; the contract §7.1 bound).
const MaxExecutionInterruptAckBytes = 1024

// Typed refusals of the decision routes. The status each one maps to is part
// of the contract (§7.1): the Worker branches on the status only.
var (
	// ErrInterruptClaimRefused: the claim is not the live ordinary RUNNING
	// claim (403, a control stop for the Worker).
	ErrInterruptClaimRefused = errors.New("execution interrupt claim refused")
	// ErrInterruptRequestInvalid: the request body is malformed (400).
	ErrInterruptRequestInvalid = errors.New("execution interrupt request invalid")
	// ErrInterruptNotFound: the ACK names no card of the claim's response (404).
	ErrInterruptNotFound = errors.New("execution interrupt not found")
	// ErrInterruptConflict: the ACK conflicts with the ledger (409).
	ErrInterruptConflict = errors.New("execution interrupt conflict")
)

// ExecutionInterruptControlStore serves the two private routes. Both run the
// shared live-claim authority under the job lock with the database clock.
type ExecutionInterruptControlStore interface {
	FetchExecutionInterrupts(context.Context, ContentClaim) ([]byte, error)
	AckExecutionInterrupt(context.Context, ContentClaim, []byte) ([]byte, error)
}

// WithExecutionInterrupts registers the decision routes. A nil store leaves
// them unregistered.
func (s *ContentServer) WithExecutionInterrupts(control ExecutionInterruptControlStore) *ContentServer {
	if s != nil {
		s.executionInterrupts = control
	}
	return s
}

func (s *ContentServer) PostExecutionInterruptFetch(w http.ResponseWriter, r *http.Request) {
	if s.executionInterrupts == nil {
		http.NotFound(w, r)
		return
	}
	if r.URL.RawQuery != "" || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		http.Error(w, "Invalid execution interrupt fetch request", http.StatusBadRequest)
		return
	}
	claim, err := parseExecutionClaim(r)
	if err != nil {
		http.Error(w, "Execution interrupt claim denied", http.StatusForbidden)
		return
	}
	if !s.takeNodeRecoverySlot(w) {
		return
	}
	defer func() { <-s.requests }()
	body, err := s.executionInterrupts.FetchExecutionInterrupts(r.Context(), claim)
	if err != nil {
		writeExecutionInterruptError(w, err)
		return
	}
	writeExecutionInterruptJSON(w, body)
}

func (s *ContentServer) PostExecutionInterruptAck(w http.ResponseWriter, r *http.Request) {
	if s.executionInterrupts == nil {
		http.NotFound(w, r)
		return
	}
	if r.URL.RawQuery != "" {
		http.Error(w, "Invalid execution interrupt acknowledgement", http.StatusBadRequest)
		return
	}
	claim, err := parseExecutionClaim(r)
	if err != nil {
		http.Error(w, "Execution interrupt claim denied", http.StatusForbidden)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxExecutionInterruptAckBytes))
	if err != nil || len(raw) == 0 {
		http.Error(w, "Invalid execution interrupt acknowledgement", http.StatusBadRequest)
		return
	}
	if !s.takeNodeRecoverySlot(w) {
		return
	}
	defer func() { <-s.requests }()
	body, err := s.executionInterrupts.AckExecutionInterrupt(r.Context(), claim, raw)
	if err != nil {
		writeExecutionInterruptError(w, err)
		return
	}
	writeExecutionInterruptJSON(w, body)
}

func writeExecutionInterruptError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInterruptClaimRefused):
		http.Error(w, "Execution interrupt claim denied", http.StatusForbidden)
	case errors.Is(err, ErrInterruptRequestInvalid):
		http.Error(w, "Invalid execution interrupt request", http.StatusBadRequest)
	case errors.Is(err, ErrInterruptNotFound):
		http.Error(w, "Execution interrupt not found", http.StatusNotFound)
	case errors.Is(err, ErrInterruptConflict):
		http.Error(w, "Execution interrupt conflicts with the ledger", http.StatusConflict)
	default:
		http.Error(w, "Execution interrupt control is unavailable", http.StatusServiceUnavailable)
	}
}

func writeExecutionInterruptJSON(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
