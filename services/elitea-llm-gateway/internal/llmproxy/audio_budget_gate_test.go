// audio_budget_gate_test.go — issue #323, the parity half.
//
// The audio data plane is built (`POST /llm/v1/audio/speech`,
// `/audio/transcriptions`, `/audio/translations`) and its BILLING is covered by
// audio_billing_test.go. Nothing covered the ADMISSION half: budget_gate_test.go
// asserts "402 before the provider" for chat, messages, responses, completions,
// embeddings and count_tokens, and never for audio.
//
// That is the gap this file closes, and it is not hypothetical. CLAUDE.md makes
// the rule explicit — "Budget gate runs before the provider on EVERY /llm
// endpoint" — because the endpoint that FORGETS the gate is indistinguishable
// from one that has it: both answer 200, and only the ceiling stops applying.
// A voice deployment is where that matters most, because pylon-indexer's ASR
// relay sends one request per utterance.
package llmproxy

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/failmode"
)

// The two audio methods trackingRouter needs. Without them the embedded
// fakeRouter answers and `called` stays false, so every assertion below would
// pass on a handler that dispatched.
func (t *trackingRouter) SpeechRequest(ctx *schemas.BifrostContext, req *schemas.BifrostSpeechRequest) (*schemas.BifrostSpeechResponse, *schemas.BifrostError) {
	t.called.Store(true)
	return t.fakeRouter.SpeechRequest(ctx, req)
}

func (t *trackingRouter) TranscriptionRequest(ctx *schemas.BifrostContext, req *schemas.BifrostTranscriptionRequest) (*schemas.BifrostTranscriptionResponse, *schemas.BifrostError) {
	t.called.Store(true)
	return t.fakeRouter.TranscriptionRequest(ctx, req)
}

// audioBudgetRequest builds the request each audio route reads: JSON for
// speech, multipart for transcription and translation.
func audioBudgetRequest(t *testing.T, path string) *http.Request {
	t.Helper()
	if path == "/llm/v1/audio/speech" {
		req := httptest.NewRequest(http.MethodPost, path,
			strings.NewReader(`{"model":"openai/tts-1","input":"hello","voice":"alloy"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(headerProjectID, "42")
		return req
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("model", "openai/whisper-1"); err != nil {
		t.Fatalf("write the model field: %v", err)
	}
	part, err := mw.CreateFormFile("file", "audio.wav")
	if err != nil {
		t.Fatalf("create the file part: %v", err)
	}
	if _, err := part.Write([]byte{0x01, 0x02, 0x03}); err != nil {
		t.Fatalf("write the file part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close the multipart writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf.Bytes()))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set(headerProjectID, "42")
	return req
}

// audioBudgetRoutes is every audio route the gateway serves. It is a list and
// not three functions so a route added later has one obvious place to join.
var audioBudgetRoutes = []string{
	"/llm/v1/audio/speech",
	"/llm/v1/audio/transcriptions",
	"/llm/v1/audio/translations",
}

// TestBudgetGate_Audio_Block402_ProviderNotCalled — a project over its ceiling
// gets 402 on every audio route, and the provider is never dialled.
//
// The route is exercised through the ROUTER and not by calling the handler
// method, so a route that is registered against the wrong handler fails here
// too.
func TestBudgetGate_Audio_Block402_ProviderNotCalled(t *testing.T) {
	for _, path := range audioBudgetRoutes {
		t.Run(path, func(t *testing.T) {
			gate := &fakeBudgetChecker{
				checkVerdict: failmode.Decision{Verdict: failmode.Block402, State: failmode.StateNATSHealthy},
				updated:      make(chan struct{}),
			}
			router := &trackingRouter{}
			router.speechResp = &schemas.BifrostSpeechResponse{Audio: []byte("should-not-reach")}
			router.transcriptionResp = &schemas.BifrostTranscriptionResponse{Text: "should-not-reach"}
			h := newBudgetHandler(router, gate, 500_000)

			rec := httptest.NewRecorder()
			h.route().ServeHTTP(rec, audioBudgetRequest(t, path))

			if rec.Code != http.StatusPaymentRequired {
				t.Fatalf("status = %d, want 402: %s admits a request over the project ceiling; "+
					"body=%s", rec.Code, path, rec.Body.String())
			}
			if router.called.Load() {
				t.Fatalf("%s called the provider after a Block402 verdict — the spend happened "+
					"and only the answer was refused", path)
			}
			if gate.checkCalls.Load() == 0 {
				t.Fatalf("%s never asked the budget gate", path)
			}
		})
	}
}

// TestBudgetGate_Audio_CheckBudgetError_Returns503 — a budget store that FAILS
// must refuse, not admit. This is the fail-closed rule (CLAUDE.md, "Enforcement
// policy"), and an endpoint can break it on its own.
func TestBudgetGate_Audio_CheckBudgetError_Returns503(t *testing.T) {
	for _, path := range audioBudgetRoutes {
		t.Run(path, func(t *testing.T) {
			gate := &fakeBudgetChecker{
				checkVerdict: failmode.Decision{Verdict: failmode.Block503, State: failmode.StateDownPGStale},
				updated:      make(chan struct{}),
			}
			router := &trackingRouter{}
			router.speechResp = &schemas.BifrostSpeechResponse{Audio: []byte("should-not-reach")}
			router.transcriptionResp = &schemas.BifrostTranscriptionResponse{Text: "should-not-reach"}
			h := newBudgetHandler(router, gate, 0)

			rec := httptest.NewRecorder()
			h.route().ServeHTTP(rec, audioBudgetRequest(t, path))

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503: %s admits a request the budget store could not "+
					"judge; body=%s", rec.Code, path, rec.Body.String())
			}
			if router.called.Load() {
				t.Fatalf("%s called the provider while the budget store was unreadable", path)
			}
		})
	}
}

// TestBudgetGate_Audio_Allow_ProviderCalled is the other direction, and it is
// the reason the two tests above measure anything: a gate that refused every
// request would satisfy both of them.
//
// It stops at admission. What an admitted audio request BILLS is asserted end
// to end in audio_billing_test.go, over the response shapes that carry a usage
// basis; repeating it here would test the fixtures, not the gate.
func TestBudgetGate_Audio_Allow_ProviderCalled(t *testing.T) {
	for _, path := range audioBudgetRoutes {
		t.Run(path, func(t *testing.T) {
			gate := &fakeBudgetChecker{
				checkVerdict: failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy},
				updated:      make(chan struct{}),
			}
			router := &trackingRouter{}
			router.speechResp = &schemas.BifrostSpeechResponse{Audio: []byte("0123456789")}
			router.transcriptionResp = &schemas.BifrostTranscriptionResponse{Text: "hello"}
			h := newBudgetHandler(router, gate, 0)

			rec := httptest.NewRecorder()
			h.route().ServeHTTP(rec, audioBudgetRequest(t, path))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			if !router.called.Load() {
				t.Fatalf("%s answered 200 without calling the provider", path)
			}
			if gate.checkCalls.Load() == 0 {
				t.Fatalf("%s never asked the budget gate, so the two refusals above prove nothing "+
					"about this route", path)
			}
		})
	}
}
