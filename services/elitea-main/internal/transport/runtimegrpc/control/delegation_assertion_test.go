package control

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/remoteagent"
)

type delegationAssertionsStub struct {
	calls     *[]string
	result    remoteagent.MintResult
	err       error
	audiences []string
}

func (s *delegationAssertionsStub) Mint(_ context.Context, executionID string, generation uint64, audience string) (remoteagent.MintResult, error) {
	*s.calls = append(*s.calls, "mint")
	s.audiences = append(s.audiences, audience)
	return s.result, s.err
}

func mintRequestFor(fence runtimedomain.Fence, audience string) *runtimev1.MintDelegationAssertionRequestV1 {
	return &runtimev1.MintDelegationAssertionRequestV1{Identity: identityForFence(fence), Fence: fenceProto(fence), Audience: audience}
}

// The live claim is verified before anything is minted, and the assertion
// comes back with its expiry and the governed flag.
func TestMintDelegationAssertionVerifiesTheClaimAndIssues(t *testing.T) {
	calls := []string{}
	lease := validLease()
	expires := time.Unix(1_900_000_000, 0).UTC()
	stub := &delegationAssertionsStub{calls: &calls, result: remoteagent.MintResult{
		Disposition: remoteagent.Issued, Token: "a.b.c", ExpiresAt: expires, Required: true}}
	server := newSteeringTestServer(t, &calls, lease).WithDelegationAssertions(stub)
	response, err := server.MintDelegationAssertion(context.Background(), mintRequestFor(lease.Fence, "https://mcp.example.com/mcp"))
	if err != nil || response.GetRejection() != nil {
		t.Fatalf("err=%v rejection=%v", err, response.GetRejection())
	}
	if !reflect.DeepEqual(calls, []string{"authorize-peer", "verify-fence", "mint"}) {
		t.Fatalf("calls=%v", calls)
	}
	if response.GetDisposition() != runtimev1.DelegationAssertionDispositionV1_DELEGATION_ASSERTION_DISPOSITION_V1_ISSUED ||
		response.GetAssertion() != "a.b.c" || response.GetExpiresAtUnixMillis() != expires.UnixMilli() || !response.GetRequired() {
		t.Fatalf("response=%v", response)
	}
}

func TestMintDelegationAssertionDisabled(t *testing.T) {
	calls := []string{}
	lease := validLease()
	stub := &delegationAssertionsStub{calls: &calls, result: remoteagent.MintResult{Disposition: remoteagent.Disabled}}
	server := newSteeringTestServer(t, &calls, lease).WithDelegationAssertions(stub)
	response, err := server.MintDelegationAssertion(context.Background(), mintRequestFor(lease.Fence, "https://mcp.example.com/mcp"))
	if err != nil || response.GetRejection() != nil ||
		response.GetDisposition() != runtimev1.DelegationAssertionDispositionV1_DELEGATION_ASSERTION_DISPOSITION_V1_DISABLED ||
		response.GetAssertion() != "" {
		t.Fatalf("err=%v response=%v", err, response)
	}
}

// A dead execution (stale or released claim) and an audience that is not an
// endpoint admitted in the run are refused; neither leaks internals.
func TestMintDelegationAssertionRefusals(t *testing.T) {
	lease := validLease()
	stale := lease.Fence
	stale.Token[0] ^= 0xff
	for _, test := range []struct {
		name      string
		stub      *delegationAssertionsStub
		request   *runtimev1.MintDelegationAssertionRequestV1
		code      runtimev1.RuntimeErrorCodeV1
		retryable bool
		required  bool
		minted    bool
	}{
		{name: "not composed", request: mintRequestFor(lease.Fence, "https://mcp.example.com/mcp"),
			code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY},
		{name: "malformed", stub: &delegationAssertionsStub{}, request: &runtimev1.MintDelegationAssertionRequestV1{},
			code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION},
		{name: "audience too long", stub: &delegationAssertionsStub{},
			request: mintRequestFor(lease.Fence, "https://mcp.example.com/"+strings.Repeat("a", 2100)),
			code:    runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION},
		{name: "dead execution", stub: &delegationAssertionsStub{}, request: mintRequestFor(stale, "https://mcp.example.com/mcp"),
			code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_STALE_FENCE},
		{name: "wrong audience", stub: &delegationAssertionsStub{err: remoteagent.ErrAudienceNotAdmitted, result: remoteagent.MintResult{Required: true}},
			request: mintRequestFor(lease.Fence, "https://evil.example.com/mcp"),
			code:    runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHORIZATION_FAILED, required: true, minted: true},
		{name: "no key ring", stub: &delegationAssertionsStub{err: remoteagent.ErrSignerUnavailable},
			request: mintRequestFor(lease.Fence, "https://mcp.example.com/mcp"),
			code:    runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY, minted: true},
		{name: "scope too large", stub: &delegationAssertionsStub{err: remoteagent.ErrScopeTooLarge},
			request: mintRequestFor(lease.Fence, "https://mcp.example.com/mcp"),
			code:    runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_RESOURCE_EXHAUSTED, minted: true},
		{name: "store down", stub: &delegationAssertionsStub{err: errors.New("database down")},
			request: mintRequestFor(lease.Fence, "https://mcp.example.com/mcp"),
			code:    runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE, retryable: true, minted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := []string{}
			server := newSteeringTestServer(t, &calls, lease)
			if test.stub != nil {
				test.stub.calls = &calls
				server.WithDelegationAssertions(test.stub)
			}
			response, err := server.MintDelegationAssertion(context.Background(), test.request)
			if err != nil {
				t.Fatal(err)
			}
			rejection := response.GetRejection()
			if rejection == nil || rejection.GetCode() != test.code || rejection.GetRetryable() != test.retryable ||
				response.GetRequired() != test.required || response.GetAssertion() != "" {
				t.Fatalf("response=%v", response)
			}
			if test.stub != nil && (len(test.stub.audiences) == 1) != test.minted {
				t.Fatalf("mint called=%v, want %v", len(test.stub.audiences) == 1, test.minted)
			}
			if strings.Contains(rejection.GetSafeMessage(), "database") {
				t.Fatalf("an internal error leaked: %q", rejection.GetSafeMessage())
			}
		})
	}
}
