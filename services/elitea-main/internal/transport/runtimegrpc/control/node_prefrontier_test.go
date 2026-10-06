package control

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtime "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/protobuf/proto"
)

type nodeInspectionWireRepository struct {
	executionapp.ClaimRepository
	decision executionapp.ClaimDecision
	calls    *[]string
	request  *executionapp.ClaimRequest
}

func (r nodeInspectionWireRepository) ClaimValidation(_ context.Context, request executionapp.ClaimRequest, _ executionapp.ClaimLeaseTTLMillis) (executionapp.ClaimDecision, error) {
	*r.calls = append(*r.calls, "claim")
	*r.request = request
	if !request.NodeRecovery || request.AgentModelCheckpointRecovery {
		return executionapp.ClaimDecision{}, executionapp.ErrInvalidClaim
	}
	return r.decision, nil
}

func TestNodeRecoveryPreFrontierNodeOnlyWireUsesActualClaimServiceAndFrozenInput(t *testing.T) {
	stored, err := os.ReadFile("../../../../../../libs/jsonschema/runtime/v1/fixtures/node-recovery-actual-frontier-a-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range []struct {
		name        string
		disposition executionapp.ClaimDisposition
		desired     runtime.DesiredState
		wire        runtimev1.ClaimDispositionV1
		receipt     string
	}{
		{"no node visit", executionapp.ClaimRecoverAgentModelCheckpoint, runtime.DesiredRunning, runtimev1.ClaimDispositionV1_CLAIM_DISPOSITION_V1_RECOVER_AGENT_MODEL_CHECKPOINT, ""},
		{"latest A resumed pending A or B inspection", executionapp.ClaimRecoverAgentModelCheckpoint, runtime.DesiredRunning, runtimev1.ClaimDispositionV1_CLAIM_DISPOSITION_V1_RECOVER_AGENT_MODEL_CHECKPOINT, ""},
		{"exact suspended visit", executionapp.ClaimRecoverNodeVisit, runtime.DesiredSuspended, runtimev1.ClaimDispositionV1_CLAIM_DISPOSITION_V1_RECOVER_NODE_VISIT, string(stored)},
		{"exact stopped visit", executionapp.ClaimRecoverNodeVisit, runtime.DesiredRunning, runtimev1.ClaimDispositionV1_CLAIM_DISPOSITION_V1_RECOVER_NODE_VISIT, string(stored)},
	} {
		t.Run(spec.name, func(t *testing.T) {
			manifest := validManifest()
			manifest.Entries[0].EntryId = "agent-request"
			manifest.Entries[0].SemanticRole = executiondomain.AgentExecutionRequestRole
			manifest.Entries[0].Content.MediaType = executiondomain.AgentExecutionInputMediaType
			lease := validLease()
			lease.Fence.ExecutionID = "0123456789abcdef0123456789abcdef"
			lease.DesiredState = spec.desired
			observed := lease.ExpiresAt.Add(-10 * time.Second)
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
			var seen executionapp.ClaimRequest
			repository := nodeInspectionWireRepository{decision: executionapp.ClaimDecision{Lease: lease, LeaseObservedAt: observed, Disposition: spec.disposition, NodeRecoveryReceipt: spec.receipt}, calls: &calls, request: &seen}
			service, err := executionapp.NewClaimService(repository, func() time.Time { return observed.Add(time.Hour) }, executionapp.MaxClaimLeaseTTLMillis.Duration())
			if err != nil {
				t.Fatal(err)
			}
			server, err := NewServer(testControlServerConfig(), workloadAuthorizerStub{calls: &calls}, verifierSpy{calls: &calls, verifier: newTestVerifier(t)}, service, inputResolverStub{calls: &calls, manifest: manifest}, &settlementControllerStub{calls: &calls})
			if err != nil {
				t.Fatal(err)
			}
			response, err := server.ClaimCommand(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			r := response.GetReceipt()
			if response.GetRejection() != nil || r.GetDisposition() != spec.wire || string(r.GetNodeRecoveryReceiptJson()) != spec.receipt || r.GetClaimId() != lease.ClaimID || r.GetClaimStartedAtUnixMicros() != observed.UnixMicro() || !proto.Equal(r.GetInputBundle(), manifest) || !proto.Equal(r.GetInputBundleRef(), command.GetInputBundleRef()) || r.GetFence().GetLeaseEpoch() != lease.Fence.LeaseEpoch || seen.AgentModelCheckpointRecovery || !seen.NodeRecovery {
				t.Fatal(response, seen)
			}
			if !reflect.DeepEqual(calls, []string{"authorize-peer", "verify-command", "claim", "resolve-reference-manifest"}) {
				t.Fatal(calls)
			}
			request.AgentModelCheckpointRecovery = true
			calls = []string{}
			response, err = server.ClaimCommand(t.Context(), request)
			if err != nil || response.GetRejection() == nil || response.GetReceipt() != nil || !reflect.DeepEqual(calls, []string{"authorize-peer", "verify-command"}) {
				t.Fatal("mixed recovery request admitted", response, err, calls)
			}
		})
	}
}
