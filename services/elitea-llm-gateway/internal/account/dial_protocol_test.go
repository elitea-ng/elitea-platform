package account

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

// dial_protocol_test.go — legacy issue #6707, the account half.
//
// Each proof drives a REAL bifrost/core over the real EliteaAccount against a
// fake AI DIAL endpoint that serves the routes the issue measured on a real
// DIAL deployment:
//
//	/openai/deployments/{m}/chat/completions  200, the default route
//	/openai/deployments/{m}/embeddings        200
//	/openai/v1/chat/completions               404 "Route is not found"
//	/openai/v1/responses                      200 (gpt only)
//	/anthropic/v1/messages                    200, native Anthropic
//
// The assertions are what the upstream RECEIVED: the path, the auth header,
// the query and the body. A status code alone is no evidence, because the
// fake answers 200 on every route that DIAL serves.

// dialRequest is one request the fake DIAL received.
type dialRequest struct {
	path   string
	query  url.Values
	header http.Header
	body   map[string]any
}

// fakeDIAL is the fake AI DIAL endpoint.
type fakeDIAL struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []dialRequest
}

func newFakeDIAL(t *testing.T) *fakeDIAL {
	t.Helper()
	d := &fakeDIAL{}
	d.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		d.mu.Lock()
		d.reqs = append(d.reqs, dialRequest{
			path: r.URL.Path, query: r.URL.Query(), header: r.Header.Clone(), body: body,
		})
		d.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/openai/v1/responses" && body["stream"] == true:
			writeDialResponsesStream(w, body)
		case strings.HasPrefix(r.URL.Path, "/openai/deployments/") &&
			strings.HasSuffix(r.URL.Path, "/chat/completions") && body["stream"] == true:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`data: {"id":"c1","object":"chat.completion.chunk","created":0,"model":"m",` +
				`"choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}` +
				"\n\ndata: [DONE]\n\n"))
		case strings.HasPrefix(r.URL.Path, "/openai/deployments/") &&
			strings.HasSuffix(r.URL.Path, "/chat/completions"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "chatcmpl-1", "object": "chat.completion", "created": 0, "model": body["model"],
				"choices": []map[string]any{{
					"index": 0, "finish_reason": "stop",
					"message": map[string]any{"role": "assistant", "content": "hi"},
				}},
				"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
			})
		case strings.HasPrefix(r.URL.Path, "/openai/deployments/") &&
			strings.HasSuffix(r.URL.Path, "/embeddings"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": "list", "model": "text-embedding-3-small",
				"data":  []map[string]any{{"object": "embedding", "index": 0, "embedding": []float64{0.1, 0.2}}},
				"usage": map[string]any{"prompt_tokens": 1, "total_tokens": 1},
			})
		case r.URL.Path == "/openai/v1/responses":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "resp_1", "object": "response", "created_at": 0, "status": "completed",
				"model": body["model"],
				"output": []map[string]any{{
					"type": "message", "id": "msg_1", "status": "completed", "role": "assistant",
					"content": []map[string]any{{"type": "output_text", "text": "hi"}},
				}},
				"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
			})
		case r.URL.Path == "/anthropic/v1/messages":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "msg_1", "type": "message", "role": "assistant", "model": body["model"],
				"content":     []map[string]any{{"type": "text", "text": "hi"}},
				"stop_reason": "end_turn",
				"usage":       map[string]any{"input_tokens": 1, "output_tokens": 1},
			})
		default:
			// DIAL's own answer for a route it does not serve, including the
			// documented-but-unserved /openai/v1/chat/completions.
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"Route is not found"}}`))
		}
	}))
	t.Cleanup(d.Close)
	return d
}

func (d *fakeDIAL) only(t *testing.T) dialRequest {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.reqs) != 1 {
		t.Fatalf("DIAL received %d requests, want exactly 1: %+v", len(d.reqs), d.reqs)
	}
	return d.reqs[0]
}

// dialAccount builds an account over one ai_dial credential that points at the
// fake DIAL and carries an api_version, so a test can see whether the version
// leaked onto a versionless route.
func dialAccount(t *testing.T, d *fakeDIAL) *EliteaAccount {
	t.Helper()
	return dialAccountOfType(t, d, DialCredentialType)
}

// dialAccountOfType is dialAccount over a credential row of configType. The
// row's own type is what decides whether the DIAL routes apply.
func dialAccountOfType(t *testing.T, d *fakeDIAL, configType string) *EliteaAccount {
	t.Helper()
	row := credentialRow("dial-1", "epam-dial", map[string]any{
		"api_base":    d.URL,
		"api_key":     "dial-key",
		"api_version": "2024-02-01",
	})
	return accountWithRowsAt(t, d.URL, [][]any{append(row, configType)})
}

// dialCtx is the context the /llm handler builds for a model on protocol p:
// the project, the dispatched model name, and the credential pin that carries
// the protocol.
func dialCtx(model string, p DialProtocol) *schemas.BifrostContext {
	bc := bifrostCtx(callerProject)
	bc.SetValue(ContextKeyRequestModel, model)
	bc.SetValue(ContextKeyLinkedCredential, LinkedCredential{
		ProjectID: callerProject, ConfigID: "dial-1", Title: "epam-dial", DialProtocol: p,
	})
	bc.SetValue(ContextKeyDispatchKind, DispatchChat)
	return bc
}

func reasoningChat(model string, reasoning *schemas.ChatReasoning) *schemas.BifrostChatRequest {
	return &schemas.BifrostChatRequest{
		Provider: schemas.Azure,
		Model:    model,
		Input:    []schemas.ChatMessage{chatUserMessage("hello")},
		Params: &schemas.ChatParameters{
			MaxCompletionTokens: schemas.Ptr(8192),
			Reasoning:           reasoning,
		},
	}
}

// TestDialAnthropicProtocolSendsThinkingToTheMessagesRoute is the headline
// proof for the anthropic protocol. The model name does NOT contain "claude",
// so bifrost's name guess would send it to the Azure-shaped route; the
// protocol must decide instead. The thinking request must arrive intact, which
// is the capability the issue says the Azure-shaped route blanket-rejects.
func TestDialAnthropicProtocolSendsThinkingToTheMessagesRoute(t *testing.T) {
	d := newFakeDIAL(t)
	core := newCore(t, dialAccount(t, d))

	resp, bErr := core.ChatCompletionRequest(
		dialCtx("dial-sonnet-4-5", DialProtocolAnthropic),
		reasoningChat("dial-sonnet-4-5", &schemas.ChatReasoning{MaxTokens: schemas.Ptr(2048)}))
	if bErr != nil {
		t.Fatalf("ChatCompletionRequest: %+v", bErr)
	}
	if resp == nil || len(resp.Choices) == 0 {
		t.Fatalf("empty completion: %+v", resp)
	}

	got := d.only(t)
	if got.path != "/anthropic/v1/messages" {
		t.Fatalf("path = %q, want the native Messages route /anthropic/v1/messages", got.path)
	}
	if v := got.header.Get("X-Api-Key"); v != "dial-key" {
		t.Errorf("x-api-key = %q, want the credential's key", v)
	}
	if v := got.header.Get("Api-Key"); v != "" {
		t.Errorf("api-key = %q, want none on the Anthropic route", v)
	}
	if got.query.Has("api-version") {
		t.Errorf("api-version = %q was sent; the Messages route takes none", got.query.Get("api-version"))
	}
	thinking, _ := got.body["thinking"].(map[string]any)
	if thinking == nil || thinking["type"] != "enabled" {
		t.Fatalf("thinking = %v, want the enabled thinking block to reach DIAL", got.body["thinking"])
	}
	if budget, _ := thinking["budget_tokens"].(float64); budget != 2048 {
		t.Errorf("thinking.budget_tokens = %v, want 2048", thinking["budget_tokens"])
	}
}

// TestDialOpenAIProtocolSendsReasoningToTheResponsesRoute is the proof for the
// openai protocol. The /llm handler marks a chat completion for conversion
// (llmproxy/modelmap.go); the context below carries the same mark. The
// reasoning effort must arrive as the Responses API's own field, which is what
// lets the legacy `reasoning_in_body_for` workaround retire.
func TestDialOpenAIProtocolSendsReasoningToTheResponsesRoute(t *testing.T) {
	d := newFakeDIAL(t)
	core := newCore(t, dialAccount(t, d))

	ctx := dialCtx("gpt-5", DialProtocolOpenAI)
	ctx.SetValue(schemas.BifrostContextKeyChangeRequestType, schemas.ResponsesRequest)
	resp, bErr := core.ChatCompletionRequest(ctx,
		reasoningChat("gpt-5", &schemas.ChatReasoning{Effort: schemas.Ptr("low")}))
	if bErr != nil {
		t.Fatalf("ChatCompletionRequest: %+v", bErr)
	}
	if resp == nil || len(resp.Choices) == 0 {
		t.Fatalf("empty completion: the Responses answer was not converted back to chat: %+v", resp)
	}

	got := d.only(t)
	if got.path != "/openai/v1/responses" {
		t.Fatalf("path = %q, want /openai/v1/responses", got.path)
	}
	if v := got.header.Get("Api-Key"); v != "dial-key" {
		t.Errorf("api-key = %q, want the credential's key", v)
	}
	if got.query.Has("api-version") {
		t.Errorf("api-version = %q was sent; the verified openai profile sends none", got.query.Get("api-version"))
	}
	reasoning, _ := got.body["reasoning"].(map[string]any)
	if reasoning == nil || reasoning["effort"] != "low" {
		t.Fatalf("reasoning = %v, want {effort: low} at the top level of the Responses body", got.body["reasoning"])
	}
	if _, nested := got.body["extra_body"]; nested {
		t.Error("extra_body was sent; the reasoning must not need the legacy nesting workaround")
	}
}

// TestDialOpenAIProtocolResponsesRequest covers the caller that already speaks
// the Responses API (/llm/v1/responses): it needs no conversion, only the
// family and the versionless route.
func TestDialOpenAIProtocolResponsesRequest(t *testing.T) {
	d := newFakeDIAL(t)
	core := newCore(t, dialAccount(t, d))

	_, bErr := core.ResponsesRequest(dialCtx("gpt-5", DialProtocolOpenAI), &schemas.BifrostResponsesRequest{
		Provider: schemas.Azure,
		Model:    "gpt-5",
		Input:    []schemas.ResponsesMessage{responsesUserMessage("hello")},
		Params:   &schemas.ResponsesParameters{Reasoning: &schemas.ResponsesParametersReasoning{Effort: schemas.Ptr("high")}},
	})
	if bErr != nil {
		t.Fatalf("ResponsesRequest: %+v", bErr)
	}
	got := d.only(t)
	if got.path != "/openai/v1/responses" || got.query.Has("api-version") {
		t.Fatalf("request = %s?%s, want /openai/v1/responses with no api-version", got.path, got.query.Encode())
	}
	if reasoning, _ := got.body["reasoning"].(map[string]any); reasoning == nil || reasoning["effort"] != "high" {
		t.Fatalf("reasoning = %v, want {effort: high}", got.body["reasoning"])
	}
}

// TestDialDefaultProtocolUsesTheDeploymentRoute is the control, and the fix
// for the route DIAL does not serve. bifrost builds /openai/v1/chat/completions
// for every non-Claude Azure chat; DIAL answers it 404. The default protocol
// must reach the deployment route with the credential's api-version, streamed
// or not, and an embedding must reach the deployment embeddings route.
func TestDialDefaultProtocolUsesTheDeploymentRoute(t *testing.T) {
	for name, p := range map[string]DialProtocol{"zero": "", "azure": DialProtocolAzure} {
		t.Run(name+"/chat", func(t *testing.T) {
			d := newFakeDIAL(t)
			core := newCore(t, dialAccount(t, d))
			resp, bErr := core.ChatCompletionRequest(dialCtx("gpt-4o", p),
				reasoningChat("gpt-4o", &schemas.ChatReasoning{Effort: schemas.Ptr("low")}))
			if bErr != nil {
				t.Fatalf("ChatCompletionRequest: %+v", bErr)
			}
			if resp == nil || len(resp.Choices) == 0 {
				t.Fatalf("empty completion: %+v", resp)
			}
			got := d.only(t)
			if got.path != "/openai/deployments/gpt-4o/chat/completions" {
				t.Fatalf("path = %q, want the deployment route DIAL serves", got.path)
			}
			if v := got.query.Get("api-version"); v != "2024-02-01" || len(got.query) != 1 {
				t.Errorf("query = %q, want exactly the credential's api-version", got.query.Encode())
			}
			if v := got.header.Get("Api-Key"); v != "dial-key" {
				t.Errorf("api-key = %q, want the credential's key", v)
			}
		})
		t.Run(name+"/embedding", func(t *testing.T) {
			d := newFakeDIAL(t)
			core := newCore(t, dialAccount(t, d))
			ctx := dialCtx("text-embedding-3-small", p)
			ctx.SetValue(ContextKeyDispatchKind, DispatchEmbedding)
			_, bErr := core.EmbeddingRequest(ctx, &schemas.BifrostEmbeddingRequest{
				Provider: schemas.Azure, Model: "text-embedding-3-small",
				Input: &schemas.EmbeddingInput{Text: schemas.Ptr("hello")},
			})
			if bErr != nil {
				t.Fatalf("EmbeddingRequest: %+v", bErr)
			}
			got := d.only(t)
			if got.path != "/openai/deployments/text-embedding-3-small/embeddings" ||
				got.query.Get("api-version") != "2024-02-01" {
				t.Fatalf("request = %s?%s, want the deployment embeddings route", got.path, got.query.Encode())
			}
		})
	}
	// A Gemini deployment is not gpt and not Claude: the deployment route is
	// the only DIAL route that serves it.
	t.Run("gemini", func(t *testing.T) {
		d := newFakeDIAL(t)
		core := newCore(t, dialAccount(t, d))
		ch, bErr := core.ChatCompletionStreamRequest(dialCtx("gemini-2.5-pro", ""),
			reasoningChat("gemini-2.5-pro", nil))
		if bErr != nil {
			t.Fatalf("ChatCompletionStreamRequest: %+v", bErr)
		}
		for range ch {
		}
		if got := d.only(t); got.path != "/openai/deployments/gemini-2.5-pro/chat/completions" {
			t.Fatalf("stream path = %q, want the deployment route", got.path)
		}
	})
}

// TestDialDefaultProtocolKeepsTheClaudeRoute proves the default protocol does
// not move a model that already worked: bifrost reads a Claude name as the
// Anthropic family and sends it to the Messages route, and so does the probe
// (DialRouteFor).
func TestDialDefaultProtocolKeepsTheClaudeRoute(t *testing.T) {
	d := newFakeDIAL(t)
	core := newCore(t, dialAccount(t, d))
	if _, bErr := core.ChatCompletionRequest(dialCtx("claude-sonnet-4-5", ""),
		reasoningChat("claude-sonnet-4-5", nil)); bErr != nil {
		t.Fatalf("ChatCompletionRequest: %+v", bErr)
	}
	if got := d.only(t); got.path != "/anthropic/v1/messages" {
		t.Fatalf("path = %q, want the Messages route a Claude name took before", got.path)
	}
	if DialRouteFor("", "claude-sonnet-4-5") != DialRouteMessages {
		t.Error("DialRouteFor disagrees with the runtime for a Claude name")
	}
}

// TestDialDeploymentRouteNeedsAKnownOperation keeps every request the gateway
// did not mark (the Responses API, text completion, audio) on the key it had
// before: the api-version alias and no endpoint override.
func TestDialDeploymentRouteNeedsAKnownOperation(t *testing.T) {
	a := dialAccount(t, newFakeDIAL(t))
	ctx := dialCtx("gpt-4o", "")
	ctx.SetValue(ContextKeyDispatchKind, DispatchKind(""))
	keys, err := a.GetKeysForProvider(ctx, schemas.Azure)
	if err != nil || len(keys) != 1 {
		t.Fatalf("GetKeysForProvider = %v, %v", keys, err)
	}
	alias := keys[0].Aliases["gpt-4o"]
	if alias.AzureAliasCfg == nil || alias.Endpoint != nil ||
		alias.APIVersion == nil || *alias.APIVersion != "2024-02-01" {
		t.Fatalf("alias = %+v, want the api-version alias and no endpoint override", alias)
	}
}

// TestDialRoutesNeedAnAIDialRow is the type gate. Three credential types share
// the Azure provider. An azure_open_ai credential serves
// /openai/v1/chat/completions, so it keeps that route, and a protocol on the
// pin does not reach it, whatever the model row claims about the link.
func TestDialRoutesNeedAnAIDialRow(t *testing.T) {
	for _, configType := range []string{"azure_open_ai", "open_ai_azure"} {
		for _, p := range []DialProtocol{"", DialProtocolOpenAI, DialProtocolAnthropic} {
			t.Run(configType+"/"+string(p), func(t *testing.T) {
				a := dialAccountOfType(t, newFakeDIAL(t), configType)
				keys, err := a.GetKeysForProvider(dialCtx("gpt-4o", p), schemas.Azure)
				if err != nil || len(keys) != 1 {
					t.Fatalf("GetKeysForProvider = %v, %v", keys, err)
				}
				alias := keys[0].Aliases["gpt-4o"]
				if alias.ModelFamily != nil {
					t.Errorf("alias.ModelFamily = %q, want none on a %s row", *alias.ModelFamily, configType)
				}
				if alias.AzureAliasCfg == nil || alias.Endpoint != nil ||
					alias.APIVersion == nil || *alias.APIVersion != "2024-02-01" {
					t.Fatalf("alias = %+v, want only the api-version alias", alias)
				}
			})
		}
	}
}

// TestAzureCredentialWithoutAPIKeyIsRefused: an Azure-class credential with no
// api_key would make bifrost authenticate with DefaultAzureCredential, which
// sends the gateway's own ambient Azure token to the tenant's endpoint. The
// account refuses it before dispatch, and the endpoint receives nothing.
func TestAzureCredentialWithoutAPIKeyIsRefused(t *testing.T) {
	for _, configType := range []string{DialCredentialType, "azure_open_ai", "open_ai_azure"} {
		for name, apiKey := range map[string]any{"absent": nil, "empty": "", "blank": "  "} {
			t.Run(configType+"/"+name, func(t *testing.T) {
				d := newFakeDIAL(t)
				data := map[string]any{"api_base": d.URL}
				if apiKey != nil {
					data["api_key"] = apiKey
				}
				row := append(credentialRow("dial-1", "epam-dial", data), configType)
				a := accountWithRowsAt(t, d.URL, [][]any{row})
				_, err := a.GetKeysForProvider(dialCtx("gpt-4o", ""), schemas.Azure)
				if !errors.Is(err, ErrIncompleteCredential) {
					t.Fatalf("error = %v, want %v", err, ErrIncompleteCredential)
				}
				if !strings.Contains(err.Error(), "api_key") {
					t.Errorf("error = %v, want it to name api_key", err)
				}

				core := newCore(t, a)
				if _, bErr := core.ChatCompletionRequest(dialCtx("gpt-4o", ""), reasoningChat("gpt-4o", nil)); bErr == nil {
					t.Fatal("the chat was dispatched with no api_key")
				}
				d.mu.Lock()
				defer d.mu.Unlock()
				if len(d.reqs) != 0 {
					t.Fatalf("the endpoint received %d requests; want none", len(d.reqs))
				}
			})
		}
	}
}

// TestDialProtocolForcesTheFamilyOnTheKey pins the key object for both
// non-default protocols: one alias for the dispatched model, the forced
// family, and no api-version.
func TestDialProtocolForcesTheFamilyOnTheKey(t *testing.T) {
	for p, want := range map[DialProtocol]schemas.ModelFamily{
		DialProtocolAnthropic: schemas.ModelFamilyAnthropic,
		DialProtocolOpenAI:    schemas.ModelFamilyOpenAI,
	} {
		t.Run(string(p), func(t *testing.T) {
			a := dialAccount(t, newFakeDIAL(t))
			keys, err := a.GetKeysForProvider(dialCtx("model-x", p), schemas.Azure)
			if err != nil {
				t.Fatalf("GetKeysForProvider: %v", err)
			}
			if len(keys) != 1 || len(keys[0].Aliases) != 1 {
				t.Fatalf("keys = %+v, want one key with one alias", keys)
			}
			alias := keys[0].Aliases["model-x"]
			if alias.ModelID != "model-x" {
				t.Errorf("alias.ModelID = %q, want the dispatched name unchanged", alias.ModelID)
			}
			if alias.ModelFamily == nil || *alias.ModelFamily != want {
				t.Fatalf("alias.ModelFamily = %v, want %q", alias.ModelFamily, want)
			}
			if alias.AzureAliasCfg != nil {
				t.Errorf("alias.AzureAliasCfg = %+v, want none: both routes are versionless", alias.AzureAliasCfg)
			}
		})
	}
}

// TestDialProtocolNeedsTheDispatchedModel refuses a forced protocol that has no
// model name to key the alias by. Taking the default route instead would send
// the model to the route its configuration chose against.
func TestDialProtocolNeedsTheDispatchedModel(t *testing.T) {
	a := dialAccount(t, newFakeDIAL(t))
	ctx := dialCtx("", DialProtocolAnthropic)
	_, err := a.GetKeysForProvider(ctx, schemas.Azure)
	if !errors.Is(err, ErrDialProtocolUnroutable) {
		t.Fatalf("error = %v, want %v", err, ErrDialProtocolUnroutable)
	}
	if got := credentialRejectionReason(err); got != DialProtocolUnroutableReason {
		t.Errorf("reason = %q, want %q", got, DialProtocolUnroutableReason)
	}
}

// TestDialProtocolReachesAzureKeysOnly proves the protocol is a statement about
// the Azure-shaped DIAL endpoint and nothing else. A pin that carries it on a
// vLLM credential builds the vLLM key unchanged.
func TestDialProtocolReachesAzureKeysOnly(t *testing.T) {
	upstream := newRecordingProvider(t)
	defer upstream.Close()
	a := newChatTestAccount(t, upstream.URL, map[string][][]any{
		callerProject: {vllmRow("v-1", "team-vllm", "k", upstream.URL, false)},
	})
	bc := bifrostCtx(callerProject)
	bc.SetValue(ContextKeyRequestModel, "m")
	bc.SetValue(ContextKeyLinkedCredential, LinkedCredential{
		ProjectID: callerProject, ConfigID: "v-1", DialProtocol: DialProtocolAnthropic,
	})
	keys, err := a.GetKeysForProvider(bc, schemas.VLLM)
	if err != nil {
		t.Fatalf("GetKeysForProvider: %v", err)
	}
	if len(keys) != 1 || keys[0].Aliases != nil || keys[0].UseAnthropicEndpoints != nil {
		t.Fatalf("vllm key = %+v, want no alias and no Anthropic switch", keys)
	}
}

func TestParseDialProtocol(t *testing.T) {
	for raw, want := range map[string]struct {
		p     DialProtocol
		known bool
	}{
		"":               {DialProtocolAzure, true},
		"azure":          {DialProtocolAzure, true},
		" OpenAI ":       {DialProtocolOpenAI, true},
		"anthropic":      {DialProtocolAnthropic, true},
		"bedrock":        {DialProtocolAzure, false},
		"<not a string>": {DialProtocolAzure, false},
	} {
		p, known := ParseDialProtocol(raw)
		if p != want.p || known != want.known {
			t.Errorf("ParseDialProtocol(%q) = (%q, %v), want (%q, %v)", raw, p, known, want.p, want.known)
		}
	}
	if !DialProtocolOpenAI.ConvertsChatToResponses() ||
		DialProtocolAnthropic.ConvertsChatToResponses() || DialProtocolAzure.ConvertsChatToResponses() {
		t.Error("only the openai protocol converts a chat completion into a Responses request")
	}
}
