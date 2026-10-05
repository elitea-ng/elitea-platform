package llmproxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/maximhq/bifrost/core/schemas"
)

// newTestRouter mirrors internal/api.NewRouter's route table so the handler's
// path-dispatch behaviour is exercised end-to-end here without importing
// internal/api (which would form an import cycle: api → llmproxy). The
// authoritative route-precedence assertions live in internal/api/router_test.go.
// newTestRouter mirrors internal/api.NewRouter so package-internal tests can
// exercise real dispatch. It is a COPY, and it cannot import the real one: the
// api package imports this one, so the dependency only runs one way. Add every
// route to BOTH tables. A route added only there is untested here; a route
// added only here is not served.
func newTestRouter(h *Handler) http.Handler {
	r := chi.NewRouter()
	r.NotFound(h.NotFound)
	r.MethodNotAllowed(h.MethodNotAllowed)
	r.Route("/llm/v1", func(r chi.Router) {
		r.Post("/messages", h.Messages)
		r.Post("/messages/count_tokens", h.CountTokens)
		r.Post("/messages/*", h.MessagesSubPath)
		r.Post("/images/edits", h.ImageEdit)
		r.Post("/images/variations", h.ImageVariation)
		r.Post("/images/generations", h.ImageGeneration)
		r.Post("/audio/speech", h.Speech)
		r.Post("/audio/transcriptions", h.Transcription)
		r.Post("/audio/translations", h.Transcription)
		r.Post("/chat/completions", h.Chat)
		r.Post("/completions", h.TextCompletion)
		r.Post("/embeddings", h.Embeddings)
		r.Post("/responses", h.Responses)
		r.Get("/realtime", h.Realtime)
		r.Post("/realtime", h.Realtime)
		r.Get("/models", h.Models)
		r.Get("/models/*", h.Model)
	})
	return r
}

// fakeRouter is a test double for LLMRouter. Each method returns the configured
// canned value; streaming methods return a channel pre-loaded with chunks.
type fakeRouter struct {
	chatResp   *schemas.BifrostChatResponse
	chatErr    *schemas.BifrostError
	streamChan chan *schemas.BifrostStreamChunk
	streamErr  *schemas.BifrostError

	textResp *schemas.BifrostTextCompletionResponse
	textErr  *schemas.BifrostError

	embResp *schemas.BifrostEmbeddingResponse
	embErr  *schemas.BifrostError

	respResp *schemas.BifrostResponsesResponse
	respErr  *schemas.BifrostError

	countResp *schemas.BifrostCountTokensResponse
	countErr  *schemas.BifrostError

	imgResp *schemas.BifrostImageGenerationResponse
	imgErr  *schemas.BifrostError

	speechResp *schemas.BifrostSpeechResponse
	speechErr  *schemas.BifrostError

	transcriptionResp *schemas.BifrostTranscriptionResponse
	transcriptionErr  *schemas.BifrostError

	// lastVK captures the virtual-key value seen on the context.
	lastVK string
	// lastResponsesReq captures the decoded Responses-API request so tests can
	// assert the wire body (e.g. response_format) survived decode +
	// ToBifrostResponsesRequest into the core request struct.
	lastResponsesReq *schemas.BifrostResponsesRequest
	// selectedKeyID, when set, is written onto the request context the way
	// bifrost/core writes the id of the key it selected.
	selectedKeyID string
}

func (f *fakeRouter) captureVK(ctx *schemas.BifrostContext) {
	if v, ok := ctx.Value(schemas.BifrostContextKeyVirtualKey).(string); ok {
		f.lastVK = v
	}
	if f.selectedKeyID != "" {
		ctx.SetValue(schemas.BifrostContextKeySelectedKeyID, f.selectedKeyID)
	}
}

func (f *fakeRouter) ChatCompletionRequest(ctx *schemas.BifrostContext, _ *schemas.BifrostChatRequest) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	f.captureVK(ctx)
	return f.chatResp, f.chatErr
}

func (f *fakeRouter) ChatCompletionStreamRequest(ctx *schemas.BifrostContext, _ *schemas.BifrostChatRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	f.captureVK(ctx)
	return f.streamChan, f.streamErr
}

func (f *fakeRouter) TextCompletionRequest(_ *schemas.BifrostContext, _ *schemas.BifrostTextCompletionRequest) (*schemas.BifrostTextCompletionResponse, *schemas.BifrostError) {
	return f.textResp, f.textErr
}

func (f *fakeRouter) EmbeddingRequest(_ *schemas.BifrostContext, _ *schemas.BifrostEmbeddingRequest) (*schemas.BifrostEmbeddingResponse, *schemas.BifrostError) {
	return f.embResp, f.embErr
}

func (f *fakeRouter) ResponsesRequest(_ *schemas.BifrostContext, req *schemas.BifrostResponsesRequest) (*schemas.BifrostResponsesResponse, *schemas.BifrostError) {
	f.lastResponsesReq = req
	return f.respResp, f.respErr
}

func (f *fakeRouter) ResponsesStreamRequest(ctx *schemas.BifrostContext, req *schemas.BifrostResponsesRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	f.lastResponsesReq = req
	f.captureVK(ctx)
	return f.streamChan, f.streamErr
}

func (f *fakeRouter) CountTokensRequest(_ *schemas.BifrostContext, _ *schemas.BifrostResponsesRequest) (*schemas.BifrostCountTokensResponse, *schemas.BifrostError) {
	return f.countResp, f.countErr
}

func (f *fakeRouter) ImageGenerationRequest(_ *schemas.BifrostContext, _ *schemas.BifrostImageGenerationRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	return f.imgResp, f.imgErr
}

func (f *fakeRouter) ImageEditRequest(_ *schemas.BifrostContext, _ *schemas.BifrostImageEditRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	return f.imgResp, f.imgErr
}

func (f *fakeRouter) ImageVariationRequest(_ *schemas.BifrostContext, _ *schemas.BifrostImageVariationRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	return f.imgResp, f.imgErr
}

func (f *fakeRouter) SpeechRequest(_ *schemas.BifrostContext, _ *schemas.BifrostSpeechRequest) (*schemas.BifrostSpeechResponse, *schemas.BifrostError) {
	return f.speechResp, f.speechErr
}

func (f *fakeRouter) TranscriptionRequest(_ *schemas.BifrostContext, _ *schemas.BifrostTranscriptionRequest) (*schemas.BifrostTranscriptionResponse, *schemas.BifrostError) {
	return f.transcriptionResp, f.transcriptionErr
}

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }

func newChunkChan(chunks ...*schemas.BifrostStreamChunk) chan *schemas.BifrostStreamChunk {
	ch := make(chan *schemas.BifrostStreamChunk, len(chunks))
	for _, c := range chunks {
		ch <- c
	}
	close(ch)
	return ch
}

func postJSON(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestChatUnary(t *testing.T) {
	fake := &fakeRouter{chatResp: &schemas.BifrostChatResponse{ID: "cmpl-1", Model: "gpt-4o"}}
	h := NewHandler(fake, nil, nil)

	rec := postJSON(t, h.route(), "/llm/v1/chat/completions", `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Server"); got != "Centry" {
		t.Errorf("Server header = %q, want Centry", got)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["id"] != "cmpl-1" {
		t.Errorf("id = %v, want cmpl-1", out["id"])
	}
}

func TestChatUnaryError_BudgetIs402(t *testing.T) {
	fake := &fakeRouter{chatErr: &schemas.BifrostError{
		StatusCode: intPtr(http.StatusPaymentRequired),
		Error:      &schemas.ErrorField{Message: "over budget"},
	}}
	h := NewHandler(fake, nil, nil)

	rec := postJSON(t, h.route(), "/llm/v1/chat/completions", `{"model":"gpt-4o","messages":[]}`)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", rec.Code)
	}
	var out openAIError
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Error.Type != "budget_exceeded" || out.Error.Code != "insufficient_quota" {
		t.Errorf("error = %+v, want budget_exceeded/insufficient_quota", out.Error)
	}
}

func TestChatStreamOpenAI_FlushesEachChunkThenDone(t *testing.T) {
	chunks := newChunkChan(
		&schemas.BifrostStreamChunk{BifrostChatResponse: &schemas.BifrostChatResponse{ID: "c1", Object: "chat.completion.chunk"}},
		&schemas.BifrostStreamChunk{BifrostChatResponse: &schemas.BifrostChatResponse{ID: "c2", Object: "chat.completion.chunk"}},
	)
	fake := &fakeRouter{streamChan: chunks}
	h := NewHandler(fake, nil, nil)

	rec := postJSON(t, h.route(), "/llm/v1/chat/completions", `{"model":"gpt-4o","messages":[],"stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	if xb := rec.Header().Get("X-Accel-Buffering"); xb != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", xb)
	}
	body := rec.Body.String()
	for _, want := range []string{`"id":"c1"`, `"id":"c2"`, "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream body missing %q; got:\n%s", want, body)
		}
	}
	// Two data chunks + DONE => three "data: " frames.
	if n := strings.Count(body, "data: "); n != 3 {
		t.Errorf("data frame count = %d, want 3", n)
	}
}

func TestChatStreamMidStreamError_NoDone(t *testing.T) {
	chunks := newChunkChan(
		&schemas.BifrostStreamChunk{BifrostChatResponse: &schemas.BifrostChatResponse{ID: "c1"}},
		&schemas.BifrostStreamChunk{BifrostError: &schemas.BifrostError{
			StatusCode: intPtr(http.StatusTooManyRequests),
			Error:      &schemas.ErrorField{Message: "slow down"},
		}},
	)
	fake := &fakeRouter{streamChan: chunks}
	h := NewHandler(fake, nil, nil)

	rec := postJSON(t, h.route(), "/llm/v1/chat/completions", `{"model":"gpt-4o","messages":[],"stream":true}`)
	body := rec.Body.String()
	if strings.Contains(body, "[DONE]") {
		t.Errorf("mid-stream error must not emit [DONE]; got:\n%s", body)
	}
	if !strings.Contains(body, "slow down") {
		t.Errorf("stream body missing error message; got:\n%s", body)
	}
}

func TestChatStreamPreError(t *testing.T) {
	fake := &fakeRouter{streamErr: &schemas.BifrostError{
		StatusCode: intPtr(http.StatusUnauthorized),
		Error:      &schemas.ErrorField{Message: "bad key"},
	}}
	h := NewHandler(fake, nil, nil)

	rec := postJSON(t, h.route(), "/llm/v1/chat/completions", `{"model":"gpt-4o","messages":[],"stream":true}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestMessagesUnaryAnthropic(t *testing.T) {
	fake := &fakeRouter{respResp: &schemas.BifrostResponsesResponse{ID: strPtr("resp-1")}}
	h := NewHandler(fake, nil, nil)

	rec := postJSON(t, h.route(), "/llm/v1/messages", `{"model":"anthropic/claude-3-5-sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestMessagesStreamAnthropic_EventFramesNoDone(t *testing.T) {
	created := &schemas.BifrostResponsesStreamResponse{
		Type:     schemas.ResponsesStreamResponseTypeCreated,
		Response: &schemas.BifrostResponsesResponse{ID: strPtr("resp-1"), Model: "claude-3-5-sonnet"},
	}
	chunks := newChunkChan(&schemas.BifrostStreamChunk{BifrostResponsesStreamResponse: created})
	fake := &fakeRouter{streamChan: chunks}
	h := NewHandler(fake, nil, nil)

	rec := postJSON(t, h.route(), "/llm/v1/messages", `{"model":"claude-3-5-sonnet","max_tokens":10,"messages":[],"stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: message_start") {
		t.Errorf("anthropic stream missing message_start event; got:\n%s", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Errorf("anthropic stream must not emit [DONE]; got:\n%s", body)
	}
}

func TestMessagesStreamAnthropic_MidStreamError(t *testing.T) {
	chunks := newChunkChan(&schemas.BifrostStreamChunk{BifrostError: &schemas.BifrostError{
		Error: &schemas.ErrorField{Type: strPtr("overloaded_error"), Message: "overloaded"},
	}})
	fake := &fakeRouter{streamChan: chunks}
	h := NewHandler(fake, nil, nil)

	rec := postJSON(t, h.route(), "/llm/v1/messages", `{"model":"claude","max_tokens":1,"messages":[],"stream":true}`)
	body := rec.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Errorf("anthropic mid-stream error missing 'event: error'; got:\n%s", body)
	}
}

func TestCountTokensSynchronous(t *testing.T) {
	fake := &fakeRouter{countResp: &schemas.BifrostCountTokensResponse{}}
	h := NewHandler(fake, nil, nil)

	rec := postJSON(t, h.route(), "/llm/v1/messages/count_tokens", `{"model":"claude","max_tokens":1,"messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q, want application/json (non-SSE)", ct)
	}
}

func TestUnknownMessagesSubPathIs404(t *testing.T) {
	fake := &fakeRouter{}
	h := NewHandler(fake, nil, nil)

	rec := postJSON(t, h.route(), "/llm/v1/messages/bogus", `{}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestResponsesUnary(t *testing.T) {
	fake := &fakeRouter{respResp: &schemas.BifrostResponsesResponse{ID: strPtr("resp-9")}}
	h := NewHandler(fake, nil, nil)

	rec := postJSON(t, h.route(), "/llm/v1/responses", `{"model":"gpt-4o","input":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestTextCompletionAndEmbeddings(t *testing.T) {
	fake := &fakeRouter{
		textResp: &schemas.BifrostTextCompletionResponse{ID: "txt-1"},
		embResp:  &schemas.BifrostEmbeddingResponse{},
	}
	h := NewHandler(fake, nil, nil)

	if rec := postJSON(t, h.route(), "/llm/v1/completions", `{"model":"gpt-3.5-turbo-instruct","prompt":"hi"}`); rec.Code != http.StatusOK {
		t.Fatalf("completions status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if rec := postJSON(t, h.route(), "/llm/v1/embeddings", `{"model":"text-embedding-3-small","input":"hi"}`); rec.Code != http.StatusOK {
		t.Fatalf("embeddings status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestImageGenerationUnary(t *testing.T) {
	fake := &fakeRouter{imgResp: &schemas.BifrostImageGenerationResponse{}}
	h := NewHandler(fake, nil, nil)

	rec := postJSON(t, h.route(), "/llm/v1/images/generations", `{"model":"dall-e-3","prompt":"a cat"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestBadJSONIs400(t *testing.T) {
	h := NewHandler(&fakeRouter{}, nil, nil)
	rec := postJSON(t, h.route(), "/llm/v1/chat/completions", `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHeaderHygieneStripsLeakedHeaders(t *testing.T) {
	fake := &fakeRouter{chatResp: &schemas.BifrostChatResponse{ID: "x"}}
	h := NewHandler(fake, nil, nil)

	// Seed the recorder with headers a naive downstream might set; the handler
	// must strip them via finish().
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	rec := httptest.NewRecorder()
	rec.Header().Set("X-LiteLLM-Version", "1.0")
	rec.Header().Set("llm_provider-openai-org", "org")
	h.route().ServeHTTP(rec, req)

	if rec.Header().Get("X-LiteLLM-Version") != "" {
		t.Errorf("x-litellm-* header not stripped")
	}
	if rec.Header().Get("llm_provider-openai-org") != "" {
		t.Errorf("llm_provider-* header not stripped")
	}
}

// route builds the full chi router around the handler for end-to-end path
// tests. It lives in the test package to avoid an import cycle with internal/api.
func (h *Handler) route() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/llm/", newTestRouter(h))
	return mux
}

// vkCapture asserts the identity header flows into the Bifrost virtual key.
func TestVirtualKeyInjectedFromHeader(t *testing.T) {
	fake := &fakeRouter{chatResp: &schemas.BifrostChatResponse{ID: "x"}}
	h := NewHandler(fake, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	req.Header.Set(headerProjectID, "4242")
	rec := httptest.NewRecorder()
	h.route().ServeHTTP(rec, req)

	if fake.lastVK != "4242" {
		t.Errorf("virtual key = %q, want 4242", fake.lastVK)
	}
}

func TestIdentitySignatureRejectedWhenInvalid(t *testing.T) {
	secret := []byte("s3cr3t")
	h := NewHandler(&fakeRouter{}, nil, secret)

	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	req.Header.Set(headerProjectID, "1")
	req.Header.Set(headerSignature, "sha256=deadbeef")
	rec := httptest.NewRecorder()
	h.route().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestIdentitySignatureAcceptedWhenValid(t *testing.T) {
	secret := []byte("s3cr3t")
	fake := &fakeRouter{chatResp: &schemas.BifrostChatResponse{ID: "ok"}}
	h := NewHandler(fake, nil, secret)

	id := identity{projectID: "77"}
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	req.Header.Set(headerProjectID, "77")
	req.Header.Set(headerSignature, id.sign(secret))
	rec := httptest.NewRecorder()
	h.route().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if fake.lastVK != "77" {
		t.Errorf("virtual key = %q, want 77", fake.lastVK)
	}
}

func TestResponsesStream_EventFramesNoDone(t *testing.T) {
	created := &schemas.BifrostResponsesStreamResponse{
		Type:     schemas.ResponsesStreamResponseTypeCreated,
		Response: &schemas.BifrostResponsesResponse{ID: strPtr("resp-1")},
	}
	chunks := newChunkChan(&schemas.BifrostStreamChunk{BifrostResponsesStreamResponse: created})
	fake := &fakeRouter{streamChan: chunks}
	h := NewHandler(fake, nil, nil)

	rec := postJSON(t, h.route(), "/llm/v1/responses", `{"model":"gpt-4o","input":"hi","stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: ") {
		t.Errorf("responses stream missing event frame; got:\n%s", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Errorf("responses stream must not emit [DONE]; got:\n%s", body)
	}
}

func TestResponsesStream_MidStreamError(t *testing.T) {
	chunks := newChunkChan(&schemas.BifrostStreamChunk{BifrostError: &schemas.BifrostError{
		StatusCode: intPtr(http.StatusServiceUnavailable),
		Error:      &schemas.ErrorField{Message: "unavailable"},
	}})
	fake := &fakeRouter{streamChan: chunks}
	h := NewHandler(fake, nil, nil)

	rec := postJSON(t, h.route(), "/llm/v1/responses", `{"model":"gpt-4o","input":"hi","stream":true}`)
	body := rec.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Errorf("responses mid-stream error missing 'event: error'; got:\n%s", body)
	}
}

func TestResponsesStreamPreError(t *testing.T) {
	fake := &fakeRouter{streamErr: &schemas.BifrostError{
		StatusCode: intPtr(http.StatusServiceUnavailable),
		Error:      &schemas.ErrorField{Message: "down"},
	}}
	h := NewHandler(fake, nil, nil)

	rec := postJSON(t, h.route(), "/llm/v1/responses", `{"model":"gpt-4o","input":"hi","stream":true}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestMessagesUnaryError_Anthropic(t *testing.T) {
	fake := &fakeRouter{respErr: &schemas.BifrostError{
		StatusCode: intPtr(http.StatusUnauthorized),
		Error:      &schemas.ErrorField{Message: "bad key"},
	}}
	h := NewHandler(fake, nil, nil)

	rec := postJSON(t, h.route(), "/llm/v1/messages", `{"model":"claude","max_tokens":1,"messages":[]}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	// Fix round-3 #4: /llm/v1/messages errors MUST be OpenAI-shaped (spec §2.5),
	// not Anthropic-shaped. Assert the nested error envelope.
	var out openAIError
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal error body: %v — must be OpenAI-shaped {error:{...}}", err)
	}
	if out.Error.Message == "" {
		t.Errorf("error.message is empty; got body=%s", rec.Body.String())
	}
	if out.Error.Code != "unauthenticated" {
		t.Errorf("error.code = %q, want unauthenticated (spec §2.5)", out.Error.Code)
	}
}

// TestMessagesUnaryError_IsOpenAIShaped is the spec §2.5 assertion: the error
// body from /llm/v1/messages MUST be {"error":{message,type,code}}, NOT the
// Anthropic {type,error:{type,message}} envelope. This test proves the fix was
// applied and prevents regression.
func TestMessagesUnaryError_IsOpenAIShaped(t *testing.T) {
	fake := &fakeRouter{respErr: &schemas.BifrostError{
		StatusCode: intPtr(http.StatusTooManyRequests),
		Error:      &schemas.ErrorField{Message: "rate limited", Type: strPtr("rate_limit_error")},
	}}
	h := NewHandler(fake, nil, nil)

	rec := postJSON(t, h.route(), "/llm/v1/messages", `{"model":"claude","max_tokens":1,"messages":[]}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body=%s", rec.Code, rec.Body.String())
	}
	// Must be OpenAI-shaped, NOT Anthropic-shaped.
	var out openAIError
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v — body=%s; must be {\"error\":{...}}", err, rec.Body.String())
	}
	if out.Error.Type == "" {
		t.Errorf("error.type is empty; /llm/v1/messages must emit OpenAI-shaped errors")
	}
	if out.Error.Code != "rate_limit_exceeded" {
		t.Errorf("error.code = %q, want rate_limit_exceeded (spec §2.5)", out.Error.Code)
	}
}

// TestMessagesStreamPreError_IsOpenAIShaped verifies that streaming /messages
// pre-errors (returned before any SSE frame) are also OpenAI-shaped.
func TestMessagesStreamPreError_IsOpenAIShaped(t *testing.T) {
	fake := &fakeRouter{streamErr: &schemas.BifrostError{
		StatusCode: intPtr(http.StatusServiceUnavailable),
		Error:      &schemas.ErrorField{Message: "overloaded"},
	}}
	h := NewHandler(fake, nil, nil)

	rec := postJSON(t, h.route(), "/llm/v1/messages", `{"model":"claude","max_tokens":1,"messages":[],"stream":true}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	var out openAIError
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v — body=%s; streaming pre-error must be OpenAI-shaped", err, rec.Body.String())
	}
	if out.Error.Message == "" {
		t.Errorf("error.message is empty; streaming pre-error must be OpenAI-shaped")
	}
}

func TestMessagesStreamPreError_Anthropic(t *testing.T) {
	fake := &fakeRouter{streamErr: &schemas.BifrostError{
		StatusCode: intPtr(http.StatusTooManyRequests),
		Error:      &schemas.ErrorField{Message: "slow"},
	}}
	h := NewHandler(fake, nil, nil)

	rec := postJSON(t, h.route(), "/llm/v1/messages", `{"model":"claude","max_tokens":1,"messages":[],"stream":true}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
}

func TestCountTokensError_Anthropic(t *testing.T) {
	fake := &fakeRouter{countErr: &schemas.BifrostError{
		StatusCode: intPtr(http.StatusBadRequest),
		Error:      &schemas.ErrorField{Message: "bad"},
	}}
	h := NewHandler(fake, nil, nil)

	rec := postJSON(t, h.route(), "/llm/v1/messages/count_tokens", `{"model":"claude","max_tokens":1,"messages":[]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	// Fix round-3 #4: count_tokens errors must also be OpenAI-shaped.
	var out openAIError
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v — must be OpenAI-shaped", err)
	}
	if out.Error.Message == "" {
		t.Errorf("error.message empty; count_tokens error must be OpenAI-shaped")
	}
}

func TestUnaryErrorPaths_OpenAIDialects(t *testing.T) {
	authErr := func() *schemas.BifrostError {
		return &schemas.BifrostError{StatusCode: intPtr(http.StatusUnauthorized), Error: &schemas.ErrorField{Message: "x"}}
	}
	cases := []struct {
		name, path, body string
		fake             *fakeRouter
	}{
		{"text", "/llm/v1/completions", `{"model":"m","prompt":"p"}`, &fakeRouter{textErr: authErr()}},
		{"embeddings", "/llm/v1/embeddings", `{"model":"m","input":"i"}`, &fakeRouter{embErr: authErr()}},
		{"image_gen", "/llm/v1/images/generations", `{"model":"m","prompt":"p"}`, &fakeRouter{imgErr: authErr()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler(tc.fake, nil, nil)
			rec := postJSON(t, h.route(), tc.path, tc.body)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			// Assert OpenAI-shaped error body: error.message non-empty and error.type set.
			var out openAIError
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("unmarshal error body: %v — body=%s; must be OpenAI-shaped {error:{...}}", err, rec.Body.String())
			}
			if out.Error.Message == "" {
				t.Errorf("error.message is empty; %s must emit OpenAI-shaped errors with non-empty message", tc.name)
			}
			if out.Error.Type == "" {
				t.Errorf("error.type is empty; %s must emit OpenAI-shaped errors with non-empty type", tc.name)
			}
		})
	}
}

func TestNotFoundAndMethodNotAllowedHandlers(t *testing.T) {
	h := NewHandler(&fakeRouter{}, nil, nil)

	recNF := httptest.NewRecorder()
	h.NotFound(recNF, httptest.NewRequest(http.MethodPost, "/whatever", nil))
	if recNF.Code != http.StatusNotFound {
		t.Errorf("NotFound status = %d, want 404", recNF.Code)
	}

	recMNA := httptest.NewRecorder()
	h.MethodNotAllowed(recMNA, httptest.NewRequest(http.MethodGet, "/llm/v1/chat/completions", nil))
	if recMNA.Code != http.StatusMethodNotAllowed {
		t.Errorf("MethodNotAllowed status = %d, want 405", recMNA.Code)
	}
}

// nonFlushWriter is an http.ResponseWriter that does NOT implement http.Flusher,
// to exercise the streaming-unsupported path.
type nonFlushWriter struct {
	header http.Header
	buf    bytes.Buffer
	status int
}

func (n *nonFlushWriter) Header() http.Header {
	if n.header == nil {
		n.header = http.Header{}
	}
	return n.header
}
func (n *nonFlushWriter) Write(p []byte) (int, error) { return n.buf.Write(p) }
func (n *nonFlushWriter) WriteHeader(s int)           { n.status = s }

func TestStreamingUnsupportedWriter(t *testing.T) {
	chunks := newChunkChan(&schemas.BifrostStreamChunk{BifrostChatResponse: &schemas.BifrostChatResponse{ID: "c1"}})
	fake := &fakeRouter{streamChan: chunks}
	h := NewHandler(fake, nil, nil)

	w := &nonFlushWriter{}
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[],"stream":true}`))
	h.Chat(w, req)

	if w.status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.status)
	}
}
