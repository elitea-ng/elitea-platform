package llmproxy

// Regression tests for the guardrail review findings on #1545:
//
//   - the input stage reads tool results on the Responses and Messages
//     routes, as it does on Chat;
//   - the output stage reads tool-call arguments, unary and streamed, on
//     Chat, Responses and Messages, and a mask keeps them valid JSON;
//   - agent-scoped rows resolve the agent without the rate limiter (no
//     NATS), and an unknown agent applies the row's fail mode.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/guardrail"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/policy"
)

// responsesCapture records the Responses request it dispatched.
type responsesCapture struct {
	trackingRouter
	mu   sync.Mutex
	last *schemas.BifrostResponsesRequest
}

func (c *responsesCapture) ResponsesRequest(ctx *schemas.BifrostContext, req *schemas.BifrostResponsesRequest) (*schemas.BifrostResponsesResponse, *schemas.BifrostError) {
	c.mu.Lock()
	c.last = req
	c.mu.Unlock()
	return c.trackingRouter.ResponsesRequest(ctx, req)
}

func (c *responsesCapture) dispatched(t *testing.T) string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil {
		t.Fatal("no request reached the provider")
	}
	raw, err := json.Marshal(c.last.Input)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func assertNoRawSecret(t *testing.T, where, text string) {
	t.Helper()
	if strings.Contains(text, guardTestSecret[:8]) || strings.Contains(text, guardTestSecret[12:24]) {
		t.Fatalf("%s carries raw secret text: %s", where, text)
	}
}

func guardPost(path, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerProjectID, "42")
	return req
}

func TestGuardrailInputMasksResponsesToolResults(t *testing.T) {
	router := &responsesCapture{}
	router.respResp = &schemas.BifrostResponsesResponse{ID: strPtr("resp-1")}
	h := guardHandler(t, &router.trackingRouter, secretsRow("input", "mask"))
	h.router = router
	body := `{"model":"openai/gpt-4o","input":[
		{"type":"function_call","call_id":"c1","name":"read","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"file holds ` + guardTestSecret + `"},
		{"type":"function_call_output","call_id":"c2","output":[{"type":"input_text","text":"block ` + guardTestSecret + `"}]}]}`
	resp, verdicts := serveLogged(t, h.Responses, guardPost("/llm/v1/responses", body))
	if resp.Code != http.StatusOK {
		t.Fatalf("status %d %s", resp.Code, resp.Body.String())
	}
	sent := router.dispatched(t)
	assertNoRawSecret(t, "the provider request", sent)
	if strings.Count(sent, guardrail.MaskMarker("github-token")) != 2 {
		t.Fatalf("both tool results must be masked: %s", sent)
	}
	assertVerdictWithoutContent(t, verdicts, guardrail.OutcomeMasked)
}

func TestGuardrailInputBlocksAResponsesToolResult(t *testing.T) {
	router := &trackingRouter{}
	h := guardHandler(t, router, secretsRow("input", "block"))
	body := `{"model":"openai/gpt-4o","input":[{"type":"function_call_output","call_id":"c1","output":"` + guardTestSecret + `"}]}`
	resp, _ := serveLogged(t, h.Responses, guardPost("/llm/v1/responses", body))
	if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), guardrailCodeBlocked) {
		t.Fatalf("status %d %s", resp.Code, resp.Body.String())
	}
	if router.called.Load() {
		t.Fatal("the provider was called for a blocked tool result")
	}
}

func TestGuardrailInputMasksAnthropicToolResults(t *testing.T) {
	router := &responsesCapture{}
	router.respResp = &schemas.BifrostResponsesResponse{ID: strPtr("resp-1")}
	h := guardHandler(t, &router.trackingRouter, secretsRow("input", "mask"))
	h.router = router
	body := `{"model":"anthropic/claude-3-5-sonnet","max_tokens":10,"messages":[
		{"role":"user","content":"read the file"},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"read","input":{}},{"type":"tool_use","id":"t2","name":"read","input":{}}]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"t1","content":"file holds ` + guardTestSecret + `"},
			{"type":"tool_result","tool_use_id":"t2","content":[{"type":"text","text":"block ` + guardTestSecret + `"}]}]}]}`
	resp, _ := serveLogged(t, h.Messages, guardPost("/llm/v1/messages", body))
	if resp.Code != http.StatusOK {
		t.Fatalf("status %d %s", resp.Code, resp.Body.String())
	}
	sent := router.dispatched(t)
	assertNoRawSecret(t, "the provider request", sent)
	if strings.Count(sent, guardrail.MaskMarker("github-token")) != 2 {
		t.Fatalf("both tool results must be masked: %s", sent)
	}
}

// toolArgs is tool-call arguments that hold the secret, with a nested value
// and a non-string value that must survive the mask.
func toolArgs() string {
	return `{"path":"/etc/app.env","content":{"token":"` + guardTestSecret + `","n":3},"tags":["a"]}`
}

// assertMaskedArgs checks that args is valid JSON, masks the secret and keeps
// every other value.
func assertMaskedArgs(t *testing.T, args string) {
	t.Helper()
	var doc struct {
		Path    string `json:"path"`
		Content struct {
			Token string `json:"token"`
			N     int    `json:"n"`
		} `json:"content"`
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(args), &doc); err != nil {
		t.Fatalf("the masked arguments are not valid JSON: %v: %q", err, args)
	}
	if doc.Content.Token != guardrail.MaskMarker("github-token") || doc.Path != "/etc/app.env" || doc.Content.N != 3 || len(doc.Tags) != 1 {
		t.Fatalf("masked arguments = %q", args)
	}
}

func TestGuardrailUnaryChatToolCallArguments(t *testing.T) {
	answer := func() *schemas.BifrostChatResponse {
		return &schemas.BifrostChatResponse{ID: "x", Choices: []schemas.BifrostResponseChoice{{
			ChatNonStreamResponseChoice: &schemas.ChatNonStreamResponseChoice{Message: &schemas.ChatMessage{
				Role: schemas.ChatMessageRoleAssistant,
				ChatAssistantMessage: &schemas.ChatAssistantMessage{ToolCalls: []schemas.ChatAssistantMessageToolCall{{
					ID: strPtr("call_1"), Type: strPtr("function"),
					Function: schemas.ChatAssistantMessageToolCallFunction{Name: strPtr("write"), Arguments: toolArgs()},
				}}},
			}},
		}}}
	}
	router := &trackingRouter{}
	router.chatResp = answer()
	h := guardHandler(t, router, secretsRow("output", "mask"))
	resp, verdicts := serveLogged(t, h.Chat, chatReqWithProject(t, "42", false))
	assertNoRawSecret(t, "the answer", resp.Body.String())
	var body struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil || len(body.Choices) != 1 || len(body.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("answer %d %s", resp.Code, resp.Body.String())
	}
	assertMaskedArgs(t, body.Choices[0].Message.ToolCalls[0].Function.Arguments)
	assertVerdictWithoutContent(t, verdicts, guardrail.OutcomeMasked)

	router2 := &trackingRouter{}
	router2.chatResp = answer()
	h2 := guardHandler(t, router2, secretsRow("output", "block"))
	resp2, _ := serveLogged(t, h2.Chat, chatReqWithProject(t, "42", false))
	if resp2.Code != http.StatusBadRequest || !strings.Contains(resp2.Body.String(), guardrailCodeBlocked) {
		t.Fatalf("block on tool arguments = %d %s", resp2.Code, resp2.Body.String())
	}
}

func chatToolDelta(id, name *string, args string, finish bool) *schemas.BifrostStreamChunk {
	tc := schemas.ChatAssistantMessageToolCall{Index: 0, ID: id, Function: schemas.ChatAssistantMessageToolCallFunction{Name: name, Arguments: args}}
	if id != nil {
		tc.Type = strPtr("function")
	}
	choice := schemas.BifrostResponseChoice{ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
		Delta: &schemas.ChatStreamResponseChoiceDelta{ToolCalls: []schemas.ChatAssistantMessageToolCall{tc}},
	}}
	if finish {
		choice.FinishReason = strPtr("tool_calls")
		choice.Delta = &schemas.ChatStreamResponseChoiceDelta{}
	}
	return &schemas.BifrostStreamChunk{BifrostChatResponse: &schemas.BifrostChatResponse{
		ID: "chatcmpl-1", Object: "chat.completion.chunk", Model: "gpt-4o",
		Choices: []schemas.BifrostResponseChoice{choice},
	}}
}

// sseToolArgs concatenates the tool-call argument deltas of a chat SSE body.
func sseToolArgs(t *testing.T, body string) (string, []string) {
	t.Helper()
	var args strings.Builder
	var errs []string
	for _, line := range strings.Split(body, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var frame struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Function struct {
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Error *openAIErrorFields `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &frame); err != nil {
			t.Fatalf("bad frame %q: %v", data, err)
		}
		if frame.Error != nil {
			errs = append(errs, frame.Error.Code)
		}
		for _, c := range frame.Choices {
			for _, tc := range c.Delta.ToolCalls {
				args.WriteString(tc.Function.Arguments)
			}
		}
	}
	return args.String(), errs
}

func streamedChatToolCall() chan *schemas.BifrostStreamChunk {
	args := toolArgs()
	cut := strings.Index(args, guardTestSecret) + 10
	return newChunkChan(
		chatToolDelta(strPtr("call_1"), strPtr("write"), "", false),
		chatToolDelta(nil, nil, args[:cut], false),
		chatToolDelta(nil, nil, args[cut:], false),
		chatToolDelta(nil, nil, "", true),
	)
}

func TestGuardrailStreamedChatToolCallArguments(t *testing.T) {
	router := &trackingRouter{}
	router.streamChan = streamedChatToolCall()
	h := guardHandler(t, router, secretsRow("output", "mask"))
	resp, verdicts := serveLogged(t, h.Chat, chatReqWithProject(t, "42", true))
	assertNoRawSecret(t, "the stream", resp.Body.String())
	args, errs := sseToolArgs(t, resp.Body.String())
	if len(errs) != 0 {
		t.Fatalf("stream errors %v", errs)
	}
	assertMaskedArgs(t, args)
	if !strings.Contains(resp.Body.String(), `"name":"write"`) || !strings.Contains(resp.Body.String(), "[DONE]") {
		t.Fatalf("the call id and name or [DONE] are missing: %s", resp.Body.String())
	}
	assertVerdictWithoutContent(t, verdicts, guardrail.OutcomeMasked)

	router2 := &trackingRouter{}
	router2.streamChan = streamedChatToolCall()
	h2 := guardHandler(t, router2, secretsRow("output", "block"))
	resp2, _ := serveLogged(t, h2.Chat, chatReqWithProject(t, "42", true))
	_, errs2 := sseToolArgs(t, resp2.Body.String())
	if len(errs2) != 1 || errs2[0] != guardrailCodeBlocked {
		t.Fatalf("block on streamed tool arguments: %v %s", errs2, resp2.Body.String())
	}
	assertNoRawSecret(t, "the blocked stream", resp2.Body.String())
}

func responsesToolEvents() chan *schemas.BifrostStreamChunk {
	args := toolArgs()
	cut := strings.Index(args, guardTestSecret) + 10
	ev := func(sr *schemas.BifrostResponsesStreamResponse) *schemas.BifrostStreamChunk {
		sr.OutputIndex = guardrailIndex(0)
		return &schemas.BifrostStreamChunk{BifrostResponsesStreamResponse: sr}
	}
	item := func(arguments string) *schemas.ResponsesMessage {
		typ := schemas.ResponsesMessageTypeFunctionCall
		return &schemas.ResponsesMessage{ID: strPtr("fc_1"), Type: &typ, ResponsesToolMessage: &schemas.ResponsesToolMessage{
			CallID: strPtr("call_1"), Name: strPtr("write"), Arguments: strPtr(arguments),
		}}
	}
	return newChunkChan(
		ev(&schemas.BifrostResponsesStreamResponse{Type: schemas.ResponsesStreamResponseTypeOutputItemAdded, Item: item("")}),
		ev(&schemas.BifrostResponsesStreamResponse{Type: schemas.ResponsesStreamResponseTypeFunctionCallArgumentsDelta, ItemID: strPtr("fc_1"), Delta: strPtr(args[:cut])}),
		ev(&schemas.BifrostResponsesStreamResponse{Type: schemas.ResponsesStreamResponseTypeFunctionCallArgumentsDelta, ItemID: strPtr("fc_1"), Delta: strPtr(args[cut:])}),
		ev(&schemas.BifrostResponsesStreamResponse{Type: schemas.ResponsesStreamResponseTypeFunctionCallArgumentsDone, ItemID: strPtr("fc_1"), Arguments: strPtr(args)}),
		ev(&schemas.BifrostResponsesStreamResponse{Type: schemas.ResponsesStreamResponseTypeOutputItemDone, Item: item(args)}),
		&schemas.BifrostStreamChunk{BifrostResponsesStreamResponse: &schemas.BifrostResponsesStreamResponse{
			Type:     schemas.ResponsesStreamResponseTypeCompleted,
			Response: &schemas.BifrostResponsesResponse{Output: []schemas.ResponsesMessage{*item(args)}},
		}},
	)
}

func TestGuardrailStreamedResponsesToolCallArguments(t *testing.T) {
	router := &trackingRouter{}
	router.streamChan = responsesToolEvents()
	h := guardHandler(t, router, secretsRow("output", "mask"))
	resp, _ := serveLogged(t, h.Responses, guardPost("/llm/v1/responses", `{"model":"openai/gpt-4o","input":"hi","stream":true}`))
	assertNoRawSecret(t, "the stream", resp.Body.String())
	var deltas strings.Builder
	var dones []string
	for _, line := range strings.Split(resp.Body.String(), "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type      string  `json:"type"`
			Delta     *string `json:"delta"`
			Arguments *string `json:"arguments"`
			Item      *struct {
				Arguments *string `json:"arguments"`
			} `json:"item"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatalf("bad event %q: %v", data, err)
		}
		switch ev.Type {
		case string(schemas.ResponsesStreamResponseTypeFunctionCallArgumentsDelta):
			deltas.WriteString(*ev.Delta)
		case string(schemas.ResponsesStreamResponseTypeFunctionCallArgumentsDone):
			dones = append(dones, *ev.Arguments)
		case string(schemas.ResponsesStreamResponseTypeOutputItemDone):
			dones = append(dones, *ev.Item.Arguments)
		}
	}
	assertMaskedArgs(t, deltas.String())
	if len(dones) != 2 {
		t.Fatalf("done events = %v", dones)
	}
	for _, d := range dones {
		assertMaskedArgs(t, d)
	}
	if strings.Count(resp.Body.String(), "event: response.function_call_arguments.delta") != 1 {
		t.Fatalf("the held deltas were sent: %s", resp.Body.String())
	}
}

func TestGuardrailStreamedAnthropicToolUseInput(t *testing.T) {
	router := &trackingRouter{}
	router.streamChan = responsesToolEvents()
	h := guardHandler(t, router, secretsRow("output", "mask"))
	resp, _ := serveLogged(t, h.Messages, messagesStreamReqWithProject(t, "42"))
	assertNoRawSecret(t, "the stream", resp.Body.String())
	if !strings.Contains(resp.Body.String(), "input_json_delta") || !strings.Contains(resp.Body.String(), "REDACTED:github-token") {
		t.Fatalf("the masked tool input did not reach the client: %s", resp.Body.String())
	}

	router2 := &trackingRouter{}
	router2.streamChan = responsesToolEvents()
	h2 := guardHandler(t, router2, secretsRow("output", "block"))
	resp2, _ := serveLogged(t, h2.Messages, messagesStreamReqWithProject(t, "42"))
	if !strings.Contains(resp2.Body.String(), "event: error") || !strings.Contains(resp2.Body.String(), guardrailCodeBlocked) {
		t.Fatalf("no guardrail error event: %s", resp2.Body.String())
	}
	assertNoRawSecret(t, "the blocked stream", resp2.Body.String())
}

func TestGuardrailUnaryResponsesToolCallArguments(t *testing.T) {
	typ := schemas.ResponsesMessageTypeFunctionCall
	router := &trackingRouter{}
	router.respResp = &schemas.BifrostResponsesResponse{ID: strPtr("resp-1"), Output: []schemas.ResponsesMessage{{
		ID: strPtr("fc_1"), Type: &typ,
		ResponsesToolMessage: &schemas.ResponsesToolMessage{CallID: strPtr("call_1"), Name: strPtr("write"), Arguments: strPtr(toolArgs())},
	}}}
	h := guardHandler(t, router, secretsRow("output", "mask"))
	resp, _ := serveLogged(t, h.Responses, guardPost("/llm/v1/responses", `{"model":"openai/gpt-4o","input":"hi"}`))
	assertNoRawSecret(t, "the answer", resp.Body.String())
	var body struct {
		Output []struct {
			Arguments string `json:"arguments"`
		} `json:"output"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil || len(body.Output) != 1 {
		t.Fatalf("answer %d %s", resp.Code, resp.Body.String())
	}
	assertMaskedArgs(t, body.Output[0].Arguments)
}

// --- agent scope ----------------------------------------------------------

type failingAgentLookup struct{}

func (failingAgentLookup) LookupAgent(context.Context, string) (policy.AgentRef, bool, error) {
	return policy.AgentRef{}, false, errors.New("database down")
}

func agentGuardRow(failMode string) policy.Row {
	return govRow(policy.TypeGuardrail, "agent-"+failMode, map[string]any{
		"scope": map[string]any{"project_ids": []any{42.0}, "agent_ids": []any{3.0}},
		"guardrail": map[string]any{
			"stage": "input", "fail_mode": failMode,
			"detectors": []any{map[string]any{"type": "secrets", "action": "block"}},
		},
	})
}

func agentReq(t *testing.T, executionID string) *http.Request {
	t.Helper()
	body := `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"use ` + guardTestSecret + `"}]}`
	req := guardPost("/llm/v1/chat/completions", body)
	if executionID != "" {
		req.Header.Set(headerExecutionID, executionID)
	}
	return req
}

// agentHandler has NO rate limiter (the posture of a gateway without NATS).
func agentHandler(t *testing.T, resolver *policy.AgentResolver, rows ...policy.Row) *Handler {
	t.Helper()
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{ID: "ok"}
	opts := []HandlerOption{
		WithBudgetGate(allowingGate(), &fakeCostEstimator{}),
		WithGovernancePolicy(fixedPolicy{snapshotOf(t, rows...)}, nil, nil),
	}
	if resolver != nil {
		opts = append(opts, WithAgentResolver(resolver))
	}
	return NewHandler(router, nil, nil, opts...)
}

// TestGuardrailAgentScopeWithoutRateLimiter is the regression for agent rows
// that resolved the agent through the rate limiter: with no limiter (no
// NATS) they silently never applied.
func TestGuardrailAgentScopeWithoutRateLimiter(t *testing.T) {
	resolver := policy.NewAgentResolver(mapAgentLookup{
		"run-a": {ProjectID: 42, ApplicationID: 3},
		"run-b": {ProjectID: 42, ApplicationID: 4},
	}, nil, nil)
	h := agentHandler(t, resolver, agentGuardRow(guardrail.FailClosed))
	if rec := chatStatus(h, agentReq(t, "run-a")); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), guardrailCodeBlocked) {
		t.Fatalf("the agent row did not apply to its agent: %d %s", rec.Code, rec.Body.String())
	}
	if rec := chatStatus(h, agentReq(t, "run-b")); rec.Code != http.StatusOK {
		t.Fatalf("the agent row applied to another agent: %d %s", rec.Code, rec.Body.String())
	}
	if rec := chatStatus(h, agentReq(t, "")); rec.Code != http.StatusOK {
		t.Fatalf("the agent row applied to a call from no run: %d %s", rec.Code, rec.Body.String())
	}
}

func TestGuardrailAgentScopeUnresolvedAppliesTheFailMode(t *testing.T) {
	for name, resolver := range map[string]*policy.AgentResolver{
		"no resolver":   nil,
		"lookup failed": policy.NewAgentResolver(failingAgentLookup{}, nil, nil),
	} {
		before := GuardrailAgentUnresolved()
		closed := agentHandler(t, resolver, agentGuardRow(guardrail.FailClosed))
		resp, verdicts := serveLogged(t, closed.Chat, agentReq(t, "run-a"))
		if resp.Code != http.StatusServiceUnavailable || !strings.Contains(resp.Body.String(), guardrailCodeUnavailable) ||
			!strings.Contains(resp.Body.String(), guardrail.LabelAgentUnresolved) {
			t.Fatalf("%s: closed row = %d %s", name, resp.Code, resp.Body.String())
		}
		if len(verdicts) != 1 || verdicts[0].Outcome != guardrail.OutcomeErrorClosed || verdicts[0].Detector != guardrail.DetectorAgentScope {
			t.Fatalf("%s: verdicts = %+v", name, verdicts)
		}
		if GuardrailAgentUnresolved() <= before {
			t.Fatalf("%s: the unresolved counter did not move", name)
		}

		open := agentHandler(t, resolver, agentGuardRow(guardrail.FailOpen))
		resp, verdicts = serveLogged(t, open.Chat, agentReq(t, "run-a"))
		if resp.Code != http.StatusOK {
			t.Fatalf("%s: open row = %d %s", name, resp.Code, resp.Body.String())
		}
		if len(verdicts) != 1 || verdicts[0].Outcome != guardrail.OutcomeErrorOpen {
			t.Fatalf("%s: verdicts = %+v", name, verdicts)
		}

		// A call from no run has no agent to read: the row does not apply.
		if rec := chatStatus(closed, agentReq(t, "")); rec.Code != http.StatusOK {
			t.Fatalf("%s: a call from no run was refused: %d", name, rec.Code)
		}
	}
}
