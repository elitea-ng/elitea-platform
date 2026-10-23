package api

// budget_pending_routes_test.go — the boot-window refusal ([human decision]
// 2026-10-16) covers EVERY spend route, through the real router.
//
// The list is not written here: it is every `inference` route in
// testdata/llm_route_classes.txt, the file TestEveryMountedRouteHasAClass keeps
// complete. A new model-call route therefore joins this test on its own, and a
// route with no request builder below fails it.

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"
	"github.com/maximhq/bifrost/core/schemas"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/llmproxy"
)

// countingRouter counts every provider dispatch, whatever the dialect.
type countingRouter struct {
	recordingRouter
	calls atomic.Int64
}

func (r *countingRouter) ChatCompletionRequest(c *schemas.BifrostContext, q *schemas.BifrostChatRequest) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	r.calls.Add(1)
	return r.recordingRouter.ChatCompletionRequest(c, q)
}
func (r *countingRouter) ChatCompletionStreamRequest(c *schemas.BifrostContext, q *schemas.BifrostChatRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	r.calls.Add(1)
	return r.recordingRouter.ChatCompletionStreamRequest(c, q)
}
func (r *countingRouter) TextCompletionRequest(c *schemas.BifrostContext, q *schemas.BifrostTextCompletionRequest) (*schemas.BifrostTextCompletionResponse, *schemas.BifrostError) {
	r.calls.Add(1)
	return r.recordingRouter.TextCompletionRequest(c, q)
}
func (r *countingRouter) EmbeddingRequest(c *schemas.BifrostContext, q *schemas.BifrostEmbeddingRequest) (*schemas.BifrostEmbeddingResponse, *schemas.BifrostError) {
	r.calls.Add(1)
	return r.recordingRouter.EmbeddingRequest(c, q)
}
func (r *countingRouter) ResponsesRequest(c *schemas.BifrostContext, q *schemas.BifrostResponsesRequest) (*schemas.BifrostResponsesResponse, *schemas.BifrostError) {
	r.calls.Add(1)
	return r.recordingRouter.ResponsesRequest(c, q)
}
func (r *countingRouter) ResponsesStreamRequest(c *schemas.BifrostContext, q *schemas.BifrostResponsesRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	r.calls.Add(1)
	return r.recordingRouter.ResponsesStreamRequest(c, q)
}
func (r *countingRouter) CountTokensRequest(c *schemas.BifrostContext, q *schemas.BifrostResponsesRequest) (*schemas.BifrostCountTokensResponse, *schemas.BifrostError) {
	r.calls.Add(1)
	return r.recordingRouter.CountTokensRequest(c, q)
}
func (r *countingRouter) ImageGenerationRequest(c *schemas.BifrostContext, q *schemas.BifrostImageGenerationRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	r.calls.Add(1)
	return r.recordingRouter.ImageGenerationRequest(c, q)
}
func (r *countingRouter) ImageEditRequest(c *schemas.BifrostContext, q *schemas.BifrostImageEditRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	r.calls.Add(1)
	return r.recordingRouter.ImageEditRequest(c, q)
}
func (r *countingRouter) ImageVariationRequest(c *schemas.BifrostContext, q *schemas.BifrostImageVariationRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	r.calls.Add(1)
	return r.recordingRouter.ImageVariationRequest(c, q)
}
func (r *countingRouter) SpeechRequest(c *schemas.BifrostContext, q *schemas.BifrostSpeechRequest) (*schemas.BifrostSpeechResponse, *schemas.BifrostError) {
	r.calls.Add(1)
	return r.recordingRouter.SpeechRequest(c, q)
}
func (r *countingRouter) TranscriptionRequest(c *schemas.BifrostContext, q *schemas.BifrostTranscriptionRequest) (*schemas.BifrostTranscriptionResponse, *schemas.BifrostError) {
	r.calls.Add(1)
	return r.recordingRouter.TranscriptionRequest(c, q)
}

// countingDialer counts realtime provider dials; it never connects.
type countingDialer struct{ dials atomic.Int64 }

func (d *countingDialer) DialRealtime(*schemas.BifrostContext, schemas.ModelProvider, string, url.Values) (*llmproxy.RealtimeUpstream, error) {
	d.dials.Add(1)
	return nil, http.ErrServerClosed
}

func jsonRequest(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func multipartRequest(t *testing.T, path string, fields map[string]string, files ...string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range files {
		part, err := mw.CreateFormFile(name, name+".bin")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte("bytes")); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf.Bytes()))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

// pendingRouteRequests builds one request per inference route. A route listed
// as inference with no entry here fails the test, so it cannot be skipped.
func pendingRouteRequests(t *testing.T) map[string]func() *http.Request {
	audio := func(path string) func() *http.Request {
		return func() *http.Request {
			return multipartRequest(t, path, map[string]string{"model": "openai/whisper-1"}, "file")
		}
	}
	upgrade := func() func() *http.Request {
		return func() *http.Request {
			req := httptest.NewRequest(http.MethodGet, "/llm/v1/realtime?model=gpt-4o-realtime-preview", nil)
			req.Header.Set("Upgrade", "websocket")
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Sec-WebSocket-Version", "13")
			req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
			return req
		}
	}
	return map[string]func() *http.Request{
		"POST /llm/v1/messages": func() *http.Request {
			return jsonRequest(http.MethodPost, "/llm/v1/messages",
				`{"model":"anthropic/claude-3-5-sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
		},
		"POST /llm/v1/images/edits": func() *http.Request {
			return multipartRequest(t, "/llm/v1/images/edits",
				map[string]string{"model": "openai/gpt-image-1", "prompt": "a cat"}, "image")
		},
		"POST /llm/v1/images/variations": func() *http.Request {
			return multipartRequest(t, "/llm/v1/images/variations",
				map[string]string{"model": "openai/dall-e-2"}, "image")
		},
		"POST /llm/v1/images/generations": func() *http.Request {
			return jsonRequest(http.MethodPost, "/llm/v1/images/generations",
				`{"model":"openai/gpt-image-1","prompt":"a cat"}`)
		},
		"POST /llm/v1/audio/speech": func() *http.Request {
			return jsonRequest(http.MethodPost, "/llm/v1/audio/speech",
				`{"model":"openai/tts-1","input":"hi","voice":"alloy"}`)
		},
		"POST /llm/v1/audio/transcriptions": audio("/llm/v1/audio/transcriptions"),
		"POST /llm/v1/audio/translations":   audio("/llm/v1/audio/translations"),
		"POST /llm/v1/chat/completions": func() *http.Request {
			return jsonRequest(http.MethodPost, "/llm/v1/chat/completions",
				`{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
		},
		"POST /llm/v1/completions": func() *http.Request {
			return jsonRequest(http.MethodPost, "/llm/v1/completions",
				`{"model":"openai/gpt-3.5-turbo-instruct","prompt":"hi"}`)
		},
		"POST /llm/v1/embeddings": func() *http.Request {
			return jsonRequest(http.MethodPost, "/llm/v1/embeddings",
				`{"model":"openai/text-embedding-3-small","input":"hi"}`)
		},
		"POST /llm/v1/responses": func() *http.Request {
			return jsonRequest(http.MethodPost, "/llm/v1/responses",
				`{"model":"openai/gpt-4o","input":"hi"}`)
		},
		"GET /llm/v1/realtime": upgrade(),
	}
}

func assertNATSUnavailable(t *testing.T, label string, status int, body []byte) {
	t.Helper()
	if status != http.StatusServiceUnavailable {
		t.Fatalf("%s: status = %d, want 503; body=%s", label, status, body)
	}
	var envelope struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("%s: body %s: %v", label, body, err)
	}
	if envelope.Error.Type != "service_unavailable" || envelope.Error.Code != "nats_unavailable" {
		t.Fatalf("%s: error = %+v, want service_unavailable/nats_unavailable", label, envelope.Error)
	}
}

// TestBudgetPending_EveryInferenceRouteIsRefused drives every inference route
// through api.NewRouter on a handler whose budget enforcement is pending, with
// and without a project header. Each must answer 503 nats_unavailable and
// reach no provider.
func TestBudgetPending_EveryInferenceRouteIsRefused(t *testing.T) {
	router := &countingRouter{}
	dialer := &countingDialer{}
	h := llmproxy.NewHandler(router, nil, nil,
		llmproxy.WithRealtimeDialer(dialer),
		llmproxy.WithBudgetEnforcementPending())
	mux := NewRouter(h)

	builders := pendingRouteRequests(t)
	var routes []string
	for key, class := range readRouteClasses(t) {
		if class == "inference" {
			routes = append(routes, key)
		}
	}
	if len(routes) == 0 {
		t.Fatal("the route-class file lists no inference route")
	}
	for _, key := range routes {
		build, ok := builders[key]
		if !ok {
			t.Errorf("%s is an inference route with no request in pendingRouteRequests: add one", key)
			continue
		}
		for _, project := range []string{"42", ""} {
			req := build()
			if project != "" {
				req.Header.Set(headerProjectID, project)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			assertNATSUnavailable(t, key+" project="+project, rec.Code, rec.Body.Bytes())
		}
	}
	if n := router.calls.Load(); n != 0 {
		t.Fatalf("%d requests reached the provider while enforcement was pending", n)
	}
	if n := dialer.dials.Load(); n != 0 {
		t.Fatalf("%d realtime sessions dialled the provider while enforcement was pending", n)
	}

	// The negative half: a route that calls no provider is NOT refused, so a
	// router-wide 503 cannot pass this test. /llm/v1/models is served from the
	// catalogue alone.
	models := httptest.NewRequest(http.MethodGet, "/llm/v1/models", nil)
	models.Header.Set(headerProjectID, "42")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, models)
	if rec.Code == http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "nats_unavailable") {
		t.Errorf("GET /llm/v1/models answered %d %s; only provider-calling routes are refused", rec.Code, rec.Body.String())
	}

	// count_tokens is classed "other" for the analytics pages (it is not a
	// model call), but it IS a provider call and it IS budget-gated
	// (DECISIONS.md: "/llm/v1/messages/count_tokens now budget-gated, was the
	// 4th unmetered path"). With no gate it would be ungoverned provider
	// traffic, so the boot window refuses it too.
	countTokens := jsonRequest(http.MethodPost, "/llm/v1/messages/count_tokens",
		`{"model":"anthropic/claude-3-5-sonnet","messages":[{"role":"user","content":"hi"}]}`)
	countTokens.Header.Set(headerProjectID, "42")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, countTokens)
	assertNATSUnavailable(t, "POST /llm/v1/messages/count_tokens", rec.Code, rec.Body.Bytes())
	if n := router.calls.Load(); n != 0 {
		t.Fatalf("count_tokens reached the provider while enforcement was pending")
	}
}

// TestBudgetPending_RealtimeWebSocketHandshakeIsRefused is the same refusal
// over a real WebSocket handshake against a listening router: the upgrade is
// refused before Accept with the OpenAI-shaped 503, and no provider is dialled.
func TestBudgetPending_RealtimeWebSocketHandshakeIsRefused(t *testing.T) {
	dialer := &countingDialer{}
	h := llmproxy.NewHandler(&countingRouter{}, nil, nil,
		llmproxy.WithRealtimeDialer(dialer),
		llmproxy.WithBudgetEnforcementPending())
	srv := httptest.NewServer(NewRouter(h))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/llm/v1/realtime?model=gpt-4o-realtime-preview"
	conn, resp, err := websocket.Dial(t.Context(), wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{headerProjectID: []string{"42"}},
	})
	if err == nil {
		_ = conn.CloseNow()
		t.Fatal("the realtime handshake was accepted while enforcement was pending")
	}
	if resp == nil {
		t.Fatalf("no handshake response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body bytes.Buffer
	_, _ = body.ReadFrom(resp.Body)
	assertNATSUnavailable(t, "realtime handshake", resp.StatusCode, body.Bytes())
	if n := dialer.dials.Load(); n != 0 {
		t.Fatalf("%d provider dials while enforcement was pending", n)
	}
}
