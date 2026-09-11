package toolkitcalltool

import (
	"context"
	"encoding/json"
	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	indexing "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpoauth"
	"google.golang.org/protobuf/proto"
	"strings"
	"testing"
)

type authToolkitReader struct{}

func (authToolkitReader) GetCurrentToolkit(ctx context.Context, p, a, k int32) (indexing.CurrentToolkitSnapshot, bool, error) {
	value, found, err := (contextToolkitReader{}).GetCurrentToolkit(ctx, p, a, k)
	value.Type = "mcp"
	value.Settings["url"] = "https://example.test/"
	return value, found, err
}

type referenceValidator struct {
	want     mcpoauth.TokenBinding
	revision int64
}

func (v referenceValidator) Validate(_ context.Context, reference string, binding mcpoauth.TokenBinding) (mcpoauth.TokenReference, error) {
	if binding != v.want {
		return mcpoauth.TokenReference{}, mcpoauth.ErrTokenUnavailable
	}
	return mcpoauth.TokenReference{Reference: reference, Revision: v.revision}, nil
}
func TestAuthorizationRetryBindsReferenceToSavedActorToolkit(t *testing.T) {
	binding := mcpoauth.TokenBinding{ProjectID: 7, ActorID: 42, ToolkitID: 19, Resource: "https://example.test/"}
	request := RunRequest{RequestID: "authorization-retry", ProjectID: 7, ActorUserID: 42, ToolkitID: 19, ToolName: "list", Arguments: json.RawMessage(`{}`), MCPAuthorizationReference: strings.Repeat("a", 43)}
	resolver, err := NewCurrentAuthoritativeInputResolver(authToolkitReader{}, contextSettings{t}, contextGuardrails{}, WithMCPAuthorization(referenceValidator{binding, 1}))
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := resolver.Resolve(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := (&RunService{}).idempotencyKey(request, inputs)
	request.MCPAuthorizationReference = strings.Repeat("b", 43)
	next, err := resolver.Resolve(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := (&RunService{}).idempotencyKey(request, next)
	if first == second {
		t.Fatal("fresh authorization reused refused result")
	}
	repeated, _ := (&RunService{}).idempotencyKey(request, next)
	if second != repeated {
		t.Fatal("same authorization retry changed identity")
	}
	for _, foreign := range []mcpoauth.TokenBinding{{ProjectID: 8, ActorID: 42, ToolkitID: 19, Resource: binding.Resource}, {ProjectID: 7, ActorID: 43, ToolkitID: 19, Resource: binding.Resource}, {ProjectID: 7, ActorID: 42, ToolkitID: 20, Resource: binding.Resource}, {ProjectID: 7, ActorID: 42, ToolkitID: 19, Resource: "https://foreign.test/"}} {
		resolver.tokens = referenceValidator{foreign, 1}
		if _, err := resolver.Resolve(context.Background(), request); err == nil {
			t.Fatal("foreign authorization reference admitted")
		}
	}
}
func TestDecodeAuthorizationResultMatchesSavedToolkit(t *testing.T) {
	admitted := testAdmitted()
	admitted.Binding.ToolkitType = "mcp"
	challenge := &runtimev1.ToolkitAuthorizationRequiredV1{ToolkitName: "saved", ToolkitType: "mcp", ToolkitId: "19", ServerUrl: "https://example.test/"}
	result := &runtimev1.ToolkitCallToolResultV1{ResultSummary: &runtimev1.ToolkitCallToolSummaryV1{Status: runtimev1.ToolkitCallToolStatusV1_TOOLKIT_CALL_TOOL_STATUS_V1_AUTHORIZATION_REQUIRED, ErrorMessage: executiondomain.ToolkitAuthorizationMessage, AuthorizationRequired: challenge}}
	encoded, err := proto.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := decodeSettlement(admitted, Settlement{PayloadType: PayloadTypeToolkitCallToolResult, Payload: encoded})
	if err != nil || outcome.Status != RunStatusAuthorizationRequired {
		t.Fatalf("typed challenge lost: %v", err)
	}
	challenge.ToolkitId = "20"
	encoded, _ = proto.Marshal(result)
	if _, err := decodeSettlement(admitted, Settlement{PayloadType: PayloadTypeToolkitCallToolResult, Payload: encoded}); err == nil {
		t.Fatal("foreign challenge exposed")
	}
}
