package llmproxy

// stream_disconnect_review_test.go — review findings on the caller-disconnect
// estimate (issue #1201, PR #1558): the slow-close reader is bounded by the
// drain pool, the prompt estimate counts tool calls in every dialect, the
// relayed-byte counters are pinned, and the estimate is attached to streams
// only.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

// TestDisconnect_SlowClose_ReaderBoundedByDrainPool: a provider that ignores
// the cancel never closes its channel, so each disconnect needs a background
// reader for up to drainHardTimeout. The readers take drain-pool slots: one
// project never holds more than its per-project share, the extra disconnects
// abandon the channel, and another project still gets a slot.
func TestDisconnect_SlowClose_ReaderBoundedByDrainPool(t *testing.T) {
	gate := allowGate()
	events := newRecordingEvents()
	h := NewHandler(&wedgedRouter{}, nil, nil,
		WithBudgetGate(gate, &fakeCostEstimator{totalNano: 1_000}),
		WithOpsEventPublisher(events),
		WithStreamDrainLimit(16), // per-project share = 16/8 = 2
		WithStreamGrace(MaxStreamGrace))
	_, _, perProject := h.drainLimit.snapshot()
	if perProject != 2 {
		t.Fatalf("per-project share = %d, want 2", perProject)
	}

	const disconnects = 5
	var wg sync.WaitGroup
	for range disconnects {
		wg.Go(func() {
			h.Chat(&failAfterWriter{okWrites: 1}, chatReqWithProject(t, "30", true))
		})
	}
	wg.Wait()

	if inUse, _, _ := h.drainLimit.snapshot(); inUse != perProject {
		t.Fatalf("drain slots in use after %d disconnects from one project = %d, want %d (the "+
			"per-project share): a slow-close reader must take a slot, and a full pool must not "+
			"start one", disconnects, inUse, perProject)
	}

	// Another project is not starved by the noisy one.
	h.Chat(&failAfterWriter{okWrites: 1}, chatReqWithProject(t, "31", true))
	if inUse, _, _ := h.drainLimit.snapshot(); inUse != perProject+1 {
		t.Fatalf("drain slots in use after a disconnect from another project = %d, want %d",
			inUse, perProject+1)
	}

	h.DrainBilling()
	if inUse, _, _ := h.drainLimit.snapshot(); inUse != 0 {
		t.Errorf("drain slots in use after DrainBilling = %d, want 0: a reader must release its slot", inUse)
	}

	// Every disconnect is still billed; the abandoned ones say so.
	outcomes := map[string]int{}
	events.mu.Lock()
	for _, raw := range events.events {
		var env softAlertEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("unmarshal envelope: %v", err)
		}
		if env.Type != unbilledStreamEventType {
			continue
		}
		var p unbilledStreamPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		outcomes[p.DrainOutcome]++
	}
	events.mu.Unlock()
	if got, want := outcomes[drainOutcomeCancelledAbandoned], disconnects-perProject; got != want {
		t.Errorf("%s events = %d, want %d (outcomes %v)", drainOutcomeCancelledAbandoned, got, want, outcomes)
	}
	if got, want := outcomes[drainOutcomeCancelledSlowClose], perProject+1; got != want {
		t.Errorf("%s events = %d, want %d (outcomes %v)", drainOutcomeCancelledSlowClose, got, want, outcomes)
	}
}

// bigToolArgs is a large tool-call argument string: 4 000 bytes of payload is at
// least 1 000 estimated tokens, far above every other part of the test
// prompts.
var bigToolArgs = `{"q":"` + strings.Repeat("x", 4000) + `"}`

// disconnectBilledInput posts a streamed request through handle, makes the
// first write to the caller fail (a disconnect), and returns the billed input
// tokens: the prompt estimate, since the silent provider reports no usage.
func disconnectBilledInput(t *testing.T, path string, body any, first *schemas.BifrostStreamChunk,
	handle func(h *Handler, w http.ResponseWriter, r *http.Request),
) int64 {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	gate := allowGate()
	calc := &fakeCostEstimator{inputRateNano: gateInputRateNano, outputRateNano: gateOutputRateNano}
	h := NewHandler(newSilentRouter(first), nil, nil,
		WithBudgetGate(gate, calc),
		WithStreamGrace(MaxStreamGrace))
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerProjectID, "30")

	handle(h, &failAfterWriter{okWrites: 0}, req)
	gate.waitForUpdate(t)
	h.DrainBilling()
	in, _ := calc.getLastTokens()
	return in
}

// TestDisconnectEstimate_CountsToolCalls: an agent loop carries most of its
// prompt in earlier tool calls. Each dialect must count them as input: chat
// assistant tool_calls, Responses function_call items, Anthropic tool_use
// blocks.
func TestDisconnectEstimate_CountsToolCalls(t *testing.T) {
	want := tokensFromBytes(int64(len(bigToolArgs)))

	t.Run("chat tool_calls", func(t *testing.T) {
		body := map[string]any{
			"model": "openai/gpt-4o", "stream": true,
			"messages": []any{
				map[string]any{"role": "user", "content": "hi"},
				map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
					"id": "call_1", "type": "function",
					"function": map[string]any{"name": "lookup", "arguments": bigToolArgs},
				}}},
				map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "ok"},
			},
		}
		in := disconnectBilledInput(t, "/llm/v1/chat/completions", body, chatDelta("c1", "x"),
			func(h *Handler, w http.ResponseWriter, r *http.Request) { h.Chat(w, r) })
		if in < want {
			t.Errorf("billed input = %d, want >= %d: the assistant tool-call arguments are prompt", in, want)
		}
	})

	t.Run("responses function_call", func(t *testing.T) {
		body := map[string]any{
			"model": "openai/gpt-4o", "stream": true,
			"input": []any{
				map[string]any{"role": "user", "content": "hi"},
				map[string]any{"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": bigToolArgs},
				map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "ok"},
			},
		}
		in := disconnectBilledInput(t, "/llm/v1/responses", body, responsesDelta("x"),
			func(h *Handler, w http.ResponseWriter, r *http.Request) { h.Responses(w, r) })
		if in < want {
			t.Errorf("billed input = %d, want >= %d: the function_call arguments are prompt", in, want)
		}
	})

	t.Run("responses function_call_output", func(t *testing.T) {
		body := map[string]any{
			"model": "openai/gpt-4o", "stream": true,
			"input": []any{
				map[string]any{"role": "user", "content": "hi"},
				map[string]any{"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": "{}"},
				map[string]any{"type": "function_call_output", "call_id": "call_1", "output": bigToolArgs},
			},
		}
		in := disconnectBilledInput(t, "/llm/v1/responses", body, responsesDelta("x"),
			func(h *Handler, w http.ResponseWriter, r *http.Request) { h.Responses(w, r) })
		if in < want {
			t.Errorf("billed input = %d, want >= %d: the function_call_output is prompt", in, want)
		}
	})

	t.Run("anthropic tool_use", func(t *testing.T) {
		var input any
		if err := json.Unmarshal([]byte(bigToolArgs), &input); err != nil {
			t.Fatal(err)
		}
		body := map[string]any{
			"model": "anthropic/claude-3-5-sonnet", "max_tokens": 10, "stream": true,
			"messages": []any{
				map[string]any{"role": "user", "content": "hi"},
				map[string]any{"role": "assistant", "content": []any{map[string]any{
					"type": "tool_use", "id": "toolu_1", "name": "lookup", "input": input,
				}}},
				map[string]any{"role": "user", "content": []any{map[string]any{
					"type": "tool_result", "tool_use_id": "toolu_1", "content": "ok",
				}}},
			},
		}
		in := disconnectBilledInput(t, "/llm/v1/messages", body, responsesDelta("x"),
			func(h *Handler, w http.ResponseWriter, r *http.Request) { h.Messages(w, r) })
		if in < want {
			t.Errorf("billed input = %d, want >= %d: the tool_use input is prompt", in, want)
		}
	})

	t.Run("chat tool definitions", func(t *testing.T) {
		body := map[string]any{
			"model": "openai/gpt-4o", "stream": true,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			"tools": []any{map[string]any{"type": "function", "function": map[string]any{
				"name": "lookup", "description": strings.Repeat("d", 4000),
				"parameters": map[string]any{"type": "object"},
			}}},
		}
		in := disconnectBilledInput(t, "/llm/v1/chat/completions", body, chatDelta("c1", "x"),
			func(h *Handler, w http.ResponseWriter, r *http.Request) { h.Chat(w, r) })
		if in < want {
			t.Errorf("billed input = %d, want >= %d: tool definitions are prompt", in, want)
		}
	})
}

// TestChatPromptEstimate_ExactToolCallCount pins the exact chat rule: text,
// plus each tool call's name and arguments.
func TestChatPromptEstimate_ExactToolCallCount(t *testing.T) {
	text, name, args := "hi", "lookup", `{"q":"abc"}`
	req := &schemas.BifrostChatRequest{Input: []schemas.ChatMessage{
		{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &text}},
		{Role: schemas.ChatMessageRoleAssistant, ChatAssistantMessage: &schemas.ChatAssistantMessage{
			ToolCalls: []schemas.ChatAssistantMessageToolCall{{
				Function: schemas.ChatAssistantMessageToolCallFunction{Name: &name, Arguments: args},
			}},
		}},
	}}
	if got, want := chatPromptEstimate(req), tokensFromBytes(int64(len(text)+len(name)+len(args))); got != want {
		t.Errorf("chatPromptEstimate = %d, want %d", got, want)
	}
}

// TestDeltaBytes_CountingIsPinned: chatDeltaBytes and responsesDeltaBytes feed
// the disconnect estimate's output floor, so what they count is billed. A
// change to either must change this test on purpose.
func TestDeltaBytes_CountingIsPinned(t *testing.T) {
	content, refusal, reasoning, args := "abc", "de", "f", "ghij"
	chat := &schemas.BifrostStreamChunk{BifrostChatResponse: &schemas.BifrostChatResponse{
		Choices: []schemas.BifrostResponseChoice{{
			ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
				Delta: &schemas.ChatStreamResponseChoiceDelta{
					Content: &content, Refusal: &refusal, Reasoning: &reasoning,
					ToolCalls: []schemas.ChatAssistantMessageToolCall{{
						Function: schemas.ChatAssistantMessageToolCallFunction{Arguments: args},
					}},
				},
			},
		}},
	}}
	if got := chatDeltaBytes(chat); got != 10 {
		t.Errorf("chatDeltaBytes = %d, want 10 (content + refusal + reasoning + tool arguments)", got)
	}

	delta := responsesDelta("hello")
	if got := responsesDeltaBytes(delta); got != 5 {
		t.Errorf("responsesDeltaBytes = %d, want 5 (the Delta only)", got)
	}
	done := responsesTrailer(1, 2)
	if got := responsesDeltaBytes(done); got != 0 {
		t.Errorf("responsesDeltaBytes(completed) = %d, want 0", got)
	}

	// The relay loops count a written event the same way.
	s := &streamSettler{}
	s.relayedResponses(delta.BifrostResponsesStreamResponse, done.BifrostResponsesStreamResponse, nil)
	if s.relayedBytes != 5 || s.relayedChunks != 1 {
		t.Errorf("relayedResponses = (%d bytes, %d chunks), want (5, 1)", s.relayedBytes, s.relayedChunks)
	}
}

// estimateProbeRouter records whether the dispatched context carries a prompt
// estimate.
type estimateProbeRouter struct {
	fakeRouter
	unary, stream atomic.Int32 // 1 = estimate present, 2 = absent
}

func probe(ctx *schemas.BifrostContext) int32 {
	if promptEstimateFrom(ctx) != nil {
		return 1
	}
	return 2
}

func (r *estimateProbeRouter) ChatCompletionRequest(ctx *schemas.BifrostContext, _ *schemas.BifrostChatRequest) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	r.unary.Store(probe(ctx))
	return nil, &schemas.BifrostError{Error: &schemas.ErrorField{Message: "probe"}}
}

func (r *estimateProbeRouter) ChatCompletionStreamRequest(ctx *schemas.BifrostContext, _ *schemas.BifrostChatRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	r.stream.Store(probe(ctx))
	ch := make(chan *schemas.BifrostStreamChunk, 1)
	ch <- chatTrailer(1, 1)
	close(ch)
	return ch, nil
}

// TestPromptEstimate_AttachedToStreamsOnly: a request that does not stream
// never bills an estimate, so it must not pay for the closure either.
func TestPromptEstimate_AttachedToStreamsOnly(t *testing.T) {
	r := &estimateProbeRouter{}
	h := NewHandler(r, nil, nil)

	h.Chat(httptest.NewRecorder(), chatReqWithProject(t, "30", false))
	if got := r.unary.Load(); got != 2 {
		t.Errorf("unary dispatch probe = %d, want 2 (no prompt estimate on a non-streaming request)", got)
	}
	h.Chat(httptest.NewRecorder(), chatReqWithProject(t, "30", true))
	if got := r.stream.Load(); got != 1 {
		t.Errorf("stream dispatch probe = %d, want 1 (a streaming request carries the prompt estimate)", got)
	}
	h.DrainBilling()
}
