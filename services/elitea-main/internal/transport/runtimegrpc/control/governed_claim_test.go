package control

import (
	"context"
	"errors"
	"testing"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/protobuf/proto"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
)

type pinReaderStub struct {
	pin   profile.Pin
	err   error
	calls *int
}

func (s pinReaderStub) ReadPin(_ context.Context, executionID string) (profile.Pin, error) {
	*s.calls++
	pin := s.pin
	pin.ExecutionID = executionID
	return pin, s.err
}

// governedClaimRequest is claimRequestForManifest's command with field 65
// set (or a foreign content id) and the worker's governed opt-in.
func governedClaimRequest(t *testing.T, contentID func(executionID string) string, governedWorker bool) *runtimev1.ClaimCommandRequestV1 {
	t.Helper()
	request := claimRequestForManifest(t, validManifest())
	command := &runtimev1.WorkerCommandV1{}
	if err := proto.Unmarshal(request.GetSignedCommand().GetWorkerCommandBytes(), command); err != nil {
		t.Fatal(err)
	}
	_, ref, err := securitycontext.Context{
		PolicyRevisionID: "2026-10-r1",
		PolicyDigest:     runtimedomain.SHA256([]byte("policy")),
		TenantProjectID:  42,
		IssuedAt:         time.UnixMilli(1_790_000_000_000).UTC(),
	}.Marshal(command.GetExecutionId())
	if err != nil {
		t.Fatal(err)
	}
	command.SecurityContextRef = ref.ReferenceProto()
	if contentID != nil {
		command.SecurityContextRef.ContentEntryId = contentID(command.GetExecutionId())
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	request.SignedCommand = signedEnvelope(raw)
	request.GovernedSecurityContextV1 = governedWorker
	return request
}

func governedTestServer(t *testing.T, calls *[]string, pins AdmissionPinReader) *Server {
	t.Helper()
	manifest := validManifest()
	server, err := NewServer(
		testControlServerConfig(),
		workloadAuthorizerStub{calls: calls},
		verifierSpy{calls: calls, verifier: newTestVerifier(t)},
		claimControllerStub{calls: calls, lease: validLease()},
		inputResolverStub{calls: calls, manifest: manifest},
		&settlementControllerStub{calls: calls},
	)
	if err != nil {
		t.Fatal(err)
	}
	if pins != nil {
		server.WithGovernedPins(pins)
	}
	return server
}

func rejectionCode(t *testing.T, response *runtimev1.ClaimCommandResponseV1, err error) runtimev1.RuntimeErrorCodeV1 {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if response.GetRejection() == nil {
		return runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSPECIFIED
	}
	return response.GetRejection().GetCode()
}

func claimed(calls []string) bool {
	for _, call := range calls {
		if call == "claim" {
			return true
		}
	}
	return false
}

// A governed command is refused to a worker that did not declare it
// verifies the security context, before any claim is attempted (#1115).
func TestGovernedCommandIsRefusedToALegacyWorkerBeforeTheClaim(t *testing.T) {
	calls := []string{}
	response, err := governedTestServer(t, &calls, nil).ClaimCommand(context.Background(), governedClaimRequest(t, nil, false))
	if code := rejectionCode(t, response, err); code != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_GOVERNED_WORKER_REQUIRED || response.GetRejection().GetRetryable() {
		t.Fatalf("code = %v (retryable %t)", code, response.GetRejection().GetRetryable())
	}
	if claimed(calls) {
		t.Fatalf("a legacy worker reached the claim: %v", calls)
	}
}

func TestGovernedCommandIsClaimedByAGovernedWorker(t *testing.T) {
	calls := []string{}
	response, err := governedTestServer(t, &calls, nil).ClaimCommand(context.Background(), governedClaimRequest(t, nil, true))
	if code := rejectionCode(t, response, err); code != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSPECIFIED || !claimed(calls) {
		t.Fatalf("code = %v, calls %v", code, calls)
	}
}

// Field 65 must name this execution's context: a reference to another
// execution's, or to another content namespace, is malformed.
func TestGovernedCommandWithAForeignContextReferenceIsMalformed(t *testing.T) {
	for name, contentID := range map[string]func(string) string{
		"other execution": func(string) string { return securitycontext.ContentIDPrefix + "another-execution" },
		"other namespace": func(string) string { return "agent.execution_request" },
	} {
		t.Run(name, func(t *testing.T) {
			calls := []string{}
			response, err := governedTestServer(t, &calls, nil).ClaimCommand(context.Background(), governedClaimRequest(t, contentID, true))
			if code := rejectionCode(t, response, err); code != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION || claimed(calls) {
				t.Fatalf("code = %v, calls %v", code, calls)
			}
		})
	}
}

// A command without field 65 for an execution pinned governed lost its
// context: it fails closed. A legacy pin is claimed as before.
func TestCommandWithoutContextForAGovernedPinFailsClosed(t *testing.T) {
	reads := 0
	calls := []string{}
	server := governedTestServer(t, &calls, pinReaderStub{pin: profile.Pin{Profile: profile.ProfileGoverned}, calls: &reads})
	response, err := server.ClaimCommand(context.Background(), claimRequestForManifest(t, validManifest()))
	if code := rejectionCode(t, response, err); code != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_GOVERNED_CONTEXT_MISSING || response.GetRejection().GetRetryable() || claimed(calls) || reads != 1 {
		t.Fatalf("code = %v, calls %v, reads %d", code, calls, reads)
	}

	calls = calls[:0]
	server = governedTestServer(t, &calls, pinReaderStub{pin: profile.Pin{Profile: profile.ProfileLegacy}, calls: &reads})
	response, err = server.ClaimCommand(context.Background(), claimRequestForManifest(t, validManifest()))
	if code := rejectionCode(t, response, err); code != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSPECIFIED || !claimed(calls) {
		t.Fatalf("legacy pin: code = %v, calls %v", code, calls)
	}

	calls = calls[:0]
	server = governedTestServer(t, &calls, pinReaderStub{err: errors.New("db down"), calls: &reads})
	response, err = server.ClaimCommand(context.Background(), claimRequestForManifest(t, validManifest()))
	if code := rejectionCode(t, response, err); code != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE || !response.GetRejection().GetRetryable() || claimed(calls) {
		t.Fatalf("pin read error: code = %v, calls %v", code, calls)
	}
}
