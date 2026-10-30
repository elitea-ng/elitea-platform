package toolkitcalltool

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/connectorapproval"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

type pinGrants []connectorapproval.ApprovedConnector

func (g pinGrants) ApprovedConnectors(context.Context, int64) ([]connectorapproval.ApprovedConnector, error) {
	return g, nil
}

// #1537 review item 2: the tool-run settings of an approved custom MCP
// server carry its pins, so the worker checks the called tool's digest.
func TestResolverWritesConnectorPinsIntoTheToolRunSettings(t *testing.T) {
	target, err := connectorapproval.CanonicalMCPURL("https://example.test/")
	if err != nil {
		t.Fatal(err)
	}
	gate, err := connectorapproval.NewGate(func(context.Context) (platformconfig.CatalogueSettings, error) {
		return platformconfig.CatalogueSettings{Mode: platformconfig.CatalogueApprovedOnly}, nil
	}, pinGrants{{
		TargetKind: connectorapproval.TargetCustomMCP, Target: target,
		Tools: []connectorapproval.DeclaredTool{{Name: "list", Digest: "d1"}},
	}}, false)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := NewCurrentAuthoritativeInputResolver(authToolkitReader{}, contextSettings{t}, contextGuardrails{}, WithConnectorGate(gate))
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := resolver.Resolve(context.Background(), RunRequest{
		RequestID: "pinned", ProjectID: 7, ActorUserID: 42, ToolkitID: 19, ToolName: "list", Arguments: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Settings map[string]any `json:"settings"`
	}
	if err := json.Unmarshal(inputs.Settings, &document); err != nil {
		t.Fatal(err)
	}
	pins, ok := document.Settings[connectorapproval.PinnedToolsSetting].([]any)
	if !ok || len(pins) != 1 || pins[0].(map[string]any)["digest"] != "d1" {
		t.Fatalf("tool-run settings carry no pins: %#v", document.Settings)
	}
}
