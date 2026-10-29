package control

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	egressauditapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/egressaudit"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/protobuf/proto"
)

type egressSummariesStub struct {
	calls     *[]string
	fences    []runtimedomain.Fence
	summaries []egressauditapp.WorkerSummary
	settled   []runtimedomain.Fence
	err       error
}

func (s *egressSummariesStub) RecordWorkerEgress(_ context.Context, fence runtimedomain.Fence, summary egressauditapp.WorkerSummary) error {
	*s.calls = append(*s.calls, "record-egress")
	if s.err != nil {
		return s.err
	}
	s.fences = append(s.fences, fence)
	s.summaries = append(s.summaries, summary)
	return nil
}

func (s *egressSummariesStub) SettleWorkerEgress(_ context.Context, fence runtimedomain.Fence) error {
	*s.calls = append(*s.calls, "settle-egress")
	s.settled = append(s.settled, fence)
	return s.err
}

func egressSummaryRequestFor(fence runtimedomain.Fence) *runtimev1.ReportEgressSummaryRequestV1 {
	return &runtimev1.ReportEgressSummaryRequestV1{
		Identity: identityForFence(fence), Fence: fenceProto(fence),
		HostsReached: []*runtimev1.EgressHostReachedV1{
			{Host: "api.github.com", Port: 443, Connections: 3},
			{Host: "bad host", Port: 443, Connections: 1},
		},
		HostsRefused: []*runtimev1.EgressHostRefusedV1{{Host: "evil.example", Port: 443, Reason: "egress_host_not_allowed", Count: 2}},
		OtherHosts:   4,
	}
}

// The live claim is verified exactly as the steering claim does, and the
// snapshot reaches the store under the verified fence; a junk host is
// counted, not stored.
func TestReportEgressSummaryVerifiesTheClaimAndStores(t *testing.T) {
	calls := []string{}
	lease := validLease()
	store := &egressSummariesStub{calls: &calls}
	server := newSteeringTestServer(t, &calls, lease).WithEgressSummaries(store)
	response, err := server.ReportEgressSummary(context.Background(), egressSummaryRequestFor(lease.Fence))
	if err != nil || response.GetRejection() != nil {
		t.Fatalf("err=%v rejection=%v", err, response.GetRejection())
	}
	if !reflect.DeepEqual(calls, []string{"authorize-peer", "verify-fence", "record-egress"}) {
		t.Fatalf("calls=%v", calls)
	}
	if len(store.fences) != 1 || store.fences[0] != lease.Fence {
		t.Fatalf("fences=%+v", store.fences)
	}
	want := egressauditapp.WorkerSummary{
		ExecutionID:  lease.Fence.ExecutionID,
		CommandID:    lease.Fence.CommandID,
		HostsReached: []egressauditapp.HostReached{{Host: "api.github.com", Port: 443, Connections: 3}},
		HostsRefused: []egressauditapp.HostRefused{{Host: "evil.example", Port: 443, Reason: "egress_host_not_allowed", Count: 2}},
		OtherHosts:   5,
	}
	if !reflect.DeepEqual(store.summaries[0], want) {
		t.Fatalf("summary=%+v", store.summaries[0])
	}
}

func TestReportEgressSummaryRefusals(t *testing.T) {
	lease := validLease()
	stale := lease.Fence
	stale.Token[0] ^= 0xff
	tooMany := egressSummaryRequestFor(lease.Fence)
	for range 257 {
		tooMany.HostsReached = append(tooMany.HostsReached, &runtimev1.EgressHostReachedV1{Host: "x.example", Port: 443})
	}
	negative := egressSummaryRequestFor(lease.Fence)
	negative.OtherHosts = -1
	for _, test := range []struct {
		name      string
		store     *egressSummariesStub
		request   *runtimev1.ReportEgressSummaryRequestV1
		code      runtimev1.RuntimeErrorCodeV1
		retryable bool
	}{
		{name: "not composed", request: egressSummaryRequestFor(lease.Fence), code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY},
		{name: "malformed", store: &egressSummariesStub{}, request: &runtimev1.ReportEgressSummaryRequestV1{}, code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION},
		{name: "too many hosts", store: &egressSummariesStub{}, request: tooMany, code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION},
		{name: "negative count", store: &egressSummariesStub{}, request: negative, code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION},
		{name: "stale fence", store: &egressSummariesStub{}, request: egressSummaryRequestFor(stale), code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_STALE_FENCE},
		{name: "store unavailable", store: &egressSummariesStub{err: errors.New("database down")}, request: egressSummaryRequestFor(lease.Fence), code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE, retryable: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := []string{}
			server := newSteeringTestServer(t, &calls, lease)
			if test.store != nil {
				test.store.calls = &calls
				server.WithEgressSummaries(test.store)
			}
			response, err := server.ReportEgressSummary(context.Background(), test.request)
			if err != nil {
				t.Fatal(err)
			}
			rejection := response.GetRejection()
			if rejection == nil || rejection.GetCode() != test.code || rejection.GetRetryable() != test.retryable {
				t.Fatalf("rejection=%v", rejection)
			}
			if test.store != nil && test.code != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE && len(test.store.summaries) != 0 {
				t.Fatal("a refused report was stored")
			}
			if strings.Contains(rejection.GetSafeMessage(), "database") {
				t.Fatalf("an internal error leaked: %q", rejection.GetSafeMessage())
			}
		})
	}
}

// A prepared settlement makes the run's worker row final; a failure to mark
// it never fails the settlement.
func TestPrepareSettlementSettlesTheWorkerEgressRow(t *testing.T) {
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
		store := &egressSummariesStub{calls: &calls, err: storeErr}
		server.WithEgressSummaries(store)
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
		if !reflect.DeepEqual(calls, []string{"authorize-peer", "prepare-settlement", "settle-egress"}) ||
			!reflect.DeepEqual(store.settled, []runtimedomain.Fence{lease.Fence}) {
			t.Fatalf("calls=%v settled=%v", calls, store.settled)
		}
	}
}
