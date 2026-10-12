package agentexecution

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The captured source keeps the agent_type the platform stores for a direct
// agent ("openai"); turn admission renames it for the runtime ("agent"). A
// continuation compares the two, so both must be read in one spelling.

func rewriteApplicationToolAgentType(t *testing.T, raw json.RawMessage, agentType string) json.RawMessage {
	t.Helper()
	var version map[string]any
	if err := json.Unmarshal(raw, &version); err != nil {
		t.Fatal(err)
	}
	for _, value := range version["tools"].([]any) {
		if tool := value.(map[string]any); tool["type"] == "application" {
			tool["agent_type"] = agentType
		}
	}
	out, err := json.Marshal(version)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// admittedHandlesFor runs the production normalizer, so a future change to
// what admission rewrites is seen by the comparison under test.
func admittedHandlesFor(t *testing.T, source json.RawMessage) json.RawMessage {
	t.Helper()
	version, err := decodeCurrentApplicationVersion(source)
	if err != nil {
		t.Fatal(err)
	}
	normalizeCurrentAgentRuntimeProfile(t.Context(), version, 7)
	var admitted []any
	for _, value := range version["tools"].([]any) {
		if tool := value.(map[string]any); tool["type"] == "application" {
			admitted = append(admitted, tool)
		}
	}
	out, err := json.Marshal(admitted)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRestoreOriginalApplicationHandlesComparesInRuntimeSpelling(t *testing.T) {
	target, _ := continuationSourceTools(t)
	source := rewriteApplicationToolAgentType(t, target.SourceVersionDetails, "openai")
	admitted := admittedHandlesFor(t, source)
	if !strings.Contains(string(admitted), `"agent_type":"agent"`) || strings.Contains(string(admitted), `"openai"`) {
		t.Fatalf("fixture did not rename the admitted handle: %s", admitted)
	}
	restored, err := restoreOriginalApplicationHandles(source, admitted, 7, 11)
	if err != nil {
		t.Fatal(err)
	}
	version, err := decodeCurrentApplicationVersion(restored)
	if err != nil {
		t.Fatal(err)
	}
	if got := version["tools"].([]any)[0].(map[string]any)["agent_type"]; got != "agent" {
		t.Fatalf("restored handle agent_type = %v", got)
	}
	// A handle admitted before the rename existed still matches.
	if _, err := restoreOriginalApplicationHandles(source, rewriteApplicationHandles(t, admitted, "openai"), 7, 11); err != nil {
		t.Fatalf("legacy spelling refused: %v", err)
	}
}

func rewriteApplicationHandles(t *testing.T, raw json.RawMessage, agentType string) json.RawMessage {
	t.Helper()
	var handles []map[string]any
	if err := json.Unmarshal(raw, &handles); err != nil {
		t.Fatal(err)
	}
	for _, handle := range handles {
		handle["agent_type"] = agentType
	}
	out, err := json.Marshal(handles)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRestoreOriginalApplicationHandlesStaysStrictBeyondAgentType(t *testing.T) {
	target, _ := continuationSourceTools(t)
	source := rewriteApplicationToolAgentType(t, target.SourceVersionDetails, "openai")
	admitted := admittedHandlesFor(t, source)
	cases := []struct {
		name   string
		mutate func(handle map[string]any)
		reason string
	}{
		{"changed version id", func(h map[string]any) { h["settings"].(map[string]any)["application_version_id"] = 99 }, "names an application the turn did not admit"},
		{"changed application id", func(h map[string]any) { h["settings"].(map[string]any)["application_id"] = 98 }, "names an application the turn did not admit"},
		{"changed name", func(h map[string]any) { h["name"] = "other"; h["toolkit_name"] = "other" }, "differs from the admitted handle"},
		{"changed agent type beyond the rename", func(h map[string]any) { h["agent_type"] = "react" }, "differs from the admitted handle"},
		{"changed variables", func(h map[string]any) { h["variables"] = []any{map[string]any{"name": "x"}} }, "not a valid frozen reference"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var handles []map[string]any
			if err := json.Unmarshal(admitted, &handles); err != nil {
				t.Fatal(err)
			}
			tc.mutate(handles[0])
			changed, _ := json.Marshal(handles)
			_, err := restoreOriginalApplicationHandles(source, changed, 7, 11)
			if !errors.Is(err, ErrUnsupportedCurrentAgentStart) || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("err = %v, want reason %q", err, tc.reason)
			}
		})
	}
}

func TestOriginalSourceContinueAcceptsStoredOpenAIChildApplication(t *testing.T) {
	resolver, request := sourceContinueFixture()
	target, handles := continuationSourceTools(t)
	target.SourceVersionDetails = rewriteApplicationToolAgentType(t, target.SourceVersionDetails, "openai")
	target.VersionDetails = rewriteApplicationToolAgentType(t, target.VersionDetails, "openai")
	resolver.target = target
	owner := &originalSourceOwnerFixture{applicationTools: handles}
	actual, err := NewCurrentApplicationToolSnapshotService(
		&currentAgentSettingsResolverStub{result: map[string]any{"url": "https://current.example.invalid/events", "selected_tools": []any{"read"}, "credential_reference": "current-authorized-reference"}},
		&currentAgentNameResolverStub{result: "documentation"}, currentAgentModelCatalogForTest(false), &currentAgentGuardrailStub{}, &currentProjectContextStub{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	service, admissions := sourceService(t, resolver, owner, &continuationActualFreezer{delegate: actual})
	if _, err := service.StartCurrentApplication(t.Context(), validCurrentApplicationStartRequest()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !strings.Contains(string(owner.source.PreRedemptionVersion), `"openai"`) {
		t.Fatal("captured source lost the stored agent_type, fixture is not the production shape")
	}
	if _, err := service.ContinueCurrentAgent(t.Context(), request); err != nil {
		t.Fatalf("continuation refused: %v", err)
	}
	_, application := submittedSource(t, admissions.requests[1].Input)
	version, err := decodeCurrentApplicationVersion(application["version_details"])
	if err != nil {
		t.Fatal(err)
	}
	if got := version["tools"].([]any)[0].(map[string]any)["agent_type"]; got != "agent" {
		t.Fatalf("continued child agent_type = %v, want the runtime spelling", got)
	}
}

func TestOriginalSourceContinueRefusalNamesItsSite(t *testing.T) {
	resolver, request := sourceContinueFixture()
	target, handles := continuationSourceTools(t)
	target.SourceVersionDetails = rewriteApplicationToolAgentType(t, target.SourceVersionDetails, "openai")
	resolver.target = target
	var entries []map[string]any
	if err := json.Unmarshal(handles, &entries); err != nil {
		t.Fatal(err)
	}
	entries[0]["settings"].(map[string]any)["application_version_id"] = 99
	owner := &originalSourceOwnerFixture{}
	if _, err := owner.CaptureOriginalRootSource(t.Context(), RootSourceCaptureRequest{7, 11, 31, 41, target.SourceVersionDetails}); err != nil {
		t.Fatal(err)
	}
	owner.applicationTools, _ = json.Marshal(entries)
	service, _ := sourceService(t, resolver, owner, &sourceRecordingFreezer{})
	_, err := service.ContinueCurrentAgent(t.Context(), request)
	if !errors.Is(err, ErrUnsupportedCurrentAgentStart) || !strings.Contains(err.Error(), "original source names an application the turn did not admit") {
		t.Fatalf("err = %v", err)
	}
}
