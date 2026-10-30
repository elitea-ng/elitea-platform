package agentexecution

import (
	"context"
	"encoding/json"
	"testing"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/connectorapproval"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

const approvedMCPURL = "https://approved.example/mcp"

// settingsByURLResolver resolves a toolkit's settings the way a
// configuration reference does: `url_ref` names the URL the resolution
// supplies (or replaces), so the raw and the resolved settings differ.
type settingsByURLResolver struct{}

func (settingsByURLResolver) Resolve(_ context.Context, request configurationapp.CurrentToolkitSettingsRequest) (map[string]any, error) {
	resolved := map[string]any{}
	for key, value := range request.Settings {
		if key != "url_ref" {
			resolved[key] = value
		}
	}
	if ref, ok := request.Settings["url_ref"].(string); ok {
		resolved["url"] = ref
	}
	return resolved, nil
}

type connectorGrantsStub []connectorapproval.ApprovedConnector

func (g connectorGrantsStub) ApprovedConnectors(context.Context, int64) ([]connectorapproval.ApprovedConnector, error) {
	return g, nil
}

type runGovernanceStub struct {
	scopes     map[string]bool
	executions map[string]bool
}

func (s runGovernanceStub) GovernedContinuationScope(_ context.Context, scope string) (bool, error) {
	return s.scopes[scope], nil
}

func (s runGovernanceStub) GovernedExecution(_ context.Context, executionID string) (bool, error) {
	return s.executions[executionID], nil
}

func connectorFreezer(t *testing.T, mode platformconfig.CatalogueMode) *CurrentApplicationToolSnapshotService {
	t.Helper()
	service, err := NewCurrentApplicationToolSnapshotService(
		settingsByURLResolver{}, &currentAgentNameResolverStub{result: "toolkit"},
		currentAgentModelCatalogForTest(true), &currentAgentGuardrailStub{}, &currentProjectContextStub{}, 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := connectorapproval.NewGate(func(context.Context) (platformconfig.CatalogueSettings, error) {
		return platformconfig.CatalogueSettings{Mode: mode}, nil
	}, connectorGrantsStub{{
		TargetKind: connectorapproval.TargetCustomMCP, Target: approvedMCPURL,
		Tools: []connectorapproval.DeclaredTool{{Name: "search", Digest: "d1"}},
	}}, false)
	if err != nil {
		t.Fatal(err)
	}
	return service.WithConnectorGate(gate)
}

func freezeTools(t *testing.T, service *CurrentApplicationToolSnapshotService, request CurrentApplicationVersionFreezeRequest, tools string) []any {
	t.Helper()
	request.ProjectID, request.ActorUserID = 7, 11
	request.VersionDetails = json.RawMessage(`{"id":41,"agent_type":"agent","llm_settings":{"model_name":"model"},"tools":` + tools + `}`)
	result, err := service.FreezeCurrentApplicationVersion(context.Background(), request)
	if err != nil {
		t.Fatalf("freeze failed: %v", err)
	}
	version, err := decodeCurrentApplicationVersion(result)
	if err != nil {
		t.Fatal(err)
	}
	return version["tools"].([]any)
}

func frozenTypes(tools []any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, raw := range tools {
		tool := raw.(map[string]any)
		out[tool["type"].(string)] = tool["settings"].(map[string]any)
	}
	return out
}

const mcpAndJiraTools = `[
  {"id":1,"type":"mcp","name":"Approved","settings":{"url":"` + approvedMCPURL + `"}},
  {"id":2,"type":"jira","name":"Jira","settings":{"base_url":"https://jira.example"}}
]`

// #1537 review item 3: on an open platform, a governed run is held to
// approved_only by the command gate (SecurityContextRef != nil), so the
// freeze must decide the same: the approved custom MCP server is pinned and
// the unapproved toolkit dropped, rather than an unpinned freeze the gate
// then refuses as a whole.
func TestFreezeAGovernedContinuationOnAnOpenPlatformPinsAndDrops(t *testing.T) {
	scope := ContinuationScope("conversation", "response")
	service := connectorFreezer(t, platformconfig.CatalogueOpen).
		WithRunGovernance(runGovernanceStub{scopes: map[string]bool{scope: true}})

	governed := frozenTypes(freezeTools(t, service, CurrentApplicationVersionFreezeRequest{ContinuationScope: scope}, mcpAndJiraTools))
	if _, kept := governed["jira"]; kept {
		t.Fatal("the unapproved toolkit was kept in a governed run")
	}
	pins, ok := governed["mcp"][connectorapproval.PinnedToolsSetting].([]any)
	if !ok || len(pins) != 1 || pins[0].(map[string]any)["name"] != "search" {
		t.Fatalf("the approved custom MCP server is not pinned: %#v", governed["mcp"])
	}

	// The same freeze of a legacy run on the open platform pins nothing and
	// drops nothing.
	open := frozenTypes(freezeTools(t, service, CurrentApplicationVersionFreezeRequest{ContinuationScope: ContinuationScope("conversation", "other")}, mcpAndJiraTools))
	if _, kept := open["jira"]; !kept {
		t.Fatal("a legacy run on an open platform dropped a toolkit")
	}
	if _, pinned := open["mcp"][connectorapproval.PinnedToolsSetting]; pinned {
		t.Fatal("a legacy run on an open platform was pinned")
	}

	// A nested child of a governed parent is governed too, and so is a
	// request that says so itself.
	child := frozenTypes(freezeTools(t, connectorFreezer(t, platformconfig.CatalogueOpen).
		WithRunGovernance(runGovernanceStub{executions: map[string]bool{"parent": true}}),
		CurrentApplicationVersionFreezeRequest{ParentExecutionID: "parent"}, mcpAndJiraTools))
	if _, kept := child["jira"]; kept {
		t.Fatal("a nested child of a governed run kept an unapproved toolkit")
	}
	direct := frozenTypes(freezeTools(t, connectorFreezer(t, platformconfig.CatalogueOpen),
		CurrentApplicationVersionFreezeRequest{GovernedRun: true}, mcpAndJiraTools))
	if _, kept := direct["jira"]; kept {
		t.Fatal("a governed request kept an unapproved toolkit")
	}
}

// #1537 review item 8: the connector is judged and pinned on the RESOLVED
// settings, the URL the worker dials.
func TestFreezeJudgesTheResolvedMCPURL(t *testing.T) {
	service := connectorFreezer(t, platformconfig.CatalogueApprovedOnly)

	// A stale raw URL (the approved A) resolved to another server B: dropped.
	stale := frozenTypes(freezeTools(t, service, CurrentApplicationVersionFreezeRequest{}, `[
  {"id":1,"type":"mcp","name":"Stale","settings":{"url":"`+approvedMCPURL+`","url_ref":"https://unapproved.example/mcp"}}
]`))
	if _, kept := stale["mcp"]; kept {
		t.Fatal("a toolkit whose resolved URL is unapproved was kept on its stale raw URL")
	}

	// A URL present only after resolution: judged on it and pinned.
	resolved := frozenTypes(freezeTools(t, service, CurrentApplicationVersionFreezeRequest{}, `[
  {"id":1,"type":"mcp","name":"Resolved","settings":{"url_ref":"`+approvedMCPURL+`"}}
]`))
	settings, kept := resolved["mcp"]
	if !kept {
		t.Fatal("a toolkit whose resolved URL is approved was dropped")
	}
	if settings["url"] != approvedMCPURL {
		t.Fatalf("resolved url = %v", settings["url"])
	}
	if pins, ok := settings[connectorapproval.PinnedToolsSetting].([]any); !ok || len(pins) != 1 {
		t.Fatalf("the resolved custom MCP server is not pinned: %#v", settings)
	}
}
