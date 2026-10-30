package agentexecution

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

type unattendedStoreStub struct {
	targets  []UnattendedSkipTarget
	closed   []string
	failed   map[string]string
	expiries int
}

func (s *unattendedStoreStub) ClaimUnattendedSkips(context.Context, int, time.Duration) ([]UnattendedSkipTarget, error) {
	return s.targets, nil
}

func (s *unattendedStoreStub) CloseUnattendedSkip(_ context.Context, target UnattendedSkipTarget) error {
	s.closed = append(s.closed, target.ExecutionID)
	return nil
}

func (s *unattendedStoreStub) FailUnattendedSkip(_ context.Context, target UnattendedSkipTarget, code string) error {
	s.failed[target.ExecutionID] = code
	return nil
}

func (s *unattendedStoreStub) ExpireUnattendedWaits(context.Context, int) (int, error) {
	s.expiries++
	return 0, nil
}

type continuerStub struct {
	requests []CurrentContinuationRequest
	errs     map[string]error
}

func (c *continuerStub) ContinueCurrentAgent(_ context.Context, request CurrentContinuationRequest) (CurrentApplicationStartOutcome, error) {
	c.requests = append(c.requests, request)
	return CurrentApplicationStartOutcome{}, c.errs[request.ResponseMessageID]
}

func TestUnattendedWorkerAnswersSkipsWithRefusalsOnly(t *testing.T) {
	refusal := []CurrentHITLDecision{{InterruptID: "i-1", Action: "block_with_comment", Value: "skipped"}}
	store := &unattendedStoreStub{failed: map[string]string{}, targets: []UnattendedSkipTarget{
		{ExecutionID: "ok", ResponseMessageID: "ok", Decisions: refusal},
		{ExecutionID: "answered", ResponseMessageID: "answered", Decisions: refusal},
		{ExecutionID: "refused", ResponseMessageID: "refused", Decisions: refusal},
		{ExecutionID: "busy", ResponseMessageID: "busy", Decisions: refusal, Attempts: 2},
		{ExecutionID: "exhausted", ResponseMessageID: "exhausted", Decisions: refusal,
			Attempts: UnattendedApprovalMaxAttempts},
	}}
	continuer := &continuerStub{errs: map[string]error{
		"answered":  ErrCurrentAgentHITLAlreadyResolved,
		"refused":   ErrInvalidCurrentAgentStart,
		"busy":      errors.New("capacity"),
		"exhausted": errors.New("capacity"),
	}}
	worker, err := NewUnattendedApprovalWorker(store, continuer, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, request := range continuer.requests {
		if request.Kind != CurrentContinuationHITL {
			t.Fatalf("continuation kind = %q", request.Kind)
		}
		for _, decision := range request.HITLDecisions {
			if decision.Action == "approve" {
				t.Fatal("an unattended skip approved a step")
			}
		}
	}
	if len(store.closed) != 1 || store.closed[0] != "answered" {
		t.Fatalf("closed = %v", store.closed)
	}
	if store.failed["refused"] != "continuation_refused" || store.failed["exhausted"] != "attempts_exhausted" ||
		len(store.failed) != 2 {
		t.Fatalf("failed = %v", store.failed)
	}
	if store.expiries != 1 {
		t.Fatalf("expiry passes = %d", store.expiries)
	}
}
