package control

import (
	"context"
	"errors"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	indexregistryapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexregistry"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

type indexRemoverStub struct {
	disposition indexregistryapp.RemovalDisposition
	err         error
	claims      []indexregistryapp.ClaimRef
	requests    []indexregistryapp.ClaimRemovalRequest
}

func (s *indexRemoverStub) RemoveIndex(_ context.Context, claim indexregistryapp.ClaimRef, request indexregistryapp.ClaimRemovalRequest) (indexregistryapp.RemovalDisposition, error) {
	s.claims = append(s.claims, claim)
	s.requests = append(s.requests, request)
	if s.err != nil {
		return "", s.err
	}
	return s.disposition, nil
}

func validRemoveIndexRequest(fence runtimedomain.Fence) *runtimev1.RemoveIndexRequestV1 {
	return &runtimev1.RemoveIndexRequestV1{
		Identity:  identityForFence(fence),
		Fence:     fenceProto(fence),
		ClaimId:   "claim-1",
		ToolkitId: 19,
		IndexName: "docs",
		IndexId:   "0b0f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5b",
	}
}

// RemoveIndex authenticates the peer, verifies the fence live, and hands the
// remover exactly the claim the fence names — the worker's words (claim id,
// toolkit, index name and id) travel, the authority does not.
func TestRemoveIndexVerifiesTheClaimAndHandsItToTheRemover(t *testing.T) {
	for _, disposition := range []indexregistryapp.RemovalDisposition{
		indexregistryapp.RemovalRemoved,
		indexregistryapp.RemovalNotFound,
		indexregistryapp.RemovalForbidden,
		indexregistryapp.RemovalActiveRun,
		indexregistryapp.RemovalSharedWriteRefused,
	} {
		t.Run(string(disposition), func(t *testing.T) {
			calls := []string{}
			lease := validLease()
			remover := &indexRemoverStub{disposition: disposition}
			config := testControlServerConfig()
			config.IndexRemovals = remover
			server, err := NewServer(config, workloadAuthorizerStub{calls: &calls}, verifierSpy{calls: &calls, verifier: newTestVerifier(t)}, claimControllerStub{calls: &calls, lease: lease}, inputResolverStub{calls: &calls, manifest: validManifest()}, &settlementControllerStub{calls: &calls})
			if err != nil {
				t.Fatal(err)
			}
			response, err := server.RemoveIndex(context.Background(), validRemoveIndexRequest(lease.Fence))
			if err != nil {
				t.Fatal(err)
			}
			if response.GetRejection() != nil {
				t.Fatalf("rejection = %v", response.GetRejection())
			}
			expected, _ := removeIndexDispositionProto(disposition)
			if response.GetDisposition() != expected {
				t.Fatalf("disposition = %v; expected %v", response.GetDisposition(), expected)
			}
			if len(calls) != 2 || calls[0] != "authorize-peer" || calls[1] != "verify-fence" {
				t.Fatalf("calls = %v", calls)
			}
			if len(remover.claims) != 1 {
				t.Fatalf("remover calls = %d", len(remover.claims))
			}
			claim := remover.claims[0]
			if claim.ExecutionID != lease.Fence.ExecutionID || claim.Generation != lease.Fence.Generation ||
				claim.ClaimID != "claim-1" || claim.WorkloadIdentity != lease.Fence.WorkloadIdentity ||
				string(claim.FenceToken) != string(lease.Fence.Token[:]) {
				t.Fatalf("claim = %+v", claim)
			}
			request := remover.requests[0]
			if request.ToolkitID != 19 || request.IndexName != "docs" || request.IndexID != "0b0f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5b" {
				t.Fatalf("request = %+v", request)
			}
		})
	}
}

// Every refusal is a typed rejection in the response, never a transport
// error, and none of them reaches the remover.
func TestRemoveIndexRejectsBeforeTheRemoverIsAsked(t *testing.T) {
	cases := map[string]struct {
		arrange func(*ServerConfig, *runtimev1.RemoveIndexRequestV1, *runtimedomain.ActiveLease, *workloadAuthorizerStub)
		code    runtimev1.RuntimeErrorCodeV1
	}{
		"nil request": {
			arrange: func(_ *ServerConfig, request *runtimev1.RemoveIndexRequestV1, _ *runtimedomain.ActiveLease, _ *workloadAuthorizerStub) {
				*request = runtimev1.RemoveIndexRequestV1{}
			},
			code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION,
		},
		"no claim id": {
			arrange: func(_ *ServerConfig, request *runtimev1.RemoveIndexRequestV1, _ *runtimedomain.ActiveLease, _ *workloadAuthorizerStub) {
				request.ClaimId = ""
			},
			code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION,
		},
		"no toolkit": {
			arrange: func(_ *ServerConfig, request *runtimev1.RemoveIndexRequestV1, _ *runtimedomain.ActiveLease, _ *workloadAuthorizerStub) {
				request.ToolkitId = 0
			},
			code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION,
		},
		"no index name": {
			arrange: func(_ *ServerConfig, request *runtimev1.RemoveIndexRequestV1, _ *runtimedomain.ActiveLease, _ *workloadAuthorizerStub) {
				request.IndexName = ""
			},
			code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION,
		},
		"not served": {
			arrange: func(config *ServerConfig, _ *runtimev1.RemoveIndexRequestV1, _ *runtimedomain.ActiveLease, _ *workloadAuthorizerStub) {
				config.IndexRemovals = nil
			},
			code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY,
		},
		"peer refused": {
			arrange: func(_ *ServerConfig, _ *runtimev1.RemoveIndexRequestV1, _ *runtimedomain.ActiveLease, peer *workloadAuthorizerStub) {
				peer.err = errors.New("unknown peer")
			},
			code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHENTICATION_FAILED,
		},
		"stale fence": {
			arrange: func(_ *ServerConfig, request *runtimev1.RemoveIndexRequestV1, _ *runtimedomain.ActiveLease, _ *workloadAuthorizerStub) {
				request.Fence.LeaseEpoch++
			},
			code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_STALE_FENCE,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			calls := []string{}
			lease := validLease()
			remover := &indexRemoverStub{disposition: indexregistryapp.RemovalRemoved}
			config := testControlServerConfig()
			config.IndexRemovals = remover
			request := validRemoveIndexRequest(lease.Fence)
			peer := workloadAuthorizerStub{calls: &calls}
			tc.arrange(&config, request, &lease, &peer)
			server, err := NewServer(config, peer, verifierSpy{calls: &calls, verifier: newTestVerifier(t)}, claimControllerStub{calls: &calls, lease: lease}, inputResolverStub{calls: &calls, manifest: validManifest()}, &settlementControllerStub{calls: &calls})
			if err != nil {
				t.Fatal(err)
			}
			response, err := server.RemoveIndex(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if response.GetRejection().GetCode() != tc.code {
				t.Fatalf("rejection = %v; expected %v", response.GetRejection(), tc.code)
			}
			if response.GetDisposition() != runtimev1.RemoveIndexDispositionV1_REMOVE_INDEX_DISPOSITION_V1_UNSPECIFIED {
				t.Fatal("a rejected request carries no disposition")
			}
			if len(remover.claims) != 0 {
				t.Fatal("the remover was asked about a refused request")
			}
		})
	}
}

// The remover's errors map to the closed rejection set the worker reads.
func TestRemoveIndexMapsRemoverErrorsToRejections(t *testing.T) {
	cases := map[string]struct {
		err       error
		code      runtimev1.RuntimeErrorCodeV1
		retryable bool
	}{
		"unauthorized claim": {err: indexregistryapp.ErrClaimUnauthorized, code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_AUTHORIZATION_FAILED},
		"invalid request":    {err: indexregistryapp.ErrInvalidClaimRemovalRequest, code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION},
		"unavailable":        {err: indexregistryapp.ErrClaimRemovalUnavailable, code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE, retryable: true},
		"other":              {err: errors.New("boom"), code: runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			calls := []string{}
			lease := validLease()
			config := testControlServerConfig()
			config.IndexRemovals = &indexRemoverStub{err: tc.err}
			server, err := NewServer(config, workloadAuthorizerStub{calls: &calls}, verifierSpy{calls: &calls, verifier: newTestVerifier(t)}, claimControllerStub{calls: &calls, lease: lease}, inputResolverStub{calls: &calls, manifest: validManifest()}, &settlementControllerStub{calls: &calls})
			if err != nil {
				t.Fatal(err)
			}
			response, err := server.RemoveIndex(context.Background(), validRemoveIndexRequest(lease.Fence))
			if err != nil {
				t.Fatal(err)
			}
			rejection := response.GetRejection()
			if rejection.GetCode() != tc.code || rejection.GetRetryable() != tc.retryable {
				t.Fatalf("rejection = %v; expected %v retryable=%v", rejection, tc.code, tc.retryable)
			}
			if rejection.GetSafeMessage() == "boom" {
				t.Fatal("the remover's error text must not reach the worker")
			}
		})
	}
}
