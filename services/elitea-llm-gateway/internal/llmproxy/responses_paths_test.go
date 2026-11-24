package llmproxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

// BF0.3b covers the less-exercised whitelisted paths: the OpenAI Responses API
// round-trip and the Anthropic count_tokens sub-path (plus its 404 sibling).
//
// Dialect note: the Responses API does not carry the chat-completions
// vocabulary literally. Its analog of `response_format` is the `text.format`
// object (schemas.ResponsesParameters.Text.Format), and its analog of
// `finish_reason` is the terminal `status` field (completed/failed/... — there
// is no `finish_reason` key in the Responses schema). These tests assert
// against the real dialect fields.

// TestResponsesAPIRoundTrip posts a Responses-API body carrying a structured
// response_format (text.format) and asserts:
//   - HTTP 200,
//   - the response_format survived decode + ToBifrostResponsesRequest into the
//     core request struct (request side),
//   - a terminal finish reason (status) is present on the serialized response.
func TestResponsesAPIRoundTrip(t *testing.T) {
	fake := &fakeRouter{respResp: &schemas.BifrostResponsesResponse{
		ID:     strPtr("resp-rt"),
		Object: "response",
		Model:  "gpt-4o",
		Status: strPtr(schemas.ResponsesResponseStatusCompleted),
	}}
	h := newTestHandler(fake, nil, nil)

	// text.format is the Responses-API response_format. json_schema with a
	// strict schema is the most structured shape a caller can send.
	body := `{
		"model":"openai/gpt-4o",
		"input":"return json",
		"text":{"format":{"type":"json_schema","name":"reply","strict":true,"schema":{"type":"object"}}}
	}`
	rec := postJSON(t, h.route(), "/llm/v1/responses", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Request side: response_format (text.format) preserved through decode.
	if fake.lastResponsesReq == nil {
		t.Fatal("router never received a decoded Responses request")
	}
	params := fake.lastResponsesReq.Params
	if params == nil || params.Text == nil || params.Text.Format == nil {
		t.Fatalf("response_format (text.format) not preserved: params=%+v", params)
	}
	if got := params.Text.Format.Type; got != "json_schema" {
		t.Errorf("text.format.type = %q, want json_schema", got)
	}
	if got := params.Text.Format.Name; got == nil || *got != "reply" {
		t.Errorf("text.format.name = %v, want reply", got)
	}
	// Model string is split into provider/model by the converter; the openai/
	// prefix must resolve to the OpenAI provider, not leak into the model.
	if fake.lastResponsesReq.Model != "gpt-4o" {
		t.Errorf("model = %q, want gpt-4o", fake.lastResponsesReq.Model)
	}
	if fake.lastResponsesReq.Provider != schemas.OpenAI {
		t.Errorf("provider = %q, want openai", fake.lastResponsesReq.Provider)
	}

	// Response side: terminal finish reason (status) present in the JSON body.
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q, want application/json (unary, non-SSE)", ct)
	}
	var out struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal response: %v; body=%s", err, rec.Body.String())
	}
	if out.Status != schemas.ResponsesResponseStatusCompleted {
		t.Errorf("status (finish_reason) = %q, want completed", out.Status)
	}
	if out.ID != "resp-rt" {
		t.Errorf("id = %q, want resp-rt", out.ID)
	}
}

// TestResponsesAPIRoundTrip_MinimalStringInput proves the string-input form
// (input as a bare string, no text.format) also round-trips to a 200 with a
// terminal status — the response_format field is optional.
func TestResponsesAPIRoundTrip_MinimalStringInput(t *testing.T) {
	fake := &fakeRouter{respResp: &schemas.BifrostResponsesResponse{
		ID:     strPtr("resp-min"),
		Object: "response",
		Status: strPtr(schemas.ResponsesResponseStatusCompleted),
	}}
	h := newTestHandler(fake, nil, nil)

	rec := postJSON(t, h.route(), "/llm/v1/responses", `{"model":"gpt-4o","input":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if fake.lastResponsesReq == nil || len(fake.lastResponsesReq.Input) == 0 {
		t.Fatalf("string input not decoded into a Responses message: %+v", fake.lastResponsesReq)
	}
	if p := fake.lastResponsesReq.Params; p != nil && p.Text != nil && p.Text.Format != nil {
		t.Errorf("no response_format sent, but text.format decoded as %+v", p.Text.Format)
	}
}

// TestCountTokensSubPath posts to the Anthropic count_tokens sub-path and
// asserts a synchronous (non-SSE) 200 body, then asserts an unknown
// /llm/v1/messages/{suffix} returns 404 rather than being misrouted to the
// OpenAI catch-all or the streaming Messages handler.
func TestCountTokensSubPath(t *testing.T) {
	total := 42
	fake := &fakeRouter{countResp: &schemas.BifrostCountTokensResponse{
		Model:       "claude-3-5-sonnet",
		InputTokens: 42,
		TotalTokens: &total,
	}}
	h := newTestHandler(fake, nil, nil)

	rec := postJSON(t, h.route(), "/llm/v1/messages/count_tokens",
		`{"model":"anthropic/claude-3-5-sonnet","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("count_tokens status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// Synchronous: JSON body, NOT text/event-stream.
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q, want application/json (synchronous, non-SSE)", ct)
	}
	if strings.Contains(ct, "event-stream") {
		t.Errorf("count_tokens must not stream; content-type = %q", ct)
	}
	body := rec.Body.String()
	if strings.Contains(body, "data:") || strings.Contains(body, "event:") {
		t.Errorf("count_tokens body has SSE framing; got:\n%s", body)
	}
	// Anthropic count_tokens wire shape is {"input_tokens": N}.
	var out struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal count_tokens: %v; body=%s", err, body)
	}
	if out.InputTokens != 42 {
		t.Errorf("input_tokens = %d, want 42", out.InputTokens)
	}

	// An unknown messages sub-path is a structured 404, not a misroute.
	recNF := postJSON(t, h.route(), "/llm/v1/messages/retrieve", `{"model":"claude","max_tokens":1,"messages":[]}`)
	if recNF.Code != http.StatusNotFound {
		t.Fatalf("unknown /messages/{suffix} status = %d, want 404; body=%s", recNF.Code, recNF.Body.String())
	}
	var nfErr openAIError
	if err := json.Unmarshal(recNF.Body.Bytes(), &nfErr); err != nil {
		t.Fatalf("unmarshal 404 body: %v; body=%s", err, recNF.Body.String())
	}
	if nfErr.Error.Type != "invalid_request_error" {
		t.Errorf("404 error type = %q, want invalid_request_error", nfErr.Error.Type)
	}
}
