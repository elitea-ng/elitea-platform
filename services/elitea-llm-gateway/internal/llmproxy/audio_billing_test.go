// audio_billing_test.go — the audio money path (issue #323).
//
// audio_test.go asserts the WIRE shape the pylon-indexer callers read. This
// file asserts what the request BILLS, and it asserts it end to end: a real
// HTTP request through the route, into the budget gate, out onto the fake
// counter. A test that calls speechUnits and stops proves the selection is
// right; it does not prove the handler uses the selection.
//
// The audio counters are process-wide expvar variables, so every test here
// snapshots them before it acts and reads the DELTA. An absolute value would
// depend on test order.
package llmproxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"unicode/utf8"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/cost"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/failmode"
)

// allowingGate returns a budget checker that admits every request and records
// the amount the handler bills.
func allowingGate() *fakeBudgetChecker {
	return &fakeBudgetChecker{
		checkVerdict: failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy},
		updated:      make(chan struct{}),
	}
}

// bifrostLikeSpeechRouter answers a speech request the way the real router
// does: BifrostSpeechResponse.BackfillParams runs on EVERY speech response and
// sets Usage.InputChars to the rune count of the input bifrost forwarded.
//
// The character-basis tests use it rather than a canned InputChars, because a
// canned one asserts a shape bifrost cannot produce (issue #323 review): the
// count is not a provider figure, it is a count of OUR text, and a test that
// pins it to an arbitrary number proves nothing about the quantity the gateway
// really bills.
type bifrostLikeSpeechRouter struct {
	fakeRouter
}

func (r *bifrostLikeSpeechRouter) SpeechRequest(ctx *schemas.BifrostContext, req *schemas.BifrostSpeechRequest) (*schemas.BifrostSpeechResponse, *schemas.BifrostError) {
	resp, bErr := r.fakeRouter.SpeechRequest(ctx, req)
	if resp != nil && req != nil && req.Input != nil {
		if resp.Usage == nil {
			resp.Usage = &schemas.SpeechUsage{}
		}
		resp.Usage.InputChars = utf8.RuneCountInString(req.Input.Input)
	}
	return resp, bErr
}

// audioMetrics snapshots the two audio counters and returns a function that
// reports how much each one moved.
func audioMetrics(t *testing.T) func() (unpriced, nonToken int64) {
	t.Helper()
	beforeUnpriced := audioUnpriced.Value()
	beforeNonToken := audioNonTokenBasis.Value()
	return func() (int64, int64) {
		return audioUnpriced.Value() - beforeUnpriced,
			audioNonTokenBasis.Value() - beforeNonToken
	}
}

// audioDefaultPricedDelta reports how far MetricAudioDefaultPriced moved.
func audioDefaultPricedDelta(t *testing.T) func() int64 {
	t.Helper()
	before := audioDefaultPriced.Value()
	return func() int64 { return audioDefaultPriced.Value() - before }
}

// TestTranscription_TokenPriceNotFromTheCatalogIsCounted closes the last silent
// hole in the audio money path.
//
// The seconds and characters bases refuse a rate that is not from the catalog,
// so they bill a real price or nothing, and the nothing is counted. The TOKEN
// basis does not: it falls back to the pylon default table like every other
// route, so an audio request CAN bill a figure the gateway invented. Until this
// counter existed nothing reported it — MetricAudioUnpriced cannot fire because
// a price was produced, and MetricAudioNonTokenBasis cannot fire because the
// basis is tokens. An invented amount reached the authoritative counter silently.
//
// gpt-4o-transcribe is the real shape: it reports TOKENS, so a deployment whose
// catalog has not synced bills it from the fallback rate.
func TestTranscription_TokenPriceNotFromTheCatalogIsCounted(t *testing.T) {
	delta := audioMetrics(t)
	defaultPriced := audioDefaultPricedDelta(t)
	gate := allowingGate()
	// A fabricated price: non-zero, plausible, and NOT from the catalog.
	calc := &fakeCostEstimator{inputRateNano: 2, outputRateNano: 4, source: cost.SourceFallback}
	in, out := 30, 7
	router := &fakeRouter{transcriptionResp: &schemas.BifrostTranscriptionResponse{
		Text:  "hello",
		Usage: &schemas.TranscriptionUsage{Type: "tokens", InputTokens: &in, OutputTokens: &out},
	}}
	h := NewHandler(router, nil, nil, WithBudgetGate(gate, calc)).route()

	rec := postAudioFile(t, h, "/llm/v1/audio/transcriptions",
		map[string]string{"model": "gpt-4o-transcribe"}, []byte("RIFF"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	gate.waitForUpdate(t)

	// The request is still billed. This counter reports the condition; it does
	// not refuse the request, because changing the token basis's pricing policy
	// for one route is a [human decision].
	if got := gate.getLastUpdateCostNano(); got != 30*2+7*4 {
		t.Fatalf("billed = %d nano, want %d", got, 30*2+7*4)
	}
	if got := defaultPriced(); got != 1 {
		t.Fatalf("MetricAudioDefaultPriced moved by %d, want 1: a fabricated token price must not be silent", got)
	}
	// The other two must stay still: a price WAS produced, on the token basis.
	if unpriced, nonToken := delta(); unpriced != 0 || nonToken != 0 {
		t.Fatalf("counters moved by (unpriced=%d, non_token=%d), want (0, 0)", unpriced, nonToken)
	}
}

// TestTranscription_TokenPriceFromTheCatalogIsNotCounted is the negative
// control. Without it, a counter that fired on EVERY token-billed audio request
// would pass the test above and be useless.
func TestTranscription_TokenPriceFromTheCatalogIsNotCounted(t *testing.T) {
	defaultPriced := audioDefaultPricedDelta(t)
	gate := allowingGate()
	calc := &fakeCostEstimator{inputRateNano: 2, outputRateNano: 4, source: cost.SourceCatalog}
	in, out := 30, 7
	router := &fakeRouter{transcriptionResp: &schemas.BifrostTranscriptionResponse{
		Text:  "hello",
		Usage: &schemas.TranscriptionUsage{Type: "tokens", InputTokens: &in, OutputTokens: &out},
	}}
	h := NewHandler(router, nil, nil, WithBudgetGate(gate, calc)).route()

	rec := postAudioFile(t, h, "/llm/v1/audio/transcriptions",
		map[string]string{"model": "gpt-4o-transcribe"}, []byte("RIFF"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	gate.waitForUpdate(t)

	if got := defaultPriced(); got != 0 {
		t.Fatalf("MetricAudioDefaultPriced moved by %d for a CATALOG price, want 0", got)
	}
}

// TestTranscription_BillsADurationOnTheSecondsBasis is the whole point of the
// change, proved through the route.
//
// whisper-1 reports `"type":"duration"` with a fractional second count and no
// tokens. Before this, that response billed ZERO and only a counter said so.
// The handler must now convert the duration ONCE to integer milliseconds and
// bill it on the seconds basis.
func TestTranscription_BillsADurationOnTheSecondsBasis(t *testing.T) {
	delta := audioMetrics(t)
	gate := allowingGate()
	// One nano-USD per input millisecond. 12.5 s = 12_500 ms = 12_500 nano.
	calc := &fakeCostEstimator{inputRateNano: 1}
	seconds := 12.5
	router := &fakeRouter{transcriptionResp: &schemas.BifrostTranscriptionResponse{
		Text:  "hello",
		Usage: &schemas.TranscriptionUsage{Type: "duration", Seconds: &seconds},
	}}
	h := NewHandler(router, nil, nil, WithBudgetGate(gate, calc)).route()

	rec := postAudioFile(t, h, "/llm/v1/audio/transcriptions",
		map[string]string{"model": "whisper-1"}, []byte("RIFF"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	gate.waitForUpdate(t)

	if got := calc.getLastUnits(); got != (cost.Units{InputMillis: 12_500}) {
		t.Fatalf("units = %+v, want InputMillis=12500: the duration must reach the calculator as milliseconds", got)
	}
	if got := gate.getLastUpdateCostNano(); got != 12_500 {
		t.Fatalf("billed = %d nano, want 12_500", got)
	}
	if unpriced, nonToken := delta(); unpriced != 0 || nonToken != 1 {
		t.Fatalf("counters moved by (unpriced=%d, non_token=%d), want (0, 1)", unpriced, nonToken)
	}
}

// TestSpeech_BillsGeneratedAudioOnTheSecondsBasis pins the direction the speech
// route pays on. The seconds are audio the model GENERATED, so they pay the
// OUTPUT per-second rate. Passing them as input would charge the wrong column
// and would silently pay a rate that prices audio sent TO a model.
func TestSpeech_BillsGeneratedAudioOnTheSecondsBasis(t *testing.T) {
	delta := audioMetrics(t)
	gate := allowingGate()
	// Two nano-USD per output millisecond; the input rate must not apply.
	calc := &fakeCostEstimator{inputRateNano: 1_000_000, outputRateNano: 2}
	// The bifrost-like router also backfills InputChars, exactly as production
	// does, so this proves the duration still wins over the character count
	// that is ALWAYS present on a real speech response.
	router := &bifrostLikeSpeechRouter{fakeRouter{speechResp: &schemas.BifrostSpeechResponse{
		Audio: []byte{0x01},
		Usage: &schemas.SpeechUsage{AudioSeconds: 30},
	}}}
	h := NewHandler(router, nil, nil, WithBudgetGate(gate, calc)).route()

	rec := postAudioJSON(t, h, "/llm/v1/audio/speech",
		`{"model":"tts-1","input":"hello","voice":"alloy"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	gate.waitForUpdate(t)

	if got := calc.getLastUnits(); got != (cost.Units{OutputMillis: 30_000}) {
		t.Fatalf("units = %+v, want OutputMillis=30000", got)
	}
	if got := gate.getLastUpdateCostNano(); got != 60_000 {
		t.Fatalf("billed = %d nano, want 60_000 (30_000 ms at 2 nano/ms)", got)
	}
	if unpriced, nonToken := delta(); unpriced != 0 || nonToken != 1 {
		t.Fatalf("counters moved by (unpriced=%d, non_token=%d), want (0, 1)", unpriced, nonToken)
	}
}

// TestSpeech_BillsInputTextOnTheCharacterBasis covers the third basis, and it
// bills the count of the EXACT TEXT THE GATEWAY FORWARDED.
//
// No provider reports that count. bifrost computes it from our own request, and
// this test computes the same number the same way, so a failure says the
// gateway billed a different text than it sent. That is what makes the basis
// defensible: a character-billed TTS provider charges for the input text, so a
// count of that text is the sale, not an estimate of it (see speechUnits).
//
// The input is deliberately not ASCII-only: the count is RUNES, and a byte
// count would over-bill every non-Latin script.
func TestSpeech_BillsInputTextOnTheCharacterBasis(t *testing.T) {
	delta := audioMetrics(t)
	gate := allowingGate()
	calc := &fakeCostEstimator{inputRateNano: 3, outputRateNano: 1_000_000}
	router := &bifrostLikeSpeechRouter{fakeRouter{speechResp: &schemas.BifrostSpeechResponse{
		Audio: []byte{0x01},
	}}}
	h := NewHandler(router, nil, nil, WithBudgetGate(gate, calc)).route()

	const input = "здравствуйте, hello"
	wantChars := int64(utf8.RuneCountInString(input)) // 19 runes, 30 bytes
	rec := postAudioJSON(t, h, "/llm/v1/audio/speech",
		`{"model":"eleven_v3","input":"`+input+`","voice":"alloy"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	gate.waitForUpdate(t)

	if got := calc.getLastUnits(); got != (cost.Units{InputChars: wantChars}) {
		t.Fatalf("units = %+v, want InputChars=%d (the runes of the forwarded text)", got, wantChars)
	}
	if got, want := gate.getLastUpdateCostNano(), wantChars*3; got != want {
		t.Fatalf("billed = %d nano, want %d (%d chars at 3 nano/char)", got, want, wantChars)
	}
	if unpriced, nonToken := delta(); unpriced != 0 || nonToken != 1 {
		t.Fatalf("counters moved by (unpriced=%d, non_token=%d), want (0, 1)", unpriced, nonToken)
	}
}

// TestSpeech_NeverBillsTwoBases is the double-billing guard at the route.
//
// gpt-4o-mini-tts publishes BOTH a token price and a per-second price upstream,
// so one response can carry both quantities. Exactly one may pay. Summing them
// charges the project twice for one request, and nothing downstream would say
// so — the counter would simply read high.
func TestSpeech_NeverBillsTwoBases(t *testing.T) {
	delta := audioMetrics(t)
	gate := allowingGate()
	calc := &fakeCostEstimator{inputRateNano: 7, outputRateNano: 11}
	router := &bifrostLikeSpeechRouter{fakeRouter{speechResp: &schemas.BifrostSpeechResponse{
		Audio: []byte{0x01},
		Usage: &schemas.SpeechUsage{
			InputTokens: 10, OutputTokens: 2, AudioSeconds: 30,
		},
	}}}
	h := NewHandler(router, nil, nil, WithBudgetGate(gate, calc)).route()

	rec := postAudioJSON(t, h, "/llm/v1/audio/speech",
		`{"model":"gpt-4o-mini-tts","input":"hello","voice":"alloy"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	gate.waitForUpdate(t)

	if got := calc.getLastUnits(); got != (cost.Units{InputTokens: 10, OutputTokens: 2}) {
		t.Fatalf("units = %+v, want the tokens ALONE: tokens come first and nothing is summed onto them", got)
	}
	// 10*7 + 2*11 = 92. Adding the 30 s or the 500 chars would raise this.
	if got := gate.getLastUpdateCostNano(); got != 92 {
		t.Fatalf("billed = %d nano, want 92 (the token rate alone)", got)
	}
	if unpriced, nonToken := delta(); unpriced != 0 || nonToken != 0 {
		t.Fatalf("counters moved by (unpriced=%d, non_token=%d), want (0, 0) for a token-billed request",
			unpriced, nonToken)
	}
}

// TestAudio_CatalogWithNoRateIsCountedNotBilled is money rule 2 at the route.
//
// The catalog carries no per-second rate for the model, and there is no default
// one to invent. The request must bill NOTHING and must raise the unpriced
// counter. Billing zero silently is the failure this counter exists to expose:
// a zero increment and a missing increment look identical on the counter the
// budget gate reads back.
func TestAudio_CatalogWithNoRateIsCountedNotBilled(t *testing.T) {
	delta := audioMetrics(t)
	gate := allowingGate()
	calc := &audioUnpricedEstimator{fakeCostEstimator{inputRateNano: 1}}
	seconds := 12.5
	router := &fakeRouter{transcriptionResp: &schemas.BifrostTranscriptionResponse{
		Text:  "hello",
		Usage: &schemas.TranscriptionUsage{Type: "duration", Seconds: &seconds},
	}}
	h := NewHandler(router, nil, nil, WithBudgetGate(gate, calc)).route()

	rec := postAudioFile(t, h, "/llm/v1/audio/transcriptions",
		map[string]string{"model": "whisper-1"}, []byte("RIFF"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	if unpriced, nonToken := delta(); unpriced != 1 || nonToken != 0 {
		t.Fatalf("counters moved by (unpriced=%d, non_token=%d), want (1, 0)", unpriced, nonToken)
	}
	if got := gate.updateCalls.Load(); got != 0 {
		t.Fatalf("UpdateUsage was called %d times for an unpriced request, want 0", got)
	}
}

// TestTranscription_NoUsageAtAllIsCountedNotBilled covers the other unpriced
// shape: a provider that reports no usage object at all.
//
// It is written against the TRANSCRIPTION route because that is the route where
// the shape is real (issue #323 review). bifrost backfills a speech response
// with the character count of the input it forwarded, so a speech response with
// no usage is a shape the real router cannot produce; its transcription
// backfill only copies the response format, and a provider that reports no
// usage lands here.
func TestTranscription_NoUsageAtAllIsCountedNotBilled(t *testing.T) {
	delta := audioMetrics(t)
	gate := allowingGate()
	calc := &fakeCostEstimator{totalNano: 999}
	router := &fakeRouter{transcriptionResp: &schemas.BifrostTranscriptionResponse{Text: "hello"}}
	h := NewHandler(router, nil, nil, WithBudgetGate(gate, calc)).route()

	rec := postAudioFile(t, h, "/llm/v1/audio/transcriptions",
		map[string]string{"model": "whisper-1"}, []byte("RIFF"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	if unpriced, nonToken := delta(); unpriced != 1 || nonToken != 0 {
		t.Fatalf("counters moved by (unpriced=%d, non_token=%d), want (1, 0)", unpriced, nonToken)
	}
	if got := gate.updateCalls.Load(); got != 0 {
		t.Fatalf("UpdateUsage was called %d times with no usage to bill, want 0", got)
	}
}

// TestChat_DoesNotTouchTheAudioCounters proves the new accounting stays on the
// audio routes. updateUsage now delegates to updateUsageUnits, and the eleven
// token call sites go through the same function; a basis test written against
// the returned Cost alone would make every zero-cost estimator stub look
// unpriced and would stop chat billing outright.
func TestChat_DoesNotTouchTheAudioCounters(t *testing.T) {
	delta := audioMetrics(t)
	gate := allowingGate()
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{
		ID:    "chat-1",
		Usage: &schemas.BifrostLLMUsage{PromptTokens: 100, CompletionTokens: 50},
	}
	h := newBudgetHandler(router, gate, 1_500_000)

	rec := httptest.NewRecorder()
	h.Chat(rec, chatReqWithProject(t, "42", false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	gate.waitForUpdate(t)

	if got := gate.getLastUpdateCostNano(); got != 1_500_000 {
		t.Fatalf("billed = %d nano, want 1_500_000: chat billing must be unchanged", got)
	}
	if unpriced, nonToken := delta(); unpriced != 0 || nonToken != 0 {
		t.Fatalf("a chat request moved the audio counters by (unpriced=%d, non_token=%d), want (0, 0)",
			unpriced, nonToken)
	}
}

// TestChat_FallbackTokenPriceDoesNotLogAsAudio is the regression for a demo
// finding on 3.3.0. Every plain text chat on an unpriced vLLM model logged
// "audio: billed a token price the catalog did not supply" and moved
// gateway_audio_default_priced_total. updateUsage passed surfaceAudio for the
// token routes, so the audio check ran on requests with no audio units. The
// test above cannot see it: its estimator reports a CATALOG price. A chat
// priced from the fallback table is billed as before and leaves every audio
// counter still.
func TestChat_FallbackTokenPriceDoesNotLogAsAudio(t *testing.T) {
	delta := audioMetrics(t)
	defaultPriced := audioDefaultPricedDelta(t)
	gate := allowingGate()
	calc := &fakeCostEstimator{inputRateNano: 2, outputRateNano: 4, source: cost.SourceFallback}
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{
		ID:    "chat-fallback",
		Usage: &schemas.BifrostLLMUsage{PromptTokens: 30, CompletionTokens: 7},
	}
	h := NewHandler(router, nil, nil, WithBudgetGate(gate, calc))

	rec := httptest.NewRecorder()
	h.Chat(rec, chatReqWithProject(t, "42", false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	gate.waitForUpdate(t)

	if got := gate.getLastUpdateCostNano(); got != 30*2+7*4 {
		t.Fatalf("billed = %d nano, want %d: chat billing must be unchanged", got, 30*2+7*4)
	}
	if got := defaultPriced(); got != 0 {
		t.Fatalf("a text chat moved MetricAudioDefaultPriced by %d, want 0", got)
	}
	if unpriced, nonToken := delta(); unpriced != 0 || nonToken != 0 {
		t.Fatalf("a text chat moved the audio counters by (unpriced=%d, non_token=%d), want (0, 0)",
			unpriced, nonToken)
	}
}
