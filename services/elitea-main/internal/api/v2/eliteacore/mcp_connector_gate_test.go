package eliteacore

import (
	"context"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/connectorapproval"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

type discoveryGrants []connectorapproval.ApprovedConnector

func (g discoveryGrants) ApprovedConnectors(context.Context, int64) ([]connectorapproval.ApprovedConnector, error) {
	return g, nil
}

func approvedOnlyDiscoveryHandler(t *testing.T, approved ...connectorapproval.ApprovedConnector) *Handler {
	t.Helper()
	gate, err := connectorapproval.NewGate(func(context.Context) (platformconfig.CatalogueSettings, error) {
		return platformconfig.CatalogueSettings{Mode: platformconfig.CatalogueApprovedOnly}, nil
	}, discoveryGrants(approved), false)
	if err != nil {
		t.Fatal(err)
	}
	return &Handler{connectors: gate}
}

// An approved pre-built entry covers the operator's server only: a caller
// URL that replaces the entry's is judged as a custom MCP URL, which needs
// its own approval (#1537 review item 9).
func TestMCPDiscoveryDecisionJudgesAPrebuiltURLOverrideAsCustomMCP(t *testing.T) {
	handler := approvedOnlyDiscoveryHandler(t, connectorapproval.ApprovedConnector{
		TargetKind: connectorapproval.TargetPrebuiltMCP, Target: "github",
	})

	catalogue, err := handler.mcpDiscoveryDecision(context.Background(), "7", "mcp_github", "https://mcp.github.example/mcp", false)
	if err != nil || catalogue.approvalRequired {
		t.Fatalf("catalogue URL: gate = %+v, err = %v; want the approved pre-built entry", catalogue, err)
	}

	override, err := handler.mcpDiscoveryDecision(context.Background(), "7", "mcp_github", "https://attacker.example/mcp", true)
	if err != nil {
		t.Fatal(err)
	}
	if !override.approvalRequired || override.kind != connectorapproval.TargetCustomMCP ||
		override.target != "https://attacker.example/mcp" {
		t.Fatalf("override: gate = %+v; want an approval-required custom MCP target", override)
	}
}

func TestPrebuiltURLOverride(t *testing.T) {
	for _, tc := range []struct {
		name, catalogue, caller string
		want                    bool
	}{
		{"no caller URL", "https://mcp.github.example/mcp", "", false},
		{"same URL", "https://mcp.github.example/mcp", "https://MCP.github.example/mcp", false},
		{"other host", "https://mcp.github.example/mcp", "https://attacker.example/mcp", true},
		{"other path", "https://mcp.github.example/mcp", "https://mcp.github.example/other", true},
		{"no catalogue URL", "", "https://mcp.github.example/mcp", true},
		{"template match", "https://mcp.example/app/{project_id}/mcp", "https://mcp.example/app/7/mcp", false},
		{"template other origin", "https://mcp.example/app/{project_id}/mcp", "https://attacker.example/app/7/mcp", true},
		{"template other prefix", "https://mcp.example/app/{project_id}/mcp", "https://mcp.example/evil/7/mcp", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := prebuiltURLOverride(tc.catalogue, tc.caller); got != tc.want {
				t.Fatalf("prebuiltURLOverride(%q, %q) = %v, want %v", tc.catalogue, tc.caller, got, tc.want)
			}
		})
	}
}
