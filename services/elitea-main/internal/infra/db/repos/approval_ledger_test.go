package repos

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

func TestApprovalCallKeepsTheWorkerDigestAndNeverDigestsMaskedArguments(t *testing.T) {
	root := map[string]any{
		"tool_name": "create_issue", "toolkit_name": "jira", "guardrail_type": "sensitive_tool",
		"tool_args": map[string]any{"summary": "Disk full", "token": "***"},
	}
	var card map[string]any
	if err := json.Unmarshal([]byte(`{"display":{"tool_name":"create_issue","toolkit_name":"jira",
		"tool_args_json":"{\"summary\":\"Disk full\",\"token\":\"***\"}"}}`), &card); err != nil {
		t.Fatal(err)
	}
	// Without the worker's digest (an earlier worker, or a 0157 card) the
	// masked arguments the person sees are not digested: no digest.
	fromRoot, fromCard := approvalCallOf(root), approvalCallOf(card)
	if fromRoot.ArgumentDigest != "" || fromCard.ArgumentDigest != "" {
		t.Fatalf("root digest %q, card digest %q: masked arguments must not be digested",
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
		"argument_digest": "sha256:" + strings.Repeat("a", 64), "tool_args": map[string]any{"x": "***"},
	})
	if worker.ArgumentDigest != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("the worker's digest is kept: %q", worker.ArgumentDigest)
	}
	if bad := approvalCallOf(map[string]any{"argument_digest": "md5:abc"}); bad.ArgumentDigest != "" {
		t.Fatalf("a malformed digest is dropped: %q", bad.ArgumentDigest)
	}
}

// TestAuditTextDigestsWhatTheAuditValidatorRefuses: a tool, toolkit or
// interrupt name that the security audit validator refuses (a JWT-shaped or
// bearer-shaped value, a control character, an over-long value) is its
// digest, so the approval event validates and the transaction commits.
func TestAuditTextDigestsWhatTheAuditValidatorRefuses(t *testing.T) {
	jwt := "tool_eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJl"
	for _, value := range []string{jwt, "Bearer abc", "line\nbreak", "c1\u0085control", strings.Repeat("x", 257)} {
		got := auditText(value)
		if !argumentDigestShape.MatchString(got) {
			t.Fatalf("auditText(%q) = %q, want a digest", value, got)
		}
		if !securityaudit.AcceptsString(got) {
			t.Fatalf("the digest of %q is refused", value)
		}
	}
	for _, value := range []string{"", "create_issue", "pipeline-static:sha256:" + strings.Repeat("a", 64)} {
		if got := auditText(value); got != value {
			t.Fatalf("auditText(%q) = %q, want it unchanged", value, got)
		}
	}
	event := securityaudit.SecurityEvent{
		ID: "0190a5b4-0000-7000-8000-000000000001", Type: securityaudit.TypeAgentApproval, Source: "elitea-main",
		Time: time.Unix(1, 0), Action: "agent.approval.paused", ToolName: auditText(jwt),
		InvocationID: auditText(jwt), ToolkitName: auditText("Bearer x"),
	}
	if err := event.Validate(); err != nil {
		t.Fatalf("an event with digested names is refused: %v", err)
	}
	event.ToolName = jwt
	if event.Validate() == nil {
		t.Fatal("the validator accepts a JWT-shaped tool name; the test no longer proves the guard")
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
