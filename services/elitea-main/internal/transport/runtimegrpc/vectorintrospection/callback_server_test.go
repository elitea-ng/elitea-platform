package vectorintrospection

import (
	"testing"
	"time"

	vectorv1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/vector/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The runtime-off server (the platform listener without a runtime plane)
// verifies callback tokens exactly as the full one does, and answers every
// worker claim token inactive: no claim exists without the runtime.
func TestTheCallbackServerVerifiesCallbacksAndNoClaims(t *testing.T) {
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	server, err := NewCallbackServer(
		fakeValidator{"deepwiki": user("1", 7, true), "inventory": user("2", 8, true), "pat": user("3", 7, true)},
		fakeFacts{
			1: {UserID: 5, ProjectID: 7, ExpiresAt: expires, Provider: "deepwiki"},
			2: {UserID: 5, ProjectID: 8, ExpiresAt: expires, Provider: "inventory"},
		},
		[]string{"dns:elitea-vector"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for token, want := range map[string]struct {
		project int64
		source  vectorv1.Source
	}{
		"deepwiki":  {7, vectorv1.Source_SOURCE_DEEPWIKI},
		"inventory": {8, vectorv1.Source_SOURCE_INVENTORY},
	} {
		response, err := server.IntrospectToken(peerContext("elitea-vector"), &vectorv1.IntrospectTokenRequest{Token: token})
		if err != nil {
			t.Fatal(err)
		}
		if !response.GetActive() || response.GetProjectId() != want.project ||
			response.GetKind() != vectorv1.TokenKind_TOKEN_KIND_ENGINE_CALLBACK ||
			len(response.GetAllowedSources()) != 1 || response.GetAllowedSources()[0] != want.source {
			t.Fatalf("%q: %v", token, response)
		}
	}
	for _, token := range []string{"pat", claimToken(0x01), "unknown"} {
		response, err := server.IntrospectToken(peerContext("elitea-vector"), &vectorv1.IntrospectTokenRequest{Token: token})
		if err != nil || response.GetActive() {
			t.Fatalf("%q: %v %v", token, response, err)
		}
	}
	if _, err := server.IntrospectToken(peerContext("elitea-worker"), &vectorv1.IntrospectTokenRequest{Token: "deepwiki"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unlisted identity: %v", err)
	}
}

func TestNewCallbackServerRefusesAnIncompleteConfiguration(t *testing.T) {
	if _, err := NewCallbackServer(nil, fakeFacts{}, []string{"dns:a"}, nil); err == nil {
		t.Fatal("nil validator accepted")
	}
	if _, err := NewCallbackServer(fakeValidator{}, nil, []string{"dns:a"}, nil); err == nil {
		t.Fatal("nil facts accepted")
	}
	if _, err := NewCallbackServer(fakeValidator{}, fakeFacts{}, nil, nil); err == nil {
		t.Fatal("no clients accepted")
	}
}

// The platform listener's declaration admits exactly the server's own clients.
func TestPlatformServiceDeclaresTheServersOwnAllowlist(t *testing.T) {
	server := newTestServer(t)
	service := server.PlatformService()
	if service.Name != vectorv1.TokenIntrospectionService_ServiceDesc.ServiceName || service.Register == nil ||
		!service.Clients.Allows("dns:elitea-vector") || service.Clients.Allows("dns:elitea-worker") {
		t.Fatalf("service %+v", service)
	}
}
