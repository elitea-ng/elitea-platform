package control

import (
	"context"
	"errors"
	"reflect"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

type vectorTokenIssuerStub struct {
	calls  *[]string
	fences *[]runtimedomain.Fence
	token  *runtimev1.VectorClaimTokenV1
	err    error
}

func (s vectorTokenIssuerStub) IssueVectorClaimToken(_ context.Context, fence runtimedomain.Fence) (*runtimev1.VectorClaimTokenV1, error) {
	*s.calls = append(*s.calls, "mint-vector-token")
	if s.fences != nil {
		*s.fences = append(*s.fences, fence)
	}
	return s.token, s.err
}

func vectorTokenServer(t *testing.T, calls *[]string, lease runtimedomain.ActiveLease, issuer VectorTokenIssuer, aborted *executionapp.ClaimAbortDisposition) *Server {
	t.Helper()
	config := testControlServerConfig()
	config.VectorTokens = issuer
	server, err := NewServer(
		config,
		workloadAuthorizerStub{calls: calls},
		verifierSpy{calls: calls, verifier: newTestVerifier(t)},
		claimControllerStub{calls: calls, lease: lease, abortDisposition: aborted},
		inputResolverStub{calls: calls, manifest: validManifest()},
		&settlementControllerStub{calls: calls},
	)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

// The vector token is minted for the accepted claim's own fence, after every
// check that can still abort the claim, and delivered in the receipt.
func TestClaimCommandDeliversTheVectorTokenMintedForTheAcceptedFence(t *testing.T) {
	calls := []string{}
	fences := []runtimedomain.Fence{}
	token := &runtimev1.VectorClaimTokenV1{
		Bearer: "elvc_token", ExpiresAtUnixMillis: 1, AllowedSources: []string{"toolkit_index"},
	}
	lease := validLease()
	server := vectorTokenServer(t, &calls, lease, vectorTokenIssuerStub{calls: &calls, fences: &fences, token: token}, nil)
	response, err := server.ClaimCommand(context.Background(), claimRequestForManifest(t, validManifest()))
	if err != nil {
		t.Fatal(err)
	}
	if response.GetRejection() != nil || response.GetReceipt().GetDisposition() != runtimev1.ClaimDispositionV1_CLAIM_DISPOSITION_V1_ACCEPTED {
		t.Fatalf("unexpected response %v", response)
	}
	if response.GetReceipt().GetVectorToken() != token {
		t.Fatalf("receipt token = %v", response.GetReceipt().GetVectorToken())
	}
	if len(fences) != 1 || fences[0] != lease.Fence {
		t.Fatalf("minted for %v, want the accepted fence", fences)
	}
	wantOrder := []string{"authorize-peer", "verify-command", "claim", "resolve-reference-manifest", "mint-vector-token"}
	if !reflect.DeepEqual(calls, wantOrder) {
		t.Fatalf("order: got %v want %v", calls, wantOrder)
	}
}

// Without an issuer (no vector introspection served), and for an execution
// that gets no token, the receipt carries none.
func TestClaimCommandCarriesNoVectorTokenWhenNoneIsMinted(t *testing.T) {
	for name, issuer := range map[string]func(*[]string) VectorTokenIssuer{
		"no issuer":  func(*[]string) VectorTokenIssuer { return nil },
		"ineligible": func(calls *[]string) VectorTokenIssuer { return vectorTokenIssuerStub{calls: calls} },
	} {
		t.Run(name, func(t *testing.T) {
			calls := []string{}
			server := vectorTokenServer(t, &calls, validLease(), issuer(&calls), nil)
			response, err := server.ClaimCommand(context.Background(), claimRequestForManifest(t, validManifest()))
			if err != nil {
				t.Fatal(err)
			}
			if response.GetRejection() != nil || response.GetReceipt().GetVectorToken() != nil {
				t.Fatalf("unexpected response %v", response)
			}
		})
	}
}

// A failed mint releases the claim with the bounded input-resolution retry;
// a claim that is no longer live is only reported stale.
func TestClaimCommandAbortsTheClaimWhenTheVectorTokenCannotBeMinted(t *testing.T) {
	tests := []struct {
		name          string
		attempt       uint64
		err           error
		wantAbort     executionapp.ClaimAbortDisposition
		wantCode      runtimev1.RuntimeErrorCodeV1
		wantRetryable bool
		wantCalls     []string
	}{
		{
			name: "transient", attempt: 1, err: errors.New("database down"),
			wantAbort: executionapp.ClaimAbortInputResolutionRetry,
			wantCode:  runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE, wantRetryable: true,
			wantCalls: []string{"authorize-peer", "verify-command", "claim", "resolve-reference-manifest", "mint-vector-token", "abort-claim"},
		},
		{
			name: "exhausted", attempt: maxInputResolutionClaimAttempts, err: errors.New("database down"),
			wantAbort: executionapp.ClaimAbortInputResolutionExhausted,
			wantCode:  runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE, wantRetryable: false,
			wantCalls: []string{"authorize-peer", "verify-command", "claim", "resolve-reference-manifest", "mint-vector-token", "abort-claim"},
		},
		{
			name: "stale", attempt: 1, err: runtimedomain.ErrStaleFence,
			wantCode:  runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_STALE_FENCE,
			wantCalls: []string{"authorize-peer", "verify-command", "claim", "resolve-reference-manifest", "mint-vector-token"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := []string{}
			lease := validLease()
			lease.Fence.ClaimAttempt = test.attempt
			var aborted executionapp.ClaimAbortDisposition
			server := vectorTokenServer(t, &calls, lease, vectorTokenIssuerStub{calls: &calls, err: test.err}, &aborted)
			response, err := server.ClaimCommand(context.Background(), claimRequestForManifest(t, validManifest()))
			if err != nil {
				t.Fatal(err)
			}
			if response.GetReceipt() != nil || response.GetRejection().GetCode() != test.wantCode ||
				response.GetRejection().GetRetryable() != test.wantRetryable {
				t.Fatalf("unexpected response %v", response)
			}
			if aborted != test.wantAbort {
				t.Fatalf("abort disposition: got %q want %q", aborted, test.wantAbort)
			}
			if !reflect.DeepEqual(calls, test.wantCalls) {
				t.Fatalf("calls: got %v want %v", calls, test.wantCalls)
			}
		})
	}
}
