package agentexecution

import (
	"errors"
	"testing"
)

// The asserted scope of a remote caller bounds the invoked agent: every
// toolkit type of the run must be in it, and the refusal names the first
// one that is not (ADR-0044 decision 4).
func TestCheckAssertedScope(t *testing.T) {
	if err := CheckAssertedScope(nil, []string{"jira"}); err != nil {
		t.Fatalf("no assertion: %v", err)
	}
	scope := &AssertedScope{ToolkitTypes: []string{"github", "mcp"}}
	if err := CheckAssertedScope(scope, []string{"github"}); err != nil {
		t.Fatalf("inside: %v", err)
	}
	if err := CheckAssertedScope(scope, nil); err != nil {
		t.Fatalf("no toolkits: %v", err)
	}
	err := CheckAssertedScope(scope, []string{"github", "jira", "slack"})
	var exceeded *AssertedScopeError
	if !errors.As(err, &exceeded) || exceeded.ToolkitType != "jira" || !errors.Is(err, ErrAssertedScopeExceeded) {
		t.Fatalf("outside: %v", err)
	}
	if err := CheckAssertedScope(&AssertedScope{}, []string{"github"}); err == nil {
		t.Fatal("an empty scope allowed a toolkit")
	}
}
