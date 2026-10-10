package vectorintrospection

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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
		},
		fakeFacts{
			1: {UserID: 5, ProjectID: 7, ExpiresAt: expires, Provider: "deepwiki"},
			3: {UserID: 5, ProjectID: 7, ExpiresAt: expires, Provider: "deepwiki"},
			5: {UserID: 5, ProjectID: 8, ExpiresAt: expires, Provider: "deepwiki"},
			6: {UserID: 6, ProjectID: 7, ExpiresAt: expires, Provider: "deepwiki"},
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
		response.GetKind() != vectorv1.TokenKind_TOKEN_KIND_ENGINE_CALLBACK || response.GetExpiresAtUnix() <= time.Now().Unix() {
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
		if _, err := NewServer(fakeValidator{}, fakeFacts{}, clients, nil); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	if _, err := NewServer(nil, fakeFacts{}, []string{"dns:a"}, nil); err == nil {
		t.Fatal("nil validator accepted")
	}
}
