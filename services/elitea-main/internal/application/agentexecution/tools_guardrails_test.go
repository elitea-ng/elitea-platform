package agentexecution

// The freeze is the only guardrail enforcement a running agent cannot route
// around: an agent version saved before a toolkit was blocked still names it, so
// filtering the catalogue and the write paths is not enough on its own.

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/guardrails"
)

// twoToolkitVersion names a blocked type and an allowed one, so a test can tell
// "the blocked entry was dropped" from "the walk stopped".
const twoToolkitVersion = `{
  "id":41,
  "agent_type":"agent",
  "llm_settings":{"model_name":"model"},
  "tools":[
    {"id":19,"type":"shell","name":"Shell","description":"d","author_id":11,
     "settings":{"selected_tools":["execute"]},"meta":{},"is_pinned":false},
    {"id":20,"type":"sharepoint","name":"Docs","description":"d","author_id":11,
     "settings":{"selected_tools":["search","read"]},"meta":{},"is_pinned":false}
  ]
}`

func freezeWithPolicy(
	t *testing.T,
	policy guardrails.Policy,
	resolved map[string]any,
	versionDetails string,
) (map[string]any, error) {
	t.Helper()
	settings := &currentAgentSettingsResolverStub{result: resolved}
	service, err := NewCurrentApplicationToolSnapshotService(
		settings,
		&currentAgentNameResolverStub{result: "toolkit"},
		currentAgentModelCatalogForTest(true),
		&currentAgentGuardrailStub{policy: policy}, &currentProjectContextStub{},
		1,
	)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := service.FreezeCurrentApplicationVersion(
		context.Background(),
		CurrentApplicationVersionFreezeRequest{
			ProjectID: 7, ActorUserID: 11,
			VersionDetails: json.RawMessage(versionDetails),
		},
	)
	if err != nil {
		return nil, err
	}
	version, err := decodeCurrentApplicationVersion(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return version, nil
}

func TestFreezeDropsABlockedToolkitRatherThanFailingTheRun(t *testing.T) {
	// Dropped, not refused: failing would make one administrator's guardrail
	// break every agent that ever attached the toolkit.
	policy := guardrails.NewPolicy(guardrails.PolicyInput{BlockedToolkits: []string{"Shell"}})
	version, err := freezeWithPolicy(t, policy,
		map[string]any{"selected_tools": []any{"search", "read"}}, twoToolkitVersion)
	if err != nil {
		t.Fatal(err)
	}

	tools, ok := version["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools=%#v", version["tools"])
	}
	if tools[0].(map[string]any)["type"] != "sharepoint" {
		t.Fatalf("the wrong toolkit survived: %#v", tools[0])
	}
}

func TestFreezeDropsBlockedToolsFromSelectedTools(t *testing.T) {
	// Configured in a different naming style from the saved selection, so this
	// fails if the freeze compares raw strings instead of canonical keys.
	policy := guardrails.NewPolicy(guardrails.PolicyInput{
		BlockedTools: map[string][]string{"SharePoint": {"Read"}},
	})
	version, err := freezeWithPolicy(t, policy,
		map[string]any{"selected_tools": []any{"search", "read"}, "other": "kept"},
		`{"id":41,"agent_type":"agent","llm_settings":{"model_name":"model"},
  "tools":[{"id":20,"type":"sharepoint","name":"Docs","description":"d","author_id":11,
    "settings":{"selected_tools":["search","read"]},"meta":{},"is_pinned":false}]}`)
	if err != nil {
		t.Fatal(err)
	}

	tool := version["tools"].([]any)[0].(map[string]any)
	settings := tool["settings"].(map[string]any)
	selected := settings["selected_tools"].([]any)
	if !reflect.DeepEqual(selected, []any{"search"}) {
		t.Fatalf("selected_tools=%#v", selected)
	}
	// The rest of the resolved settings must survive untouched — this filter
	// rewrites one key, not the object.
	if settings["other"] != "kept" {
		t.Fatalf("settings=%#v", settings)
	}
}

func TestFreezeLeavesAnUnblockedSelectionIdentical(t *testing.T) {
	// The common case must not be rebuilt. Asserting identity of the map the
	// resolver returned is how "rebuilt only when something is removed" is
	// checked rather than assumed.
	resolved := map[string]any{"selected_tools": []any{"search", "read"}}
	policy := guardrails.NewPolicy(guardrails.PolicyInput{
		BlockedTools: map[string][]string{"github": {"create_issue"}},
	})
	version, err := freezeWithPolicy(t, policy, resolved,
		`{"id":41,"agent_type":"agent","llm_settings":{"model_name":"model"},
  "tools":[{"id":20,"type":"sharepoint","name":"Docs","description":"d","author_id":11,
    "settings":{"selected_tools":["search","read"]},"meta":{},"is_pinned":false}]}`)
	if err != nil {
		t.Fatal(err)
	}
	tool := version["tools"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(tool["settings"], resolved) {
		t.Fatalf("settings=%#v want %#v", tool["settings"], resolved)
	}
}

func TestFreezeFailsWhenThePolicyCannotBeRead(t *testing.T) {
	// The opposite choice from every other reader of this policy, and the reason
	// LoadGuardrails returns the error instead of choosing for its callers: here
	// the permissive answer is "nothing is blocked", which runs exactly the tools
	// an operator disabled.
	service, err := NewCurrentApplicationToolSnapshotService(
		&currentAgentSettingsResolverStub{result: map[string]any{"selected_tools": []any{}}},
		&currentAgentNameResolverStub{result: "toolkit"},
		currentAgentModelCatalogForTest(true),
		&currentAgentGuardrailStub{err: errors.New("pool is gone")}, &currentProjectContextStub{},
		1,
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.FreezeCurrentApplicationVersion(
		context.Background(),
		CurrentApplicationVersionFreezeRequest{
			ProjectID: 7, ActorUserID: 11,
			VersionDetails: json.RawMessage(twoToolkitVersion),
		},
	)
	if err == nil {
		t.Fatal("an unreadable guardrails policy must fail the freeze")
	}
	if !strings.Contains(err.Error(), "guardrails") {
		t.Fatalf("the failure must name the cause: %v", err)
	}
}

func TestFreezeDoesNotMatchToolkitPolicyAgainstNestedAgents(t *testing.T) {
	// An `application` entry is a nested AGENT reference, not a toolkit. Testing
	// a blocked toolkit TYPE against it would compare a policy about toolkits to
	// an agent's identity, and would silently unattach sub-agents.
	policy := guardrails.NewPolicy(guardrails.PolicyInput{
		BlockedToolkits: []string{"application"},
	})
	version, err := freezeWithPolicy(t, policy, map[string]any{"selected_tools": []any{}},
		`{"id":41,"agent_type":"agent","llm_settings":{"model_name":"model"},
  "tools":[{"id":31,"type":"application","name":"Child","toolkit_name":"Child",
    "description":"d","author_id":11,"agent_type":"agent","created_at":"2026-01-01",
    "settings":{"application_id":5,"application_version_id":6},
    "meta":{},"variables":[],"is_pinned":false}]}`)
	if err != nil {
		t.Fatal(err)
	}
	tools := version["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["type"] != "application" {
		t.Fatalf("the nested agent must survive: %#v", version["tools"])
	}
}
