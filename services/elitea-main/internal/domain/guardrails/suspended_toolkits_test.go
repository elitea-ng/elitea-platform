package guardrails

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSuspendedToolkitsTravelInTheRuntimePolicyOnly(t *testing.T) {
	t.Parallel()

	base := NewPolicy(PolicyInput{BlockedToolkits: []string{"Shell"}})
	before, err := json.Marshal(base.Runtime())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(before), "suspended_toolkit_types") {
		t.Fatalf("a policy with no suspension must keep its old bytes: %s", before)
	}
	if got := base.WithSuspendedToolkits(nil); !got.ToolkitBlocked("shell") || got.ToolkitSuspended("shell") {
		t.Fatal("an empty suspension list changes nothing")
	}

	suspended := base.WithSuspendedToolkits([]string{"Git Hub", "---", "jira"})
	if !suspended.ToolkitSuspended("github") || !suspended.ToolkitSuspended("GIT-HUB") || !suspended.ToolkitSuspended("Jira") {
		t.Fatal("suspended types match with the blocked_toolkits canonical rule")
	}
	if suspended.ToolkitBlocked("github") {
		t.Fatal("a suspended type is not blocked: the catalogue and the freeze keep it")
	}
	if base.ToolkitSuspended("github") {
		t.Fatal("WithSuspendedToolkits must not change the receiver")
	}
	runtime := suspended.Runtime()
	if strings.Join(runtime.SuspendedToolkitTypes, ",") != "github,jira" {
		t.Fatalf("runtime list = %v, want sorted canonical keys", runtime.SuspendedToolkitTypes)
	}
	if NewPolicy(PolicyInput{}).WithSuspendedToolkits([]string{"x"}).Empty() {
		t.Fatal("a policy with a suspended type is not empty")
	}
}
