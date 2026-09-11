package agentexecution

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Two fields where what the product STORES and what the runtime ACCEPTS had
// drifted apart, both found the same way: a turn the browser admitted, that
// then stopped with no answer, because the runtime refused the profile after
// the execution already existed.
//
// `agent_type` is reconciled here, by rewriting the stored name to the one the
// runtime matches on. `step_limit` is reconciled in the other direction — the
// runtime now admits the key (services/elitea-worker-rust/src/agents/
// assembly.rs::validate_application_meta_step_limit) and Main forwards the
// number onto the execution input — because the Python worker reads it out of
// the version and could not be left without it.
//
// Each assertion is half of a pair; the other half is a test in the runtime.
// Changing one side alone reopens the defect.

func freezeVersionForRuntimeProfileTest(t *testing.T, versionDetails string) map[string]any {
	t.Helper()
	compatible := true
	service, err := NewCurrentApplicationToolSnapshotService(
		&currentAgentSettingsResolverStub{}, &currentAgentNameResolverStub{},
		currentAgentModelCatalogForTest(compatible), &currentAgentGuardrailStub{}, 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.FreezeCurrentApplicationVersion(
		context.Background(), CurrentApplicationVersionFreezeRequest{
			ProjectID: 7, ActorUserID: 11,
			VersionDetails: json.RawMessage(versionDetails),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	version, err := decodeCurrentApplicationVersion(result)
	if err != nil {
		t.Fatal(err)
	}
	return version
}

// TestFreezePreservesAuthoredStepLimit pins that the freeze does NOT remove the
// key. It is tempting to strip it — the runtime used to refuse a version meta
// that carried it at all — but the Python worker reads it from exactly there to
// set its LangGraph recursion limit
// (services/elitea-worker-python/src/elitea_worker/agents/sdk_adapter.py:910-912),
// and both workers have to keep honouring the same authored number. The runtime
// now admits the key and takes the effective limit from the execution input,
// which currentApplicationStepsLimit fills from this same value.
func TestFreezePreservesAuthoredStepLimit(t *testing.T) {
	version := freezeVersionForRuntimeProfileTest(t, `{
		"llm_settings":{"model_name":"model","model_project_id":7},
		"meta":{"step_limit":50,"internal_tools":["ask_user"],"variables":{}},
		"tools":[]
	}`)

	meta, ok := version["meta"].(map[string]any)
	if !ok {
		t.Fatalf("meta is %T, want an object", version["meta"])
	}
	limit, valid := currentAgentJSONInteger(meta["step_limit"])
	if !valid || limit != 50 {
		t.Fatalf("step_limit=%v, want the authored 50", meta["step_limit"])
	}
	tools, ok := meta["internal_tools"].([]any)
	if !ok || len(tools) != 1 || tools[0] != "ask_user" {
		t.Fatalf("internal_tools=%v, want the authored [\"ask_user\"]", meta["internal_tools"])
	}
}

// TestCurrentApplicationStepsLimitReadsTheAuthoredValue covers the other half:
// the number has to reach the execution input, because that is the only place
// the runtime reads it (services/elitea-worker-rust/src/agents/assembly.rs:152).
// Before this the application path never set the field, so a stored agent ran
// on the runtime default no matter what its author chose.
func TestCurrentApplicationStepsLimitReadsTheAuthoredValue(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    *int32
		wantErr bool
	}{
		{name: "authored", version: `{"meta":{"step_limit":50}}`, want: int32Pointer(50)},
		{name: "no meta at all", version: `{}`},
		{name: "meta without the key", version: `{"meta":{"internal_tools":[]}}`},
		{name: "explicit null", version: `{"meta":{"step_limit":null}}`},
		{name: "zero", version: `{"meta":{"step_limit":0}}`, wantErr: true},
		{name: "negative", version: `{"meta":{"step_limit":-5}}`, wantErr: true},
		{name: "above the runtime ceiling", version: `{"meta":{"step_limit":1025}}`, wantErr: true},
		{name: "not a number", version: `{"meta":{"step_limit":"many"}}`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			limit, err := currentApplicationStepsLimit(decodedCurrentApplicationVersionForTest(t, test.version))
			if test.wantErr {
				if !errors.Is(err, ErrUnsupportedCurrentAgentStart) {
					t.Fatalf("err=%v, want ErrUnsupportedCurrentAgentStart", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case test.want == nil && limit != nil:
				t.Fatalf("steps limit=%d, want none", *limit)
			case test.want != nil && (limit == nil || *limit != *test.want):
				t.Fatalf("steps limit=%v, want %d", limit, *test.want)
			}
		})
	}
}

func int32Pointer(value int32) *int32 { return &value }

// TestFreezeRewritesStoredDirectAgentType covers the second half of the pair.
// The write validator admits only openai/react/dial/pipeline
// (internal/api/v2/eliteacore/handler.go:2378) and defaults an empty value to
// "openai" (:2447), so no caller can author the "agent" spelling the runtime
// matches on. `pipeline` is named identically on both sides and must survive
// untouched — rewriting it would turn every pipeline into a direct agent.
func TestFreezeRewritesStoredDirectAgentType(t *testing.T) {
	tests := []struct {
		name    string
		stored  string
		want    string
		toolWas string
		toolNow string
	}{
		{name: "direct agent", stored: "openai", want: "agent", toolWas: "openai", toolNow: "agent"},
		{name: "pipeline", stored: "pipeline", want: "pipeline", toolWas: "pipeline", toolNow: "pipeline"},
		{name: "react is left for the runtime to refuse", stored: "react", want: "react", toolWas: "react", toolNow: "react"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// The nested entry carries the FULL stored shape on purpose:
			// freezeCurrentStoredApplicationReference refuses a reference that
			// is missing any of these fields, so a thinner fixture would fail
			// the freeze for a reason that has nothing to do with agent_type.
			version := freezeVersionForRuntimeProfileTest(t, `{
				"llm_settings":{"model_name":"model","model_project_id":7},
				"agent_type":"`+test.stored+`",
				"tools":[{
					"id":44,"type":"application","name":"nested-agent","description":null,
					"author_id":11,"settings":{"application_id":3,"application_version_id":4},
					"meta":{},"created_at":"2026-08-07T10:00:00Z","toolkit_name":"nested-agent",
					"author":null,"agent_type":"`+test.toolWas+`","online":null,"icon_meta":null,
					"variables":[],"is_pinned":false,"indexes_count":null
				}]
			}`)

			if version["agent_type"] != test.want {
				t.Fatalf("agent_type=%v, want %q", version["agent_type"], test.want)
			}
			tools, ok := version["tools"].([]any)
			if !ok || len(tools) != 1 {
				t.Fatalf("tools=%v, want one entry", version["tools"])
			}
			tool, ok := tools[0].(map[string]any)
			if !ok {
				t.Fatalf("tool entry is %T, want an object", tools[0])
			}
			// A nested agent reference is validated by the SAME runtime rule
			// (services/elitea-worker-rust/src/agents/application_tools.rs:1043),
			// so leaving the nested spelling alone would refuse any agent that
			// calls another agent even though the parent itself now passes.
			if tool["agent_type"] != test.toolNow {
				t.Fatalf("nested agent_type=%v, want %q", tool["agent_type"], test.toolNow)
			}
		})
	}
}

// TestFreezeLeavesAbsentAgentTypeAlone guards the "" case the write validator
// also admits: an absent agent_type is what the runtime treats as the direct
// profile by default, so inventing a value here would be a behaviour change
// dressed as a normalisation.
func TestFreezeLeavesAbsentAgentTypeAlone(t *testing.T) {
	version := freezeVersionForRuntimeProfileTest(t, `{
		"llm_settings":{"model_name":"model","model_project_id":7},
		"tools":[]
	}`)

	if _, present := version["agent_type"]; present {
		t.Fatalf("freeze invented an agent_type: %v", version["agent_type"])
	}
}

// Builder flags become frozen MCP references before they leave the runtime list.
func TestFreezeConvertsInternalMCPFromVersionMeta(t *testing.T) {
	tests := []struct {
		name     string
		authored string
		want     []any
	}{
		{name: "the seeded default", authored: `["internal_mcp"]`, want: []any{}},
		{name: "mixed with an admitted name", authored: `["internal_mcp","ask_user"]`, want: []any{"ask_user"}},
		{name: "nothing to drop", authored: `["ask_user"]`, want: []any{"ask_user"}},
		{name: "already empty", authored: `[]`, want: []any{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			version := freezeVersionForRuntimeProfileTest(t, `{
				"llm_settings":{"model_name":"model","model_project_id":7},
				"meta":{"internal_tools":`+test.authored+`},
				"tools":[]
			}`)

			if strings.Contains(test.authored, "internal_mcp") && len(version["tools"].([]any)) == 0 {
				t.Fatal("selected internal MCP produced no runtime tool references")
			}
			meta, ok := version["meta"].(map[string]any)
			if !ok {
				t.Fatalf("meta is %T, want an object", version["meta"])
			}
			got, ok := meta["internal_tools"].([]any)
			if !ok {
				t.Fatalf("internal_tools is %T, want an array", meta["internal_tools"])
			}
			if len(got) != len(test.want) {
				t.Fatalf("internal_tools=%v, want %v", got, test.want)
			}
			for index, value := range test.want {
				if got[index] != value {
					t.Fatalf("internal_tools=%v, want %v", got, test.want)
				}
			}
		})
	}
}

// TestFreezeLeavesAMalformedInternalToolListAlone is the boundary case: a list
// that is not an array, or that holds something other than strings, is not this
// function's to repair. The runtime's own catalogue refuses it with a reason,
// and quietly reshaping it here would hide input the operator should see.
func TestFreezeLeavesAMalformedInternalToolListAlone(t *testing.T) {
	version := freezeVersionForRuntimeProfileTest(t, `{
		"llm_settings":{"model_name":"model","model_project_id":7},
		"meta":{"internal_tools":"internal_mcp"},
		"tools":[]
	}`)

	meta, ok := version["meta"].(map[string]any)
	if !ok {
		t.Fatalf("meta is %T, want an object", version["meta"])
	}
	if meta["internal_tools"] != "internal_mcp" {
		t.Fatalf("internal_tools=%v, want the authored value untouched", meta["internal_tools"])
	}
}

// TestCurrentRuntimeInternalToolsForwardsThePlatformCatalogue pins the layer's
// job as FORWARDING, not judging: every name the agent form can author reaches
// the execution input (deduplicated, internal_mcp excepted — it is materialized
// through the frozen tools projection), and each WORKER then decides what it
// serves. The Python worker runs the whole set; the native runtime skips what
// it lacks with a logged agent_internal_tool_skipped. Refusing here — the old
// behaviour for everything but ask_user — turned every form toggle into an
// agent that stopped answering on both workers at once.
func TestCurrentRuntimeInternalToolsForwardsThePlatformCatalogue(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{
			name: "the whole authorable catalogue",
			raw:  `["ask_user","attachments","data_analysis","image_generation","internal_mcp","lazy_tools_mode","planner","pyodide","swarm"]`,
			want: `["ask_user","attachments","data_analysis","image_generation","lazy_tools_mode","planner","pyodide","swarm"]`,
		},
		{name: "internal_mcp flag is consumed after tool projection", raw: `["internal_mcp"]`, want: `[]`},
		{name: "duplicates collapse", raw: `["pyodide","pyodide","ask_user"]`, want: `["pyodide","ask_user"]`},
		{name: "empty stays empty", raw: `[]`, want: `[]`},
		{name: "off the catalogue is refused", raw: `["not_a_platform_tool"]`, wantErr: true},
		{name: "a non-string entry is refused", raw: `[42]`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := currentRuntimeInternalTools(json.RawMessage(test.raw))
			if test.wantErr {
				if !errors.Is(err, ErrUnsupportedCurrentAgentStart) {
					t.Fatalf("err=%v, want ErrUnsupportedCurrentAgentStart", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != test.want {
				t.Fatalf("forwarded %s, want %s", encoded, test.want)
			}
		})
	}
}
