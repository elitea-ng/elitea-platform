package workeregress

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/protobuf/proto"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

type platformStub struct {
	settings platformconfig.WorkerEgressSettings
	err      error
}

func (p platformStub) WorkerEgressSettings(context.Context) (platformconfig.WorkerEgressSettings, error) {
	return p.settings, p.err
}

type projectStub struct {
	entries map[int64][]string
	err     error
	calls   int
}

func (p *projectStub) WorkerEgressProjectEntries(_ context.Context, projectID int64) ([]string, error) {
	p.calls++
	return p.entries[projectID], p.err
}

type inputStub struct {
	entries map[string][]byte
	calls   int
}

func (i *inputStub) InputEntries(_ context.Context, bundle string, ids []string) (map[string][]byte, error) {
	i.calls++
	out := map[string][]byte{}
	for _, id := range ids {
		if content, ok := i.entries[bundle+"/"+id]; ok {
			out[id] = content
		}
	}
	return out, nil
}

func agentCommand(governed bool) *runtimev1.WorkerCommandV1 {
	command := &runtimev1.WorkerCommandV1{
		ResourceProjectId: "7",
		InputBundleRef:    &runtimev1.ExecutionInputBundleReferenceV1{InputBundleId: "bundle-1"},
		CapabilityCommand: &runtimev1.WorkerCommandV1_AgentExecution{
			AgentExecution: &runtimev1.AgentExecutionCommandV1{RequestEntryId: "agent-request"},
		},
	}
	if governed {
		command.SecurityContextRef = &runtimev1.ExecutionSecurityContextReferenceV1{}
	}
	return command
}

func agentInput(t *testing.T) []byte {
	t.Helper()
	input := &runtimev1.AgentExecutionInputV1{
		// An ad hoc run's toolkits.
		Tools: []byte(`[{"type":"mcp","name":"docs","settings":{"url":"https://mcp.example.com/sse","headers":{}}}]`),
		// An application run's frozen version: a GitHub toolkit with its
		// expanded configuration, and a Jira toolkit pointing at the same
		// host twice.
		Application: []byte(`{"version_details":{"instructions":"see https://evil.example.net",
			"tools":[
				{"type":"github","settings":{"github_configuration":{"base_url":"https://API.GitHub.com/"}}},
				{"type":"jira","settings":{"jira_configuration":{"base_url":"jira.corp.example:8443"},"url":"https://jira.corp.example:8443"}},
				{"type":"custom","settings":{"note":"https://not-a-url-field.example","endpoint":"{template}"}}
			]}}`),
		UserInput:   []byte(`"fetch https://user-typed.example"`),
		ChatHistory: []byte(`[{"content":"https://history.example"}]`),
	}
	content, err := proto.MarshalOptions{Deterministic: true}.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func newResolver(t *testing.T, mode platformconfig.WorkerEgressMode, platform []string, project []string, inputs *inputStub) (*Resolver, *projectStub) {
	t.Helper()
	projects := &projectStub{entries: map[int64][]string{7: project}}
	resolver, err := NewResolver(platformStub{settings: platformconfig.WorkerEgressSettings{Mode: mode, Allowlist: platform}}, projects, inputs)
	if err != nil {
		t.Fatal(err)
	}
	return resolver, projects
}

// Open and not governed: no policy at all, and nothing beyond the platform
// setting is read, so a command is byte for byte what it was.
func TestOpenOmitsThePolicy(t *testing.T) {
	inputs := &inputStub{}
	resolver, projects := newResolver(t, platformconfig.WorkerEgressOpen, []string{"example.com"}, []string{"corp.example"}, inputs)
	policy, err := resolver.WorkerEgressPolicy(context.Background(), agentCommand(false))
	if err != nil || policy != nil {
		t.Fatalf("open: %v %v", policy, err)
	}
	if projects.calls != 0 || inputs.calls != 0 {
		t.Fatalf("open read the project (%d) or the input (%d)", projects.calls, inputs.calls)
	}
	before := agentCommand(false)
	after := agentCommand(false)
	after.EgressPolicy = policy
	a, _ := proto.MarshalOptions{Deterministic: true}.Marshal(before)
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(after)
	if string(a) != string(b) {
		t.Fatal("an open command's bytes changed")
	}
}

// Allowlist mode merges the platform, the project and the toolkits' hosts,
// normalised, deduplicated and sorted; nothing from user text is added.
func TestAllowlistMergesAndDedupesTheSources(t *testing.T) {
	inputs := &inputStub{entries: map[string][]byte{"bundle-1/agent-request": agentInput(t)}}
	resolver, _ := newResolver(t, platformconfig.WorkerEgressAllowlist,
		[]string{"Example.COM", "*.pypi.org"}, []string{"example.com", "api.github.com", "10.0.0.0/8"}, inputs)
	policy, err := resolver.WorkerEgressPolicy(context.Background(), agentCommand(false))
	if err != nil {
		t.Fatal(err)
	}
	if policy.GetMode() != runtimev1.WorkerEgressModeV1_WORKER_EGRESS_MODE_V1_ALLOWLIST {
		t.Fatalf("mode %v", policy.GetMode())
	}
	want := []string{"*.pypi.org", "10.0.0.0/8", "api.github.com", "example.com", "jira.corp.example", "mcp.example.com"}
	if !slices.Equal(policy.GetAllow(), want) {
		t.Fatalf("allow = %v, want %v", policy.GetAllow(), want)
	}
}

// The governed profile forces the allowlist under an open platform.
func TestGovernedForcesTheAllowlist(t *testing.T) {
	inputs := &inputStub{entries: map[string][]byte{"bundle-1/agent-request": agentInput(t)}}
	resolver, _ := newResolver(t, platformconfig.WorkerEgressOpen, nil, nil, inputs)
	policy, err := resolver.WorkerEgressPolicy(context.Background(), agentCommand(true))
	if err != nil || policy.GetMode() != runtimev1.WorkerEgressModeV1_WORKER_EGRESS_MODE_V1_ALLOWLIST {
		t.Fatalf("governed: %v %v", policy, err)
	}
	if !slices.Contains(policy.GetAllow(), "api.github.com") || len(policy.GetAllow()) != 3 {
		t.Fatalf("governed allow = %v", policy.GetAllow())
	}
}

// An allowlist run with no source at all still carries the policy: an empty
// allowlist refuses every public host rather than opening them.
func TestEmptyAllowlistIsStillAPolicy(t *testing.T) {
	command := &runtimev1.WorkerCommandV1{
		ResourceProjectId: "7",
		InputBundleRef:    &runtimev1.ExecutionInputBundleReferenceV1{InputBundleId: "bundle-1"},
		CapabilityCommand: &runtimev1.WorkerCommandV1_ToolkitCallTool{
			ToolkitCallTool: &runtimev1.ToolkitCallToolCommandV1{SettingsEntryId: "settings"},
		},
	}
	inputs := &inputStub{entries: map[string][]byte{"bundle-1/settings": []byte(`{"timeout":5}`)}}
	resolver, _ := newResolver(t, platformconfig.WorkerEgressAllowlist, nil, nil, inputs)
	policy, err := resolver.WorkerEgressPolicy(context.Background(), command)
	if err != nil || policy == nil || len(policy.GetAllow()) != 0 {
		t.Fatalf("empty: %v %v", policy, err)
	}
}

func TestMalformedOrOversizeRefuses(t *testing.T) {
	inputs := &inputStub{entries: map[string][]byte{"bundle-1/agent-request": agentInput(t)}}
	for name, platform := range map[string][]string{
		"a scheme":         {"https://example.com"},
		"a stray wildcard": {"a.*.example.com"},
		"a blank":          {""},
	} {
		resolver, _ := newResolver(t, platformconfig.WorkerEgressAllowlist, platform, nil, inputs)
		if _, err := resolver.WorkerEgressPolicy(context.Background(), agentCommand(false)); !errors.Is(err, ErrPolicyRefused) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	resolver, _ := newResolver(t, platformconfig.WorkerEgressAllowlist, nil, []string{"bad host"}, inputs)
	if _, err := resolver.WorkerEgressPolicy(context.Background(), agentCommand(false)); !errors.Is(err, ErrPolicyRefused) {
		t.Fatalf("a malformed project entry: %v", err)
	}

	many := make([]string, 0, MaxEntries+1)
	for i := range MaxEntries + 1 {
		many = append(many, fmt.Sprintf("h%d.example.com", i))
	}
	if _, err := Assemble(many, nil, nil); !errors.Is(err, ErrPolicyRefused) || !strings.Contains(err.Error(), "1025") {
		t.Fatalf("1025 entries: %v", err)
	}
	if allow, err := Assemble(many[:MaxEntries], nil, nil); err != nil || len(allow) != MaxEntries {
		t.Fatalf("1024 entries: %d %v", len(allow), err)
	}
	long := make([]string, 0, 200)
	for i := range 200 {
		long = append(long, fmt.Sprintf("%s%d.example.com", strings.Repeat("a", 60)+"."+strings.Repeat("b", 60), i))
	}
	if _, err := Assemble(long, nil, nil); !errors.Is(err, ErrPolicyRefused) {
		t.Fatalf("oversize bytes: %v", err)
	}

	unreadable := &inputStub{entries: map[string][]byte{"bundle-1/agent-request": []byte("not a proto \xff\xff")}}
	resolver, _ = newResolver(t, platformconfig.WorkerEgressAllowlist, nil, nil, unreadable)
	if _, err := resolver.WorkerEgressPolicy(context.Background(), agentCommand(false)); !errors.Is(err, ErrPolicyRefused) {
		t.Fatalf("an unreadable input: %v", err)
	}
	missing := &inputStub{}
	resolver, _ = newResolver(t, platformconfig.WorkerEgressAllowlist, nil, nil, missing)
	if _, err := resolver.WorkerEgressPolicy(context.Background(), agentCommand(false)); !errors.Is(err, ErrPolicyRefused) {
		t.Fatalf("a missing input: %v", err)
	}
	broken, err := NewResolver(platformStub{err: errors.New("down")}, &projectStub{}, &inputStub{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broken.WorkerEgressPolicy(context.Background(), agentCommand(false)); !errors.Is(err, ErrPolicyRefused) {
		t.Fatalf("an unreadable platform setting must refuse, not run open: %v", err)
	}
}

func TestHostsFromSettingsReadsOnlyURLFields(t *testing.T) {
	hosts, err := HostsFromSettings([]byte(`{
		"base_url": "https://gitlab.example.com/api/v4",
		"nested": [{"server_url": "wss://socket.example.com:9443/x"}, {"token_endpoint": "https://login.example.com/token"}],
		"smtp_host": "mail.example.com",
		"api_base": "http://[2001:db8::1]:8080",
		"database_url": "postgres://db.example.com/x",
		"description": "https://ignored.example.com",
		"url": "https://{{host}}/",
		"count": 3
	}`))
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(hosts)
	want := []string{"2001:db8::1", "gitlab.example.com", "login.example.com", "mail.example.com", "socket.example.com"}
	if !slices.Equal(hosts, want) {
		t.Fatalf("hosts = %v, want %v", hosts, want)
	}
	if _, err := HostsFromSettings([]byte(`{`)); err == nil {
		t.Fatal("unreadable settings were accepted")
	}
}
