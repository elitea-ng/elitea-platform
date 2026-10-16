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
		{"typed sensitive", runtimev1.ToolkitCallToolStatusV1_TOOLKIT_CALL_TOOL_STATUS_V1_SENSITIVE_REFUSED, executiondomain.ToolkitPolicyRefusedMessage, RunStatusSensitiveRefused},
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

// #1292 round 3: any AUTHORIZATION_FAILED (a Python-shaped "Execution
// authorization failed." included) is the run's credentials or context, never
// a refusal of the tool; UNSUPPORTED_CAPABILITY is "this worker cannot run
// it", never "it changes data".
func TestRuntimeFailureClassification(t *testing.T) {
	failure := func(code, message string) RunOutcome {
		return RunOutcome{Status: RunStatusRuntimeFailure, FailureCode: code, ErrorMessage: message}
	}
	for _, message := range []string{executiondomain.ToolkitRunCredentialsRefusedMessage, "Execution authorization failed."} {
		outcome := failure(FailureCodeAuthorizationFailed, message)
		if !outcome.RunCredentialsRefused() || outcome.WorkerCannotRunTool() {
			t.Fatalf("%q is the run's credentials", message)
		}
	}
	unsupported := failure(FailureCodeUnsupportedCapability, "Configuration type is not supported.")
	if !unsupported.WorkerCannotRunTool() || unsupported.RunCredentialsRefused() {
		t.Fatal("UNSUPPORTED_CAPABILITY is the worker not running the tool")
	}
	for _, other := range []RunOutcome{
		failure("RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE", "down"),
		{Status: RunStatusUnknownTool, FailureCode: FailureCodeAuthorizationFailed},
	} {
		if other.WorkerCannotRunTool() || other.RunCredentialsRefused() {
			t.Fatalf("misclassified %+v", other)
		}
	}
}
