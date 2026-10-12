package vectorintrospection

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
	"time"

	vectorv1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/vector/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type fakeValidator map[string]auth.User

func (v fakeValidator) ValidateToken(_ context.Context, token string) (auth.User, error) {
	if token == "down" {
		return auth.User{}, fmt.Errorf("%w: db", auth.ErrCredentialValidationUnavailable)
	}
	user, ok := v[token]
	if !ok {
		return auth.User{}, fmt.Errorf("%w: unknown", auth.ErrCredentialRejected)
	}
	return user, nil
}

type fakeFacts map[int64]repos.CallbackTokenFacts

func (f fakeFacts) Facts(_ context.Context, tokenID int64) (repos.CallbackTokenFacts, error) {
	if tokenID == 99 {
		return repos.CallbackTokenFacts{}, errors.New("db down")
	}
	facts, ok := f[tokenID]
	if !ok {
		return repos.CallbackTokenFacts{}, repos.ErrCallbackTokenFactsNotFound
	}
	return facts, nil
}

type fakeClaimTokens map[[32]byte]repos.VectorClaimTokenFacts

func (f fakeClaimTokens) Facts(_ context.Context, hash [32]byte) (repos.VectorClaimTokenFacts, error) {
	if hash == sha256.Sum256([]byte(claimToken(0x99))) {
		return repos.VectorClaimTokenFacts{}, errors.New("db down")
	}
	facts, ok := f[hash]
	if !ok {
		return repos.VectorClaimTokenFacts{}, repos.ErrVectorClaimTokenFactsNotFound
	}
	return facts, nil
}

// claimToken is a well-formed claim token whose 32 random bytes all equal b.
func claimToken(b byte) string {
	return ClaimTokenPrefix + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32))
}

func mustHash(t *testing.T, token string) [32]byte {
	t.Helper()
	hash, ok := HashClaimToken(token)
	if !ok {
		t.Fatalf("%q is not a claim token", token)
	}
	return hash
}

func ptr[T any](value T) *T { return &value }

func user(tokenID string, project int64, active bool) auth.User {
	return auth.User{
		UserID: "5", TokenID: tokenID,
		TokenProjectID: ptr(project), TokenProjectActive: ptr(active),
	}
}

func peerContext(dnsNames ...string) context.Context {
	certificate := &x509.Certificate{DNSNames: dnsNames}
	return peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{
		State: tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{certificate}}},
	}})
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	server, err := NewServer(
		fakeValidator{
			"callback":       user("1", 7, true),
			"pat":            user("2", 7, true),
			"suspended":      user("3", 7, false),
			"unbound":        {UserID: "5", TokenID: "4"},
			"other-project":  user("5", 7, true),
			"other-owner":    user("6", 7, true),
			"facts-down":     user("99", 7, true),
			"elnat_whatever": user("1", 7, true),
			"inventory":      user("7", 7, true),
			"unknown-source": user("8", 7, true),
			// A string with the claim prefix never reaches the PAT validator,
			// even when the validator would admit it.
			claimToken(0x0f): user("1", 7, true),
		},
		fakeFacts{
			1: {UserID: 5, ProjectID: 7, ExpiresAt: expires, Provider: "deepwiki"},
			3: {UserID: 5, ProjectID: 7, ExpiresAt: expires, Provider: "deepwiki"},
			5: {UserID: 5, ProjectID: 8, ExpiresAt: expires, Provider: "deepwiki"},
			6: {UserID: 6, ProjectID: 7, ExpiresAt: expires, Provider: "deepwiki"},
			7: {UserID: 5, ProjectID: 7, ExpiresAt: expires, Provider: "inventory"},
			8: {UserID: 5, ProjectID: 7, ExpiresAt: expires, Provider: "elsewhere"},
		},
		fakeClaimTokens{
			mustHash(t, claimToken(0x01)): {ProjectID: 7, ActorID: 42, Sources: []string{"toolkit_index"}, ExpiresAt: expires},
			mustHash(t, claimToken(0x02)): {ProjectID: 7, ActorID: 42, Sources: []string{"not-a-source"}, ExpiresAt: expires},
			mustHash(t, claimToken(0x03)): {ProjectID: 7, ActorID: 42, ExpiresAt: expires},
		},
		[]string{"dns:elitea-vector"},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func TestIntrospectAdmitsALiveCallbackToken(t *testing.T) {
	server := newTestServer(t)
	response, err := server.IntrospectToken(peerContext("elitea-vector"), &vectorv1.IntrospectTokenRequest{Token: "callback"})
	if err != nil {
		t.Fatal(err)
	}
	if !response.GetActive() || response.GetProjectId() != 7 || response.GetPrincipal() != "user:5" ||
		response.GetKind() != vectorv1.TokenKind_TOKEN_KIND_ENGINE_CALLBACK || response.GetExpiresAtUnix() <= time.Now().Unix() ||
		len(response.GetAllowedSources()) != 1 || response.GetAllowedSources()[0] != vectorv1.Source_SOURCE_DEEPWIKI {
		t.Fatalf("unexpected response %v", response)
	}
}

func TestIntrospectRefusesEverythingElse(t *testing.T) {
	server := newTestServer(t)
	for _, token := range []string{"", "unknown", "pat", "suspended", "unbound", "other-project", "other-owner", "elnat_whatever"} {
		response, err := server.IntrospectToken(peerContext("elitea-vector"), &vectorv1.IntrospectTokenRequest{Token: token})
		if err != nil {
			t.Fatalf("%q: %v", token, err)
		}
		if response.GetActive() || response.GetProjectId() != 0 {
			t.Fatalf("%q admitted: %v", token, response)
		}
	}
}

func TestIntrospectReportsUnavailabilityAsAnError(t *testing.T) {
	server := newTestServer(t)
	for _, token := range []string{"down", "facts-down"} {
		_, err := server.IntrospectToken(peerContext("elitea-vector"), &vectorv1.IntrospectTokenRequest{Token: token})
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("%q: got %v, want Unavailable", token, err)
		}
	}
}

func TestIntrospectAnswersOnlyConfiguredClients(t *testing.T) {
	server := newTestServer(t)
	request := &vectorv1.IntrospectTokenRequest{Token: "callback"}
	if _, err := server.IntrospectToken(peerContext("elitea-worker"), request); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("other identity: %v", err)
	}
	if _, err := server.IntrospectToken(peerContext("elitea-vector", "extra"), request); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("ambiguous identity: %v", err)
	}
	if _, err := server.IntrospectToken(context.Background(), request); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("no peer: %v", err)
	}
}

func TestNewServerRefusesAnIncompleteConfiguration(t *testing.T) {
	for name, clients := range map[string][]string{
		"none":      nil,
		"bare name": {"elitea-vector"},
	} {
		if _, err := NewServer(fakeValidator{}, fakeFacts{}, fakeClaimTokens{}, clients, nil); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	if _, err := NewServer(nil, fakeFacts{}, fakeClaimTokens{}, []string{"dns:a"}, nil); err == nil {
		t.Fatal("nil validator accepted")
	}
	if _, err := NewServer(fakeValidator{}, fakeFacts{}, nil, []string{"dns:a"}, nil); err == nil {
		t.Fatal("nil claim token facts accepted")
	}
}

// A callback token may use only the source of the provider whose facade
// recorded its grant.
func TestIntrospectNamesTheCallbackProvidersSource(t *testing.T) {
	server := newTestServer(t)
	for token, want := range map[string]vectorv1.Source{
		"callback":  vectorv1.Source_SOURCE_DEEPWIKI,
		"inventory": vectorv1.Source_SOURCE_INVENTORY,
	} {
		response, err := server.IntrospectToken(peerContext("elitea-vector"), &vectorv1.IntrospectTokenRequest{Token: token})
		if err != nil {
			t.Fatal(err)
		}
		if !response.GetActive() || len(response.GetAllowedSources()) != 1 || response.GetAllowedSources()[0] != want {
			t.Fatalf("%q: %v", token, response)
		}
	}
	// A provider with no vector source admits nothing.
	response, err := server.IntrospectToken(peerContext("elitea-vector"), &vectorv1.IntrospectTokenRequest{Token: "unknown-source"})
	if err != nil || response.GetActive() {
		t.Fatalf("unknown provider: %v %v", response, err)
	}
}

func TestIntrospectAdmitsALiveWorkerClaimToken(t *testing.T) {
	server := newTestServer(t)
	response, err := server.IntrospectToken(peerContext("elitea-vector"), &vectorv1.IntrospectTokenRequest{Token: claimToken(0x01)})
	if err != nil {
		t.Fatal(err)
	}
	if !response.GetActive() || response.GetProjectId() != 7 || response.GetPrincipal() != "user:42" ||
		response.GetKind() != vectorv1.TokenKind_TOKEN_KIND_WORKER_CLAIM ||
		response.GetExpiresAtUnix() <= time.Now().Unix() ||
		len(response.GetAllowedSources()) != 1 ||
		response.GetAllowedSources()[0] != vectorv1.Source_SOURCE_TOOLKIT_INDEX {
		t.Fatalf("unexpected response %v", response)
	}
}

func TestIntrospectRefusesEveryOtherClaimToken(t *testing.T) {
	server := newTestServer(t)
	for name, token := range map[string]string{
		"unknown (settled, cancelled, lost)": claimToken(0x04),
		"unknown source keyword":             claimToken(0x02),
		"no sources":                         claimToken(0x03),
		"validator would admit it":           claimToken(0x0f),
		"short":                              claimToken(0x01)[:ClaimTokenLength-1],
		"long":                               claimToken(0x01) + "A",
		"bad alphabet":                       claimToken(0x01)[:ClaimTokenLength-1] + "+",
		"prefix only":                        ClaimTokenPrefix,
	} {
		response, err := server.IntrospectToken(peerContext("elitea-vector"), &vectorv1.IntrospectTokenRequest{Token: token})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if response.GetActive() || response.GetProjectId() != 0 || len(response.GetAllowedSources()) != 0 {
			t.Fatalf("%s admitted: %v", name, response)
		}
	}
	if _, err := server.IntrospectToken(peerContext("elitea-vector"), &vectorv1.IntrospectTokenRequest{Token: claimToken(0x99)}); status.Code(err) != codes.Unavailable {
		t.Fatalf("store down: got %v, want Unavailable", err)
	}
}
