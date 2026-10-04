package llmproxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/account"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/requestlog"
)

// dial_protocol_stream_test.go — a streamed /llm chat completion on the AI
// DIAL openai protocol, end to end through the gateway (legacy issue #6707).
//
// The path under test is the one the workers use: POST /llm/v1/chat/completions
// with stream:true for an ai_dial model on the openai protocol. The handler
// marks the chat for conversion (applyDialRequestShape), REAL bifrost core
// sends it through ResponsesStream to a fake DIAL /openai/v1/responses SSE
// endpoint, rewrites each Responses event into a chat chunk, and the /llm SSE
// writer and the stream settler (streamOpenAI, stream_drain.go) do the rest.
// The test reads what the client received and what the request log billed.
//
// The account here is a minimal schemas.Account that builds the key the real
// account builds for this protocol (one Azure key, the forced openai family
// on the dispatched model). account/dial_protocol_test.go proves the real
// account builds exactly that key; the real account cannot be built outside
// its package without a database.

const (
	dialStreamPrompt     = 21
	dialStreamCompletion = 5
)

// dialResponsesSSE is a fake DIAL that serves /openai/v1/responses as SSE.
type dialResponsesSSE struct {
	*httptest.Server
	mu    sync.Mutex
	paths []string
	body  map[string]any
}

func newDialResponsesSSE(t *testing.T) *dialResponsesSSE {
	t.Helper()
	d := &dialResponsesSSE{}
	d.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		d.mu.Lock()
		d.paths = append(d.paths, r.URL.Path)
		d.body = body
		d.mu.Unlock()
		if r.URL.Path != "/openai/v1/responses" || body["stream"] != true {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"Route is not found"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		seq := 0
		send := func(event map[string]any) {
			event["sequence_number"] = seq
			seq++
			data, _ := json.Marshal(event)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		resp := func(status string, usage map[string]any) map[string]any {
			out := map[string]any{"id": "resp_9", "object": "response", "created_at": 1,
				"status": status, "model": body["model"], "output": []any{}}
			if usage != nil {
				out["usage"] = usage
			}
			return out
		}
		send(map[string]any{"type": "response.created", "response": resp("in_progress", nil)})
		send(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{
			"type": "message", "id": "msg_9", "role": "assistant", "status": "in_progress", "content": []any{}}})
		for _, delta := range []string{"Bon", "jour"} {
			send(map[string]any{"type": "response.output_text.delta", "item_id": "msg_9",
				"output_index": 0, "content_index": 0, "delta": delta})
		}
		send(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{
			"type": "message", "id": "msg_9", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": "Bonjour"}}}})
		send(map[string]any{"type": "response.completed", "response": resp("completed", map[string]any{
			"input_tokens": dialStreamPrompt, "output_tokens": dialStreamCompletion,
			"total_tokens": dialStreamPrompt + dialStreamCompletion,
		})})
	}))
	t.Cleanup(d.Close)
	return d
}

// dialStreamAccount serves one Azure key on the fake DIAL, with the family
// the openai protocol forces on the dispatched model.
type dialStreamAccount struct{ endpoint string }

func (a dialStreamAccount) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	return []schemas.ModelProvider{schemas.Azure}, nil
}

func (a dialStreamAccount) GetKeysForProvider(ctx context.Context, provider schemas.ModelProvider) ([]schemas.Key, error) {
	if provider != schemas.Azure {
		return nil, nil
	}
	link, _ := ctx.Value(account.ContextKeyLinkedCredential).(account.LinkedCredential)
	if link.DialProtocol != account.DialProtocolOpenAI {
		return nil, fmt.Errorf("the pin carries protocol %q, want openai", link.DialProtocol)
	}
	model, _ := ctx.Value(account.ContextKeyRequestModel).(string)
	return []schemas.Key{{
		ID: "cred-dial", Name: "team-dial", Value: schemas.SecretVar{Val: "dial-key", SecretType: schemas.SecretTypePlainText},
		Models:         schemas.WhiteList{"*"},
		AzureKeyConfig: &schemas.AzureKeyConfig{Endpoint: schemas.SecretVar{Val: a.endpoint, SecretType: schemas.SecretTypePlainText}},
		Aliases: schemas.KeyAliases{model: schemas.AliasConfig{
			ModelID: model, ModelFamily: schemas.Ptr(schemas.ModelFamilyOpenAI),
		}},
	}}, nil
}

func (a dialStreamAccount) GetConfigForProvider(schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	cfg := &schemas.ProviderConfig{
		ConcurrencyAndBufferSize: schemas.ConcurrencyAndBufferSize{Concurrency: 1, BufferSize: 10},
	}
	cfg.NetworkConfig.AllowPrivateNetwork = true
	return cfg, nil
}

// sseChatFrames parses the `data:` frames of an OpenAI chat SSE body.
func sseChatFrames(t *testing.T, body string) (chunks []map[string]any, done bool) {
	t.Helper()
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			done = true
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("frame is not JSON: %q", payload)
		}
		chunks = append(chunks, chunk)
	}
	return chunks, done
}

func TestDialOpenAIProtocolStreamedChatThroughTheGateway(t *testing.T) {
	d := newDialResponsesSSE(t)
	core, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: dialStreamAccount{endpoint: d.URL}, InitialPoolSize: 1,
	})
	if err != nil {
		t.Fatalf("bifrost.Init: %v", err)
	}
	t.Cleanup(core.Shutdown)

	db := &fakeModelDB{
		rows: []fakeModelRow{dialModelRow(`"openai"`)},
		credsBySchema: map[string][]fakeCredentialRow{
			mapProjectID: {{id: "cred-dial", typ: account.DialCredentialType, title: "team-dial"}},
		},
	}
	h := NewHandler(NewBifrostRouter(core), nil, nil,
		WithModelResolver(NewModelResolver(ModelResolverConfig{DB: db})))

	sink := &logCaptureSink{}
	recorder := requestlog.New(sink, discardLogger())
	rec := postAs(t, requestlog.Middleware(recorder)(h.route()), "/llm/v1/chat/completions", mapProjectID,
		`{"model":"Team Model","stream":true,"stream_options":{"include_usage":true},`+
			`"messages":[{"role":"user","content":"hi"}]}`)
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	recorder.Stop(stopCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	d.mu.Lock()
	paths := append([]string(nil), d.paths...)
	d.mu.Unlock()
	if len(paths) != 1 || paths[0] != "/openai/v1/responses" {
		t.Fatalf("DIAL received %v, want exactly one /openai/v1/responses", paths)
	}

	chunks, done := sseChatFrames(t, rec.Body.String())
	if !done {
		t.Error("the stream did not end with data: [DONE]")
	}
	var text, finish string
	var usage map[string]any
	for _, chunk := range chunks {
		if chunk["object"] != "chat.completion.chunk" {
			t.Fatalf("frame object = %v, want chat.completion.chunk: %v", chunk["object"], chunk)
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}
		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			choice, _ := c.(map[string]any)
			if reason, _ := choice["finish_reason"].(string); reason != "" {
				finish = reason
			}
			if delta, _ := choice["delta"].(map[string]any); delta != nil {
				if content, _ := delta["content"].(string); content != "" {
					text += content
				}
			}
		}
	}
	if text != "Bonjour" {
		t.Errorf("assembled content = %q, want %q", text, "Bonjour")
	}
	if finish == "" {
		t.Error("no frame carried a finish_reason")
	}
	if usage == nil || usage["prompt_tokens"] != float64(dialStreamPrompt) ||
		usage["completion_tokens"] != float64(dialStreamCompletion) {
		t.Errorf("usage frame = %v, want prompt %d / completion %d", usage, dialStreamPrompt, dialStreamCompletion)
	}

	rows := sink.all()
	if len(rows) != 1 {
		t.Fatalf("logged rows = %d, want 1", len(rows))
	}
	if rows[0].PromptToks != dialStreamPrompt || rows[0].OutputToks != dialStreamCompletion {
		t.Errorf("settled tokens = (%d, %d), want (%d, %d)",
			rows[0].PromptToks, rows[0].OutputToks, dialStreamPrompt, dialStreamCompletion)
	}
	if !rows[0].Streaming {
		t.Error("the request log row is not marked streaming")
	}
}
