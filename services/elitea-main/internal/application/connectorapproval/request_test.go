package connectorapproval

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/mcpregistry"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

func TestNormalizeDraftTargets(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		kind        TargetKind
		target      string
		wantTarget  string
		wantType    string
		wantRefused bool
	}{
		{name: "toolkit type", kind: TargetToolkitType, target: " jira ", wantTarget: "jira", wantType: "jira"},
		{name: "an MCP type is not a toolkit type", kind: TargetToolkitType, target: "mcp_github", wantRefused: true},
		{name: "the custom MCP type is not a toolkit type", kind: TargetToolkitType, target: "mcp", wantRefused: true},
		{name: "a platform-internal type needs no approval", kind: TargetToolkitType, target: "artifact", wantRefused: true},
		{name: "prebuilt", kind: TargetPrebuiltMCP, target: "github", wantTarget: "github", wantType: "mcp_github"},
		{name: "custom", kind: TargetCustomMCP, target: "HTTPS://MCP.Example.com/mcp", wantTarget: "https://mcp.example.com/mcp", wantType: "mcp"},
		{name: "custom over http", kind: TargetCustomMCP, target: "http://mcp.example.com/mcp", wantRefused: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			draft := Draft{TargetKind: testCase.kind, Target: testCase.target, ProjectIDs: []int64{1}, Justification: "why"}
			if testCase.kind == TargetCustomMCP {
				draft.Tools = []DeclaredTool{{Name: "search", InputSchema: map[string]any{"type": "object"}}}
			}
			got, toolkitType, err := NormalizeDraft(draft)
			if testCase.wantRefused {
				var invalid ValidationError
				if !errors.As(err, &invalid) {
					t.Fatalf("got %v, want a validation error", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Target != testCase.wantTarget || toolkitType != testCase.wantType {
				t.Fatalf("target %q type %q", got.Target, toolkitType)
			}
		})
	}
}

func TestNormalizeDraftToolsHostsAndScope(t *testing.T) {
	got, _, err := NormalizeDraft(Draft{
		TargetKind: TargetCustomMCP, Target: "https://mcp.example.com/mcp", ProjectIDs: []int64{3, 1, 3},
		Tools: []DeclaredTool{
			{Name: "search", Description: "Search.", InputSchema: map[string]any{"type": "object"}, Digest: "forged"},
			{Name: "send", Category: CategoryExternalSend, InputSchema: map[string]any{}},
		},
		EgressHosts: []string{"API.example.com", "api.example.com"}, DataClasses: []string{"PII", "pii", ""},
		Justification: "  docs  ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.ProjectIDs, []int64{1, 3}) {
		t.Fatalf("projects %v", got.ProjectIDs)
	}
	if got.Tools[0].Category != CategoryWrite {
		t.Fatalf("an undeclared category is %q, want write (ADR-0041)", got.Tools[0].Category)
	}
	want, _ := ToolDigest("search", "Search.", map[string]any{"type": "object"})
	if got.Tools[0].Digest != want {
		t.Fatal("the client's digest was kept instead of the server's")
	}
	if len(got.EgressHosts) != 1 || !slices.Equal(got.DataClasses, []string{"pii"}) || got.Justification != "docs" {
		t.Fatalf("hosts %v classes %v justification %q", got.EgressHosts, got.DataClasses, got.Justification)
	}

	for name, draft := range map[string]Draft{
		"no scope":            {TargetKind: TargetToolkitType, Target: "jira", Justification: "x"},
		"both scopes":         {TargetKind: TargetToolkitType, Target: "jira", PlatformWide: true, ProjectIDs: []int64{1}, Justification: "x"},
		"no justification":    {TargetKind: TargetToolkitType, Target: "jira", ProjectIDs: []int64{1}},
		"unknown category":    {TargetKind: TargetToolkitType, Target: "jira", ProjectIDs: []int64{1}, Justification: "x", Tools: []DeclaredTool{{Name: "a", Category: "admin"}}},
		"custom, no schema":   {TargetKind: TargetCustomMCP, Target: "https://m.example.com", ProjectIDs: []int64{1}, Justification: "x", Tools: []DeclaredTool{{Name: "a"}}},
		"custom, no tools":    {TargetKind: TargetCustomMCP, Target: "https://m.example.com", ProjectIDs: []int64{1}, Justification: "x"},
		"bad egress host":     {TargetKind: TargetToolkitType, Target: "jira", ProjectIDs: []int64{1}, Justification: "x", EgressHosts: []string{"https://a b"}},
		"duplicate tool":      {TargetKind: TargetToolkitType, Target: "jira", ProjectIDs: []int64{1}, Justification: "x", Tools: []DeclaredTool{{Name: "a"}, {Name: "A"}}},
		"unknown target kind": {TargetKind: "server", Target: "jira", ProjectIDs: []int64{1}, Justification: "x"},
	} {
		if _, _, err := NormalizeDraft(draft); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

type staticGrants []ApprovedConnector

func (g staticGrants) ApprovedConnectors(context.Context, int64) ([]ApprovedConnector, error) {
	return g, nil
}

func settingsOf(mode platformconfig.CatalogueMode) SettingsReader {
	return func(context.Context) (platformconfig.CatalogueSettings, error) {
		return platformconfig.CatalogueSettings{Mode: mode}, nil
	}
}

func TestGateDecisions(t *testing.T) {
	search, _ := ToolDigest("search", "", map[string]any{"type": "object"})
	grants := staticGrants{
		{TargetKind: TargetToolkitType, Target: "jira"},
		{TargetKind: TargetPrebuiltMCP, Target: "github"},
		{TargetKind: TargetCustomMCP, Target: "https://mcp.example.com/mcp", Tools: []DeclaredTool{{Name: "search", Digest: search}}},
	}
	gate, err := NewGate(settingsOf(platformconfig.CatalogueApprovedOnly), grants, false)
	if err != nil {
		t.Fatal(err)
	}
	view, err := gate.ForProject(t.Context(), 1, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		toolkitType string
		url         string
		allowed     bool
	}{
		{"jira", "", true},
		{"confluence", "", false},
		{"mcp_github", "", true},
		{"mcp_slack", "", false},
		{"mcp", "https://MCP.example.com/mcp", true},
		{"mcp", "https://other.example.com/mcp", false},
		{"mcp", "not a url", false},
		{"artifact", "", true},
	} {
		decision := view.Decide(testCase.toolkitType, map[string]any{"url": testCase.url})
		if decision.Allowed != testCase.allowed {
			t.Errorf("%s %s: allowed=%v", testCase.toolkitType, testCase.url, decision.Allowed)
		}
	}
	if !view.AllowsType("mcp") || view.AllowsType("mcp_slack") {
		t.Fatal("the type catalogue answer is wrong")
	}

	kept, hidden := FilterTools(view.Decide("mcp", map[string]any{"url": "https://mcp.example.com/mcp"}).Pins, []mcpregistry.Tool{
		{Name: "search", InputSchema: map[string]any{"type": "object"}},
		{Name: "search", Description: "changed", InputSchema: map[string]any{"type": "object"}},
		{Name: "drop", InputSchema: map[string]any{"type": "object"}},
	})
	if len(kept) != 1 || !slices.Equal(hidden, []string{"search", "drop"}) {
		t.Fatalf("kept %v hidden %v", kept, hidden)
	}

	open, _ := NewGate(settingsOf(platformconfig.CatalogueOpen), staticGrants{}, false)
	if view, _ := open.ForProject(t.Context(), 1, false); view.ApprovedOnly() {
		t.Fatal("open mode gated")
	}
	if view, _ := open.ForProject(t.Context(), 1, true); !view.ApprovedOnly() {
		t.Fatal("a governed run is not held to approved_only")
	}
	governed, _ := NewGate(settingsOf(platformconfig.CatalogueOpen), staticGrants{}, true)
	if view, _ := governed.ForProject(t.Context(), 1, false); !view.ApprovedOnly() {
		t.Fatal("the governed profile does not force approved_only")
	}
	failing, _ := NewGate(func(context.Context) (platformconfig.CatalogueSettings, error) {
		return platformconfig.DefaultCatalogueSettings(), errors.New("down")
	}, staticGrants{}, false)
	if view, err := failing.ForProject(t.Context(), 1, false); err == nil || !view.ApprovedOnly() {
		t.Fatal("an unreadable setting read as open")
	}
}

func TestCatalogueSettingsNarrowOnBadValues(t *testing.T) {
	for _, testCase := range []struct {
		values platformconfig.Values
		mode   platformconfig.CatalogueMode
		attest bool
	}{
		{platformconfig.Values{}, platformconfig.CatalogueOpen, false},
		{platformconfig.Values{"mode": "open"}, platformconfig.CatalogueOpen, false},
		{platformconfig.Values{"mode": "approved_only"}, platformconfig.CatalogueApprovedOnly, false},
		{platformconfig.Values{"mode": "opne"}, platformconfig.CatalogueApprovedOnly, false},
		{platformconfig.Values{"require_architecture_attestation": "yes"}, platformconfig.CatalogueOpen, true},
	} {
		got := platformconfig.CatalogueSettingsFromValues(testCase.values)
		if got.Mode != testCase.mode || got.RequireArchitectureAttestation != testCase.attest {
			t.Errorf("%v: %+v", testCase.values, got)
		}
	}
}
