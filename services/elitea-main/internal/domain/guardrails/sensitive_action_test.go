package guardrails

import "testing"

func TestSensitiveActionRendersTheSDKCopy(t *testing.T) {
	policy := NewPolicy(PolicyInput{SensitiveTools: map[string][]string{"github": {"create_file"}}})
	action, ok := policy.SensitiveAction("create_file", "my_gh", "github", "github", "my_gh")
	if !ok {
		t.Fatal("create_file under github is sensitive")
	}
	if action.ActionLabel != "my_gh.create_file" ||
		action.PolicyMessage != "Your organization requires approval before running the sensitive action 'my_gh.create_file'." ||
		action.MatchedIdentifier != "github" {
		t.Fatalf("action = %+v", action)
	}
	if _, ok := policy.SensitiveAction("read_file", "my_gh", "github", "github"); ok {
		t.Fatal("read_file is not sensitive")
	}

	custom := NewPolicy(PolicyInput{
		SensitiveTools:  map[string][]string{"*": {"delete_file"}},
		CompanyName:     "Acme",
		MessageTemplate: "{company_name}: confirm {tool_name} on {toolkit_name}",
	})
	action, ok = custom.SensitiveAction("delete_file", "", "", "artifact")
	if !ok || action.PolicyMessage != "Acme: confirm delete_file on *" || action.MatchedIdentifier != "*" {
		t.Fatalf("wildcard action = %+v, %v", action, ok)
	}
}

// {toolkit_type} is the toolkit's type, not its instance label.
func TestSensitiveActionFillsTheToolkitTypeWithTheType(t *testing.T) {
	policy := NewPolicy(PolicyInput{
		SensitiveTools:  map[string][]string{"github": {"create_file"}},
		MessageTemplate: "{toolkit_label} ({toolkit_type}): {action_name}",
	})
	action, ok := policy.SensitiveAction("create_file", "my_gh", "github", "github", "my_gh")
	if !ok || action.PolicyMessage != "my_gh (github): my_gh.create_file" {
		t.Fatalf("action = %+v, %v", action, ok)
	}
}
