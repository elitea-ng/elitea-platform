package connectorapproval

import (
	"reflect"
	"testing"
)

// A pin carries the category its approved request declared (ADR-0041
// decision 5), so the worker's approval policy can read it. A pin without
// one writes no key: the worker then treats the tool as write.
func TestPinsCarryTheDeclaredCategory(t *testing.T) {
	t.Parallel()
	digest := "0000000000000000000000000000000000000000000000000000000000000000"
	grants := BuildGrants([]ApprovedConnector{{
		TargetKind: TargetCustomMCP,
		Target:     "https://mcp.example.com/",
		Tools: []DeclaredTool{
			{Name: "send", Category: CategoryExternalSend, Digest: digest},
			{Name: "lookup", Digest: digest},
		},
	}})
	pins := grants.CustomMCP["https://mcp.example.com/"]
	want := []PinnedTool{
		{Name: "lookup", Digest: digest},
		{Name: "send", Digest: digest, Category: CategoryExternalSend},
	}
	if !reflect.DeepEqual(pins, want) {
		t.Fatalf("pins = %+v", pins)
	}
	setting := PinsSetting(pins)
	if _, has := setting[0].(map[string]any)["category"]; has {
		t.Fatal("a pin without a category writes the key")
	}
	if setting[1].(map[string]any)["category"] != "external_send" {
		t.Fatalf("setting = %v", setting)
	}
}
