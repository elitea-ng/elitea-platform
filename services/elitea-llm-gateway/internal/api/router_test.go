package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/llmproxy"
)

// recordingRouter is an llmproxy.LLMRouter that records nothing but returns a
// canned unary success for every call, so NewRouter can be exercised end-to-end
// with real llmproxy.Handler dispatch. The route-precedence assertions below
// key off the *handler* reached (via the distinct response shapes/paths), not
// off any provider behaviour.
type recordingRouter struct{}

func (recordingRouter) ChatCompletionRequest(_ *schemas.BifrostContext, _ *schemas.BifrostChatRequest) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	return &schemas.BifrostChatResponse{ID: "chat"}, nil
}
func (recordingRouter) ChatCompletionStreamRequest(_ *schemas.BifrostContext, _ *schemas.BifrostChatRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	ch := make(chan *schemas.BifrostStreamChunk)
	close(ch)
	return ch, nil
}
func (recordingRouter) TextCompletionRequest(_ *schemas.BifrostContext, _ *schemas.BifrostTextCompletionRequest) (*schemas.BifrostTextCompletionResponse, *schemas.BifrostError) {
	return &schemas.BifrostTextCompletionResponse{ID: "text"}, nil
}
func (recordingRouter) EmbeddingRequest(_ *schemas.BifrostContext, _ *schemas.BifrostEmbeddingRequest) (*schemas.BifrostEmbeddingResponse, *schemas.BifrostError) {
	return &schemas.BifrostEmbeddingResponse{}, nil
}
func (recordingRouter) ResponsesRequest(_ *schemas.BifrostContext, _ *schemas.BifrostResponsesRequest) (*schemas.BifrostResponsesResponse, *schemas.BifrostError) {
	id := "resp"
	return &schemas.BifrostResponsesResponse{ID: &id}, nil
}
func (recordingRouter) ResponsesStreamRequest(_ *schemas.BifrostContext, _ *schemas.BifrostResponsesRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	ch := make(chan *schemas.BifrostStreamChunk)
	close(ch)
	return ch, nil
}
func (recordingRouter) CountTokensRequest(_ *schemas.BifrostContext, _ *schemas.BifrostResponsesRequest) (*schemas.BifrostCountTokensResponse, *schemas.BifrostError) {
	return &schemas.BifrostCountTokensResponse{}, nil
}
func (recordingRouter) ImageGenerationRequest(_ *schemas.BifrostContext, _ *schemas.BifrostImageGenerationRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	return &schemas.BifrostImageGenerationResponse{}, nil
}
func (recordingRouter) ImageEditRequest(_ *schemas.BifrostContext, _ *schemas.BifrostImageEditRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	return &schemas.BifrostImageGenerationResponse{}, nil
}
func (recordingRouter) ImageVariationRequest(_ *schemas.BifrostContext, _ *schemas.BifrostImageVariationRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	return &schemas.BifrostImageGenerationResponse{}, nil
}

func (recordingRouter) SpeechRequest(_ *schemas.BifrostContext, _ *schemas.BifrostSpeechRequest) (*schemas.BifrostSpeechResponse, *schemas.BifrostError) {
	return &schemas.BifrostSpeechResponse{Audio: []byte("audio")}, nil
}
func (recordingRouter) TranscriptionRequest(_ *schemas.BifrostContext, _ *schemas.BifrostTranscriptionRequest) (*schemas.BifrostTranscriptionResponse, *schemas.BifrostError) {
	return &schemas.BifrostTranscriptionResponse{Text: "hello"}, nil
}

func testRouter() http.Handler {
	h := llmproxy.NewHandler(recordingRouter{}, nil, nil)
	return NewRouter(h)
}

func post(t *testing.T, r http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestMessagesExactRoutePrecedesCatchAll is the Build-gate regression: the exact
// Anthropic /messages route must win over any OpenAI catch-all. A well-formed
// Anthropic body must reach the Messages handler and return 200, not be
// misrouted or 404'd.
func TestMessagesExactRoutePrecedesCatchAll(t *testing.T) {
	rec := post(t, testRouter(), "/llm/v1/messages",
		`{"model":"claude-3-5-sonnet","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestCountTokensRoutePrecedesSubPathCatchAll(t *testing.T) {
	rec := post(t, testRouter(), "/llm/v1/messages/count_tokens",
		`{"model":"claude","max_tokens":1,"messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("count_tokens status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// It must NOT have been swallowed by /messages/* (which returns 404).
	if strings.Contains(rec.Body.String(), "unknown messages sub-path") {
		t.Error("count_tokens was misrouted to the /messages/* catch-all")
	}
}

func TestUnknownMessagesSubPathIs404(t *testing.T) {
	rec := post(t, testRouter(), "/llm/v1/messages/bogus", `{}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestOpenAIRoutesResolve(t *testing.T) {
	routes := []struct {
		path, body string
	}{
		{"/llm/v1/chat/completions", `{"model":"gpt-4o","messages":[]}`},
		{"/llm/v1/completions", `{"model":"gpt-3.5-turbo-instruct","prompt":"hi"}`},
		{"/llm/v1/embeddings", `{"model":"text-embedding-3-small","input":"hi"}`},
		{"/llm/v1/responses", `{"model":"gpt-4o","input":"hi"}`},
		{"/llm/v1/images/generations", `{"model":"dall-e-3","prompt":"a cat"}`},
		{"/llm/v1/audio/speech", `{"model":"tts-1","input":"hi","voice":"alloy"}`},
	}
	r := testRouter()
	for _, tc := range routes {
		rec := post(t, r, tc.path, tc.body)
		if rec.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200; body=%s", tc.path, rec.Code, rec.Body.String())
		}
	}
}

func TestUnknownRouteIs404(t *testing.T) {
	rec := post(t, testRouter(), "/llm/v1/nonexistent", `{}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestWrongMethodIs405(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/llm/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	testRouter().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}
