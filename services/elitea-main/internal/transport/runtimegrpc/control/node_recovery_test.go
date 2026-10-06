package control

import (
	"context"
	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtime "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/protobuf/proto"
	"os"
	"reflect"
	"testing"
)

type nodeRecoveryClaimController struct {
	claimControllerStub
	receipt string
}

func (s nodeRecoveryClaimController) Claim(ctx context.Context, r executionapp.ClaimRequest) (executionapp.ClaimDecision, error) {
	if !r.NodeRecovery || r.AgentModelCheckpointRecovery || r.CapabilityID != executiondomain.AgentApplicationCapability {
		return executionapp.ClaimDecision{}, executionapp.ErrInvalidClaim
	}
	d, err := s.claimControllerStub.Claim(ctx, r)
	d.NodeRecoveryReceipt = s.receipt
	return d, err
}
func TestNodeRecoveryClaimWireKeepsFrozenInspectionAndDBFence(t *testing.T) {
	receipt, err := os.ReadFile("../../../../../../libs/jsonschema/runtime/v1/fixtures/node-recovery-required-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	manifest := validManifest()
	manifest.Entries[0].EntryId = "agent-request"
	manifest.Entries[0].SemanticRole = executiondomain.AgentExecutionRequestRole
	manifest.Entries[0].Content.MediaType = executiondomain.AgentExecutionInputMediaType
	lease := validLease()
	lease.Fence.ExecutionID = "0123456789abcdef0123456789abcdef"
	lease.DesiredState = runtime.DesiredSuspended
	command := validVerifierAgentCommand(executiondomain.AgentApplicationCapability, runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_APPLICATION)
	command.ProtocolRevision = "elitea.runtime.v1"
	command.LimitsRevision = "elitea.runtime.limits.conformance.v3"
	command.CommandId = lease.Fence.CommandID
	command.IdempotencyKey = "outbox-1"
	command.ExecutionId = lease.Fence.ExecutionID
	command.RootExecutionId = lease.Fence.ExecutionID
	command.InputBundleRef = inputReferenceForManifest(t, manifest)
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	request := &runtimev1.ClaimCommandRequestV1{WorkloadSessionId: lease.Fence.WorkloadSessionID, ProducerId: lease.Fence.ProducerID, SignedCommand: signedEnvelope(raw), NodeRecovery: true}
	calls := []string{}
	observed := lease.ExpiresAt.Add(-executionapp.MaxClaimLeaseTTLMillis.Duration())
	controller := nodeRecoveryClaimController{claimControllerStub: claimControllerStub{calls: &calls, lease: lease, disposition: executionapp.ClaimRecoverNodeVisit, leaseObservedAt: observed}, receipt: string(receipt)}
	server, err := NewServer(testControlServerConfig(), workloadAuthorizerStub{calls: &calls}, verifierSpy{calls: &calls, verifier: newTestVerifier(t)}, controller, inputResolverStub{calls: &calls, manifest: manifest}, &settlementControllerStub{calls: &calls})
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.ClaimCommand(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	r := response.GetReceipt()
	if response.GetRejection() != nil || r.GetDisposition() != runtimev1.ClaimDispositionV1_CLAIM_DISPOSITION_V1_RECOVER_NODE_VISIT || r.GetDesiredState() != runtimev1.DesiredExecutionStateV1_DESIRED_EXECUTION_STATE_V1_SUSPENDED || string(r.GetNodeRecoveryReceiptJson()) != string(receipt) || r.GetInputBundle() == nil || r.GetInputBundleRef() == nil || r.GetClaimStartedAtUnixMicros() != observed.UnixMicro() || r.GetIdentity().GetGeneration() != lease.Fence.Generation || r.GetClaimId() != lease.ClaimID {
		t.Fatal(response)
	}
	if !reflect.DeepEqual(calls, []string{"authorize-peer", "verify-command", "claim", "resolve-reference-manifest"}) {
		t.Fatal(calls)
	}
}
