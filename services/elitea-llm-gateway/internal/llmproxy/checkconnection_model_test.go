package llmproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeModelProvider stands in for a provider's completion route. It records
// the last request and answers with a fixed status and body.
type fakeModelProvider struct {
	*httptest.Server
	hits    atomic.Int64
	status  int
	body    string
	delay   time.Duration
	mu      sync.Mutex
	path    string
	query   string
	headers http.Header
	request map[string]any
}

func newFakeModelProvider(status int, body string) *fakeModelProvider {
	fp := &fakeModelProvider{status: status, body: body}
	fp.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fp.hits.Add(1)
		raw, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		_ = json.Unmarshal(raw, &decoded)
		fp.mu.Lock()
		fp.path = r.URL.EscapedPath()
		fp.query = r.URL.RawQuery
		fp.headers = r.Header.Clone()
		fp.request = decoded
		fp.mu.Unlock()
		if fp.delay > 0 {
			select {
			case <-time.After(fp.delay):
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(fp.status)
		_, _ = w.Write([]byte(fp.body))
	}))
	return fp
}

func (fp *fakeModelProvider) last() (path, query string, headers http.Header, request map[string]any) {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	return fp.path, fp.query, fp.headers, fp.request
}

// privateModelHandler is a handler whose policy allows the loopback test
// server: open_ai with a non-OpenAI base resolves to the vLLM class, which is
// allowed a private address when the allowlist names one.
func privateModelHandler() *Handler {
	return newCheckConnectionHandler(fakeEgressPolicy{allow: true, privateNetwork: true})
}

const openAICompletionBody = `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"p"},"finish_reason":"length"}]}`

func TestCheckModelConnection_SuccessSendsOneTokenCompletion(t *testing.T) {
	fp := newFakeModelProvider(http.StatusOK, openAICompletionBody)
	defer fp.Close()

	rec := doCheckConnection(t, privateModelHandler(), checkConnectionRequest{
		Type: "open_ai", APIBase: fp.URL + "/v1", APIKey: "sk-test", Model: " my-model ",
	})
	resp := decodeCheckConnectionResponse(t, rec)
	if !resp.Success || resp.Probe != checkModelProbeKind {
		t.Fatalf("got %+v, want a successful completion probe", resp)
	}
	path, _, headers, request := fp.last()
	if path != "/v1/chat/completions" {
		t.Fatalf("path = %q, want /v1/chat/completions", path)
	}
	if headers.Get("Authorization") != "Bearer sk-test" {
		t.Fatalf("Authorization = %q", headers.Get("Authorization"))
	}
	if request["model"] != "my-model" {
		t.Fatalf("model = %v, want the trimmed name", request["model"])
	}
	if request["max_tokens"] != float64(1) {
		t.Fatalf("max_tokens = %v, want 1", request["max_tokens"])
	}
}

func TestCheckModelConnection_WrongModelIsModelNotFoundAndScrubbed(t *testing.T) {
	providerMessage := `{"error":{"message":"The model ` + "`gpt-x`" +
		` does not exist at https://internal.corp.example/v1 via 10.20.30.40:8443 for key sk-test (llm-proxy.internal.svc)\nTraceback: line 1"}}`
	fp := newFakeModelProvider(http.StatusNotFound, providerMessage)
	defer fp.Close()

	rec := doCheckConnection(t, privateModelHandler(), checkConnectionRequest{
		Type: "open_ai", APIBase: fp.URL, APIKey: "sk-test", Model: "gpt-x",
	})
	resp := decodeCheckConnectionResponse(t, rec)
	if resp.Success || resp.Reason != checkConnectionReasonModelNotFound {
		t.Fatalf("got %+v, want model_not_found", resp)
	}
	for _, leaked := range []string{"https://", "internal.corp.example", "10.20.30.40", "sk-test", "llm-proxy.internal.svc", "Traceback"} {
		if strings.Contains(resp.Detail, leaked) {
			t.Fatalf("detail %q leaks %q", resp.Detail, leaked)
		}
	}
	if !strings.Contains(resp.Detail, "does not exist") {
		t.Fatalf("detail %q lost the provider's reason", resp.Detail)
	}
}

func TestCheckModelConnection_StatusClassification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"auth", http.StatusUnauthorized, `{"error":{"message":"Incorrect API key"}}`, checkConnectionReasonUnauth},
		{"forbidden", http.StatusForbidden, `{}`, checkConnectionReasonUnauth},
		{"rate limited", http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`, checkConnectionReasonRateLimited},
		{"bad request naming the model", http.StatusBadRequest, `{"error":{"message":"Invalid model name passed"}}`, checkConnectionReasonModelNotFound},
		{"bad request on the route", http.StatusBadRequest, `{"error":{"message":"Unrecognized request argument supplied: messages"}}`, checkConnectionReasonProtocol},
		{"wrong method", http.StatusMethodNotAllowed, `nope`, checkConnectionReasonProtocol},
		{"server error", http.StatusBadGateway, `<html>bad gateway</html>`, checkConnectionReasonUpstream},
		{"a page, not a completion", http.StatusOK, `<html>welcome</html>`, checkConnectionReasonProtocol},
		{"JSON, not a completion", http.StatusOK, `{"data":[{"id":"m"}]}`, checkConnectionReasonProtocol},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fp := newFakeModelProvider(tc.status, tc.body)
			defer fp.Close()
			rec := doCheckConnection(t, privateModelHandler(), checkConnectionRequest{
				Type: "vllm", APIBase: fp.URL, Model: "m",
			})
			resp := decodeCheckConnectionResponse(t, rec)
			if resp.Success || resp.Reason != tc.want {
				t.Fatalf("got %+v, want reason %q", resp, tc.want)
			}
			if strings.Contains(resp.Detail, "<html>") {
				t.Fatalf("detail %q forwards a provider page", resp.Detail)
			}
		})
	}
}

func TestCheckModelConnection_TimeoutReportsTimedOut(t *testing.T) {
	previous := checkModelProbeTimeout
	checkModelProbeTimeout = 150 * time.Millisecond
	t.Cleanup(func() { checkModelProbeTimeout = previous })

	fp := newFakeModelProvider(http.StatusOK, openAICompletionBody)
	fp.delay = 5 * time.Second
	defer fp.Close()

	started := time.Now()
	rec := doCheckConnection(t, privateModelHandler(), checkConnectionRequest{
		Type: "vllm", APIBase: fp.URL, Model: "slow-model",
	})
	resp := decodeCheckConnectionResponse(t, rec)
	if resp.Success || resp.Reason != checkConnectionReasonTimeout {
		t.Fatalf("got %+v, want timeout", resp)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("the probe took %s, the bound is not applied", elapsed)
	}
}

func TestCheckModelConnection_EgressDeniedNeverCallsProvider(t *testing.T) {
	fp := newFakeModelProvider(http.StatusOK, openAICompletionBody)
	defer fp.Close()

	h := newCheckConnectionHandler(fakeEgressPolicy{allow: false, privateNetwork: true})
	rec := doCheckConnection(t, h, checkConnectionRequest{Type: "vllm", APIBase: fp.URL, Model: "m"})
	resp := decodeCheckConnectionResponse(t, rec)
	if resp.Success || resp.Reason != checkConnectionReasonEgress {
		t.Fatalf("got %+v, want egress_not_allowed", resp)
	}
	if fp.hits.Load() != 0 {
		t.Fatal("the provider must not be called for a host off the allowlist")
	}
}

func TestCheckModelConnection_CloudTypeNeverDialsPrivateAddress(t *testing.T) {
	fp := newFakeModelProvider(http.StatusOK, openAICompletionBody)
	defer fp.Close()

	rec := doCheckConnection(t, privateModelHandler(), checkConnectionRequest{
		Type: "azure_open_ai", APIBase: fp.URL, APIKey: "k", Model: "gpt",
	})
	resp := decodeCheckConnectionResponse(t, rec)
	if resp.Success {
		t.Fatal("an Azure credential must never reach a private address")
	}
	if fp.hits.Load() != 0 {
		t.Fatal("the private address was dialled")
	}
}

func TestCheckModelConnection_RefusesBadInputBeforeAnyDial(t *testing.T) {
	fp := newFakeModelProvider(http.StatusOK, openAICompletionBody)
	defer fp.Close()

	for _, req := range []checkConnectionRequest{
		{Type: "vllm", APIBase: fp.URL, Model: "two words"},
		{Type: "vllm", APIBase: fp.URL, Model: strings.Repeat("m", checkModelMaxNameLength+1)},
		// A dots-only name is a dot-segment in the deployment routes.
		{Type: "ai_dial", APIBase: fp.URL, Model: ".."},
		{Type: "azure_open_ai", APIBase: fp.URL, Model: "."},
		{Type: "amazon_bedrock", AWSRegionName: "us-east-1", Model: "..."},
	} {
		rec := doCheckConnection(t, privateModelHandler(), req)
		resp := decodeCheckConnectionResponse(t, rec)
		if resp.Success || resp.Reason != checkConnectionReasonInvalidModel {
			t.Fatalf("got %+v, want invalid_model", resp)
		}
	}
	rec := doCheckConnection(t, privateModelHandler(), checkConnectionRequest{Type: "github", APIBase: fp.URL, Model: "m"})
	if resp := decodeCheckConnectionResponse(t, rec); resp.Reason != checkConnectionReasonUnsupported {
		t.Fatalf("got %+v, want unsupported_type", resp)
	}
	if fp.hits.Load() != 0 {
		t.Fatal("refused input reached the provider")
	}
}

func TestCheckModelConnection_LatencyIsReported(t *testing.T) {
	fp := newFakeModelProvider(http.StatusOK, openAICompletionBody)
	fp.delay = 20 * time.Millisecond
	defer fp.Close()

	rec := doCheckConnection(t, privateModelHandler(), checkConnectionRequest{Type: "vllm", APIBase: fp.URL, Model: "m"})
	resp := decodeCheckConnectionResponse(t, rec)
	if !resp.Success || resp.LatencyMS < 20 {
		t.Fatalf("got %+v, want success with latency >= 20 ms", resp)
	}
}

// --- probe-level tests: they bypass the SSRF client so the cloud-shaped
// routes can run against a loopback server.

func TestProbeAzureChatCompletion_DeploymentRouteAndVersion(t *testing.T) {
	fp := newFakeModelProvider(http.StatusOK, openAICompletionBody)
	defer fp.Close()

	err := probeAzureChatCompletion(context.Background(), fp.Client(), checkConnectionRequest{
		APIBase: fp.URL, APIKey: "azure-key", Model: "gpt 4o/x",
	})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	path, query, headers, request := fp.last()
	if path != "/openai/deployments/gpt%204o%2Fx/chat/completions" {
		t.Fatalf("path = %q, want the escaped deployment route", path)
	}
	if query != "api-version="+defaultAzureCompletionAPIVersion {
		t.Fatalf("query = %q", query)
	}
	if headers.Get("api-key") != "azure-key" || headers.Get("Authorization") != "" {
		t.Fatalf("headers = %v, want the api-key header only", headers)
	}
	if request["max_completion_tokens"] != float64(1) {
		t.Fatalf("request = %v, want max_completion_tokens for a current api-version", request)
	}
}

func TestProbeAzureChatCompletion_OldVersionUsesMaxTokens(t *testing.T) {
	fp := newFakeModelProvider(http.StatusOK, openAICompletionBody)
	defer fp.Close()

	err := probeAzureChatCompletion(context.Background(), fp.Client(), checkConnectionRequest{
		APIBase: fp.URL, APIKey: "k", Model: "gpt", APIVersion: "2023-05-15",
	})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if _, _, _, request := fp.last(); request["max_tokens"] != float64(1) {
		t.Fatalf("request = %v, want max_tokens for an old api-version", request)
	}
}

func TestProbeDialCompletion_UsesTheRuntimeAzureRoute(t *testing.T) {
	// The runtime sends every ai_dial credential to Bifrost's Azure provider,
	// so the probe uses DIAL's Azure-compatible deployment route and nothing
	// else.
	fp := newFakeModelProvider(http.StatusOK, openAICompletionBody)
	defer fp.Close()
	err := probeDialCompletion(context.Background(), fp.Client(), checkConnectionRequest{
		APIBase: fp.URL, APIKey: "dial-key", Model: "m",
	})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	path, query, headers, request := fp.last()
	if path != "/openai/deployments/m/chat/completions" || query != "api-version="+defaultAzureAPIVersion {
		t.Fatalf("route = %q?%q, want the deployment route", path, query)
	}
	if headers.Get("api-key") != "dial-key" || headers.Get("anthropic-version") != "" {
		t.Fatalf("headers = %v, want the api-key header only", headers)
	}
	if request["max_tokens"] != float64(1) {
		t.Fatalf("request = %v, want max_tokens", request)
	}
}

func TestProbeOpenAIChatCompletion_UsesTheRuntimeRoute(t *testing.T) {
	// The runtime removes one trailing /v1 and Bifrost adds /v1 back, so an
	// api_base with or without /v1 reaches /v1/chat/completions.
	for _, suffix := range []string{"", "/", "/v1", "/v1/", "/V1"} {
		for _, credentialType := range []string{"vllm", "open_ai"} {
			t.Run(credentialType+" api_base"+suffix, func(t *testing.T) {
				fp := newFakeModelProvider(http.StatusOK, openAICompletionBody)
				defer fp.Close()
				err := probeOpenAIChatCompletion(context.Background(), fp.Client(), checkConnectionRequest{
					Type: credentialType, APIBase: fp.URL + suffix, APIKey: "k", Model: "m",
				})
				if err != nil {
					t.Fatalf("probe: %v", err)
				}
				if path, _, _, _ := fp.last(); path != "/v1/chat/completions" {
					t.Fatalf("path = %q, want /v1/chat/completions", path)
				}
			})
		}
	}
}

func TestCheckModelConnection_APIBaseWithoutV1IsNotModelNotFound(t *testing.T) {
	// A server that serves /v1/chat/completions only. Before the probe used
	// the runtime route, it posted {api_base}/chat/completions, got a 404 and
	// reported "Model not found" for a model that serves chat.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(openAICompletionBody))
	}))
	defer server.Close()
	rec := doCheckConnection(t, privateModelHandler(), checkConnectionRequest{
		Type: "vllm", APIBase: server.URL, Model: "m",
	})
	if resp := decodeCheckConnectionResponse(t, rec); !resp.Success {
		t.Fatalf("got %+v, want success on the runtime route", resp)
	}
}

func TestProbeOpenAIChatCompletion_AnthropicEndpoints(t *testing.T) {
	fp := newFakeModelProvider(http.StatusOK, `{"type":"message","content":[]}`)
	defer fp.Close()
	err := probeOpenAIChatCompletion(context.Background(), fp.Client(), checkConnectionRequest{
		Type: "vllm", APIBase: fp.URL + "/v1", APIKey: "k", Model: "claude-local", UseAnthropicEndpoints: true,
	})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	path, _, headers, request := fp.last()
	if path != "/v1/messages" {
		t.Fatalf("path = %q, want /v1/messages", path)
	}
	if headers.Get("Authorization") != "Bearer k" || headers.Get("anthropic-version") != anthropicAPIVersion {
		t.Fatalf("headers = %v, want Bearer auth and the anthropic-version header", headers)
	}
	if request["max_tokens"] != float64(1) || request["model"] != "claude-local" {
		t.Fatalf("request = %v", request)
	}
}

func TestProbeOpenAIChatCompletion_AnthropicEndpointsWrongDialectIsAFailure(t *testing.T) {
	// An upstream that answers /v1/messages with an OpenAI completion does not
	// serve the Anthropic dialect the runtime will speak to it.
	fp := newFakeModelProvider(http.StatusOK, openAICompletionBody)
	defer fp.Close()
	err := probeOpenAIChatCompletion(context.Background(), fp.Client(), checkConnectionRequest{
		Type: "vllm", APIBase: fp.URL, APIKey: "k", Model: "m", UseAnthropicEndpoints: true,
	})
	reason, _ := classifyModelProbeError(context.Background(), err)
	if reason != checkConnectionReasonProtocol {
		t.Fatalf("reason = %q, want protocol_error", reason)
	}
}

func TestOpenAICompatibleRootURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://api.openai.com/v1/": "https://api.openai.com",
		"http://host:8000":           "http://host:8000",
		"http://host:8000/v1":        "http://host:8000",
		" http://host:8000/x/v1 ":    "http://host:8000/x",
		"http://host:8000/v1/v1":     "http://host:8000/v1",
	} {
		if got := openAICompatibleRootURL(in); got != want {
			t.Errorf("openAICompatibleRootURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// tokenFieldProvider refuses one token-budget field with OpenAI's
// "Unsupported parameter" 400, and answers a completion otherwise.
func tokenFieldProvider(refused string) (*httptest.Server, *[]map[string]any, *sync.Mutex) {
	var mu sync.Mutex
	var seen []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		mu.Lock()
		seen = append(seen, request)
		mu.Unlock()
		if _, sent := request[refused]; sent {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Unsupported parameter: '` + refused +
				`' is not supported with this model. Use 'max_completion_tokens' instead.","type":"invalid_request_error","param":"` +
				refused + `","code":"unsupported_parameter"}}`))
			return
		}
		_, _ = w.Write([]byte(openAICompletionBody))
	}))
	return server, &seen, &mu
}

func TestCheckModelConnection_ReasoningModelRefusingMaxTokensIsRetried(t *testing.T) {
	// An o-series or gpt-5 model behind an OpenAI-compatible endpoint that is
	// not api.openai.com. The first request carries max_tokens and is refused;
	// the retry carries max_completion_tokens and is answered.
	server, seen, mu := tokenFieldProvider("max_tokens")
	defer server.Close()

	rec := doCheckConnection(t, privateModelHandler(), checkConnectionRequest{
		Type: "open_ai", APIBase: server.URL + "/openai/v1", APIKey: "k", Model: "o4-mini",
	})
	resp := decodeCheckConnectionResponse(t, rec)
	if !resp.Success {
		t.Fatalf("got %+v, want success after the retry", resp)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*seen) != 2 {
		t.Fatalf("requests = %d, want 2", len(*seen))
	}
	if _, ok := (*seen)[1]["max_tokens"]; ok || (*seen)[1]["max_completion_tokens"] != float64(1) {
		t.Fatalf("retry = %v, want max_completion_tokens only", (*seen)[1])
	}
}

func TestCheckModelConnection_UnsupportedTokenFieldIsNotAProtocolError(t *testing.T) {
	// Before the retry, OpenAI's "Unsupported parameter: 'max_tokens'" 400
	// was classified as protocol_error: "Wrong API protocol or route" for a
	// model and a route that are correct.
	err := &modelProbeError{status: http.StatusBadRequest,
		message: "Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead."}
	if !refusesTokenField(err, "max_tokens") {
		t.Fatal("the OpenAI refusal of max_tokens is not recognised")
	}
	if refusesTokenField(&modelProbeError{status: http.StatusBadRequest, message: "Invalid model name"}, "max_tokens") {
		t.Fatal("a message that names no token field must not trigger the retry")
	}
	if refusesTokenField(&modelProbeError{status: http.StatusNotFound, message: "max_tokens"}, "max_tokens") {
		t.Fatal("only a 400 triggers the retry")
	}
}

func TestCheckModelConnection_RetryHappensOnce(t *testing.T) {
	// A server that refuses BOTH fields gets two requests, not a loop, and
	// the second refusal is the verdict.
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Unsupported parameter: max_tokens and max_completion_tokens"}}`))
	}))
	defer server.Close()
	rec := doCheckConnection(t, privateModelHandler(), checkConnectionRequest{Type: "vllm", APIBase: server.URL, Model: "m"})
	if resp := decodeCheckConnectionResponse(t, rec); resp.Success || resp.Reason != checkConnectionReasonProtocol {
		t.Fatalf("got %+v, want protocol_error", resp)
	}
	if hits.Load() != 2 {
		t.Fatalf("requests = %d, want 2", hits.Load())
	}
}

func TestCheckModelConnection_RedirectIsRefusedAndNotFollowed(t *testing.T) {
	// The model probe shares the redirect-refusing client. A 302 to another
	// host must be the verdict, and the second host must never be dialled:
	// following it would carry the redeemed key to an unvalidated host.
	other := newFakeModelProvider(http.StatusOK, openAICompletionBody)
	defer other.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/v1/chat/completions", http.StatusFound)
	}))
	defer redirector.Close()

	rec := doCheckConnection(t, privateModelHandler(), checkConnectionRequest{
		Type: "vllm", APIBase: redirector.URL, APIKey: "k", Model: "m",
	})
	resp := decodeCheckConnectionResponse(t, rec)
	if resp.Success || resp.Reason != checkConnectionReasonProtocol {
		t.Fatalf("got %+v, want protocol_error", resp)
	}
	if other.hits.Load() != 0 {
		t.Fatal("the redirect target was dialled")
	}
}

func TestCheckModelConnection_ConcurrencyBound(t *testing.T) {
	// Every slot is taken: the next probe is refused before any dial.
	for range cap(modelProbeSlots) {
		modelProbeSlots <- struct{}{}
	}
	t.Cleanup(func() {
		for range cap(modelProbeSlots) {
			<-modelProbeSlots
		}
	})
	fp := newFakeModelProvider(http.StatusOK, openAICompletionBody)
	defer fp.Close()
	rec := doCheckConnection(t, privateModelHandler(), checkConnectionRequest{Type: "vllm", APIBase: fp.URL, Model: "m"})
	resp := decodeCheckConnectionResponse(t, rec)
	if resp.Success || resp.Reason != checkConnectionReasonRateLimited || resp.Probe != checkModelProbeKind {
		t.Fatalf("got %+v, want rate_limited from the model probe", resp)
	}
	if fp.hits.Load() != 0 {
		t.Fatal("a refused probe reached the provider")
	}
}

func TestCheckModelConnection_SlotIsReleased(t *testing.T) {
	fp := newFakeModelProvider(http.StatusOK, openAICompletionBody)
	defer fp.Close()
	for range cap(modelProbeSlots) + 2 {
		rec := doCheckConnection(t, privateModelHandler(), checkConnectionRequest{Type: "vllm", APIBase: fp.URL, Model: "m"})
		if resp := decodeCheckConnectionResponse(t, rec); !resp.Success {
			t.Fatalf("got %+v, want every sequential probe to run", resp)
		}
	}
	if len(modelProbeSlots) != 0 {
		t.Fatalf("slots held = %d after the probes finished", len(modelProbeSlots))
	}
}

func TestProbeOllamaChat_NativeRouteOneToken(t *testing.T) {
	fp := newFakeModelProvider(http.StatusOK, `{"message":{"role":"assistant","content":"p"},"done":true}`)
	defer fp.Close()
	if err := probeOllamaChat(context.Background(), fp.Client(), checkConnectionRequest{APIBase: fp.URL, Model: "llama3"}); err != nil {
		t.Fatalf("probe: %v", err)
	}
	path, _, _, request := fp.last()
	if path != "/api/chat" {
		t.Fatalf("path = %q", path)
	}
	options, _ := request["options"].(map[string]any)
	if options["num_predict"] != float64(1) || request["stream"] != false {
		t.Fatalf("request = %v, want a one-token, non-streaming chat", request)
	}
}

func TestProbeOpenAIChatCompletion_OpenAIOriginUsesMaxCompletionTokens(t *testing.T) {
	if !isOpenAIAPIOrigin("https://api.openai.com/v1") || isOpenAIAPIOrigin("http://vllm:8000/v1") {
		t.Fatal("isOpenAIAPIOrigin misreads the origin")
	}
}

func TestCheckConnectionBedrockRuntimeTargets(t *testing.T) {
	targets, err := checkConnectionBedrockRuntimeTargets(checkConnectionRequest{
		AWSAccessKeyID: "AKIA", AWSSecretAccessKey: "secret", AWSRegionName: "eu-west-1",
	})
	if err != nil || len(targets) != 1 || targets[0] != "https://bedrock-runtime.eu-west-1.amazonaws.com" {
		t.Fatalf("targets = %v, err = %v", targets, err)
	}
	if _, err := checkConnectionBedrockRuntimeTargets(checkConnectionRequest{AWSRegionName: "eu-west-1"}); err == nil {
		t.Fatal("an incomplete Bedrock credential must be refused before any dial")
	}
}

func TestScrubProviderMessage(t *testing.T) {
	cases := []struct {
		in      string
		secrets []string
		absent  []string
		present []string
	}{
		{
			in:      "Deployment gpt-4.1 not found at https://x.openai.azure.com/openai?key=abc",
			absent:  []string{"https://", "openai.azure.com", "key=abc"},
			present: []string{"gpt-4.1", "not found"},
		},
		{
			in:      "connect to 192.168.1.10:8000 and [fd00::1]:443 failed",
			absent:  []string{"192.168.1.10", "fd00::1"},
			present: []string{"failed"},
		},
		{
			in:      "bad key AbCdEfGhIjKlMnOpQrStUvWxYz0123456789 for claude-3.5-sonnet",
			absent:  []string{"AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"},
			present: []string{"claude-3.5-sonnet"},
		},
		{
			in:      "the key my-secret-value is wrong",
			secrets: []string{"my-secret-value"},
			absent:  []string{"my-secret-value"},
		},
		{
			in:      "first line\n  at frame (file.go:12)\n  at frame",
			absent:  []string{"frame"},
			present: []string{"first line"},
		},
	}
	for _, tc := range cases {
		got := scrubProviderMessage(tc.in, tc.secrets)
		for _, value := range tc.absent {
			if strings.Contains(got, value) {
				t.Errorf("scrub(%q) = %q, still contains %q", tc.in, got, value)
			}
		}
		for _, value := range tc.present {
			if !strings.Contains(got, value) {
				t.Errorf("scrub(%q) = %q, lost %q", tc.in, got, value)
			}
		}
	}
	long := scrubProviderMessage(strings.Repeat("word ", 200), nil)
	if len([]rune(long)) > scrubbedMessageMaxRunes+1 {
		t.Fatalf("scrubbed message is %d runes, above the bound", len([]rune(long)))
	}
}

func TestProviderErrorMessage(t *testing.T) {
	cases := map[string]string{
		`{"error":{"message":"nested"}}`:        "nested",
		`{"error":"flat"}`:                      "flat",
		`{"message":"top"}`:                     "top",
		`{"Message":"aws"}`:                     "aws",
		`[{"error":{"message":"vertex"}}]`:      "vertex",
		`<html><body>proxy error</body></html>`: "",
	}
	for body, want := range cases {
		if got := providerErrorMessage([]byte(body)); got != want {
			t.Errorf("providerErrorMessage(%q) = %q, want %q", body, got, want)
		}
	}
}
