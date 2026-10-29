package workeregress

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/protobuf/proto"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// The implicit host table is pinned against the Rust clients' compiled-in
// constants: a moved endpoint fails here instead of silently refusing the
// family's calls under an allowlist.
func TestImplicitHostsMatchTheRustClients(t *testing.T) {
	families := filepath.Join("..", "..", "..", "..", "..", "libs", "rust", "agent-runtime", "src", "toolkits", "families")
	for _, pin := range []struct {
		file, constant string
		types          []string
	}{
		{"azure/client.rs", "ARM_ORIGIN", []string{"azure"}},
		{"azure/client.rs", "TOKEN_ORIGIN", []string{"azure"}},
		{"bigquery/client.rs", "API_ORIGIN", []string{"bigquery"}},
		{"gcp/client.rs", "TOKEN_URL", []string{"gcp", "bigquery"}},
		{"figma/client.rs", "API_ROOT", []string{"figma"}},
		{"google_places/client.rs", "GOOGLE_MAPS_ORIGIN", []string{"google_places"}},
		{"google_places/client.rs", "GOOGLE_PLACES_ORIGIN", []string{"google_places"}},
		{"sharepoint/client.rs", "GRAPH_BASE", []string{"sharepoint"}},
		{"slack/client.rs", "SLACK_API_ORIGIN", []string{"slack"}},
		{"xray_cloud/config.rs", "DEFAULT_BASE_URL", []string{"xray_cloud"}},
		{"zephyr_essential/config.rs", "DEFAULT_BASE_URL", []string{"zephyr_essential"}},
		{"zephyr_scale/config.rs", "CLOUD_API", []string{"zephyr_scale"}},
		{"zephyr_squad/client.rs", "API_ROOT", []string{"zephyr_squad"}},
		{"web/adapters/tavily.rs", "ORIGINS", []string{"web"}},
		{"web/adapters/firecrawl.rs", "ORIGINS", []string{"web"}},
		{"web/adapters/brave.rs", "ORIGINS", []string{"web"}},
		{"web/adapters/ddg_instant.rs", "ORIGINS", []string{"web"}},
		{"web/adapters/shodan.rs", "ORIGINS", []string{"host_intel"}},
	} {
		source, err := os.ReadFile(filepath.Join(families, filepath.FromSlash(pin.file)))
		if err != nil {
			t.Fatalf("read %s: %v", pin.file, err)
		}
		pattern := regexp.MustCompile(`const ` + pin.constant + `: &(?:'static )?(?:str|\[&(?:'static )?str\]) = &?\[?"(https://[^"]+)"`)
		match := pattern.FindSubmatch(source)
		if match == nil {
			t.Fatalf("%s no longer declares %s as an https endpoint: update toolkitImplicitHosts", pin.file, pin.constant)
		}
		host, ok := hostOf(string(match[1]))
		if !ok {
			t.Fatalf("%s %s = %q has no host", pin.file, pin.constant, match[1])
		}
		for _, toolkitType := range pin.types {
			if !slices.Contains(toolkitImplicitHosts[toolkitType], host) {
				t.Errorf("%s %s calls %s, missing from toolkitImplicitHosts[%q]", pin.file, pin.constant, host, toolkitType)
			}
		}
	}
	// Every implicit host is an entry the assembly accepts.
	for toolkitType, hosts := range toolkitImplicitHosts {
		allow, err := Assemble(nil, nil, hosts)
		if err != nil || len(allow) != len(hosts) {
			t.Fatalf("%s: %v %v", toolkitType, allow, err)
		}
	}
}

// A run gets the compiled-in hosts of each toolkit type it uses, whichever
// command carries the toolkit, and the hosts a type derives from a settings
// value that is not a URL.
func TestToolkitTypeAddsItsImplicitHosts(t *testing.T) {
	hosts, err := HostsForToolkit("service_now", []byte(`{"servicenow_configuration":{"base_url":"acme"}}`))
	if err != nil || !slices.Equal(hosts, []string{"acme.service-now.com"}) {
		t.Fatalf("service_now: %v %v", hosts, err)
	}
	hosts, err = HostsForToolkit("rally", []byte(`{"rally_configuration":{"server":"rally1.rallydev.com"}}`))
	if err != nil || !slices.Equal(hosts, []string{"rally1.rallydev.com"}) {
		t.Fatalf("rally: %v %v", hosts, err)
	}
	hosts, err = HostsFromTypedToolkit([]byte(`{"type":"sharepoint","settings":{"site_url":"https://contoso.sharepoint.com/sites/a"}}`))
	slices.Sort(hosts)
	if err != nil || !slices.Equal(hosts, []string{"contoso.sharepoint.com", "graph.microsoft.com"}) {
		t.Fatalf("index ingest: %v %v", hosts, err)
	}

	call := &runtimev1.WorkerCommandV1{
		ResourceProjectId: "7",
		InputBundleRef:    &runtimev1.ExecutionInputBundleReferenceV1{InputBundleId: "bundle-1"},
		CapabilityCommand: &runtimev1.WorkerCommandV1_ToolkitCallTool{
			ToolkitCallTool: &runtimev1.ToolkitCallToolCommandV1{ToolkitType: "figma", SettingsEntryId: "settings"},
		},
	}
	inputs := &inputStub{entries: map[string][]byte{
		"bundle-1/settings": []byte(`{"type":"figma","settings":{"token":"secret-ref://x"}}`),
		"bundle-1/agent":    mustAgentInput(t, `[{"type":"azure","settings":{}},{"type":"gcp"}]`),
	}}
	resolver, _ := newResolver(t, platformconfig.WorkerEgressAllowlist, nil, nil, inputs)
	policy, err := resolver.WorkerEgressPolicy(context.Background(), call)
	if err != nil || !slices.Equal(policy.GetAllow(), []string{"api.figma.com"}) {
		t.Fatalf("tool call: %v %v", policy, err)
	}
	agent := agentCommand(false)
	agent.GetAgentExecution().RequestEntryId = "agent"
	policy, err = resolver.WorkerEgressPolicy(context.Background(), agent)
	want := []string{"*.googleapis.com", "googleapis.com", "login.microsoftonline.com", "management.azure.com", "oauth2.googleapis.com"}
	if err != nil || !slices.Equal(policy.GetAllow(), want) {
		t.Fatalf("agent: %v %v, want %v", policy, err, want)
	}
}

// A configuration validation opens no connection: it carries the mode with
// no entries, reads none of the policy's sources and is never refused, even
// when an administrator's entry would refuse every other command.
func TestConfigurationValidationCarriesAnEmptyAllowlist(t *testing.T) {
	validation := &runtimev1.WorkerCommandV1{
		CapabilityCommand: &runtimev1.WorkerCommandV1_ConfigurationValidation{
			ConfigurationValidation: &runtimev1.ConfigurationValidationCommandV1{SettingsEntryId: "settings"},
		},
	}
	inputs := &inputStub{}
	resolver, projects := newResolver(t, platformconfig.WorkerEgressAllowlist, []string{"https://not-an-entry"}, nil, inputs)
	policy, err := resolver.WorkerEgressPolicy(context.Background(), validation)
	if err != nil || policy.GetMode() != runtimev1.WorkerEgressModeV1_WORKER_EGRESS_MODE_V1_ALLOWLIST || policy.GetAllow() == nil || len(policy.GetAllow()) != 0 {
		t.Fatalf("allowlist: %v %v", policy, err)
	}
	if projects.calls != 0 || inputs.calls != 0 {
		t.Fatalf("validation read the project (%d) or the input (%d)", projects.calls, inputs.calls)
	}
	validation.SecurityContextRef = &runtimev1.ExecutionSecurityContextReferenceV1{}
	open, _ := newResolver(t, platformconfig.WorkerEgressOpen, nil, nil, inputs)
	if policy, err := open.WorkerEgressPolicy(context.Background(), validation); err != nil || len(policy.GetAllow()) != 0 ||
		policy.GetMode() != runtimev1.WorkerEgressModeV1_WORKER_EGRESS_MODE_V1_ALLOWLIST {
		t.Fatalf("governed: %v %v", policy, err)
	}
	validation.SecurityContextRef = nil
	if policy, err := open.WorkerEgressPolicy(context.Background(), validation); err != nil || policy != nil {
		t.Fatalf("open: %v %v", policy, err)
	}
}

func mustAgentInput(t *testing.T, tools string) []byte {
	t.Helper()
	content, err := proto.Marshal(&runtimev1.AgentExecutionInputV1{Tools: []byte(tools)})
	if err != nil {
		t.Fatal(err)
	}
	return content
}
