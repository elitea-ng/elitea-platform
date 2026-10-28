package providerwire

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/providers/openai"
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
)

// workerChatBody is the request shape of the refused project-context run
// (execution 0d6927e1…, 2026-10-28): the ad hoc persona text followed by the
// active project-context block, the read_project_context tool, and a short
// user turn. The markup in the system text is what the upstream edge scored.
const workerChatBody = `{"model":"eu.anthropic.claude-haiku-4-5-20251001-v1:0","messages":[` +
	`{"role":"system","content":"Use a skeptical, critically analytical style with dry humor.\n\n` +
	`<active_instruction id=\"project-context:118:2\" revision=\"703b0c43\">\nThe revision verification color is LIME-953.\n</active_instruction>"},` +
	`{"role":"user","content":"say \"hi\""}],` +
	`"tools":[{"type":"function","function":{"name":"read_project_context",` +
	`"description":"Read the frozen project instructions. They remain active for this run.",` +
	`"parameters":{"type":"object","properties":{},"additionalProperties":false}}}],` +
	`"tool_choice":"auto","stream":true,"stream_options":{"include_usage":true},` +
	`"max_completion_tokens":4096,"temperature":0.6}`

// captureUpstream is a fake OpenAI-compatible upstream that keeps the body of
// each chat request and answers one streamed chunk.
type captureUpstream struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (c *captureUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.bodies = append(c.bodies, body)
	c.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m",`+
		`"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`+"\n\n"+
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],`+
		`"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`+"\n\ndata: [DONE]\n\n")
}

// vllmAccount serves one vLLM key on the capture upstream, as
// internal/account builds it for an OpenAI-compatible credential.
type vllmAccount struct{ root string }

func (vllmAccount) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	return []schemas.ModelProvider{schemas.VLLM}, nil
}

func (a vllmAccount) GetKeysForProvider(_ context.Context, provider schemas.ModelProvider) ([]schemas.Key, error) {
	if provider != schemas.VLLM {
		return nil, nil
	}
	return []schemas.Key{{
		ID: "cred", Name: "cred", Models: schemas.WhiteList{"*"},
		Value:         schemas.SecretVar{Val: "test-key", SecretType: schemas.SecretTypePlainText},
		VLLMKeyConfig: &schemas.VLLMKeyConfig{URL: schemas.SecretVar{Val: a.root, SecretType: schemas.SecretTypePlainText}},
	}}, nil
}

func (vllmAccount) GetConfigForProvider(schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	cfg := &schemas.ProviderConfig{
		ConcurrencyAndBufferSize: schemas.ConcurrencyAndBufferSize{Concurrency: 1, BufferSize: 10},
	}
	cfg.NetworkConfig.AllowPrivateNetwork = true
	return cfg, nil
}

// TestProviderBodiesAreCompact sends the refused run's chat request through
// real bifrost core to a vLLM upstream, as the /llm handler does, and asserts
// the upstream received compact JSON with the instruction text intact. Without
// this package the body arrives two-space indented and the first assertion
// fails.
func TestProviderBodiesAreCompact(t *testing.T) {
	upstream := &captureUpstream{}
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)

	core, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: vllmAccount{root: server.URL}, InitialPoolSize: 1,
	})
	if err != nil {
		t.Fatalf("bifrost.Init: %v", err)
	}
	t.Cleanup(core.Shutdown)

	var req openai.OpenAIChatRequest
	if err := json.Unmarshal([]byte(workerChatBody), &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	ctx := schemas.NewBifrostContext(context.Background(), time.Now().Add(10*time.Second))
	chat := req.ToBifrostChatRequest(ctx)
	chat.Provider = schemas.VLLM
	stream, bErr := core.ChatCompletionStreamRequest(ctx, chat)
	if bErr != nil {
		t.Fatalf("ChatCompletionStreamRequest: %+v", bErr.Error)
	}
	for range stream { // drain until the provider closes the stream
	}

	upstream.mu.Lock()
	bodies := upstream.bodies
	upstream.mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("upstream received %d requests, want 1", len(bodies))
	}
	body := bodies[0]
	var compact bytes.Buffer
	if err := json.Compact(&compact, body); err != nil {
		t.Fatalf("upstream body is not JSON: %v", err)
	}
	if !bytes.Equal(compact.Bytes(), body) {
		t.Fatalf("upstream body carries insignificant whitespace (%d bytes, %d compact):\n%s",
			len(body), compact.Len(), body)
	}

	var sent struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name       string          `json:"name"`
				Parameters json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("decode upstream body: %v", err)
	}
	if len(sent.Messages) != 2 || sent.Messages[0].Role != "system" ||
		!strings.Contains(sent.Messages[0].Content, "<active_instruction id=\"project-context:118:2\"") ||
		!strings.Contains(sent.Messages[0].Content, "LIME-953") {
		t.Errorf("instruction text changed on the wire: %+v", sent.Messages)
	}
	if len(sent.Tools) != 1 || sent.Tools[0].Function.Name != "read_project_context" {
		t.Fatalf("tool declaration changed on the wire: %s", body)
	}
	var params map[string]any
	if err := json.Unmarshal(sent.Tools[0].Function.Parameters, &params); err != nil ||
		params["type"] != "object" || params["additionalProperties"] != false || len(params) != 3 {
		t.Errorf("tool parameters changed on the wire: %s", sent.Tools[0].Function.Parameters)
	}
}

// TestMarshalSortedIndentIsCompactAndSorted pins the replaced encoder itself:
// keys stay sorted (prompt-cache stability) and no whitespace is added.
func TestMarshalSortedIndentIsCompactAndSorted(t *testing.T) {
	got, err := providerUtils.MarshalSortedIndent(map[string]any{"b": 1, "a": []any{"<x>", 2}}, "", "  ")
	if err != nil {
		t.Fatalf("MarshalSortedIndent: %v", err)
	}
	if want := `{"a":["<x>",2],"b":1}`; string(got) != want {
		t.Fatalf("MarshalSortedIndent = %s, want %s", got, want)
	}
}
