package control

import (
	"context"
	"errors"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/epoch"
)

type runCheckerStub struct {
	governed bool
	stale    []epoch.SecurityEpoch
	err      error
	calls    int
}

func (s *runCheckerStub) Check(context.Context, string) (bool, []epoch.SecurityEpoch, error) {
	s.calls++
	return s.governed, s.stale, s.err
}

type revokerStub struct{ revoked []string }

func (s *revokerStub) RevokeAuthority(_ context.Context, executionID string, _ uint64) error {
	s.revoked = append(s.revoked, executionID)
	return nil
}

var staleBinding = []epoch.SecurityEpoch{{Kind: epoch.KindBinding, ID: "0192b6e4-0000-7000-8000-000000000001", Value: 3}}

func withEpochs(t *testing.T, server *Server, checker GovernedRunChecker, revoker AuthorityRevoker) *Server {
	t.Helper()
	if _, err := server.WithGovernedEpochs(checker, revoker); err != nil {
		t.Fatal(err)
	}
	return server
}

// A governed claim whose binding epoch is stale is refused before any claim
// attempt, never retryable, and the run is cancelled durably.
func TestGovernedClaimWithAStaleBindingEpochIsRejected(t *testing.T) {
	calls := []string{}
	checker := &runCheckerStub{governed: true, stale: staleBinding}
	revoker := &revokerStub{}
	server := withEpochs(t, governedTestServer(t, &calls, nil), checker, revoker)
	response, err := server.ClaimCommand(context.Background(), governedClaimRequest(t, nil, true))
	code := rejectionCode(t, response, err)
	if code != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHORIZATION_FAILED || response.GetRejection().GetRetryable() {
		t.Fatalf("code = %v (retryable %t)", code, response.GetRejection().GetRetryable())
	}
	if response.GetRejection().GetSafeMessage() != authorityRevokedMessage {
		t.Fatalf("message = %q", response.GetRejection().GetSafeMessage())
	}
	if claimed(calls) || checker.calls != 1 || len(revoker.revoked) != 1 {
		t.Fatalf("calls %v, checks %d, revoked %v", calls, checker.calls, revoker.revoked)
	}
}

func TestGovernedClaimWithCurrentEpochsIsClaimed(t *testing.T) {
	calls := []string{}
	checker := &runCheckerStub{governed: true}
	server := withEpochs(t, governedTestServer(t, &calls, nil), checker, &revokerStub{})
	response, err := server.ClaimCommand(context.Background(), governedClaimRequest(t, nil, true))
	if code := rejectionCode(t, response, err); code != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSPECIFIED || !claimed(calls) {
		t.Fatalf("code = %v, calls %v", code, calls)
	}
}

func TestGovernedClaimEpochReadFailureIsRetryable(t *testing.T) {
	calls := []string{}
	server := withEpochs(t, governedTestServer(t, &calls, nil), &runCheckerStub{err: errors.New("db down")}, &revokerStub{})
	response, err := server.ClaimCommand(context.Background(), governedClaimRequest(t, nil, true))
	if code := rejectionCode(t, response, err); code != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE ||
		!response.GetRejection().GetRetryable() || claimed(calls) {
		t.Fatalf("code = %v, calls %v", code, calls)
	}
}

// A legacy command (no field 65) never reaches the epoch check: the claim
// path is unchanged.
func TestLegacyClaimNeverChecksEpochs(t *testing.T) {
	calls := []string{}
	checker := &runCheckerStub{governed: true, stale: staleBinding}
	server := withEpochs(t, governedTestServer(t, &calls, nil), checker, &revokerStub{})
	response, err := server.ClaimCommand(context.Background(), claimRequestForManifest(t, validManifest()))
	if code := rejectionCode(t, response, err); code != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSPECIFIED || !claimed(calls) || checker.calls != 0 {
		t.Fatalf("code = %v, calls %v, checks %d", code, calls, checker.calls)
	}
}

func renewRequestFor(t *testing.T) *runtimev1.RenewLeaseRequestV1 {
	t.Helper()
	lease := validLease()
	return &runtimev1.RenewLeaseRequestV1{
		Identity: identityForFence(lease.Fence), Fence: fenceProto(lease.Fence), IdempotencyKey: "renew-1",
	}
}

// A governed run whose authority was revoked is told CANCELLED at its next
// lease renewal and cancelled durably.
func TestRenewLeaseCancelsAGovernedRunWithAStaleEpoch(t *testing.T) {
	calls := []string{}
	revoker := &revokerStub{}
	server := withEpochs(t, governedTestServer(t, &calls, nil), &runCheckerStub{governed: true, stale: staleBinding}, revoker)
	response, err := server.RenewLease(context.Background(), renewRequestFor(t))
	if err != nil || response.GetRejection() != nil {
		t.Fatalf("renewal: %v %v", response, err)
	}
	if response.GetDesiredState() != runtimev1.DesiredExecutionStateV1_DESIRED_EXECUTION_STATE_V1_CANCELLED || len(revoker.revoked) != 1 {
		t.Fatalf("desired = %v, revoked %v", response.GetDesiredState(), revoker.revoked)
	}
}

// Legacy runs, current governed runs and a failed read keep today's answer.
func TestRenewLeaseKeepsRunningWhenNotRevoked(t *testing.T) {
	for name, checker := range map[string]*runCheckerStub{
		"legacy":      {governed: false},
		"current":     {governed: true},
		"read failed": {err: errors.New("db down")},
	} {
		t.Run(name, func(t *testing.T) {
			calls := []string{}
			revoker := &revokerStub{}
			server := withEpochs(t, governedTestServer(t, &calls, nil), checker, revoker)
			response, err := server.RenewLease(context.Background(), renewRequestFor(t))
			if err != nil || response.GetRejection() != nil ||
				response.GetDesiredState() != runtimev1.DesiredExecutionStateV1_DESIRED_EXECUTION_STATE_V1_RUNNING || len(revoker.revoked) != 0 {
				t.Fatalf("response %v err %v revoked %v", response, err, revoker.revoked)
			}
		})
	}
}

// A stale fence never reaches the epoch read.
func TestRenewLeaseWithAStaleFenceNeverChecksEpochs(t *testing.T) {
	calls := []string{}
	checker := &runCheckerStub{governed: true, stale: staleBinding}
	server := withEpochs(t, governedTestServer(t, &calls, nil), checker, &revokerStub{})
	request := renewRequestFor(t)
	request.Fence.FenceToken[0] ^= 0xff
	response, err := server.RenewLease(context.Background(), request)
	if err != nil || response.GetRejection() == nil || checker.calls != 0 {
		t.Fatalf("response %v err %v checks %d", response, err, checker.calls)
	}
}

// Flag off: nothing composed, so neither claim nor renewal reads anything
// new; WithGovernedEpochs(nil, nil) keeps it so.
func TestFlagOffComposesNoEpochCheck(t *testing.T) {
	calls := []string{}
	server := withEpochs(t, governedTestServer(t, &calls, nil), nil, nil)
	if server.epochs != nil {
		t.Fatal("a nil checker composed an epoch check")
	}
	response, err := server.ClaimCommand(context.Background(), governedClaimRequest(t, nil, true))
	if code := rejectionCode(t, response, err); code != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSPECIFIED || !claimed(calls) {
		t.Fatalf("code = %v, calls %v", code, calls)
	}
	renewal, err := server.RenewLease(context.Background(), renewRequestFor(t))
	if err != nil || renewal.GetDesiredState() != runtimev1.DesiredExecutionStateV1_DESIRED_EXECUTION_STATE_V1_RUNNING {
		t.Fatalf("renewal %v err %v", renewal, err)
	}
}
