package connectorapproval

import (
	"strings"
	"testing"
)

// #1537 review item 5: SanitizeInventoryDraft keeps what can be requested,
// names what it dropped, and leaves a draft NormalizeInventoryDraft accepts.
func TestSanitizeInventoryDraftKeepsTheValidPartAndNamesTheRest(t *testing.T) {
	long := strings.Repeat("x", MaxToolDescription+10)
	draft := Draft{
		TargetKind: TargetToolkitType, Target: "github", ProjectIDs: []int64{1},
		Tools: []DeclaredTool{
			{Name: "get me"},
			{Name: "list_repos", Description: long},
			{Name: "LIST_REPOS"},
		},
		EgressHosts:   []string{"api.github.com", "not a host!"},
		Justification: "Created from the connectors in use.",
	}
	if _, _, err := NormalizeInventoryDraft(draft); err == nil {
		t.Fatal("the raw draft should not normalise")
	}
	sanitized := SanitizeInventoryDraft(draft)
	normalized, _, err := NormalizeInventoryDraft(sanitized)
	if err != nil {
		t.Fatalf("the sanitised draft does not normalise: %v", err)
	}
	if len(normalized.Tools) != 1 || normalized.Tools[0].Name != "list_repos" ||
		len(normalized.Tools[0].Description) != MaxToolDescription {
		t.Fatalf("tools %+v", normalized.Tools)
	}
	if len(normalized.EgressHosts) != 1 || normalized.EgressHosts[0] != "api.github.com" {
		t.Fatalf("hosts %v", normalized.EgressHosts)
	}
	for _, note := range []string{`dropped the tool "get me"`, `truncated the description of the tool "list_repos"`, `dropped the host "not a host!"`} {
		if !strings.Contains(normalized.Justification, note) {
			t.Fatalf("justification %q does not name %q", normalized.Justification, note)
		}
	}

	// A custom MCP server's over-long description is not truncated (it is
	// part of the digest): the tool is dropped.
	mcp := SanitizeInventoryDraft(Draft{
		TargetKind: TargetCustomMCP, Target: "https://docs.example/mcp", ProjectIDs: []int64{1},
		Tools: []DeclaredTool{
			{Name: "search", Description: long, InputSchema: map[string]any{"type": "object"}},
			{Name: "fetch", InputSchema: map[string]any{"type": "object"}},
			{Name: "noschema"},
		},
		Justification: "x",
	})
	if len(mcp.Tools) != 1 || mcp.Tools[0].Name != "fetch" {
		t.Fatalf("custom MCP tools %+v", mcp.Tools)
	}
	if _, _, err := NormalizeInventoryDraft(mcp); err != nil {
		t.Fatal(err)
	}
}
