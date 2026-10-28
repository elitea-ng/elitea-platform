package mcp

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
)

// An MCP tool call the kill switch refused (ADR-0041 decision 1) is a tool
// error that names execution_suspended and its scope, or the unreadable
// state, instead of the generic "could not be run" of a server fault.
func TestMCPToolRunMapsSafetyRefusals(t *testing.T) {
	t.Parallel()
	tool := Tool{Name: "github_list_issues"}
	suspended := fmt.Errorf("admit: %w", &safety.SuspendedError{SuspensionID: 6, Scope: safety.ScopeToolkitType, Key: "github"})
	for _, tc := range []struct {
		err  error
		want string
	}{
		{suspended, "execution_suspended, scope toolkit_type"},
		{fmt.Errorf("admit: %w: db down", safety.ErrStateUnavailable), "suspension_state_unavailable"},
	} {
		result := toolkitRunError(tool, tc.err)
		text := resultText(t, result)
		if result["isError"] != true || !strings.Contains(text, tc.want) || !strings.Contains(text, "Nothing was executed") &&
			!strings.Contains(text, "nothing was executed") {
			t.Errorf("%v: result %v, want %q", tc.err, result, tc.want)
		}
	}
	if _, ok := suspendedResult(tool.Name, errors.New("other")); ok {
		t.Fatal("an unrelated error was answered as a refusal")
	}
}

func resultText(t *testing.T, result map[string]any) string {
	t.Helper()
	content, ok := result["content"].([]map[string]any)
	if !ok || len(content) != 1 {
		t.Fatalf("result content = %v", result["content"])
	}
	text, _ := content[0]["text"].(string)
	return text
}
