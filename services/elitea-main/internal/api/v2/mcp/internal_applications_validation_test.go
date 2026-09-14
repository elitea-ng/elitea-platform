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
