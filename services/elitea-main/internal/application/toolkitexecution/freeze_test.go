package toolkitexecution

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/guardrails"
)

func TestCurrentReadToolFreezerUsesReferenceModeAndPreservesExactTarget(t *testing.T) {
	reader := &toolkitReaderStub{toolkit: exposedToolkit()}
	settings := &settingsResolverStub{resolved: map[string]any{
		"token":          map[string]any{"configuration_uuid": "sealed-reference"},
		"selected_tools": []any{"get_issue"},
	}}
	policy := guardrails.NewPolicy(guardrails.PolicyInput{})
	freezer, err := NewCurrentReadToolFreezer(reader, settings, guardrailResolverStub{policy: policy})
	if err != nil {
		t.Fatal(err)
	}

	frozen, err := freezer.Freeze(context.Background(), FreezeCurrentReadToolRequest{
		ProjectID: 7, ActorID: 11, ToolkitID: 19, ToolName: "get_issue",
		Arguments: map[string]any{"issue": json.Number("9007199254740993")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if reader.projectID != 7 || reader.userID != 11 || reader.toolkitID != 19 {
		t.Fatalf("reader identity = %d/%d/%d", reader.projectID, reader.userID, reader.toolkitID)
	}
	if settings.request.Mode != configurationapp.CurrentToolkitSettingsReferenceMode ||
		settings.request.ToolkitType != "github" || settings.request.ProjectID != 7 ||
		settings.request.UserID != 11 {
		t.Fatalf("settings request = %+v", settings.request)
	}
	if frozen.ToolkitType != "github" || frozen.ToolkitName != "Source Control" || frozen.ToolName != "get_issue" {
		t.Fatalf("frozen target = %+v", frozen)
	}
	var toolkit map[string]any
	decodeUseNumber(t, frozen.ToolkitJSON, &toolkit)
	if toolkit["type"] != "github" || toolkit["toolkit_name"] != "Source Control" ||
		toolkit["id"] != json.Number("19") {
		t.Fatalf("toolkit snapshot = %#v", toolkit)
	}
	var arguments map[string]any
	decodeUseNumber(t, frozen.ArgumentsJSON, &arguments)
	if arguments["issue"] != json.Number("9007199254740993") {
		t.Fatalf("arguments = %#v", arguments)
	}
	if len(frozen.GuardrailsJSON) == 0 {
		t.Fatal("guardrail snapshot was omitted")
	}
}

func TestCurrentReadToolFreezerRechecksExposureSelectionAndPolicy(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*CurrentMCPToolkitSnapshot)
		policy  guardrails.PolicyInput
		wantErr error
	}{
		{
			name: "not exposed",
			mutate: func(toolkit *CurrentMCPToolkitSnapshot) {
				toolkit.Meta = map[string]any{"mcp_options": map[string]any{"available_by_mcp": false}}
			},
			wantErr: ErrCurrentReadToolkitNotVisible,
		},
		{
			name: "tool removed after listing",
			mutate: func(toolkit *CurrentMCPToolkitSnapshot) {
				toolkit.Settings["selected_tools"] = []any{"list_issues"}
			},
			wantErr: ErrCurrentReadToolNotSelected,
		},
		{
			name:    "blocked",
			policy:  guardrails.PolicyInput{BlockedTools: map[string][]string{"github": {"get_issue"}}},
			wantErr: ErrCurrentReadToolRestricted,
		},
		{
			name:    "sensitive by instance",
			policy:  guardrails.PolicyInput{SensitiveTools: map[string][]string{"Source Control": {"get_issue"}}},
			wantErr: ErrCurrentReadToolRestricted,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			toolkit := exposedToolkit()
			if test.mutate != nil {
				test.mutate(&toolkit)
			}
			policy := guardrails.NewPolicy(test.policy)
			settings := &settingsResolverStub{resolved: map[string]any{"selected_tools": []any{"get_issue"}}}
			freezer, err := NewCurrentReadToolFreezer(
				&toolkitReaderStub{toolkit: toolkit}, settings, guardrailResolverStub{policy: policy},
			)
			if err != nil {
				t.Fatal(err)
			}
			_, err = freezer.Freeze(context.Background(), FreezeCurrentReadToolRequest{
				ProjectID: 7, ActorID: 11, ToolkitID: 19, ToolName: "get_issue", Arguments: map[string]any{},
			})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Freeze() error = %v, want %v", err, test.wantErr)
			}
			if settings.calls != 0 {
				t.Fatal("restricted target reached settings expansion")
			}
		})
	}
}

func TestCurrentReadToolFreezerNormalizesLegacySelectedTools(t *testing.T) {
	toolkit := exposedToolkit()
	toolkit.Settings["selected_tools"] = []any{
		map[string]any{"name": "get_issue", "legacy": true},
		42,
		map[string]any{"name": ""},
		"get_issue",
	}
	freezer := testFreezer(t, toolkit)
	frozen, err := freezer.Freeze(context.Background(), FreezeCurrentReadToolRequest{
		ProjectID: 7, ActorID: 11, ToolkitID: 19, ToolName: "get_issue", Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	decodeUseNumber(t, frozen.ToolkitJSON, &saved)
	settings, _ := saved["settings"].(map[string]any)
	selected, _ := settings["selected_tools"].([]any)
	if len(selected) != 1 || selected[0] != "get_issue" {
		t.Fatalf("normalized selected tools = %#v", selected)
	}
}

func TestCurrentReadToolFreezerFailsClosedOnMissingOrOversizedInput(t *testing.T) {
	toolkit := exposedToolkit()
	toolkit.Settings["selected_tools"] = []any{42, map[string]any{"name": ""}}
	freezer := testFreezer(t, toolkit)
	_, err := freezer.Freeze(context.Background(), FreezeCurrentReadToolRequest{
		ProjectID: 7, ActorID: 11, ToolkitID: 19, ToolName: "get_issue", Arguments: map[string]any{},
	})
	if !errors.Is(err, ErrCurrentReadToolNotSelected) {
		t.Fatalf("non-authorizing selected tool error = %v", err)
	}

	freezer = testFreezer(t, exposedToolkit())
	_, err = freezer.Freeze(context.Background(), FreezeCurrentReadToolRequest{
		ProjectID: 7, ActorID: 11, ToolkitID: 19, ToolName: "get_issue",
		Arguments: map[string]any{"value": string(make([]byte, MaxCurrentReadToolArgumentsBytes))},
	})
	if !errors.Is(err, ErrInvalidCurrentReadTool) {
		t.Fatalf("oversized arguments error = %v", err)
	}
}

func TestCurrentReadToolAdmissionStagePreservesIdentityAndRedactsCauseText(t *testing.T) {
	sensitiveCause := errors.New("credential title and provider response must not be logged")
	freezer, err := NewCurrentReadToolFreezer(
		&toolkitReaderStub{err: sensitiveCause},
		&settingsResolverStub{},
		guardrailResolverStub{},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = freezer.Freeze(context.Background(), FreezeCurrentReadToolRequest{
		ProjectID: 7, ActorID: 11, ToolkitID: 19, ToolName: "get_issue", Arguments: map[string]any{},
	})
	if !errors.Is(err, sensitiveCause) {
		t.Fatalf("wrapped error lost its cause: %v", err)
	}
	if stage := CurrentReadToolAdmissionStageOf(err); stage != CurrentReadToolAdmissionToolkitLookup {
		t.Fatalf("admission stage = %q, want %q", stage, CurrentReadToolAdmissionToolkitLookup)
	}
	if strings.Contains(err.Error(), "credential") || strings.Contains(err.Error(), "provider") {
		t.Fatalf("admission error leaked wrapped text: %q", err.Error())
	}
	if stage := CurrentReadToolAdmissionStageOf(context.Canceled); stage != "" {
		t.Fatalf("non-admission error stage = %q, want empty", stage)
	}
}

func exposedToolkit() CurrentMCPToolkitSnapshot {
	return CurrentMCPToolkitSnapshot{
		ID: 19, Type: "github", Name: "Source Control",
		Settings: map[string]any{"selected_tools": []any{"get_issue"}, "token": "secret-ref"},
		Meta:     map[string]any{"mcp_options": map[string]any{"available_by_mcp": true}},
	}
}

func testFreezer(t *testing.T, toolkit CurrentMCPToolkitSnapshot) *CurrentReadToolFreezer {
	t.Helper()
	policy := guardrails.NewPolicy(guardrails.PolicyInput{})
	freezer, err := NewCurrentReadToolFreezer(
		&toolkitReaderStub{toolkit: toolkit},
		&settingsResolverStub{resolved: toolkit.Settings},
		guardrailResolverStub{policy: policy},
	)
	if err != nil {
		t.Fatal(err)
	}
	return freezer
}

type toolkitReaderStub struct {
	toolkit                      CurrentMCPToolkitSnapshot
	found                        bool
	err                          error
	projectID, userID, toolkitID int32
}

func (s *toolkitReaderStub) GetCurrentMCPToolkit(
	_ context.Context, projectID, userID, toolkitID int32,
) (CurrentMCPToolkitSnapshot, bool, error) {
	s.projectID, s.userID, s.toolkitID = projectID, userID, toolkitID
	if s.err != nil {
		return CurrentMCPToolkitSnapshot{}, false, s.err
	}
	if !s.found && s.toolkit.ID == 0 {
		return CurrentMCPToolkitSnapshot{}, false, nil
	}
	return s.toolkit, true, nil
}

type settingsResolverStub struct {
	resolved map[string]any
	err      error
	request  configurationapp.CurrentToolkitSettingsRequest
	calls    int
}

func (s *settingsResolverStub) Resolve(
	_ context.Context, request configurationapp.CurrentToolkitSettingsRequest,
) (map[string]any, error) {
	s.calls++
	s.request = request
	return s.resolved, s.err
}

type guardrailResolverStub struct {
	policy guardrails.Policy
	err    error
}

func (s guardrailResolverStub) ResolveCurrentAgentGuardrails(context.Context) (guardrails.Policy, error) {
	return s.policy, s.err
}

func decodeUseNumber(t *testing.T, encoded []byte, target any) {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		t.Fatal(err)
	}
}
