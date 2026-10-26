package control

import (
	"context"
	"errors"
	"testing"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

type steeringClaimerStub struct {
	messages []agentexecutionapp.ClaimedSteeringMessage
	err      error
	requests []agentexecutionapp.SteeringClaimRequest
}

func (s *steeringClaimerStub) Claim(_ context.Context, request agentexecutionapp.SteeringClaimRequest) ([]agentexecutionapp.ClaimedSteeringMessage, error) {
	s.requests = append(s.requests, request)
	return s.messages, s.err
}

func newSteeringTestServer(t *testing.T, calls *[]string, lease runtimedomain.ActiveLease) *Server {
	t.Helper()
	server, err := NewServer(testControlServerConfig(), workloadAuthorizerStub{calls: calls}, verifierSpy{calls: calls, verifier: newTestVerifier(t)}, claimControllerStub{calls: calls, lease: lease}, inputResolverStub{calls: calls, manifest: validManifest()}, &settlementControllerStub{calls: calls})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func steeringRequestFor(fence runtimedomain.Fence) *runtimev1.ClaimSteeringMessagesRequestV1 {
	return &runtimev1.ClaimSteeringMessagesRequestV1{Identity: identityForFence(fence), Fence: fenceProto(fence)}
}

// The worker's safe point: the peer is authenticated, the fence verified
// live, and the claimer gets exactly that fence. The messages travel in
// queue order with their ids.
func TestClaimSteeringMessagesVerifiesTheFenceAndKeepsTheOrder(t *testing.T) {
	calls := []string{}
	lease := validLease()
	queued := time.Date(2026, 10, 26, 10, 15, 2, 0, time.UTC)
	claimer := &steeringClaimerStub{messages: []agentexecutionapp.ClaimedSteeringMessage{
		{ID: "7f0c3c9a-2d4e-4b8f-9a51-0d6c2f1e8b11", Content: "first", QueuedAt: queued},
		{ID: "7f0c3c9a-2d4e-4b8f-9a51-0d6c2f1e8b12", Content: "second", QueuedAt: queued.Add(time.Second)},
	}}
	server := newSteeringTestServer(t, &calls, lease).WithSteeringMessages(claimer)
	request := steeringRequestFor(lease.Fence)
	request.AcknowledgedSteeringIds = []string{"7f0c3c9a-2d4e-4b8f-9a51-0d6c2f1e8b10"}
	response, err := server.ClaimSteeringMessages(context.Background(), request)
	if err != nil || response.GetRejection() != nil {
		t.Fatalf("err=%v rejection=%v", err, response.GetRejection())
	}
	if len(calls) != 2 || calls[0] != "authorize-peer" || calls[1] != "verify-fence" {
		t.Fatalf("calls=%v", calls)
	}
	if len(claimer.requests) != 1 || claimer.requests[0].Fence != lease.Fence ||
		len(claimer.requests[0].Acknowledged) != 1 || claimer.requests[0].AcknowledgeOnly {
		t.Fatalf("requests=%+v", claimer.requests)
	}
	messages := response.GetMessages()
	if len(messages) != 2 || messages[0].GetContent() != "first" || messages[1].GetContent() != "second" ||
		messages[0].GetSteeringId() != "7f0c3c9a-2d4e-4b8f-9a51-0d6c2f1e8b11" ||
		messages[0].GetQueuedAtUnixMillis() != queued.UnixMilli() {
		t.Fatalf("messages=%v", messages)
	}
}

func TestClaimSteeringMessagesRefusals(t *testing.T) {
	lease := validLease()
	stale := lease.Fence
	stale.Token[0] ^= 0xff
	for _, test := range []struct {
		name      string
		claimer   SteeringClaimer
		request   *runtimev1.ClaimSteeringMessagesRequestV1
		code      runtimev1.RuntimeErrorCodeV1
		retryable bool
	}{
		{name: "not composed", request: steeringRequestFor(lease.Fence), code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY},
		{name: "malformed", claimer: &steeringClaimerStub{}, request: &runtimev1.ClaimSteeringMessagesRequestV1{}, code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION},
		{name: "stale fence", claimer: &steeringClaimerStub{}, request: steeringRequestFor(stale), code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_STALE_FENCE},
		{name: "claim lost the lease", claimer: &steeringClaimerStub{err: runtimedomain.ErrStaleFence}, request: steeringRequestFor(lease.Fence), code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_STALE_FENCE},
		{name: "bad acknowledgement", claimer: &steeringClaimerStub{err: agentexecutionapp.ErrInvalidSteering}, request: steeringRequestFor(lease.Fence), code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION},
		{name: "store unavailable", claimer: &steeringClaimerStub{err: errors.New("database down")}, request: steeringRequestFor(lease.Fence), code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE, retryable: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := []string{}
			server := newSteeringTestServer(t, &calls, lease)
			if test.claimer != nil {
				server.WithSteeringMessages(test.claimer)
			}
			response, err := server.ClaimSteeringMessages(context.Background(), test.request)
			if err != nil {
				t.Fatal(err)
			}
			rejection := response.GetRejection()
			if rejection == nil || rejection.GetCode() != test.code || rejection.GetRetryable() != test.retryable {
				t.Fatalf("rejection=%v", rejection)
			}
			if len(response.GetMessages()) != 0 {
				t.Fatalf("a refusal carried messages: %v", response.GetMessages())
			}
		})
	}
}
