package llmproxy

// guardrail_slot.go — tool results on the input stage (ADR-0043 decision 4,
// issue #1500).
//
// A detector with `content: tool_results` reads only the tool results of a
// request, and a detector in the `prompt_injection` slot is one of them
// (internal/guardrail/slot.go). This file names the tool-result texts of
// each dialect:
//
//   - Chat: the content of every message with the role `tool` (and the
//     legacy role `function`).
//   - Responses: the output of every function_call_output,
//     custom_tool_call_output and shell call output item.
//   - Messages: the route converts an Anthropic tool_result block to a
//     function_call_output, so the Responses rule applies.
//
// Text completions and embeddings have no tool results.
//
// The texts are the same pointers that chatRequestTexts and
// responsesRequestTexts return, so the input stage marks a part as a tool
// result by pointer identity.

import (
	"context"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/guardrail"
)

// chatToolResultTexts returns the text parts of the tool messages of req.
func chatToolResultTexts(req *schemas.BifrostChatRequest) []*string {
	var out []*string
	for i := range req.Input {
		switch req.Input[i].Role {
		case schemas.ChatMessageRoleTool, "function":
			out = chatMessageContentTexts(req.Input[i].Content, out)
		}
	}
	return out
}

// responsesToolResultTexts returns the output texts of the tool result items
// of req.
func responsesToolResultTexts(req *schemas.BifrostResponsesRequest) []*string {
	var out []*string
	for i := range req.Input {
		out = responsesToolOutputTexts(req.Input[i].ResponsesToolMessage, out)
	}
	return out
}

// checkInputScoped runs the input stage. It marks each text that is in tools, runs guardrail.CheckInput and
// writes the masked values back. It returns the refusing verdict, or nil.
func checkInputScoped(ctx context.Context, pipe *guardrail.Pipeline, texts, tools []*string) *guardrail.Verdict {
	isTool := make(map[*string]bool, len(tools))
	for _, t := range tools {
		isTool[t] = true
	}
	parts := make([]string, len(texts))
	marks := make([]bool, len(texts))
	for i, t := range texts {
		parts[i] = *t
		marks[i] = isTool[t]
	}
	masked, blocked := pipe.CheckInput(ctx, parts, marks)
	if blocked != nil {
		return blocked
	}
	for i, t := range texts {
		*t = masked[i]
	}
	return nil
}
