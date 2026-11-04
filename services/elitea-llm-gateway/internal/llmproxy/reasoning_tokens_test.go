package llmproxy

import (
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

// The reasoning count of a usage block (ADR-0038 decision 5).
func TestReasoningTokensFromLLMUsage(t *testing.T) {
	cases := []struct {
		name  string
		usage *schemas.BifrostLLMUsage
		want  int64
		ok    bool
	}{
		{"nil", nil, 0, false},
		{"no details", &schemas.BifrostLLMUsage{CompletionTokens: 10}, 0, false},
		{"nested", &schemas.BifrostLLMUsage{CompletionTokensDetails: &schemas.ChatCompletionTokensDetails{ReasoningTokens: 7}}, 7, true},
		{"sglang top level", &schemas.BifrostLLMUsage{ReasoningTokens: 5}, 5, true},
		{"nested wins", &schemas.BifrostLLMUsage{ReasoningTokens: 5, CompletionTokensDetails: &schemas.ChatCompletionTokensDetails{ReasoningTokens: 7}}, 7, true},
	}
	for _, tc := range cases {
		got, ok := reasoningTokensFromLLMUsage(tc.usage)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: got (%d, %v), want (%d, %v)", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestReasoningTokensFromResponsesUsage(t *testing.T) {
	if _, ok := reasoningTokensFromResponsesUsage(&schemas.ResponsesResponseUsage{}); ok {
		t.Fatal("a block without output details reports no count")
	}
	got, ok := reasoningTokensFromResponsesUsage(&schemas.ResponsesResponseUsage{
		OutputTokensDetails: &schemas.ResponsesResponseOutputTokens{ReasoningTokens: 11},
	})
	if !ok || got != 11 {
		t.Fatalf("got (%d, %v), want (11, true)", got, ok)
	}
}
