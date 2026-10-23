package llmproxy

// stream_disconnect_gate_test.go — the issue #9 gate.
//
// This file deliberately uses ONLY the handler API that existed before the fix,
// so it compiles and runs against `main` (where it FAILS) as well as against the
// fix (where it passes). That is the point: the previous attempt at this bug
// shipped with tests that passed against a fake which could not reproduce the
// failure — the fake's channel was pre-filled and pre-closed, so it kept
// producing after a simulated write error, which the real producer cannot do.
//
// The reproduction here has the two properties that fake lacked:
//  1. a REAL client hangup through a real httptest server (not a stub
//     ResponseWriter that returns an error), and
//  2. a producer that genuinely honours ctx.Done() on every send and closes the
//     channel when the context dies — exactly like bifrost's stream goroutine
//     (providers/utils.GateSendChunk + CloseStream).
//
// Against main the request context IS the provider context, so the hangup
// cancels it, the producer tears down before the usage trailer, and the stream
// is billed nothing. Against the fix the provider context is decoupled for the
// grace period, the trailer arrives, and the drain bills it.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/failmode"
)

// ctxHonouringRouter is a fake router whose stream producer behaves like
// bifrost's: every send is guarded by ctx.Done(), and the channel is closed when
// the producer returns. The trailer (the usage-carrying chunk) is emitted only
// after trailerDelay, and only if the context is still alive — modelling the
// real ordering, where usage arrives at the very end of generation, well after
// a client may have hung up.
type ctxHonouringRouter struct {
	fakeRouter

	preamble     []*schemas.BifrostStreamChunk
	trailer      *schemas.BifrostStreamChunk
	trailerDelay time.Duration
	// holdOpen keeps the channel open after the trailer is sent, until the
	// context is cancelled — a provider that has delivered its usage but has
	// not finished unwinding. That is the state a drain is in when a SIGTERM
	// lands on it.
	holdOpen bool

	// tornDown records that the provider context died before the trailer could
	// be sent — i.e. the gateway destroyed the authoritative usage record.
	tornDown atomic.Bool
	// trailerSent records that the trailer actually reached the channel.
	trailerSent atomic.Bool
}

func (r *ctxHonouringRouter) stream(ctx *schemas.BifrostContext) chan *schemas.BifrostStreamChunk {
	ch := make(chan *schemas.BifrostStreamChunk)
	go func() {
		defer close(ch) // bifrost: `defer providerUtils.CloseStream(ctx, responseChan)`
		for _, c := range r.preamble {
			select {
			case ch <- c:
			case <-ctx.Done():
				r.tornDown.Store(true)
				return
			}
		}
		select {
		case <-time.After(r.trailerDelay):
		case <-ctx.Done():
			r.tornDown.Store(true)
			return
		}
		if r.trailer == nil {
			return
		}
		select {
		case ch <- r.trailer:
			r.trailerSent.Store(true)
		case <-ctx.Done():
			r.tornDown.Store(true)
			return
		}
		if r.holdOpen {
			<-ctx.Done()
		}
	}()
	return ch
}

func (r *ctxHonouringRouter) ChatCompletionStreamRequest(ctx *schemas.BifrostContext, _ *schemas.BifrostChatRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	return r.stream(ctx), nil
}

func (r *ctxHonouringRouter) ResponsesStreamRequest(ctx *schemas.BifrostContext, _ *schemas.BifrostResponsesRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	return r.stream(ctx), nil
}

// chatDelta builds one OpenAI-dialect content-delta chunk.
func chatDelta(id, text string) *schemas.BifrostStreamChunk {
	return &schemas.BifrostStreamChunk{BifrostChatResponse: &schemas.BifrostChatResponse{
		ID:     id,
		Object: "chat.completion.chunk",
		Choices: []schemas.BifrostResponseChoice{{
			Index: 0,
			ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
				Delta: &schemas.ChatStreamResponseChoiceDelta{Content: strPtr(text)},
			},
		}},
	}}
}

// chatTrailer builds the final usage-carrying chunk of an OpenAI chat stream.
func chatTrailer(in, out int) *schemas.BifrostStreamChunk {
	return &schemas.BifrostStreamChunk{BifrostChatResponse: &schemas.BifrostChatResponse{
		ID:     "chatcmpl-trailer",
		Object: "chat.completion.chunk",
		Usage:  &schemas.BifrostLLMUsage{PromptTokens: in, CompletionTokens: out},
	}}
}

// responsesDelta builds one Responses-API delta event.
func responsesDelta(text string) *schemas.BifrostStreamChunk {
	return &schemas.BifrostStreamChunk{BifrostResponsesStreamResponse: &schemas.BifrostResponsesStreamResponse{
		Type:  schemas.ResponsesStreamResponseTypeOutputTextDelta,
		Delta: strPtr(text),
	}}
}

// responsesTrailer builds the response.completed event carrying usage.
func responsesTrailer(in, out int) *schemas.BifrostStreamChunk {
	return &schemas.BifrostStreamChunk{BifrostResponsesStreamResponse: &schemas.BifrostResponsesStreamResponse{
		Type: schemas.ResponsesStreamResponseTypeCompleted,
		Response: &schemas.BifrostResponsesResponse{
			ID:    strPtr("resp-trailer"),
			Usage: &schemas.ResponsesResponseUsage{InputTokens: in, OutputTokens: out},
		},
	}}
}

// Rates chosen so the billed amount is unambiguous: 11*1_000 + 22*10_000 = 231_000.
const (
	gateInputRateNano  = int64(1_000)
	gateOutputRateNano = int64(10_000)
	gateWantCostNano   = 11*gateInputRateNano + 22*gateOutputRateNano
)

// TestStreamDisconnect_RealHangup_BillsProviderUsage is the issue #9 acceptance
// gate for all three SSE dialects.
//
// A client starts a streaming request, reads the first chunk, then hangs up.
// The provider goes on generating and emits its usage trailer afterwards. The
// gateway MUST bill that trailer: the tokens were produced and the provider
// charges for them, so leaving them unbilled is free inference and a hard-budget
// bypass.
//
// MUST FAIL against main: there the provider stream is bound to the request
// context, so the hangup tears the producer down (assert tornDown) and nothing
// is ever billed.
func TestStreamDisconnect_RealHangup_BillsProviderUsage(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		body     string
		preamble []*schemas.BifrostStreamChunk
		trailer  *schemas.BifrostStreamChunk
	}{
		{
			name:     "openai_chat",
			path:     "/llm/v1/chat/completions",
			body:     `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`,
			preamble: []*schemas.BifrostStreamChunk{chatDelta("c1", "hello "), chatDelta("c2", "world")},
			trailer:  chatTrailer(11, 22),
		},
		{
			name:     "responses",
			path:     "/llm/v1/responses",
			body:     `{"model":"openai/gpt-4o","input":"hi","stream":true}`,
			preamble: []*schemas.BifrostStreamChunk{responsesDelta("hello "), responsesDelta("world")},
			trailer:  responsesTrailer(11, 22),
		},
		{
			name:     "anthropic_messages",
			path:     "/llm/v1/messages",
			body:     `{"model":"anthropic/claude-3-5-sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}],"stream":true}`,
			preamble: []*schemas.BifrostStreamChunk{responsesDelta("hello "), responsesDelta("world")},
			trailer:  responsesTrailer(11, 22),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate := &fakeBudgetChecker{
				checkVerdict: failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy},
				updated:      make(chan struct{}),
			}
			router := &ctxHonouringRouter{
				preamble: tc.preamble,
				trailer:  tc.trailer,
				// Long enough that the hangup is certainly observed by the
				// server first, short enough to stay well inside the grace.
				trailerDelay: 300 * time.Millisecond,
			}
			calc := &fakeCostEstimator{inputRateNano: gateInputRateNano, outputRateNano: gateOutputRateNano}
			h := NewHandler(router, nil, nil, WithBudgetGate(gate, calc))

			srv := newStreamTestServer(t, h)
			defer srv.close()

			srv.hangUpMidStream(t, tc.path, tc.body)

			// The client is gone. The provider's usage trailer must still be
			// billed, exactly once.
			gate.waitForUpdate(t)
			h.DrainBilling()

			if router.tornDown.Load() {
				t.Error("provider stream was torn down by the client hangup — " +
					"the usage trailer was destroyed by the cancellation we are billing for (issue #9)")
			}
			if !router.trailerSent.Load() {
				t.Error("provider never got to send its usage trailer")
			}
			if got := gate.updateCalls.Load(); got != 1 {
				t.Fatalf("UpdateUsage calls = %d, want exactly 1 (bill the disconnected stream once)", got)
			}
			if got := gate.getLastUpdateCostNano(); got != gateWantCostNano {
				t.Errorf("billed %d nano-USD, want %d (provider-reported 11 in / 22 out)", got, gateWantCostNano)
			}
			if in, out := calc.getLastTokens(); in != 11 || out != 22 {
				t.Errorf("billed tokens = (%d, %d), want (11, 22) — the numbers must come from the "+
					"provider trailer, never from an estimate", in, out)
			}
		})
	}
}

// TestStreamCleanCompletion_BillsExactlyOnce pins the other half of the
// contract: a stream the client reads to completion is billed once and only
// once — the disconnect path must not double-bill it.
func TestStreamCleanCompletion_BillsExactlyOnce(t *testing.T) {
	gate := &fakeBudgetChecker{
		checkVerdict: failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy},
		updated:      make(chan struct{}),
	}
	router := &ctxHonouringRouter{
		preamble:     []*schemas.BifrostStreamChunk{chatDelta("c1", "hello "), chatDelta("c2", "world")},
		trailer:      chatTrailer(11, 22),
		trailerDelay: time.Millisecond,
	}
	calc := &fakeCostEstimator{inputRateNano: gateInputRateNano, outputRateNano: gateOutputRateNano}
	h := NewHandler(router, nil, nil, WithBudgetGate(gate, calc))

	srv := newStreamTestServer(t, h)
	defer srv.close()

	body := `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`
	resp := srv.post(t, "/llm/v1/chat/completions", body)
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("read stream: %v", err)
	}
	_ = resp.Body.Close()

	gate.waitForUpdate(t)
	h.DrainBilling()

	if got := gate.updateCalls.Load(); got != 1 {
		t.Fatalf("UpdateUsage calls = %d, want exactly 1 (no double billing on the clean path)", got)
	}
	if got := gate.getLastUpdateCostNano(); got != gateWantCostNano {
		t.Errorf("billed %d nano-USD, want %d", got, gateWantCostNano)
	}
}

// ── real-server harness ───────────────────────────────────────────────────────

// streamTestServer is a real HTTP server plus a client whose connections can be
// cut mid-response. httptest.ResponseRecorder cannot model this: a disconnect is
// a transport event that cancels the request context, which is precisely the
// mechanism under test.
type streamTestServer struct {
	url       string
	transport *http.Transport
	close     func()
}

func newStreamTestServer(t *testing.T, h *Handler) *streamTestServer {
	t.Helper()
	srv := httptest.NewServer(h.route())
	tr := &http.Transport{DisableKeepAlives: true}
	return &streamTestServer{
		url:       srv.URL,
		transport: tr,
		close: func() {
			tr.CloseIdleConnections()
			srv.Close()
		},
	}
}

func (s *streamTestServer) post(t *testing.T, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, s.url+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerProjectID, "30")
	client := &http.Client{Transport: s.transport, Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	return resp
}

// hangUpMidStream reads far enough to be sure the stream is flowing, then drops
// the connection without draining it — the real "user closed the tab" event.
func (s *streamTestServer) hangUpMidStream(t *testing.T, path, body string) {
	t.Helper()
	resp := s.post(t, path, body)

	// Block until the first SSE frame has actually been flushed, so the hangup
	// lands mid-stream rather than before the provider produced anything.
	buf := make([]byte, 1)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("read first stream byte: %v", err)
	}

	// Close the body without consuming it: net/http cannot reuse a connection
	// whose response body was abandoned, so it closes the TCP connection and the
	// server cancels the request context.
	_ = resp.Body.Close()
	s.transport.CloseIdleConnections()
}
