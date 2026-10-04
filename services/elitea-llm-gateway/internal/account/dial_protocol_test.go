package account

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
//	/openai/v1/chat/completions  404 "Route is not found"
//	/openai/v1/responses         200 (gpt only)
//	/anthropic/v1/messages       200, native Anthropic
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
		switch r.URL.Path {
		case "/openai/v1/responses":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "resp_1", "object": "response", "created_at": 0, "status": "completed",
				"model": body["model"],
				"output": []map[string]any{{
					"type": "message", "id": "msg_1", "status": "completed", "role": "assistant",
					"content": []map[string]any{{"type": "output_text", "text": "hi"}},
				}},
				"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
			})
		case "/anthropic/v1/messages":
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
	return accountWithRowsAt(t, d.URL, [][]any{
		credentialRow("dial-1", "epam-dial", map[string]any{
			"api_base":    d.URL,
			"api_key":     "dial-key",
			"api_version": "2024-02-01",
		}),
	})
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

// TestDialDefaultProtocolIsUnchanged is the control. The default protocol, and
// a pin with no protocol at all, must build exactly the key the gateway built
// before the field existed: the credential's api-version alias, and no family.
// The same chat request then takes the Azure-shaped route, which the fake DIAL
// answers 404 — the defect the openai protocol exists to avoid.
func TestDialDefaultProtocolIsUnchanged(t *testing.T) {
	for name, p := range map[string]DialProtocol{"zero": "", "azure": DialProtocolAzure} {
		t.Run(name, func(t *testing.T) {
			d := newFakeDIAL(t)
			a := dialAccount(t, d)

			keys, err := a.GetKeysForProvider(dialCtx("gpt-5", p), schemas.Azure)
			if err != nil {
				t.Fatalf("GetKeysForProvider: %v", err)
			}
			if len(keys) != 1 {
				t.Fatalf("keys = %d, want 1", len(keys))
			}
			alias, ok := keys[0].Aliases["gpt-5"]
			if !ok || alias.AzureAliasCfg == nil || alias.APIVersion == nil ||
				*alias.APIVersion != "2024-02-01" {
				t.Fatalf("alias = %+v, want the credential's api-version alias exactly as before", alias)
			}
			if alias.ModelFamily != nil {
				t.Fatalf("alias.ModelFamily = %q, want none on the default protocol", *alias.ModelFamily)
			}

			core := newCore(t, a)
			_, _ = core.ChatCompletionRequest(dialCtx("gpt-5", p),
				reasoningChat("gpt-5", &schemas.ChatReasoning{Effort: schemas.Ptr("low")}))
			if got := d.only(t); got.path != "/openai/v1/chat/completions" {
				t.Fatalf("path = %q, want the unchanged Azure-shaped chat route", got.path)
			}
		})
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
