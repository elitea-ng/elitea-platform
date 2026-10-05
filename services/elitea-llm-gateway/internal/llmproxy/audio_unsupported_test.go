package llmproxy

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

// TestAudio_AProviderThatCannotServeTheRouteAnswers501 is the refusal the
// browser voice client reads. bifrost's vLLM and Ollama adapters do not
// implement speech (and Ollama not transcription either), and an `open_ai`
// credential with a self-hosted api_base is routed to the vLLM adapter. That
// refusal carries no status. It used to reach the caller as a 500
// `api_error`, which reads as a gateway crash, so the client had nothing to
// tell the user except "something failed".
func TestAudio_AProviderThatCannotServeTheRouteAnswers501(t *testing.T) {
	unsupported := &schemas.BifrostError{
		Error: &schemas.ErrorField{
			Message: "speech is not supported by vllm provider",
			Code:    strPtr("unsupported_operation"),
		},
	}

	t.Run("speech", func(t *testing.T) {
		h := audioHandler(&fakeRouter{speechErr: unsupported})
		rec := postAudioJSON(t, h, "/llm/v1/audio/speech", `{"model":"tts-1","input":"hello"}`)
		assertUnsupported(t, rec.Code, rec.Body.Bytes())
	})
	t.Run("transcription", func(t *testing.T) {
		h := audioHandler(&fakeRouter{transcriptionErr: unsupported})
		rec := postAudioFile(t, h, "/llm/v1/audio/transcriptions",
			map[string]string{"model": "whisper-1"}, []byte("RIFF"))
		assertUnsupported(t, rec.Code, rec.Body.Bytes())
	})
}

// TestUnsupportedOperation_Is501OnEveryRoute pins that the remap is GLOBAL, on
// purpose (DECISIONS.md, "Error mapping"). statusAndType serves every handler,
// so embeddings on a provider without embeddings, or chat on a provider
// without chat, also answer 501 `invalid_request_error`, not 500 `api_error`.
// A future change that scopes the remap to audio must change this test and
// that decision together.
func TestUnsupportedOperation_Is501OnEveryRoute(t *testing.T) {
	unsupported := func() *schemas.BifrostError {
		return &schemas.BifrostError{Error: &schemas.ErrorField{
			Message: "operation is not supported by this provider",
			Code:    strPtr("unsupported_operation"),
		}}
	}
	cases := []struct {
		name, path, body string
		fake             *fakeRouter
	}{
		{"embeddings", "/llm/v1/embeddings", `{"model":"m","input":"i"}`, &fakeRouter{embErr: unsupported()}},
		{"chat", "/llm/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, &fakeRouter{chatErr: unsupported()}},
		{"image_gen", "/llm/v1/images/generations", `{"model":"m","prompt":"p"}`, &fakeRouter{imgErr: unsupported()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postJSON(t, NewHandler(tc.fake, nil, nil).route(), tc.path, tc.body)
			assertUnsupported(t, rec.Code, rec.Body.Bytes())
		})
	}
}

func assertUnsupported(t *testing.T, status int, raw []byte) {
	t.Helper()
	if status != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", status, raw)
	}
	var body struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode body: %v; body=%s", err, raw)
	}
	if body.Error.Code != "unsupported_operation" || body.Error.Type != "invalid_request_error" {
		t.Fatalf("error = %+v, want invalid_request_error/unsupported_operation", body.Error)
	}
}
