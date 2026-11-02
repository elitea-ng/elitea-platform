package mcp

import "testing"

func TestParseInternalVersionUpdateValidatesCurrentApplicationShapes(t *testing.T) {
	state := internalVersionState{
		name:             "base",
		agentType:        "react",
		instructions:     "current instructions",
		meta:             map[string]any{},
		pipelineSettings: map[string]any{},
	}
	tests := []struct {
		name      string
		arguments map[string]any
		valid     bool
	}{
		{
			name:      "current react agent type",
			arguments: map[string]any{"agent_type": "react"},
			valid:     true,
		},
		{
			name:      "invented agent type",
			arguments: map[string]any{"agent_type": "agent"},
		},
		{
			name: "complete variable",
			arguments: map[string]any{"variables": []any{
				map[string]any{"name": "region", "value": "eu"},
			}},
			valid: true,
		},
		{
			name: "variable without value",
			arguments: map[string]any{"variables": []any{
				map[string]any{"name": "region"},
			}},
		},
		{
			name:      "bare tag string",
			arguments: map[string]any{"tags": []any{"release"}},
		},
		{
			name: "tag object",
			arguments: map[string]any{"tags": []any{
				map[string]any{"name": "release", "data": map[string]any{"channel": "stable"}},
			}},
			valid: true,
		},
		{
			name:      "empty conversation starter",
			arguments: map[string]any{"conversation_starters": []any{" "}},
		},
		{
			name:      "non-empty conversation starter",
			arguments: map[string]any{"conversation_starters": []any{"How can I help?"}},
			valid:     true,
		},
		// ADR-0043 (#1500): the meta gets the checks of the REST save.
		{
			name:      "citations required",
			arguments: map[string]any{"meta": map[string]any{"citations": "required"}},
			valid:     true,
		},
		{
			name:      "unknown citations value",
			arguments: map[string]any{"meta": map[string]any{"citations": "always"}},
		},
		{
			name: "valid meta output contract",
			arguments: map[string]any{"meta": map[string]any{
				"output_contract": map[string]any{"type": "format", "format": "json"},
			}},
			valid: true,
		},
		{
			name: "invalid meta output contract",
			arguments: map[string]any{"meta": map[string]any{
				"output_contract": map[string]any{"type": "format", "format": "xml"},
			}},
		},
		{
			name: "meta output contract on a pipeline",
			arguments: map[string]any{"agent_type": "pipeline", "meta": map[string]any{
				"output_contract": map[string]any{"type": "format", "format": "json"},
			}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, problem := parseInternalVersionUpdate(test.arguments, state)
			if got := problem == nil; got != test.valid {
				t.Fatalf("valid = %t, want %t; problem = %#v", got, test.valid, problem)
			}
		})
	}
}

// A switch to a pipeline checks the stored meta too: a pipeline cannot have a
// meta output contract.
func TestParseInternalVersionUpdateRefusesAPipelineSwitchOverAMetaContract(t *testing.T) {
	state := internalVersionState{
		name:      "base",
		agentType: "react",
		meta: map[string]any{
			"output_contract": map[string]any{"type": "format", "format": "json"},
		},
		pipelineSettings: map[string]any{},
	}
	if _, problem := parseInternalVersionUpdate(map[string]any{"agent_type": "pipeline"}, state); problem == nil {
		t.Fatal("the switch to a pipeline was accepted over a meta output contract")
	}
	if _, problem := parseInternalVersionUpdate(map[string]any{"welcome_message": "hi"}, state); problem != nil {
		t.Fatalf("an unrelated update was refused: %#v", problem)
	}
}
