package control

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runevidence"
)

type runStepsStub struct {
	calls     *[]string
	execution string
	steps     []runevidence.StepInput
	anchored  []string
	err       error
}

func (s *runStepsStub) Report(_ context.Context, executionID string, _ uint64, steps []runevidence.StepInput) (int, error) {
	*s.calls = append(*s.calls, "report-steps")
	if s.err != nil {
		return 0, s.err
	}
	s.execution, s.steps = executionID, steps
	return len(steps), nil
}

func (s *runStepsStub) RequestAnchor(_ context.Context, executionID string) error {
	*s.calls = append(*s.calls, "request-anchor")
	s.anchored = append(s.anchored, executionID)
	return s.err
}

func TestReportRunStepsVerifiesTheClaimAndStores(t *testing.T) {
	calls := []string{}
	lease := validLease()
	store := &runStepsStub{calls: &calls}
	server := newSteeringTestServer(t, &calls, lease).WithRunSteps(store)
	request := &runtimev1.ReportRunStepsRequestV1{
		Identity: identityForFence(lease.Fence), Fence: fenceProto(lease.Fence),
		Steps: []*runtimev1.RunStepV1{{
			StepId: "0192f0c1-7b6e-7c3a-8f00-0000000000a1", Kind: runtimev1.RunStepKindV1_RUN_STEP_KIND_V1_TOOL_CALL,
			StartedAtUnixMs: 1_700_000_000_000, EndedAtUnixMs: 1_700_000_000_500, Name: "search", ToolkitId: "17",
			Outputs: &runtimev1.RunStepContentV1{Content: []byte(`{"id":1}`), MediaType: "application/json"},
		}},
	}
	response, err := server.ReportRunSteps(context.Background(), request)
	if err != nil || response.GetRejection() != nil || response.GetAccepted() != 1 {
		t.Fatalf("err=%v response=%v", err, response)
	}
	if !reflect.DeepEqual(calls, []string{"authorize-peer", "verify-fence", "report-steps"}) {
		t.Fatalf("calls=%v", calls)
	}
	got := store.steps[0]
	if store.execution != lease.Fence.ExecutionID || got.Kind != runevidence.KindToolCall || got.ToolkitID != "17" ||
		string(got.Outputs.Bytes) != `{"id":1}` || got.EndedAt.Sub(got.StartedAt).Milliseconds() != 500 {
		t.Fatalf("step=%+v", got)
	}
}

func TestReportRunStepsRefusals(t *testing.T) {
	lease := validLease()
	stale := lease.Fence
	stale.Token[0] ^= 0xff
	request := func(steps int) *runtimev1.ReportRunStepsRequestV1 {
		r := &runtimev1.ReportRunStepsRequestV1{Identity: identityForFence(lease.Fence), Fence: fenceProto(lease.Fence)}
		for range steps {
			r.Steps = append(r.Steps, &runtimev1.RunStepV1{StepId: "0192f0c1-7b6e-7c3a-8f00-0000000000a1"})
		}
		return r
	}
	staleRequest := request(1)
	staleRequest.Identity, staleRequest.Fence = identityForFence(stale), fenceProto(stale)
	for _, test := range []struct {
		name      string
		store     *runStepsStub
		request   *runtimev1.ReportRunStepsRequestV1
		code      runtimev1.RuntimeErrorCodeV1
		retryable bool
	}{
		{name: "not composed", request: request(1), code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY},
		{name: "malformed", store: &runStepsStub{}, request: &runtimev1.ReportRunStepsRequestV1{}, code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION},
		{name: "too many steps", store: &runStepsStub{}, request: request(65), code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION},
		{name: "stale fence", store: &runStepsStub{}, request: staleRequest, code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_STALE_FENCE},
		{name: "outside the contract", store: &runStepsStub{err: runevidence.ErrInvalidReport}, request: request(1), code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION},
		{name: "store unavailable", store: &runStepsStub{err: errors.New("database down")}, request: request(1), code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE, retryable: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := []string{}
			server := newSteeringTestServer(t, &calls, lease)
			if test.store != nil {
				test.store.calls = &calls
				server.WithRunSteps(test.store)
			}
			response, err := server.ReportRunSteps(context.Background(), test.request)
			if err != nil {
				t.Fatal(err)
			}
			rejection := response.GetRejection()
			if rejection == nil || rejection.GetCode() != test.code || rejection.GetRetryable() != test.retryable {
				t.Fatalf("rejection=%v", rejection)
			}
			if test.store != nil && test.store.steps != nil {
				t.Fatal("a refused report was stored")
			}
			if test.name == "stale fence" && strings.Contains(strings.Join(calls, ","), "report-steps") {
				t.Fatal("a stale claim reached the store")
			}
			if strings.Contains(rejection.GetSafeMessage(), "database") {
				t.Fatalf("an internal error leaked: %q", rejection.GetSafeMessage())
			}
		})
	}
}

// A prepared settlement only requests the run's anchor (main.run_step_anchor
// appends it off this path); a failed request never fails the settlement.
func TestPrepareSettlementRequestsTheRunStepAnchor(t *testing.T) {
	for _, storeErr := range []error{nil, errors.New("database down")} {
		calls := []string{}
		lease := validLease()
		settlements := &settlementControllerStub{calls: &calls, receipt: executionapp.SettlementReceipt{ID: "settlement-receipt-1", Outcome: executionapp.SettlementSucceeded}}
		server, err := NewServer(testControlServerConfig(), workloadAuthorizerStub{calls: &calls},
			verifierSpy{calls: &calls, verifier: newTestVerifier(t)}, claimControllerStub{calls: &calls, lease: lease},
			inputResolverStub{calls: &calls, manifest: validManifest()}, settlements)
		if err != nil {
			t.Fatal(err)
		}
		store := &runStepsStub{calls: &calls, err: storeErr}
		server.WithRunSteps(store)
		proposal := &runtimev1.SettlementProposalV1{
			ProposalId: "proposal-1", RequestedOutcome: runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_SUCCEEDED,
			TerminalLogicalOutputId: "validation:revision-1", TerminalEventId: "event-1", TerminalSequence: 1,
			TerminalPayloadDigest: testDigest([]byte("terminal-output")), PrepareIdempotencyKey: "settlement-key-1",
		}
		proposalBytes, _ := proto.MarshalOptions{Deterministic: true}.Marshal(proposal)
		response, err := server.PrepareSettlement(context.Background(), &runtimev1.PrepareSettlementRequestV1{
			Identity: identityForFence(lease.Fence), Fence: fenceProto(lease.Fence), Proposal: proposal,
			ProposalDigest: testDigest(proposalBytes), IdempotencyKey: "settlement-key-1",
		})
		if err != nil || response.GetRejection() != nil {
			t.Fatalf("settlement: %v %v", err, response.GetRejection())
		}
		if !reflect.DeepEqual(calls, []string{"authorize-peer", "prepare-settlement", "request-anchor"}) ||
			!reflect.DeepEqual(store.anchored, []string{lease.Fence.ExecutionID}) {
			t.Fatalf("calls=%v anchored=%v", calls, store.anchored)
		}
	}
}
