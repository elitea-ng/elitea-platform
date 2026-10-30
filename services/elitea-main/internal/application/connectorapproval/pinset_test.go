package connectorapproval

import (
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/mcpregistry"
)

// A pin's category says how the approval policy treats the tool, not which
// tool was approved: a pin with a category still matches its tool.
func TestPinWithACategoryStillMatchesItsTool(t *testing.T) {
	tool := mcpregistry.Tool{Name: "search", Description: "Search.", InputSchema: map[string]any{"type": "object"}}
	digest, err := ToolDigest(tool.Name, tool.Description, tool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	pins := []PinnedTool{{Name: "search", Digest: digest, Category: CategoryExternalSend}}
	kept, hidden := FilterTools(pins, []mcpregistry.Tool{tool})
	if len(kept) != 1 || len(hidden) != 0 {
		t.Fatalf("kept %v hidden %v", kept, hidden)
	}
	if !NewPinSet(pins).Has("search", digest) || NewPinSet(pins).Has("search", "other") {
		t.Fatal("pin set must match by name and digest only")
	}
	if !frozenPinsWithin([]any{map[string]any{"name": "search", "digest": digest, "category": "external_send"}}, pins) {
		t.Fatal("a frozen pin with a category is refused")
	}
}
