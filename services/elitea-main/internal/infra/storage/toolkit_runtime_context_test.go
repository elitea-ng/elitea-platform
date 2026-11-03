package storage

import (
	"context"
	"encoding/json"
	"errors"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/mcpoauth"
	"strings"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/protobuf/proto"
)

type toolkitTokenLoader struct {
	want      mcpoauth.TokenBinding
	revision  int64
	loads     int
	revoked   bool
	tokenType string
}

func (l *toolkitTokenLoader) Validate(_ context.Context, reference string, binding mcpoauth.TokenBinding) (mcpoauth.TokenReference, error) {
	if binding != l.want || l.revoked {
		return mcpoauth.TokenReference{}, mcpoauth.ErrTokenUnavailable
	}
	return mcpoauth.TokenReference{Reference: reference, Revision: l.revision}, nil
}
func (l *toolkitTokenLoader) Load(_ context.Context, _ string, binding mcpoauth.TokenBinding) (mcpoauth.AccessToken, error) {
	l.loads++
	if binding != l.want || l.revoked {
		return mcpoauth.AccessToken{}, mcpoauth.ErrTokenUnavailable
	}
	tokenType := l.tokenType
	if tokenType == "" {
		tokenType = "Bearer"
	}
	return mcpoauth.AccessToken{AccessToken: "transient-token", TokenType: tokenType}, nil
}
func TestToolkitContextRedeemsOnlyExactClaimAuthorization(t *testing.T) {
	for _, scope := range []struct{ capability, role string }{
		{executiondomain.ToolkitCallToolCapability, executiondomain.ToolkitCallToolRuntimeContextRole},
		{executiondomain.ToolkitAvailableToolsCapability, "toolkit.available_tools.runtime_context"},
	} {
		t.Run(scope.capability, func(t *testing.T) {
			loader := &toolkitTokenLoader{want: mcpoauth.TokenBinding{ProjectID: 7, ActorID: 42, ToolkitID: 19, Resource: "https://mcp.example.test/"}, revision: 1}
			materializer := newCurrentConfigurationsMaterializerForTest(t, &currentMaterializationUnsecreterStub{})
			materializer.toolkitTokens = loader
			source := []byte(`{"toolkit_security":{"blocked_toolkits":[],"blocked_tools":{},"sensitive_tools":{}},"mcp_token_reference":{"reference":"` + strings.Repeat("a", 43) + `","revision":1,"toolkit_id":19,"resource":"https://mcp.example.test/"}}`)
			authorization := ContentAuthorization{ResourceProjectID: "7", ActorID: "42", CapabilityID: scope.capability, SemanticRole: scope.role}
			for i := 0; i < 2; i++ {
				result, err := materializer.MaterializeContent(context.Background(), authorization, source, 256*1024)
				if err != nil {
					t.Fatal(err)
				}
				var object map[string]json.RawMessage
				if json.Unmarshal(result, &object) != nil {
					t.Fatal("invalid materialized JSON")
				}
				if _, exists := object["mcp_token_reference"]; exists || !strings.Contains(string(result), "transient-token") {
					t.Fatal("reference did not redeem transiently")
				}
				var tokens map[string]map[string]string
				if json.Unmarshal(object["mcp_tokens"], &tokens) != nil || len(tokens[loader.want.Resource]) != 1 || tokens[loader.want.Resource]["access_token"] != "transient-token" {
					t.Fatal("materialized token violates the Rust token contract")
				}
				if strings.Contains(string(source), "transient-token") {
					t.Fatal("source snapshot mutated")
				}
			}
			loader.tokenType = "DPoP"
			if _, err := materializer.MaterializeContent(context.Background(), authorization, source, 256*1024); err == nil {
				t.Fatal("unsupported token type accepted")
			}
			loader.tokenType = ""
			for _, foreign := range []ContentAuthorization{
				{ResourceProjectID: "8", ActorID: "42", CapabilityID: scope.capability, SemanticRole: scope.role},
				{ResourceProjectID: "7", ActorID: "43", CapabilityID: scope.capability, SemanticRole: scope.role},
			} {
				if _, err := materializer.MaterializeContent(context.Background(), foreign, source, 256*1024); err == nil {
					t.Fatal("foreign claim redeemed token")
				}
			}
			loader.revision = 2
			if _, err := materializer.MaterializeContent(context.Background(), authorization, source, 256*1024); err == nil {
				t.Fatal("changed credential revision accepted")
			}
			loader.revision = 1
			loader.revoked = true
			if _, err := materializer.MaterializeContent(context.Background(), authorization, source, 256*1024); err == nil {
				t.Fatal("revoked grant redeemed")
			}
			if loader.loads != 3 {
				t.Fatal("invalid authorization reached token loader")
			}
		})
	}
}

// FU-250 (E1, revocation window): a fan-out wake's mcp_token_references
// resolve through the one release path for the live claim's project and
// actor only, project into mcp_tokens under their resource, and never leave
// Main; a grant that no longer resolves is dropped (the Worker ends that
// branch typed); a malformed or duplicate entry refuses the content.
func TestAgentWakeReferencesResolveForTheClaimActorOnly(t *testing.T) {
	resource := "https://mcp.example.test/"
	reference := strings.Repeat("a", 43)
	wake := func(references ...*runtimev1.McpTokenReferenceV1) []byte {
		request := currentAgentMaterializationRequest(`[]`)
		request.McpTokenReferences = references
		source, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		return source
	}
	entry := func(resource string, toolkit, revision uint64) *runtimev1.McpTokenReferenceV1 {
		return &runtimev1.McpTokenReferenceV1{Resource: resource, Reference: reference, Revision: revision, ToolkitId: toolkit}
	}
	authorization := ContentAuthorization{ResourceProjectID: "7", ActorID: "42",
		CapabilityID: executiondomain.AgentAdhocCapability, SemanticRole: executiondomain.AgentExecutionRequestRole}
	materialize := func(loader *toolkitTokenLoader, source []byte) (*runtimev1.AgentExecutionInputV1, []byte, error) {
		materializer := newCurrentConfigurationsMaterializerForTest(t, &currentMaterializationUnsecreterStub{})
		materializer.toolkitTokens = loader
		result, err := materializer.MaterializeContent(context.Background(), authorization, source, 256*1024)
		if err != nil {
			return nil, nil, err
		}
		var input runtimev1.AgentExecutionInputV1
		if err := proto.Unmarshal(result, &input); err != nil {
			t.Fatal(err)
		}
		return &input, result, nil
	}

	// The claim's own project and actor, the entry's toolkit and resource.
	loader := &toolkitTokenLoader{want: mcpoauth.TokenBinding{ProjectID: 7, ActorID: 42, ToolkitID: 19, Resource: resource}, revision: 1}
	input, raw, err := materialize(loader, wake(entry(resource, 19, 1)))
	if err != nil {
		t.Fatal(err)
	}
	var tokens map[string]map[string]string
	if json.Unmarshal(input.GetMcpTokens(), &tokens) != nil || tokens[resource]["access_token"] != "transient-token" ||
		len(input.GetMcpTokenReferences()) != 0 || strings.Contains(string(raw), reference) {
		t.Fatalf("resolved wake input: tokens=%s references=%v", input.GetMcpTokens(), input.GetMcpTokenReferences())
	}

	// Another actor's grant (the binding does not match) and a revoked one
	// are dropped, never projected.
	for _, loader := range []*toolkitTokenLoader{
		{want: mcpoauth.TokenBinding{ProjectID: 7, ActorID: 43, ToolkitID: 19, Resource: resource}, revision: 1},
		{want: mcpoauth.TokenBinding{ProjectID: 7, ActorID: 42, ToolkitID: 19, Resource: resource}, revision: 1, revoked: true},
	} {
		input, raw, err := materialize(loader, wake(entry(resource, 19, 1)))
		if err != nil {
			t.Fatal(err)
		}
		if string(input.GetMcpTokens()) != `{}` || len(input.GetMcpTokenReferences()) != 0 ||
			strings.Contains(string(raw), "transient-token") || loader.loads != 0 {
			t.Fatalf("an unresolvable grant reached the input: %s loads=%d", input.GetMcpTokens(), loader.loads)
		}
	}

	// Malformed or duplicate entries refuse the content.
	for name, source := range map[string][]byte{
		"zero toolkit":       wake(entry(resource, 0, 1)),
		"zero revision":      wake(entry(resource, 19, 0)),
		"duplicate resource": wake(entry(resource, 19, 1), entry(resource, 19, 1)),
	} {
		if _, _, err := materialize(loader, source); !errors.Is(err, ErrContentRejected) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
