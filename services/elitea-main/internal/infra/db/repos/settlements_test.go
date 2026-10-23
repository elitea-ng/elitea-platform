package repos

import (
	"context"
	"errors"
	"strings"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/protobuf/proto"
)

func TestPrepareSettlementReplaysOnlyForExactPersistedSettledFence(t *testing.T) {
	frame := testValidationFrame(t)
	proposal := frame.Settlement
	generatorCalled := false
	store := &scriptedStore{scriptedExecutor: &scriptedExecutor{rowResults: []scriptedRow{
		{values: []any{proposal.Fence.CommandID}},
		{values: settlementReplayRow(proposal, "receipt-1")},
	}}}
	repository, err := newSettlementsRepository(store, func() (string, error) {
		generatorCalled = true
		return "unsafe-new-receipt", nil
	})
	if err != nil {
		t.Fatal(err)
	}

	receipt, err := repository.PrepareSettlement(context.Background(), proposal)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.ID != "receipt-1" || receipt.Outcome != proposal.Outcome {
		t.Fatalf("exact settled-fence replay returned the wrong receipt: %+v", receipt)
	}
	if generatorCalled || len(store.rowCalls) != 2 || len(store.execCalls) != 0 {
		t.Fatalf("idempotent replay created or mutated settlement state: generated=%t rows=%d execs=%d", generatorCalled, len(store.rowCalls), len(store.execCalls))
	}
	replaySQL := store.rowCalls[1].sql
	for _, predicate := range []string{
		"JOIN elitea_runtime.execution_claims AS c",
		"c.claim_id = s.claim_id",
		"c.fence_token = s.fence_token",
		"c.workload_identity = s.workload_identity",
		"c.workload_session_id = s.workload_session_id",
		"c.producer_id = s.producer_id",
		"c.claim_attempt = s.claim_attempt",
		"c.lease_epoch = s.lease_epoch",
		"c.released_at IS NOT NULL",
		"COALESCE(c.release_reason, '')",
	} {
		if !strings.Contains(replaySQL, predicate) {
			t.Fatalf("settlement replay query lost persisted authority field %q", predicate)
		}
	}
	if strings.Contains(replaySQL, "c.released_at IS NULL") {
		t.Fatal("settled-claim idempotent replay incorrectly requires a live lease")
	}
}

func TestPrepareSettlementReplayRejectsDifferentAuthenticatedFenceBeforeReceipt(t *testing.T) {
	frame := testValidationFrame(t)
	original := frame.Settlement
	tests := []struct {
		name   string
		mutate func(*executionapp.SettlementProposal)
	}{
		{name: "workload identity", mutate: func(p *executionapp.SettlementProposal) {
			p.Fence.WorkloadIdentity = "spiffe://elitea.test/worker/other"
		}},
		{name: "workload session", mutate: func(p *executionapp.SettlementProposal) { p.Fence.WorkloadSessionID = "other-session" }},
		{name: "producer", mutate: func(p *executionapp.SettlementProposal) { p.Fence.ProducerID = "other-producer" }},
		{name: "claim attempt", mutate: func(p *executionapp.SettlementProposal) { p.Fence.ClaimAttempt++ }},
		{name: "lease epoch", mutate: func(p *executionapp.SettlementProposal) { p.Fence.LeaseEpoch++ }},
		{name: "fence token", mutate: func(p *executionapp.SettlementProposal) {
			p.Fence.Token = runtimedomain.FenceToken(runtimedomain.SHA256([]byte("other-token")))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proposal := original
			test.mutate(&proposal)
			generatorCalled := false
			store := &scriptedStore{scriptedExecutor: &scriptedExecutor{rowResults: []scriptedRow{
				{values: []any{original.Fence.CommandID}},
				{values: settlementReplayRow(original, "secret-receipt")},
			}}}
			repository, err := newSettlementsRepository(store, func() (string, error) {
				generatorCalled = true
				return "unsafe-new-receipt", nil
			})
			if err != nil {
				t.Fatal(err)
			}

			receipt, err := repository.PrepareSettlement(context.Background(), proposal)
			if !errors.Is(err, runtimedomain.ErrStaleFence) {
				t.Fatalf("different authenticated %s did not fail stale: receipt=%+v err=%v", test.name, receipt, err)
			}
			if receipt != (executionapp.SettlementReceipt{}) || generatorCalled || len(store.rowCalls) != 2 || len(store.execCalls) != 0 {
				t.Fatalf("different authenticated %s observed or mutated settlement: receipt=%+v generated=%t rows=%d execs=%d", test.name, receipt, generatorCalled, len(store.rowCalls), len(store.execCalls))
			}
		})
	}
}

func settlementReplayRow(proposal executionapp.SettlementProposal, receiptID string) []any {
	return []any{
		receiptID,
		string(proposal.Outcome),
		proposal.IdempotencyKey,
		proposal.ProposalDigest[:],
		proposal.Fence.Token[:],
		proposal.Fence.WorkloadIdentity,
		proposal.Fence.WorkloadSessionID,
		proposal.Fence.ProducerID,
		int64(proposal.Fence.ClaimAttempt),
		int64(proposal.Fence.LeaseEpoch),
		true,
		"SETTLED",
	}
}

func TestSettlementProposalBytesPreserveVerifiedIdempotencyContract(t *testing.T) {
	proposal := executionapp.SettlementProposal{
		Fence: runtimedomain.Fence{
			CommandID:         "command-1",
			ExecutionID:       "execution-1",
			Generation:        1,
			WorkloadIdentity:  "spiffe://elitea.test/worker/1",
			WorkloadSessionID: "session-1",
			ProducerID:        "producer-1",
			ClaimAttempt:      1,
			LeaseEpoch:        1,
			Token:             runtimedomain.FenceToken(runtimedomain.SHA256([]byte("token"))),
		},
		ProposalID:              "proposal-1",
		Outcome:                 executionapp.SettlementSucceeded,
		TerminalLogicalOutputID: "validation:revision-1",
		TerminalEventID:         "event-1",
		TerminalSequence:        1,
		TerminalPayloadDigest:   runtimedomain.SHA256([]byte("result")),
		IdempotencyKey:          "prepare-1",
	}
	wire := &runtimev1.SettlementProposalV1{
		ProposalId:              proposal.ProposalID,
		RequestedOutcome:        runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_SUCCEEDED,
		TerminalLogicalOutputId: proposal.TerminalLogicalOutputID,
		TerminalEventId:         proposal.TerminalEventID,
		TerminalSequence:        proposal.TerminalSequence,
		TerminalPayloadDigest: &runtimev1.DigestV1{
			Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256,
			Value:     proposal.TerminalPayloadDigest[:],
		},
		PrepareIdempotencyKey: proposal.IdempotencyKey,
	}
	expected, err := proto.MarshalOptions{Deterministic: true}.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	proposal.ProposalDigest = runtimedomain.SHA256(expected)

	actual, err := settlementProposalBytes(proposal)
	if err != nil {
		t.Fatal(err)
	}
	if string(actual) != string(expected) {
		t.Fatal("repository did not preserve the exact verified settlement proposal bytes")
	}
	var decoded runtimev1.SettlementProposalV1
	if err := proto.Unmarshal(actual, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.GetPrepareIdempotencyKey() != proposal.IdempotencyKey {
		t.Fatalf("prepare idempotency key was lost: %q", decoded.GetPrepareIdempotencyKey())
	}
}

func TestSettlementErrorCodeNamesOnlyARegisteredRuntimeFailureCode(t *testing.T) {
	encode := func(code runtimev1.RuntimeErrorCodeV1) []byte {
		raw, err := proto.Marshal(&runtimev1.RuntimeErrorV1{Code: code, SafeMessage: "private detail"})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	got := settlementErrorCode(executionapp.SettlementFailed, payloadTypeRuntimeFailure, encode(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_RECOVERY_REFUSED))
	if got == nil || *got != "RECOVERY_REFUSED" {
		t.Fatalf("recovery refusal code = %v", got)
	}
	got = settlementErrorCode(executionapp.SettlementFailed, payloadTypeRuntimeFailure, encode(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY))
	if got == nil || *got != "UNSUPPORTED_CAPABILITY" {
		t.Fatalf("unsupported capability code = %v", got)
	}
	for name, input := range map[string]struct {
		payloadType string
		payload     []byte
	}{
		"unspecified":  {payloadTypeRuntimeFailure, encode(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSPECIFIED)},
		"unregistered": {payloadTypeRuntimeFailure, encode(runtimev1.RuntimeErrorCodeV1(9999))},
		"malformed":    {payloadTypeRuntimeFailure, []byte{0xff, 0xff}},
		"empty":        {payloadTypeRuntimeFailure, nil},
		"agent result": {"AGENT_EXECUTION_RESULT", encode(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_RECOVERY_REFUSED)},
	} {
		if got := settlementErrorCode(executionapp.SettlementFailed, input.payloadType, input.payload); got != nil {
			t.Fatalf("%s: error code = %q, want none", name, *got)
		}
	}
	// Only a FAILED settlement names a failure code (review round 4 item 5).
	for _, outcome := range []executionapp.SettlementOutcome{
		executionapp.SettlementCancelled,
		executionapp.SettlementSucceeded,
		executionapp.SettlementOutcomeUnknown,
	} {
		if got := settlementErrorCode(outcome, payloadTypeRuntimeFailure, encode(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_RECOVERY_REFUSED)); got != nil {
			t.Fatalf("%s settlement: error code = %q, want none", outcome, *got)
		}
	}
}
