package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	vectorv1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/vector/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
)

type introspectionValidator map[string]auth.User

func (v introspectionValidator) ValidateToken(_ context.Context, token string) (auth.User, error) {
	if user, ok := v[token]; ok {
		return user, nil
	}
	return auth.User{}, auth.ErrCredentialRejected
}

type introspectionFacts map[int64]repos.CallbackTokenFacts

func (f introspectionFacts) Facts(_ context.Context, tokenID int64) (repos.CallbackTokenFacts, error) {
	if facts, ok := f[tokenID]; ok {
		return facts, nil
	}
	return repos.CallbackTokenFacts{}, repos.ErrCallbackTokenFactsNotFound
}

type introspectionRegistrar struct {
	impl vectorv1.TokenIntrospectionServiceServer
}

func (r *introspectionRegistrar) RegisterService(_ *grpc.ServiceDesc, impl any) {
	r.impl, _ = impl.(vectorv1.TokenIntrospectionServiceServer)
}

func peerWithDNS(name string) context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{
		State: tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{{DNSNames: []string{name}}}}},
	}})
}

func ptrTo[T any](value T) *T { return &value }

// elitea-vector's token introspection on the platform listener: registered
// only when ELITEA_VECTOR_INTROSPECTION_CLIENTS names someone, with the
// runtime plane OFF (runtime nil), admitting only the listed identity, and
// answering a DeepWiki or Inventory callback token with its project and the
// provider's source.
func TestPlatformListenerServesVectorIntrospectionOnlyWhenAllowlisted(t *testing.T) {
	base, err := platformServices(http.NotFoundHandler(), platformClients{Model: "dns:elitea-worker"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	dependencies := runtimecomposition.PlatformVectorIntrospection{
		Validator: introspectionValidator{
			"deepwiki-bearer":  {UserID: "5", TokenID: "1", TokenProjectID: ptrTo(int64(7)), TokenProjectActive: ptrTo(true)},
			"inventory-bearer": {UserID: "5", TokenID: "2", TokenProjectID: ptrTo(int64(9)), TokenProjectActive: ptrTo(true)},
			"a-pat":            {UserID: "5", TokenID: "3", TokenProjectID: ptrTo(int64(7)), TokenProjectActive: ptrTo(true)},
		},
		CallbackFacts: introspectionFacts{
			1: {UserID: 5, ProjectID: 7, ExpiresAt: expires, Provider: "deepwiki"},
			2: {UserID: 5, ProjectID: 9, ExpiresAt: expires, Provider: "inventory"},
		},
	}

	// No allowlist: nothing new registers.
	services, err := withVectorIntrospection(base, runtimecomposition.Config{PlatformAddress: ":9446"}, nil, dependencies)
	if err != nil || len(services) != len(base) {
		t.Fatalf("unconfigured: %d services, err %v", len(services), err)
	}

	config := runtimecomposition.Config{PlatformAddress: ":9446", VectorIntrospectionClients: []string{"dns:elitea-vector"}}
	services, err = withVectorIntrospection(base, config, nil, dependencies)
	if err != nil || len(services) != len(base)+1 {
		t.Fatalf("configured: %d services, err %v", len(services), err)
	}
	service := services[len(services)-1]
	if service.Name != "elitea.vector.v1.TokenIntrospectionService" ||
		!service.Clients.Allows("dns:elitea-vector") || service.Clients.Allows("dns:elitea-worker") {
		t.Fatalf("service %+v", service)
	}
	registrar := &introspectionRegistrar{}
	service.Register(registrar)
	if registrar.impl == nil {
		t.Fatal("the registered implementation is not a TokenIntrospectionServiceServer")
	}

	for token, want := range map[string]struct {
		project int64
		source  vectorv1.Source
	}{
		"deepwiki-bearer":  {7, vectorv1.Source_SOURCE_DEEPWIKI},
		"inventory-bearer": {9, vectorv1.Source_SOURCE_INVENTORY},
	} {
		response, err := registrar.impl.IntrospectToken(peerWithDNS("elitea-vector"), &vectorv1.IntrospectTokenRequest{Token: token})
		if err != nil {
			t.Fatal(err)
		}
		if !response.GetActive() || response.GetProjectId() != want.project ||
			len(response.GetAllowedSources()) != 1 || response.GetAllowedSources()[0] != want.source {
			t.Fatalf("%s: %v", token, response)
		}
	}
	// A PAT has no grant; a worker claim token has no claim without the runtime.
	for _, token := range []string{"a-pat", "elvc_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
		response, err := registrar.impl.IntrospectToken(peerWithDNS("elitea-vector"), &vectorv1.IntrospectTokenRequest{Token: token})
		if err != nil || response.GetActive() {
			t.Fatalf("%s: %v %v", token, response, err)
		}
	}
	// An unlisted identity is refused.
	if _, err := registrar.impl.IntrospectToken(peerWithDNS("elitea-worker"),
		&vectorv1.IntrospectTokenRequest{Token: "deepwiki-bearer"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unlisted identity: %v", err)
	}

	// Allowlisted with no validator (an OIDC-only deployment's absent form
	// graph) is a boot error, not a listener that panics on its first call.
	if _, err := withVectorIntrospection(base, config, nil, runtimecomposition.PlatformVectorIntrospection{
		CallbackFacts: dependencies.CallbackFacts,
	}); err == nil {
		t.Fatal("an introspection with no token validator was accepted")
	}
}
