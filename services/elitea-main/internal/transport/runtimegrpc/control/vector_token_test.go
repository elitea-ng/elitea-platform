package control

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

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

// A failed mint degrades: the claim is delivered without a vector token, the
// failure is logged at warn and counted, and neither the claim nor the
// input-resolution retry budget is touched. Only a claim that is no longer
// live is rejected, as stale.
func TestClaimCommandDeliversWithoutAVectorTokenWhenTheMintFails(t *testing.T) {
	for _, attempt := range []uint64{1, maxInputResolutionClaimAttempts, maxInputResolutionClaimAttempts + 5} {
		t.Run(fmt.Sprintf("attempt %d", attempt), func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(previous) })

			reader := sdkmetric.NewManualReader()
			calls := []string{}
			lease := validLease()
			lease.Fence.ClaimAttempt = attempt
			config := testControlServerConfig()
			config.VectorTokens = vectorTokenIssuerStub{calls: &calls, err: errors.New("database down")}
			config.Meter = sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")
			server, err := NewServer(
				config,
				workloadAuthorizerStub{calls: &calls},
				verifierSpy{calls: &calls, verifier: newTestVerifier(t)},
				claimControllerStub{calls: &calls, lease: lease, abortDisposition: new(executionapp.ClaimAbortDisposition)},
				inputResolverStub{calls: &calls, manifest: validManifest()},
				&settlementControllerStub{calls: &calls},
			)
			if err != nil {
				t.Fatal(err)
			}
			response, err := server.ClaimCommand(context.Background(), claimRequestForManifest(t, validManifest()))
			if err != nil {
				t.Fatal(err)
			}
			if response.GetRejection() != nil ||
				response.GetReceipt().GetDisposition() != runtimev1.ClaimDispositionV1_CLAIM_DISPOSITION_V1_ACCEPTED {
				t.Fatalf("the claim must proceed, got %v", response)
			}
			if response.GetReceipt().GetVectorToken() != nil {
				t.Fatalf("receipt token = %v, want none", response.GetReceipt().GetVectorToken())
			}
			if response.GetReceipt().GetInputBundle() == nil {
				t.Fatal("the receipt must still carry the input bundle")
			}
			wantCalls := []string{"authorize-peer", "verify-command", "claim", "resolve-reference-manifest", "mint-vector-token"}
			if !reflect.DeepEqual(calls, wantCalls) {
				t.Fatalf("calls: got %v want %v (no abort, no retry budget spent)", calls, wantCalls)
			}
			if got := mintFailureCount(t, reader); got != 1 {
				t.Fatalf("mint failure counter = %d, want 1", got)
			}
			output := logs.String()
			if !strings.Contains(output, "level=WARN") || !strings.Contains(output, "vector claim token not minted") {
				t.Fatalf("expected a warn log, got %q", output)
			}
		})
	}
}

// A claim that is no longer live cannot be delivered, mint failure or not.
func TestClaimCommandRejectsAStaleClaimWhenTheVectorTokenIsMinted(t *testing.T) {
	calls := []string{}
	server := vectorTokenServer(t, &calls, validLease(), vectorTokenIssuerStub{calls: &calls, err: runtimedomain.ErrStaleFence}, nil)
	response, err := server.ClaimCommand(context.Background(), claimRequestForManifest(t, validManifest()))
	if err != nil {
		t.Fatal(err)
	}
	if response.GetReceipt() != nil ||
		response.GetRejection().GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_STALE_FENCE {
		t.Fatalf("unexpected response %v", response)
	}
}

func mintFailureCount(t *testing.T, reader sdkmetric.Reader) int64 {
	t.Helper()
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatal(err)
	}
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "elitea.runtime.vector_token.mint_failures" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok || len(sum.DataPoints) != 1 {
				t.Fatalf("unexpected metric data %v", m.Data)
			}
			return sum.DataPoints[0].Value
		}
	}
	return 0
}
