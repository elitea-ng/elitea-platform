package account

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

// dial_protocol_stream_test.go — the openai DIAL protocol on a STREAMED chat
// completion with tools (legacy issue #6707).
//
// Both workers stream chat completions with tools. On the openai protocol the
// /llm handler marks the chat for conversion, so core sends it through
// ResponsesStream and rewrites every Responses event into a chat chunk. These
// tests drive the REAL core over the REAL account against an SSE fake of
// DIAL's /openai/v1/responses, and assert what the caller receives: the text
// deltas, the tool-call deltas, the finish reason and the final usage. The
// /llm stream settler bills from that usage (llmproxy/stream_drain.go).

// dialStreamUsage is the usage the fake reports on response.completed.
const (
	dialStreamInputTokens  = 12
	dialStreamOutputTokens = 7
)

// writeDialResponsesStream answers a streamed Responses request the way the
// OpenAI Responses API does: lifecycle events, output_text deltas, a
// function_call item with argument deltas, and response.completed with usage.
// The function call is sent only when the request declared a tool.
func writeDialResponsesStream(w http.ResponseWriter, body map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	seq := 0
	send := func(event map[string]any) {
		event["sequence_number"] = seq
		seq++
		raw, _ := json.Marshal(event)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], raw)
		if flusher != nil {
			flusher.Flush()
		}
	}
	model := body["model"]
	response := func(status string, output []any, usage map[string]any) map[string]any {
		r := map[string]any{
			"id": "resp_1", "object": "response", "created_at": 1, "status": status,
			"model": model, "output": output,
		}
		if usage != nil {
			r["usage"] = usage
		}
		return r
	}
	tools, _ := body["tools"].([]any)

	send(map[string]any{"type": "response.created", "response": response("in_progress", []any{}, nil)})
	send(map[string]any{"type": "response.in_progress", "response": response("in_progress", []any{}, nil)})
	send(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{
		"type": "message", "id": "msg_1", "role": "assistant", "status": "in_progress", "content": []any{},
	}})
	send(map[string]any{"type": "response.content_part.added", "item_id": "msg_1", "output_index": 0,
		"content_index": 0, "part": map[string]any{"type": "output_text", "text": ""}})
	for _, delta := range []string{"Hel", "lo"} {
		send(map[string]any{"type": "response.output_text.delta", "item_id": "msg_1", "output_index": 0,
			"content_index": 0, "delta": delta})
	}
	send(map[string]any{"type": "response.output_text.done", "item_id": "msg_1", "output_index": 0,
		"content_index": 0, "text": "Hello"})
	send(map[string]any{"type": "response.content_part.done", "item_id": "msg_1", "output_index": 0,
		"content_index": 0, "part": map[string]any{"type": "output_text", "text": "Hello"}})
	message := map[string]any{
		"type": "message", "id": "msg_1", "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": "Hello"}},
	}
	send(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": message})
	output := []any{message}

	if len(tools) > 0 {
		send(map[string]any{"type": "response.output_item.added", "output_index": 1, "item": map[string]any{
			"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "get_weather",
			"arguments": "", "status": "in_progress",
		}})
		for _, delta := range []string{`{"city":`, `"Paris"}`} {
			send(map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc_1",
				"output_index": 1, "delta": delta})
		}
		send(map[string]any{"type": "response.function_call_arguments.done", "item_id": "fc_1",
			"output_index": 1, "arguments": `{"city":"Paris"}`})
		call := map[string]any{
			"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "get_weather",
			"arguments": `{"city":"Paris"}`, "status": "completed",
		}
		send(map[string]any{"type": "response.output_item.done", "output_index": 1, "item": call})
		output = append(output, call)
	}

	send(map[string]any{"type": "response.completed", "response": response("completed", output, map[string]any{
		"input_tokens": dialStreamInputTokens, "output_tokens": dialStreamOutputTokens,
		"total_tokens": dialStreamInputTokens + dialStreamOutputTokens,
	})})
}

// dialStreamResult is what a caller assembles from the converted chat chunks.
type dialStreamResult struct {
	text         string
	toolName     string
	toolCallID   string
	toolArgs     string
	finishReason string
	usage        *schemas.BifrostLLMUsage
	chunks       int
}

func collectDialStream(t *testing.T, ch chan *schemas.BifrostStreamChunk) dialStreamResult {
	t.Helper()
	var out dialStreamResult
	for chunk := range ch {
		if chunk == nil {
			continue
		}
		if chunk.BifrostError != nil {
			t.Fatalf("stream error: %+v", chunk.BifrostError)
		}
		resp := chunk.BifrostChatResponse
		if resp == nil {
			t.Fatalf("chunk is not a chat chunk: %+v", chunk)
		}
		out.chunks++
		if resp.Usage != nil {
			out.usage = resp.Usage
		}
		for _, choice := range resp.Choices {
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				out.finishReason = *choice.FinishReason
			}
			delta := choice.ChatStreamResponseChoice
			if delta == nil || delta.Delta == nil {
				continue
			}
			if delta.Delta.Content != nil {
				out.text += *delta.Delta.Content
			}
			for _, call := range delta.Delta.ToolCalls {
				if call.ID != nil && *call.ID != "" {
					out.toolCallID = *call.ID
				}
				if call.Function.Name != nil && *call.Function.Name != "" {
					out.toolName = *call.Function.Name
				}
				out.toolArgs += call.Function.Arguments
			}
		}
	}
	return out
}

func weatherTool() schemas.ChatTool {
	return schemas.ChatTool{
		Type: schemas.ChatToolTypeFunction,
		Function: &schemas.ChatToolFunction{
			Name: "get_weather",
			Parameters: &schemas.ToolFunctionParameters{
				Type:       "object",
				Properties: schemas.NewOrderedMapFromPairs(schemas.KV("city", map[string]any{"type": "string"})),
			},
		},
	}
}

// streamOpenAIProtocolChat sends one streamed chat completion on the openai
// protocol, marked for conversion exactly as the /llm handler marks it.
func streamOpenAIProtocolChat(t *testing.T, d *fakeDIAL, tools []schemas.ChatTool) dialStreamResult {
	t.Helper()
	core := newCore(t, dialAccount(t, d))
	ctx := dialCtx("gpt-5", DialProtocolOpenAI)
	ctx.SetValue(schemas.BifrostContextKeyChangeRequestType, schemas.ResponsesRequest)
	req := reasoningChat("gpt-5", nil)
	req.Params.Tools = tools
	ch, bErr := core.ChatCompletionStreamRequest(ctx, req)
	if bErr != nil {
		t.Fatalf("ChatCompletionStreamRequest: %+v", bErr)
	}
	return collectDialStream(t, ch)
}

func TestDialOpenAIProtocolStreamsChatThroughResponses(t *testing.T) {
	d := newFakeDIAL(t)
	got := streamOpenAIProtocolChat(t, d, nil)

	req := d.only(t)
	if req.path != "/openai/v1/responses" || req.body["stream"] != true {
		t.Fatalf("request = %s stream=%v, want a streamed /openai/v1/responses", req.path, req.body["stream"])
	}
	if req.query.Has("api-version") {
		t.Errorf("api-version = %q was sent; the openai profile sends none", req.query.Get("api-version"))
	}
	if got.text != "Hello" {
		t.Errorf("assembled text = %q, want %q", got.text, "Hello")
	}
	if got.finishReason == "" {
		t.Error("no chunk carried a finish_reason")
	}
	assertDialStreamUsage(t, got.usage)
}

func TestDialOpenAIProtocolStreamsToolCalls(t *testing.T) {
	d := newFakeDIAL(t)
	got := streamOpenAIProtocolChat(t, d, []schemas.ChatTool{weatherTool()})

	req := d.only(t)
	tools, _ := req.body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v, want the one declared tool in the Responses body", req.body["tools"])
	}
	if tool, _ := tools[0].(map[string]any); tool["name"] != "get_weather" || tool["type"] != "function" {
		t.Errorf("tool = %v, want the Responses function tool get_weather", tools[0])
	}
	if got.toolName != "get_weather" || got.toolCallID != "call_1" {
		t.Errorf("tool call = %q (id %q), want get_weather (id call_1)", got.toolName, got.toolCallID)
	}
	if got.toolArgs != `{"city":"Paris"}` {
		t.Errorf("tool arguments = %q, want the concatenated argument deltas", got.toolArgs)
	}
	if !strings.Contains(got.finishReason, "tool") && got.finishReason != "stop" {
		t.Errorf("finish_reason = %q, want a terminal reason", got.finishReason)
	}
	assertDialStreamUsage(t, got.usage)
}

func assertDialStreamUsage(t *testing.T, usage *schemas.BifrostLLMUsage) {
	t.Helper()
	if usage == nil {
		t.Fatal("no chunk carried usage; the stream settler would bill zero tokens")
	}
	if usage.PromptTokens != dialStreamInputTokens || usage.CompletionTokens != dialStreamOutputTokens {
		t.Errorf("usage = (%d, %d), want (%d, %d)", usage.PromptTokens, usage.CompletionTokens,
			dialStreamInputTokens, dialStreamOutputTokens)
	}
}
