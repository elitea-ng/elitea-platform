package failure_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	controltransport "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/control"
	"google.golang.org/protobuf/proto"
)

// This failure test exercises the in-process crash-recovery contract over the
// checked cross-language corpus. It does not claim networked Redis/PostgreSQL
// failover coverage; service-backed fault injection remains a deployment gate.
func TestCrashAfterTerminalACKRecoversSettlementWithoutInputOrBusinessReplay(t *testing.T) {
	envelope := &runtimev1.WorkerExecutionEnvelopeV1{}
	readRecoveryProto(t, "envelope.pb", envelope)
	output := &runtimev1.ExecutionOutputFrameV1{}
	readRecoveryProto(t, "expected-output.pb", output)
	command := &runtimev1.WorkerCommandV1{}
	if err := proto.Unmarshal(envelope.GetSignedCommand().GetWorkerCommandBytes(), command); err != nil {
		t.Fatal(err)
	}
	fence := recoveryFence(t, command, envelope.GetFence())
	lease := runtimedomain.ActiveLease{
		ClaimID:      "claim-recovery-1",
		Fence:        fence,
		ExpiresAt:    time.Now().UTC().Add(time.Minute),
		DesiredState: runtimedomain.DesiredRunning,
	}
	wireProposal := output.GetSettlementProposal()
	encodedProposal, err := proto.MarshalOptions{Deterministic: true}.Marshal(wireProposal)
	if err != nil {
		t.Fatal(err)
	}
	terminalDigest := recoveryDigest(t, wireProposal.GetTerminalPayloadDigest())
	proposal := executionapp.SettlementProposal{
		Fence:                   fence,
		ProposalID:              wireProposal.GetProposalId(),
		Outcome:                 executionapp.SettlementSucceeded,
		TerminalLogicalOutputID: wireProposal.GetTerminalLogicalOutputId(),
		TerminalEventID:         wireProposal.GetTerminalEventId(),
		TerminalSequence:        wireProposal.GetTerminalSequence(),
		TerminalPayloadDigest:   terminalDigest,
		ProposalDigest:          runtimedomain.SHA256(encodedProposal),
		IdempotencyKey:          wireProposal.GetPrepareIdempotencyKey(),
	}
	claims := &recoveryClaims{decision: executionapp.ClaimDecision{
		Lease:       lease,
		Disposition: executionapp.ClaimRecoverTerminalACK,
		SettlementRecovery: &executionapp.SettlementRecovery{
			Proposal: &proposal,
		},
	}}
	inputs := &recoveryInputs{}
	settlements := &recoverySettlements{
		expected: fence,
		receipt:  executionapp.SettlementReceipt{ID: "settlement-receipt-recovery-1", Outcome: executionapp.SettlementSucceeded},
	}
	server, err := controltransport.NewServer(
		controltransport.ServerConfig{
			MaxInputManifestBytes: 64 * 1024,
			MaxInputEntries:       16,
			MaxInputContentBytes:  256 * 1024,
			MaxStringBytes:        256,
		},
		recoveryAuthorizer{},
		newRecoveryVerifier(t),
		claims,
		inputs,
		settlements,
	)
	if err != nil {
		t.Fatal(err)
	}

	claim := claimRecoveryCommand(t, server, envelope)
	if claim.GetDisposition() != runtimev1.ClaimDispositionV1_CLAIM_DISPOSITION_V1_RECOVER_TERMINAL_ACK || claim.GetInputBundle() != nil || claim.GetInputBundleRef() != nil || inputs.calls != 0 {
		t.Fatalf("terminal recovery fetched business input: receipt=%v input_calls=%d", claim, inputs.calls)
	}
	recovery := claim.GetSettlementRecovery()
	if recovery == nil || !proto.Equal(recovery.GetProposal(), wireProposal) || recovery.GetIdempotencyKey() != wireProposal.GetPrepareIdempotencyKey() {
		t.Fatalf("persisted terminal settlement material was not recovered exactly: %v", recovery)
	}
	prepared, err := server.PrepareSettlement(context.Background(), &runtimev1.PrepareSettlementRequestV1{
		Identity:       claim.GetIdentity(),
		Fence:          claim.GetFence(),
		Proposal:       recovery.GetProposal(),
		ProposalDigest: recovery.GetProposalDigest(),
		IdempotencyKey: recovery.GetIdempotencyKey(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.GetRejection() != nil || prepared.GetSettlementReceiptId() != settlements.receipt.ID || settlements.calls != 1 {
		t.Fatalf("recovered terminal proposal was not prepared idempotently: %v", prepared)
	}

	claims.decision = executionapp.ClaimDecision{
		Lease:       lease,
		Disposition: executionapp.ClaimRecoverSettlement,
		SettlementRecovery: &executionapp.SettlementRecovery{
			Receipt: &settlements.receipt,
		},
	}
	claim = claimRecoveryCommand(t, server, envelope)
	preparedRecovery := claim.GetSettlementRecovery()
	if claim.GetDisposition() != runtimev1.ClaimDispositionV1_CLAIM_DISPOSITION_V1_RECOVER_SETTLEMENT || preparedRecovery.GetSettlementReceiptId() != settlements.receipt.ID || preparedRecovery.GetOutcome() != runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_SUCCEEDED || inputs.calls != 0 || settlements.calls != 1 {
		t.Fatalf("prepared settlement recovery reran work: receipt=%v input_calls=%d settlement_calls=%d", claim, inputs.calls, settlements.calls)
	}
}

type recoveryAuthorizer struct{}

func (recoveryAuthorizer) AuthorizeWorkload(_ context.Context, _, _ string) (string, error) {
	return "spiffe://elitea.test/workload/python-reference", nil
}

type recoveryClaims struct {
	decision executionapp.ClaimDecision
}

func (c *recoveryClaims) Claim(_ context.Context, request executionapp.ClaimRequest) (executionapp.ClaimDecision, error) {
	fence := c.decision.Lease.Fence
	if request.OutboxID == "" || request.SignedEnvelopeDigest.IsZero() || request.CommandID != fence.CommandID || request.ExecutionID != fence.ExecutionID || request.Generation != fence.Generation || request.WorkloadIdentity != fence.WorkloadIdentity || request.WorkloadSessionID != fence.WorkloadSessionID || request.ProducerID != fence.ProducerID {
		return executionapp.ClaimDecision{}, executionapp.ErrInvalidClaim
	}
	return c.decision, nil
}

func (c *recoveryClaims) BeginExecution(context.Context, runtimedomain.Fence) (executionapp.BeginExecutionDisposition, error) {
	return executionapp.BeginExecutionAlreadyStarted, nil
}

func (c *recoveryClaims) AuthorizeInvocation(context.Context, runtimedomain.Fence) (executionapp.AuthorizeInvocationDisposition, error) {
	return executionapp.AuthorizeInvocationAlready, nil
}

func (c *recoveryClaims) Abort(context.Context, runtimedomain.Fence, executionapp.ClaimAbortDisposition) error {
	return errors.New("recovery claim must not be aborted")
}

func (c *recoveryClaims) Renew(_ context.Context, fence runtimedomain.Fence) (runtimedomain.ActiveLease, error) {
	if fence != c.decision.Lease.Fence {
		return runtimedomain.ActiveLease{}, runtimedomain.ErrStaleFence
	}
	return c.decision.Lease, nil
}

func (c *recoveryClaims) VerifyActive(_ context.Context, fence runtimedomain.Fence) error {
	if fence != c.decision.Lease.Fence {
		return runtimedomain.ErrStaleFence
	}
	return nil
}

func (c *recoveryClaims) ObserveDesiredState(_ context.Context, fence runtimedomain.Fence) (runtimedomain.DesiredState, error) {
	if fence != c.decision.Lease.Fence {
		return "", runtimedomain.ErrStaleFence
	}
	return c.decision.Lease.DesiredState, nil
}

type recoveryInputs struct {
	calls int
}

func (r *recoveryInputs) ResolveClaimInput(context.Context, runtimedomain.Fence, *runtimev1.ExecutionInputBundleReferenceV1) (*runtimev1.ExecutionInputBundleV1, error) {
	r.calls++
	return nil, executionapp.ErrInvalidClaim
}

type recoverySettlements struct {
	expected runtimedomain.Fence
	receipt  executionapp.SettlementReceipt
	calls    int
}

func (s *recoverySettlements) PrepareSettlement(_ context.Context, proposal executionapp.SettlementProposal) (executionapp.SettlementReceipt, error) {
	if proposal.Fence != s.expected {
		return executionapp.SettlementReceipt{}, runtimedomain.ErrStaleFence
	}
	s.calls++
	return s.receipt, nil
}

func claimRecoveryCommand(t *testing.T, server *controltransport.Server, envelope *runtimev1.WorkerExecutionEnvelopeV1) *runtimev1.ClaimReceiptV1 {
	t.Helper()
	response, err := server.ClaimCommand(context.Background(), &runtimev1.ClaimCommandRequestV1{
		WorkloadSessionId: envelope.GetFence().GetWorkloadSessionId(),
		ProducerId:        envelope.GetFence().GetProducerId(),
		SignedCommand:     envelope.GetSignedCommand(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetRejection() != nil || response.GetReceipt() == nil {
		t.Fatalf("recovery claim rejected: %v", response)
	}
	return response.GetReceipt()
}

func newRecoveryVerifier(t *testing.T) *controltransport.ConformanceCommandVerifier {
	t.Helper()
	verifier, err := controltransport.NewConformanceCommandVerifier(controltransport.ConformanceVerifierConfig{
		EnvelopeSchemaRevision: "elitea.runtime.signed-worker-command.v1",
		ProtocolRevision:       "elitea.runtime.v1",
		CapabilityVersion:      "1",
		LimitsRevision:         "elitea.runtime.limits.conformance.v2",
		KeyID:                  "elitea-runtime-v1-conformance-hmac",
		HMACKey:                []byte("ELITEA_RUNTIME_V1_TEST_ONLY_NOT_A_SECRET"),
		MaxWorkerCommandBytes:  32 * 1024,
		MaxInputManifestBytes:  64 * 1024,
		MaxStringBytes:         256,
	})
	if err != nil {
		t.Fatal(err)
	}
	return verifier
}

func recoveryFence(t *testing.T, command *runtimev1.WorkerCommandV1, wire *runtimev1.ExecutionFenceV1) runtimedomain.Fence {
	t.Helper()
	if len(wire.GetFenceToken()) != 32 {
		t.Fatal("invalid recovery fence token")
	}
	var token runtimedomain.FenceToken
	copy(token[:], wire.GetFenceToken())
	return runtimedomain.Fence{
		CommandID:         command.GetCommandId(),
		ExecutionID:       command.GetExecutionId(),
		Generation:        command.GetGeneration(),
		WorkloadIdentity:  "spiffe://elitea.test/workload/python-reference",
		WorkloadSessionID: wire.GetWorkloadSessionId(),
		ProducerID:        wire.GetProducerId(),
		ClaimAttempt:      wire.GetClaimAttempt(),
		LeaseEpoch:        wire.GetLeaseEpoch(),
		Token:             token,
	}
}

func recoveryDigest(t *testing.T, digest *runtimev1.DigestV1) runtimedomain.Digest {
	t.Helper()
	if digest.GetAlgorithm() != runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256 || len(digest.GetValue()) != 32 {
		t.Fatal("invalid recovery digest")
	}
	var mapped runtimedomain.Digest
	copy(mapped[:], digest.GetValue())
	return mapped
}

func readRecoveryProto(t *testing.T, name string, message proto.Message) {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate failure test source")
	}
	path := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "../../../../testdata/proto/runtime/v1/configuration-validation/valid", name))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := proto.Unmarshal(raw, message); err != nil {
		t.Fatal(err)
	}
}
