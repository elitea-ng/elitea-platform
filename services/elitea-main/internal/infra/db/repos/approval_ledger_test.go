package repos

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestApprovalCallDigestsTheArgumentsAndNeverKeepsThem(t *testing.T) {
	root := map[string]any{
		"tool_name": "create_issue", "toolkit_name": "jira", "guardrail_type": "sensitive_tool",
		"tool_args": map[string]any{"summary": "Disk full", "project": "OPS"},
	}
	var card map[string]any
	if err := json.Unmarshal([]byte(`{"display":{"tool_name":"create_issue","toolkit_name":"jira",
		"tool_args_json":"{\"project\":\"OPS\",\"summary\":\"Disk full\"}"}}`), &card); err != nil {
		t.Fatal(err)
	}
	fromRoot, fromCard := approvalCallOf(root), approvalCallOf(card)
	if !argumentDigestShape.MatchString(fromRoot.ArgumentDigest) || fromRoot.ArgumentDigest != fromCard.ArgumentDigest {
		t.Fatalf("root digest %q, card digest %q: the same arguments must give one digest",
			fromRoot.ArgumentDigest, fromCard.ArgumentDigest)
	}
	if fromCard.ToolName != "create_issue" || fromCard.ToolkitName != "jira" || fromRoot.GuardrailType != "sensitive_tool" {
		t.Fatalf("call = %+v", fromCard)
	}
	encoded, err := json.Marshal(fromRoot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "Disk full") {
		t.Fatalf("the call keeps the arguments: %s", encoded)
	}
	worker := approvalCallOf(map[string]any{
		"argument_digest": "sha256:" + strings.Repeat("a", 64), "tool_args": map[string]any{"x": 1},
	})
	if worker.ArgumentDigest != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("the worker's digest is preferred: %q", worker.ArgumentDigest)
	}
}

func TestApprovalOutcomesMapActionsEffectsAndRefusals(t *testing.T) {
	for action, want := range map[string]string{
		"approve": ApprovalOutcomeApproved, "reject": ApprovalOutcomeRejected,
	} {
		if got := approvalOutcomeOfAction(action); got != want {
			t.Fatalf("%s -> %s, want %s", action, got, want)
		}
	}
	if approvalEffect(ApprovalOutcomeApproved) != "allow" || approvalEffect(ApprovalOutcomeUnattendedSkipped) != "deny" ||
		approvalEffect(ApprovalOutcomePaused) != "" {
		t.Fatal("effects")
	}
	if skipRefusal([]string{"approve", "reject", "block_with_comment"}) != "block_with_comment" ||
		skipRefusal([]string{"approve", "reject"}) != "reject" || skipRefusal([]string{"approve"}) != "" {
		t.Fatal("a skip refuses with block_with_comment, else reject, and never approves")
	}
}
