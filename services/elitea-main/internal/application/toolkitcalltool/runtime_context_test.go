package toolkitcalltool

import (
	"context"
	"encoding/json"
	"errors"
	configurations "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	indexing "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/guardrails"
	"testing"
)

type contextToolkitReader struct{}

func (contextToolkitReader) GetCurrentToolkit(_ context.Context, project, actor, toolkit int32) (indexing.CurrentToolkitSnapshot, bool, error) {
	if project != 7 || actor != 42 || toolkit != 19 {
		return indexing.CurrentToolkitSnapshot{}, false, errors.New("unexpected scope")
	}
	return indexing.CurrentToolkitSnapshot{ID: 19, Type: "github", Name: "saved", Settings: map[string]any{"url": "https://example.test"}}, true, nil
}

type contextSettings struct{ t *testing.T }

func (s contextSettings) Resolve(_ context.Context, request configurations.CurrentToolkitSettingsRequest) (map[string]any, error) {
	if request.Mode != configurations.CurrentToolkitSettingsReferenceMode {
		s.t.Fatal("credentials were requested before claim")
	}
	return request.Settings, nil
}

type contextGuardrails struct{ err error }

func (s contextGuardrails) ResolveCurrentAgentGuardrails(context.Context) (guardrails.Policy, error) {
	return guardrails.NewPolicy(guardrails.PolicyInput{BlockedTools: map[string][]string{"github": {"delete_file"}}, SensitiveTools: map[string][]string{"*": {"create_file"}}}), s.err
}
func TestResolverFreezesActualPolicyAndRefusesPolicyFailure(t *testing.T) {
	for _, failure := range []error{nil, errors.New("database unavailable"), context.Canceled} {
		resolver, err := NewCurrentAuthoritativeInputResolver(contextToolkitReader{}, contextSettings{t}, contextGuardrails{failure})
		if err != nil {
			t.Fatal(err)
		}
		request := RunRequest{RequestID: "context-retry", ProjectID: 7, ActorUserID: 42, ToolkitID: 19, ToolName: "list_issues", Arguments: json.RawMessage(`{"number":9007199254740993,"nested":{"empty":[]}}`)}
		inputs, err := resolver.Resolve(context.Background(), request)
		if failure != nil {
			if err == nil {
				t.Fatal("policy failure admitted unguarded inputs")
			}
			if errors.Is(failure, context.Canceled) && !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation lost")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		var value RuntimeContext
		if json.Unmarshal(inputs.RuntimeContext, &value) != nil || !validRuntimeContext(inputs.RuntimeContext) {
			t.Fatal("invalid runtime context")
		}
		if value.ToolkitSecurity.BlockedTools["github"][0] != "deletefile" || value.ToolkitSecurity.SensitiveTools["*"][0] != "createfile" {
			t.Fatal("saved guardrails lost")
		}
		if string(inputs.Arguments) != string(request.Arguments) {
			t.Fatal("arguments changed")
		}
	}
}
func TestRuntimeContextRejectsMissingPolicyAndCredentialMaterial(t *testing.T) {
	for _, raw := range []string{`{}`, `{"toolkit_security":null}`, `{"toolkit_security":{}}`, `{"toolkit_security":{"blocked_toolkits":[],"blocked_tools":{},"sensitive_tools":{}},"mcp_tokens":{"token":"secret"}}`} {
		inputs := testInputs()
		inputs.RuntimeContext = json.RawMessage(raw)
		if _, _, err := newTestFactory(t).Build(context.Background(), inputs); err == nil {
			t.Fatalf("accepted unsafe context %s", raw)
		}
	}
}
func TestContextSnapshotIsIndependentAndChangesIdempotency(t *testing.T) {
	original := testInputs()
	cloned := original.Clone()
	cloned.RuntimeContext[0] = 'x'
	if original.RuntimeContext[0] != '{' {
		t.Fatal("clone aliases runtime context")
	}
	service := &RunService{}
	request := RunRequest{RequestID: "context-retry", ProjectID: 7, ActorUserID: 42, ToolkitID: 19, ToolName: "list_issues", Arguments: json.RawMessage(`{}`)}
	before, err := service.idempotencyKey(request, original)
	if err != nil {
		t.Fatal(err)
	}
	original.RuntimeContext = json.RawMessage(`{"toolkit_security":{"blocked_toolkits":["github"],"blocked_tools":{},"sensitive_tools":{}}}`)
	after, err := service.idempotencyKey(request, original)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("changed policy reused previous tool result")
	}
}
