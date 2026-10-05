package agentexecution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type continuationActualFreezer struct {
	delegate CurrentApplicationVersionFreezer
	calls    []CurrentApplicationVersionFreezeRequest
}

func (f *continuationActualFreezer) FreezeCurrentApplicationVersion(ctx context.Context, req CurrentApplicationVersionFreezeRequest) (json.RawMessage, error) {
	f.calls = append(f.calls, req)
	return f.delegate.FreezeCurrentApplicationVersion(ctx, req)
}

func continuationSourceTools(t *testing.T) (CurrentApplicationTarget, json.RawMessage) {
	t.Helper()
	tool := map[string]any{
		"id": 44, "type": "application", "name": "nested-agent", "description": nil,
		"author_id": 11, "toolkit_name": "nested-agent", "agent_type": "agent", "created_at": "2026-08-07T10:00:00Z",
		"settings": map[string]any{"application_id": 3, "application_version_id": 4},
		"meta":     map[string]any{}, "variables": []any{}, "is_pinned": false, "author": nil, "online": nil, "icon_meta": nil, "indexes_count": nil,
	}
	version := map[string]any{"id": 41, "application_id": 31, "agent_type": "agent", "instructions": "Original source instructions", "meta": map[string]any{}, "llm_settings": map[string]any{"model_name": "model"}, "skills": []any{},
		"tools": []any{tool, map[string]any{"id": 52, "type": "mcp", "name": "documentation", "settings": map[string]any{"url": "https://example.invalid/events", "selected_tools": []any{"read"}}}},
	}
	raw, err := json.Marshal(version)
	if err != nil {
		t.Fatal(err)
	}
	tool[currentNestedSkillRegistryField] = []any{map[string]any{"application_id": 3, "application_version_id": 4, "application_name": "nested-agent", "skills": []any{map[string]any{"skill_id": 7, "name": "Original skill", "icon_meta": nil}}}}
	runtime, err := json.Marshal(version)
	if err != nil {
		t.Fatal(err)
	}
	target := rootSourceTarget()
	target.SourceVersionDetails = raw
	target.VersionDetails = runtime
	handles, err := json.Marshal([]any{tool})
	if err != nil {
		t.Fatal(err)
	}
	return target, handles
}

func TestOriginalSourceContinueRunsActualFreezerWithOriginalRegistryAndFreshSettings(t *testing.T) {
	resolver, request := sourceContinueFixture()
	target, handles := continuationSourceTools(t)
	resolver.target = target
	owner := &originalSourceOwnerFixture{applicationTools: handles}
	settings := &currentAgentSettingsResolverStub{result: map[string]any{"url": "https://current.example.invalid/events", "selected_tools": []any{"read"}, "credential_reference": "current-authorized-reference"}}
	names := &currentAgentNameResolverStub{result: "documentation"}
	actual, err := NewCurrentApplicationToolSnapshotService(settings, names, currentAgentModelCatalogForTest(false), &currentAgentGuardrailStub{}, &currentProjectContextStub{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	freezer := &continuationActualFreezer{delegate: actual}
	service, admissions := sourceService(t, resolver, owner, freezer)
	if _, err := service.StartCurrentApplication(t.Context(), validCurrentApplicationStartRequest()); err != nil {
		t.Fatal(err)
	}
	original := owner.source
	// The companion owner returns only application handles from original input.
	// A later resolver edit and formerly redeemed toolkit settings cannot win.
	resolver.target.VersionDetails = bytes.Replace(target.VersionDetails, []byte(`"application_version_id":4`), []byte(`"application_version_id":99`), 1)
	resolver.target.SourceVersionDetails = bytes.Replace(target.SourceVersionDetails, []byte("Original source instructions"), []byte("Edited current source"), 1)
	settings.result = map[string]any{"url": "https://current.example.invalid/events", "selected_tools": []any{"read"}, "credential_reference": "new-current-authorized-reference"}
	if _, err := service.ContinueCurrentAgent(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	ref, application := submittedSource(t, admissions.requests[1].Input)
	version, err := decodeCurrentApplicationVersion(application["version_details"])
	if err != nil {
		t.Fatal(err)
	}
	tools := version["tools"].([]any)
	app := tools[0].(map[string]any)
	if ref != original.Reference || len(owner.captures) != 1 || app["settings"].(map[string]any)["application_version_id"] != json.Number("4") || !bytes.Contains(application["version_details"], []byte("Original skill")) || bytes.Contains(application["version_details"], []byte("Edited current source")) {
		t.Fatal("continuation lost or retargeted its original admitted handles")
	}
	if len(settings.requests) != 2 || len(names.requests) != 2 || !bytes.Contains(application["version_details"], []byte("new-current-authorized-reference")) || bytes.Contains(original.PreRedemptionVersion, []byte("credential_reference")) || bytes.Contains(original.PreRedemptionVersion, []byte(currentNestedSkillRegistryField)) {
		t.Fatal("current credential freeze bypassed or raw source polluted")
	}
	if len(freezer.calls) != 2 || !bytes.Contains(freezer.calls[1].VersionDetails, []byte("Original skill")) || bytes.Contains(freezer.calls[1].VersionDetails, []byte("new-current-authorized-reference")) {
		t.Fatal("actual freezer did not receive original registry plus raw settings")
	}
}

func TestOriginalSourceContinuationRefusesDriftedOriginalHandlesBeforeFreezer(t *testing.T) {
	for _, name := range []string{"missing", "extra", "changed revision", "changed handle", "invalid registry", "duplicate"} {
		t.Run(name, func(t *testing.T) {
			resolver, request := sourceContinueFixture()
			target, handles := continuationSourceTools(t)
			resolver.target = target
			owner := &originalSourceOwnerFixture{}
			if _, err := owner.CaptureOriginalRootSource(t.Context(), RootSourceCaptureRequest{7, 11, 31, 41, target.SourceVersionDetails}); err != nil {
				t.Fatal(err)
			}
			var entries []map[string]any
			// Test edits only; production uses the number-preserving codec.
			if json.Unmarshal(handles, &entries) != nil {
				t.Fatal("fixture")
			}
			switch name {
			case "missing":
				entries = nil
			case "extra":
				other := map[string]any{}
				for k, v := range entries[0] {
					other[k] = v
				}
				other["id"] = 45
				entries = append(entries, other)
			case "changed revision":
				entries[0]["settings"].(map[string]any)["application_version_id"] = 99
			case "changed handle":
				entries[0]["name"] = "different"
				entries[0]["toolkit_name"] = "different"
			case "invalid registry":
				entries[0][currentNestedSkillRegistryField] = []any{map[string]any{"application_id": 3}}
			case "duplicate":
				entries = append(entries, entries[0])
			}
			owner.applicationTools, _ = json.Marshal(entries)
			freezer := &sourceRecordingFreezer{}
			service, admissions := sourceService(t, resolver, owner, freezer)
			if _, err := service.ContinueCurrentAgent(t.Context(), request); !errors.Is(err, ErrUnsupportedCurrentAgentStart) || len(freezer.calls) != 0 || len(admissions.requests) != 0 {
				t.Fatalf("err=%v freeze=%d writes=%d", err, len(freezer.calls), len(admissions.requests))
			}
		})
	}
}
