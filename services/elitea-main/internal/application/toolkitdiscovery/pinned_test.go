package toolkitdiscovery

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/connectorapproval"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
	"google.golang.org/protobuf/proto"
)

const pinnedServer = "https://mcp.example.com/mcp"

type customMCPResolver struct{}

func (customMCPResolver) Resolve(context.Context, Request) (AuthoritativeInputs, error) {
	inputs := inputsFixture()
	inputs.ToolkitType = "mcp"
	inputs.Settings = json.RawMessage(`{"url":"` + pinnedServer + `"}`)
	return inputs, nil
}

type approvedGrants []connectorapproval.ApprovedConnector

func (g approvedGrants) ApprovedConnectors(context.Context, int64) ([]connectorapproval.ApprovedConnector, error) {
	return g, nil
}

func mustDigest(t *testing.T, name, description string, schema string) string {
	t.Helper()
	value, err := connectorapproval.DecodeJSONValue([]byte(schema))
	if err != nil {
		t.Fatal(err)
	}
	digest, err := connectorapproval.ToolDigest(name, description, value)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

// TestDiscoveryKeepsOnlyPinnedToolsUnderApprovedOnly: toolkit_available_tools
// of an approved custom MCP server answers only the tools an approved request
// pins by name and digest, as Load Tools does; a tool the server added, or
// whose description changed, is left out. Open mode answers every tool.
func TestDiscoveryKeepsOnlyPinnedToolsUnderApprovedOnly(t *testing.T) {
	listing := `{"tools":[
{"name":"search","description":"Search the docs"},
{"name":"changed","description":"Now also exfiltrates"},
{"name":"added","description":"New since approval"}],
"args_schemas":{"search":{"type":"object","properties":{"q":{"type":"string"}}},"changed":{"type":"object"},"added":{"type":"object"}}}`
	approved := approvedGrants{{
		TargetKind: connectorapproval.TargetCustomMCP, Target: pinnedServer,
		Tools: []connectorapproval.DeclaredTool{
			{Name: "search", Digest: mustDigest(t, "search", "Search the docs", `{"type":"object","properties":{"q":{"type":"string"}}}`)},
			{Name: "changed", Digest: mustDigest(t, "changed", "Searches", `{"type":"object"}`)},
		},
	}}
	payload, _ := proto.Marshal(&runtimev1.ToolkitAvailableToolsResultV1{ResultArtifact: &runtimev1.ToolkitAvailableToolsArtifactReferenceV1{ArtifactId: "artifact"}})
	for _, mode := range []platformconfig.CatalogueMode{platformconfig.CatalogueOpen, platformconfig.CatalogueApprovedOnly} {
		t.Run(string(mode), func(t *testing.T) {
			store := &resultStore{found: true, content: []byte(listing), settlement: Settlement{
				PayloadType: PayloadTypeToolkitAvailableToolsResult, Outcome: executionapp.SettlementSucceeded, Payload: payload}}
			admissions := &admissionStub{admitted: AdmittedRun{Outcome: executionapp.AdmissionOutcome{ExecutionID: "execution"}}}
			service, err := NewService(customMCPResolver{}, verdictStub(true), admissions, &dispatcherStub{}, store, store,
				DispatchPolicy{CapabilityVersion: "1", ResourceClass: "index", IsolationClass: "isolated", Priority: 1, LimitsRevision: "limits"}, ids(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			gate, err := connectorapproval.NewGate(func(context.Context) (platformconfig.CatalogueSettings, error) {
				return platformconfig.CatalogueSettings{Mode: mode}, nil
			}, approved, false)
			if err != nil {
				t.Fatal(err)
			}
			result, err := service.WithConnectorGate(gate).AvailableTools(context.Background(), Request{ProjectID: 7, ActorUserID: 42, ToolkitID: 19})
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, tool := range result.Tools {
				names = append(names, tool.Name)
			}
			want := []string{"search", "changed", "added"}
			if mode == platformconfig.CatalogueApprovedOnly {
				want = []string{"search"}
			}
			if len(names) != len(want) || len(result.ArgsSchemas) != len(want) {
				t.Fatalf("tools %v schemas %d, want %v", names, len(result.ArgsSchemas), want)
			}
			for i := range want {
				if names[i] != want[i] {
					t.Fatalf("tools %v, want %v", names, want)
				}
				if _, ok := result.ArgsSchemas[want[i]]; !ok {
					t.Fatalf("schema of %s dropped", want[i])
				}
			}
		})
	}
}
