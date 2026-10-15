package toolkitcalltool

import (
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"google.golang.org/protobuf/proto"
)

func decodeSummary(t *testing.T, summary *runtimev1.ToolkitCallToolSummaryV1) RunOutcome {
	t.Helper()
	encoded, err := proto.Marshal(&runtimev1.ToolkitCallToolResultV1{ResultSummary: summary})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := decodeSettlement(testAdmitted(), Settlement{PayloadType: PayloadTypeToolkitCallToolResult, Payload: encoded})
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

// #1292 round 2: a refusal of the tool is a TYPED status, and the round-1
// worker's UNKNOWN_TOOL + fixed sentence is read as the same status (fallback
// only); any other UNKNOWN_TOOL stays "no tool by that name".
func TestDecodeSettlementClassifiesToolRefusalsByStatus(t *testing.T) {
	const (
		unknown   = runtimev1.ToolkitCallToolStatusV1_TOOLKIT_CALL_TOOL_STATUS_V1_UNKNOWN_TOOL
		effect    = runtimev1.ToolkitCallToolStatusV1_TOOLKIT_CALL_TOOL_STATUS_V1_EFFECT_REFUSED
		policy    = runtimev1.ToolkitCallToolStatusV1_TOOLKIT_CALL_TOOL_STATUS_V1_POLICY_REFUSED
		anySimple = "anything at all"
	)
	for _, tc := range []struct {
		name    string
		status  runtimev1.ToolkitCallToolStatusV1
		message string
		want    RunStatus
	}{
		{"typed effect", effect, executiondomain.ToolkitEffectfulToolMessage, RunStatusEffectRefused},
		{"typed effect, any sentence", effect, anySimple, RunStatusEffectRefused},
		{"typed policy", policy, executiondomain.ToolkitPolicyRefusedMessage, RunStatusPolicyRefused},
		{"older worker effect", unknown, executiondomain.ToolkitEffectfulToolMessage, RunStatusEffectRefused},
		{"older worker policy", unknown, executiondomain.ToolkitPolicyRefusedMessage, RunStatusPolicyRefused},
		{"a truly unknown tool", unknown, "The toolkit operation is not selected.", RunStatusUnknownTool},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeSummary(t, &runtimev1.ToolkitCallToolSummaryV1{Status: tc.status, ErrorMessage: tc.message})
			if got.Status != tc.want {
				t.Fatalf("status %q, want %q", got.Status, tc.want)
			}
		})
	}
}

// Item 4 (rolling deploy): only the current worker's run-credentials sentence
// means the credentials; an older worker's bare AUTHORIZATION_FAILED is its
// refusal of the tool.
func TestRuntimeFailureClassificationForRollingDeploy(t *testing.T) {
	failure := func(code, message string) RunOutcome {
		return RunOutcome{Status: RunStatusRuntimeFailure, FailureCode: code, ErrorMessage: message}
	}
	credentials := failure(FailureCodeAuthorizationFailed, executiondomain.ToolkitRunCredentialsRefusedMessage)
	if !credentials.RunCredentialsRefused() || credentials.WorkerRefusedTool() {
		t.Fatal("the current worker's credentials refusal is about the credentials")
	}
	older := failure(FailureCodeAuthorizationFailed, "Execution authorization failed.")
	if older.RunCredentialsRefused() || !older.WorkerRefusedTool() {
		t.Fatal("an older worker's AUTHORIZATION_FAILED is a refusal of the tool")
	}
	if !failure(FailureCodeUnsupportedCapability, "x").WorkerRefusedTool() {
		t.Fatal("UNSUPPORTED_CAPABILITY is an older worker's effect refusal")
	}
	for _, other := range []RunOutcome{
		failure("RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE", "down"),
		{Status: RunStatusUnknownTool, FailureCode: FailureCodeAuthorizationFailed},
	} {
		if other.WorkerRefusedTool() || other.RunCredentialsRefused() {
			t.Fatalf("misclassified %+v", other)
		}
	}
}
