package output

import (
	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"strings"
	"testing"
)

func TestCodeNodeFailureHasActionablePublicPolicy(t *testing.T) {
	policy, ok := runtimeFailurePolicyFor(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PIPELINE_CODE_FAILED)
	if !ok || policy.Code != "PIPELINE_CODE_FAILED" || !strings.Contains(policy.SafeMessage, "Code node") || !strings.Contains(policy.SafeMessage, "Later nodes did not run") {
		t.Fatalf("Code failure policy missing: %+v", policy)
	}
}
