package vectorintrospection

import (
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

type recordingStore struct {
	requests []repos.VectorClaimTokenMint
	answer   repos.VectorClaimTokenMinted
	err      error
}

func (s *recordingStore) Mint(_ context.Context, request repos.VectorClaimTokenMint) (repos.VectorClaimTokenMinted, error) {
	s.requests = append(s.requests, request)
	return s.answer, s.err
}

func testFence() runtimedomain.Fence {
	return runtimedomain.Fence{
		CommandID: "command-1", ExecutionID: "execution-1", Generation: 1,
		WorkloadIdentity: "spiffe://elitea.test/worker", WorkloadSessionID: "session-1",
		ProducerID: "producer-1", ClaimAttempt: 1, LeaseEpoch: 1,
	}
}

// The issuer stores only the hash of the bearer it returns, and asks for the
// capabilities and the one source a worker claim token is for.
func TestClaimTokenIssuerStoresOnlyTheHashOfTheBearerItReturns(t *testing.T) {
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	store := &recordingStore{answer: repos.VectorClaimTokenMinted{
		Minted: true, ProjectID: 7, ActorID: 42, Sources: []string{"toolkit_index"}, ExpiresAt: expires,
	}}
	issuer, err := NewClaimTokenIssuer(store)
	if err != nil {
		t.Fatal(err)
	}
	first, err := issuer.IssueVectorClaimToken(context.Background(), testFence())
	if err != nil || first == nil {
		t.Fatalf("issue: %v %v", first, err)
	}
	second, err := issuer.IssueVectorClaimToken(context.Background(), testFence())
	if err != nil || second == nil {
		t.Fatalf("issue: %v %v", second, err)
	}
	if first.GetBearer() == second.GetBearer() {
		t.Fatal("two mints returned the same bearer")
	}
	for index, token := range []string{first.GetBearer(), second.GetBearer()} {
		if !strings.HasPrefix(token, ClaimTokenPrefix) || len(token) != ClaimTokenLength {
			t.Fatalf("malformed bearer %q", token)
		}
		request := store.requests[index]
		if request.TokenSHA256 != sha256.Sum256([]byte(token)) {
			t.Fatal("the stored hash is not the bearer's SHA-256")
		}
		if request.Fence != testFence() || request.MaxLifetime != MaxClaimTokenLifetime {
			t.Fatalf("unexpected mint request %+v", request)
		}
		if !slices.Equal(request.Sources, []string{"toolkit_index"}) {
			t.Fatalf("sources = %v", request.Sources)
		}
		if !slices.Equal(request.Capabilities, []string{
			"index.ingest.v1", "toolkit.call_tool.v1", "agent.execute.application.v1", "agent.execute.adhoc.v1",
		}) {
			t.Fatalf("capabilities = %v", request.Capabilities)
		}
	}
	if first.GetExpiresAtUnixMillis() != expires.UnixMilli() || !slices.Equal(first.GetAllowedSources(), []string{"toolkit_index"}) {
		t.Fatalf("unexpected token %v", first)
	}
}

func TestClaimTokenIssuerMintsNothingForAnIneligibleExecution(t *testing.T) {
	issuer, err := NewClaimTokenIssuer(&recordingStore{})
	if err != nil {
		t.Fatal(err)
	}
	token, err := issuer.IssueVectorClaimToken(context.Background(), testFence())
	if err != nil || token != nil {
		t.Fatalf("ineligible: %v %v", token, err)
	}
}

func TestClaimTokenIssuerPassesAStaleClaimThrough(t *testing.T) {
	issuer, err := NewClaimTokenIssuer(&recordingStore{err: runtimedomain.ErrStaleFence})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := issuer.IssueVectorClaimToken(context.Background(), testFence()); !errors.Is(err, runtimedomain.ErrStaleFence) {
		t.Fatalf("stale: %v", err)
	}
}

func TestClaimTokenIssuerFailsWithoutEntropy(t *testing.T) {
	store := &recordingStore{}
	issuer, err := NewClaimTokenIssuer(store)
	if err != nil {
		t.Fatal(err)
	}
	issuer.random = iotest.ErrReader(errors.New("no entropy"))
	if _, err := issuer.IssueVectorClaimToken(context.Background(), testFence()); err == nil {
		t.Fatal("minted without entropy")
	}
	if len(store.requests) != 0 {
		t.Fatal("stored a hash without entropy")
	}
	if _, err := NewClaimTokenIssuer(nil); err == nil {
		t.Fatal("nil store accepted")
	}
}
