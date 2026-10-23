package llmproxy

// stream_disconnect_test.go — the rest of the issue #9 contract: what happens
// when the grace period does NOT recover the usage trailer, and the guardrails
// around the mechanism (bounded concurrency, bounded shutdown, no estimate ever
// reaching the money path, no context leak on the blocked path).
//
// The end-to-end reproduction lives in stream_disconnect_gate_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/config"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/failmode"
)

// recordingEvents captures gateway.events.* publishes so tests can assert the
// loss is metered rather than silently dropped.
type recordingEvents struct {
	mu     sync.Mutex
	events [][]byte
	fired  chan struct{}
	once   sync.Once
}

func newRecordingEvents() *recordingEvents {
	return &recordingEvents{fired: make(chan struct{})}
}

func (r *recordingEvents) PublishOpsEvent(_ context.Context, event []byte) error {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
	r.once.Do(func() { close(r.fired) })
	return nil
}

// waitForEvent blocks until the first event is published (or the test times out).
func (r *recordingEvents) waitForEvent(t *testing.T) {
	t.Helper()
	select {
	case <-r.fired:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a gateway.events.* publish")
	}
}

// decodeUnbilled returns the decoded payload of the first budget.unbilled_stream
// event, failing the test when none was published.
func (r *recordingEvents) decodeUnbilled(t *testing.T) unbilledStreamPayload {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, raw := range r.events {
		var env softAlertEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("unmarshal envelope: %v", err)
		}
		if env.Type != unbilledStreamEventType {
			continue
		}
		var p unbilledStreamPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		return p
	}
	t.Fatalf("no %s event published; got %d event(s)", unbilledStreamEventType, len(r.events))
	return unbilledStreamPayload{}
}

// silentRouter returns a stream that emits its preamble and then goes quiet
// forever, closing only when the provider context is cancelled — a provider
// that never sends a usage trailer.
type silentRouter struct {
	fakeRouter
	preamble []*schemas.BifrostStreamChunk
	// closed is closed by the producer when it finally returns, so tests can
	// assert the provider stream really was torn down.
	closed chan struct{}
}

func newSilentRouter(preamble ...*schemas.BifrostStreamChunk) *silentRouter {
	return &silentRouter{preamble: preamble, closed: make(chan struct{})}
}

func (r *silentRouter) stream(ctx *schemas.BifrostContext) chan *schemas.BifrostStreamChunk {
	ch := make(chan *schemas.BifrostStreamChunk)
	go func() {
		defer close(ch)
		defer close(r.closed)
		for _, c := range r.preamble {
			select {
			case ch <- c:
			case <-ctx.Done():
				return
			}
		}
		<-ctx.Done() // no trailer, ever
	}()
	return ch
}

func (r *silentRouter) ChatCompletionStreamRequest(ctx *schemas.BifrostContext, _ *schemas.BifrostChatRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	return r.stream(ctx), nil
}

func (r *silentRouter) ResponsesStreamRequest(ctx *schemas.BifrostContext, _ *schemas.BifrostResponsesRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	return r.stream(ctx), nil
}

// failAfterWriter is an http.ResponseWriter whose Write starts failing after
// okWrites successful calls — the in-process stand-in for "the client went
// away mid-stream". It is only the SSE-loop half of a disconnect; the other
// half (the provider context dying with the request) is what the real-hangup
// gate test in stream_disconnect_gate_test.go covers, and what a stub writer
// fundamentally cannot model.
type failAfterWriter struct {
	okWrites int
	writes   int
	hdr      http.Header
	code     int
}

func (f *failAfterWriter) Header() http.Header {
	if f.hdr == nil {
		f.hdr = http.Header{}
	}
	return f.hdr
}

func (f *failAfterWriter) WriteHeader(code int) { f.code = code }

func (f *failAfterWriter) Write(p []byte) (int, error) {
	if f.writes >= f.okWrites {
		return 0, errClientGone
	}
	f.writes++
	return len(p), nil
}

func (f *failAfterWriter) Flush() {}

var errClientGone = errors.New("client disconnected")

// allowGate builds a budget checker that admits everything and records billing.
func allowGate() *fakeBudgetChecker {
	return &fakeBudgetChecker{
		checkVerdict: failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy},
		updated:      make(chan struct{}),
	}
}

// TestStreamDisconnect_NoTrailer_BillsNothingAndMetersLoss is the anti-estimate
// guard. When the grace period expires with no provider usage, the ONLY correct
// outcomes are: bill nothing, and make the loss loudly visible. Billing an
// observed-output estimate here was the second rejected attempt (issue #9) — it
// puts a bytes/4 heuristic on the money path, which DECISIONS.md forbids.
func TestStreamDisconnect_NoTrailer_BillsNothingAndMetersLoss(t *testing.T) {
	gate := allowGate()
	events := newRecordingEvents()
	router := newSilentRouter(chatDelta("c1", "hello "), chatDelta("c2", "world"))
	calc := &fakeCostEstimator{inputRateNano: gateInputRateNano, outputRateNano: gateOutputRateNano}
	h := NewHandler(router, nil, nil,
		WithBudgetGate(gate, calc),
		WithOpsEventPublisher(events),
		WithStreamGrace(50*time.Millisecond))

	w := &failAfterWriter{okWrites: 1} // client disconnects on the second frame
	h.Chat(w, chatReqWithProject(t, "30", true))

	events.waitForEvent(t)
	h.DrainBilling()

	if got := gate.updateCalls.Load(); got != 0 {
		t.Fatalf("UpdateUsage calls = %d, want 0 — with no provider usage NOTHING may be billed "+
			"(an observed-bytes estimate on the money path is forbidden by DECISIONS.md)", got)
	}
	p := events.decodeUnbilled(t)
	if p.ProjectID != "30" {
		t.Errorf("event project_id = %q, want 30", p.ProjectID)
	}
	if p.Reason != lossReasonWriteError {
		t.Errorf("event reason = %q, want %q", p.Reason, lossReasonWriteError)
	}
	if p.DrainOutcome != drainOutcomeGraceExpired {
		t.Errorf("event drain_outcome = %q, want %q — this provider never sends a trailer, so the "+
			"grace MUST be reported as expired; collapsing it into channel_closed hides a grace that "+
			"is too short for real traffic", p.DrainOutcome, drainOutcomeGraceExpired)
	}
	if p.ObservedOutputBytes <= 0 {
		t.Errorf("observed_output_bytes = %d, want > 0 (the magnitude of the loss must be visible)", p.ObservedOutputBytes)
	}

	// The provider stream must actually have been torn down once the grace
	// expired — the whole point of the bound is that we stop paying for a
	// generation nobody is reading.
	select {
	case <-router.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("provider stream was never torn down after the grace period")
	}
}

// TestStreamDisconnect_SilentProvider_LoopExitsOnClientGone covers the hazard
// the decoupling introduces: once the provider stream no longer dies with the
// request, a client that leaves while the provider is silent produces NO write
// error, so a loop that only watches the channel would park until the
// provider's own idle timeout (bifrost: 120 s). The SSE loop must notice the
// request context instead, settle, and return promptly.
func TestStreamDisconnect_SilentProvider_LoopExitsOnClientGone(t *testing.T) {
	gate := allowGate()
	events := newRecordingEvents()
	router := newSilentRouter(chatDelta("c1", "hello"))
	h := NewHandler(router, nil, nil,
		WithBudgetGate(gate, &fakeCostEstimator{totalNano: 1_000}),
		WithOpsEventPublisher(events),
		WithStreamGrace(50*time.Millisecond))

	reqCtx, cancelReq := context.WithCancel(context.Background())
	req := chatReqWithProject(t, "30", true).WithContext(reqCtx)

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		h.Chat(httptest.NewRecorder(), req)
	}()

	// Let the first chunk through, then hang up. The provider stays silent.
	time.Sleep(50 * time.Millisecond)
	cancelReq()

	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("SSE loop did not return after the client disconnected — it is parked waiting " +
			"for a chunk from a provider that has nothing to say")
	}

	events.waitForEvent(t)
	h.DrainBilling()

	if got := gate.updateCalls.Load(); got != 0 {
		t.Fatalf("UpdateUsage calls = %d, want 0 (no provider usage was ever reported)", got)
	}
	if p := events.decodeUnbilled(t); p.Reason != lossReasonClientGone {
		t.Errorf("event reason = %q, want %q — a silent provider cannot produce a write error, so "+
			"anything else means the loss was mislabelled", p.Reason, lossReasonClientGone)
	}
}

// TestStreamDisconnect_MultimodalPayload_BillsZeroNotAnEstimate is the direct
// regression guard for the rejected estimate: a stream carrying a large
// base64-ish payload and no usage trailer must bill exactly zero. The earlier
// attempt billed len(bytes)/4 "tokens" here, over-charging inline-image
// requests by 2–3 orders of magnitude.
func TestStreamDisconnect_MultimodalPayload_BillsZeroNotAnEstimate(t *testing.T) {
	gate := allowGate()
	events := newRecordingEvents()
	// ~256 KB of "inline base64" output — 65k phantom tokens under a bytes/4
	// heuristic, and at these rates ~650_000_000 nano-USD of phantom spend.
	huge := strings.Repeat("QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo=", 8192)
	router := newSilentRouter(chatDelta("c1", huge))
	calc := &fakeCostEstimator{inputRateNano: gateInputRateNano, outputRateNano: gateOutputRateNano}
	h := NewHandler(router, nil, nil,
		WithBudgetGate(gate, calc),
		WithOpsEventPublisher(events),
		WithStreamGrace(50*time.Millisecond))

	w := &failAfterWriter{okWrites: 0} // disconnected before the first frame lands
	h.Chat(w, chatReqWithProject(t, "30", true))

	events.waitForEvent(t)
	h.DrainBilling()

	if got := gate.updateCalls.Load(); got != 0 {
		t.Fatalf("UpdateUsage calls = %d, want 0", got)
	}
	if got := gate.getLastUpdateCostNano(); got != 0 {
		t.Fatalf("billed %d nano-USD for a stream with no provider usage, want 0", got)
	}
	if in, out := calc.getLastTokens(); in != 0 || out != 0 {
		t.Fatalf("cost calculator was called with (%d, %d) tokens — no estimated token count "+
			"may ever reach the cost path", in, out)
	}
	// The magnitude is still reported, as an observability dimension only.
	if p := events.decodeUnbilled(t); p.ObservedOutputBytes < int64(len(huge)) {
		t.Errorf("observed_output_bytes = %d, want >= %d", p.ObservedOutputBytes, len(huge))
	}
}

// TestStreamDisconnect_DrainSaturated_MetersLoss: when every drain slot is
// taken, an abandoned stream is cut loose immediately instead of queueing (the
// slot bounds open provider sockets, so queueing would defeat it) — and the
// loss is metered with the saturation reason rather than dropped.
func TestStreamDisconnect_DrainSaturated_StillRecoversTheTrailer(t *testing.T) {
	gate := allowGate()
	events := newRecordingEvents()
	calc := &fakeCostEstimator{inputRateNano: gateInputRateNano, outputRateNano: gateOutputRateNano}
	// The trailer lands well inside SaturatedStreamGrace but far outside the
	// grace=0 the fail-open version used: this test FAILS if saturation ever
	// goes back to cancelling immediately.
	router := &ctxHonouringRouter{
		preamble:     []*schemas.BifrostStreamChunk{chatDelta("c1", "hello "), chatDelta("c2", "world")},
		trailer:      chatTrailer(11, 22),
		trailerDelay: 100 * time.Millisecond,
	}
	h := NewHandler(router, nil, nil,
		WithBudgetGate(gate, calc),
		WithOpsEventPublisher(events),
		WithStreamGrace(5*time.Second),
		WithStreamDrainLimit(1))

	// Occupy the only slot (global limit 1 ⇒ per-project limit 1 too).
	if !h.drainLimit.acquire("99") {
		t.Fatal("could not take the single drain slot")
	}

	start := time.Now()
	h.Chat(&failAfterWriter{okWrites: 1}, chatReqWithProject(t, "30", true))
	gate.waitForUpdate(t)
	h.DrainBilling()

	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("saturated drain waited %v — it must fall back to the SHORT grace, not the full one", elapsed)
	}
	if got := gate.updateCalls.Load(); got != 1 {
		t.Fatalf("UpdateUsage calls = %d, want 1 — saturation must fall back to a short grace, never "+
			"to grace=0: cutting the stream immediately destroys a trailer that was 100ms away and "+
			"reopens the issue-#9 bypass", got)
	}
	if got := gate.getLastUpdateCostNano(); got != gateWantCostNano {
		t.Errorf("billed %d nano-USD, want %d", got, gateWantCostNano)
	}
}

// TestStreamDisconnect_DrainSaturated_MetersLossWhenGraceTooShort: when the
// short grace is genuinely not enough, the loss is still reported — and tagged
// so operators can tell "we were full" from "full AND we lost the trailer".
func TestStreamDisconnect_DrainSaturated_MetersLossWhenGraceTooShort(t *testing.T) {
	gate := allowGate()
	events := newRecordingEvents()
	router := newSilentRouter(chatDelta("c1", "hello"), chatDelta("c2", " world"))
	h := NewHandler(router, nil, nil,
		WithBudgetGate(gate, &fakeCostEstimator{totalNano: 1_000}),
		WithOpsEventPublisher(events),
		WithStreamGrace(5*time.Second),
		WithStreamDrainLimit(1))

	if !h.drainLimit.acquire("99") {
		t.Fatal("could not take the single drain slot")
	}

	h.Chat(&failAfterWriter{okWrites: 1}, chatReqWithProject(t, "30", true))
	events.waitForEvent(t)
	h.DrainBilling()

	if got := gate.updateCalls.Load(); got != 0 {
		t.Fatalf("UpdateUsage calls = %d, want 0", got)
	}
	p := events.decodeUnbilled(t)
	if !strings.HasPrefix(p.DrainOutcome, drainOutcomeSaturated) {
		t.Errorf("drain_outcome = %q, want a %q outcome", p.DrainOutcome, drainOutcomeSaturated)
	}
	if !strings.Contains(p.DrainOutcome, drainOutcomeGraceExpired) {
		t.Errorf("drain_outcome = %q, want it to also carry %q — saturation alone does not tell an "+
			"operator whether the short grace was actually insufficient", p.DrainOutcome, drainOutcomeGraceExpired)
	}
}

// TestStreamDisconnect_DrainReleasesItsSlot: a completed drain must return its
// slot, or the pool bleeds capacity until every disconnect is reported as
// saturated.
func TestStreamDisconnect_DrainReleasesItsSlot(t *testing.T) {
	gate := allowGate()
	events := newRecordingEvents()
	router := newSilentRouter(chatDelta("c1", "hello"), chatDelta("c2", " world"))
	h := NewHandler(router, nil, nil,
		WithBudgetGate(gate, &fakeCostEstimator{totalNano: 1_000}),
		WithOpsEventPublisher(events),
		WithStreamGrace(50*time.Millisecond),
		WithStreamDrainLimit(1))

	w := &failAfterWriter{okWrites: 1}
	h.Chat(w, chatReqWithProject(t, "30", true))
	events.waitForEvent(t)
	h.DrainBilling()

	if !h.drainLimit.acquire("30") {
		t.Fatal("drain slot was never released — the bounded pool leaks capacity")
	}
}

// TestStreamDisconnect_DrainBillingDoesNotWaitFullGrace: graceful shutdown must
// not be held hostage by an abandoned stream. With a long grace configured,
// DrainBilling has to cut the drains loose promptly.
func TestStreamDisconnect_DrainBillingDoesNotWaitFullGrace(t *testing.T) {
	gate := allowGate()
	router := newSilentRouter(chatDelta("c1", "hello"), chatDelta("c2", " world"))
	h := NewHandler(router, nil, nil,
		WithBudgetGate(gate, &fakeCostEstimator{totalNano: 1_000}),
		WithStreamGrace(MaxStreamGrace)) // 15 s

	w := &failAfterWriter{okWrites: 1}
	h.Chat(w, chatReqWithProject(t, "30", true))

	done := make(chan struct{})
	go func() { defer close(done); h.DrainBilling() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("DrainBilling blocked on the stream grace period; a deploy would truncate on shutdown")
	}
}

// TestStreamContext_CancelledWhenBudgetBlocks: a request rejected before
// dispatch owns a stream context nobody will ever settle. It must be cancelled
// on the spot, otherwise every blocked streaming request leaks a context (and
// the watcher goroutine BifrostContext starts for it).
func TestStreamContext_CancelledWhenBudgetBlocks(t *testing.T) {
	gate := &fakeBudgetChecker{
		checkVerdict: failmode.Decision{Verdict: failmode.Block402, State: failmode.StateNATSHealthy},
		updated:      make(chan struct{}),
	}
	router := &trackingRouter{}
	h := newBudgetHandler(router, gate, 1_000)

	captured := make(chan *schemas.BifrostContext, 4)
	h.streamCtxHook = func(ctx *schemas.BifrostContext) { captured <- ctx }

	rec := httptest.NewRecorder()
	h.Chat(rec, chatReqWithProject(t, "30", true))

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", rec.Code)
	}
	if router.called.Load() {
		t.Fatal("provider was dispatched despite a 402")
	}
	var ctx *schemas.BifrostContext
	select {
	case ctx = <-captured:
	default:
		t.Fatal("no stream context was built")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("stream context outlived a blocked request — cancel was not called on the 402 path")
	}
}

// TestStreamContext_PreservesIdentityValues: decoupling cancellation must not
// decouple values. Per-project credential resolution reads the virtual key from
// this context inside bifrost — losing it would break every streamed request.
func TestStreamContext_PreservesIdentityValues(t *testing.T) {
	h := NewHandler(&fakeRouter{}, nil, nil, WithStreamGrace(DefaultStreamGrace))
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", strings.NewReader("{}"))
	req.Header.Set(headerProjectID, "4242")
	req.Header.Set(headerUserID, "user-7")

	reqCtx, cancelReq := context.WithCancel(context.Background())
	ctx, sc, ok := h.newStreamContext(httptest.NewRecorder(), req.WithContext(reqCtx))
	if !ok {
		t.Fatal("newStreamContext rejected the request")
	}
	defer sc.cancel()

	if got, _ := ctx.Value(schemas.BifrostContextKeyVirtualKey).(string); got != "4242" {
		t.Errorf("virtual key = %q, want 4242 (per-project credentials resolve from this)", got)
	}
	if got, _ := ctx.Value(schemas.BifrostContextKeyUserID).(string); got != "user-7" {
		t.Errorf("user id = %q, want user-7", got)
	}
	// The whole point: the request's cancellation must NOT reach it. Fire the
	// real cancel — asserting against a context that was never cancellable
	// passes even with the decoupling removed entirely.
	cancelReq()
	select {
	case <-ctx.Done():
		t.Fatal("the client's cancellation reached the provider context — the stream would be torn " +
			"down on disconnect and the usage trailer destroyed (issue #9)")
	case <-time.After(200 * time.Millisecond):
	}
	if ctx.Err() != nil {
		t.Errorf("stream context cancelled by the request: %v", ctx.Err())
	}
}

// TestStreamGraceDisabled_KeepsRequestBoundContext: LLM_STREAM_GRACE_MS=0 is the
// kill switch. It must restore the pre-fix behaviour exactly — provider stream
// bound to the request — so an operator can turn the mechanism off without a
// deploy of new code.
func TestStreamGraceDisabled_KeepsRequestBoundContext(t *testing.T) {
	h := NewHandler(&fakeRouter{}, nil, nil, WithStreamGrace(0))
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", strings.NewReader("{}"))
	req.Header.Set(headerProjectID, "1")
	reqCtx, cancelReq := context.WithCancel(context.Background())

	ctx, sc, ok := h.newStreamContext(httptest.NewRecorder(), req.WithContext(reqCtx))
	if !ok {
		t.Fatal("newStreamContext rejected the request")
	}
	defer sc.cancel()

	cancelReq() // simulate the client hanging up
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("with the grace disabled the provider context must still follow the request context")
	}
}

// TestWithStreamGrace_Clamped pins the configured bound: an operator cannot set
// a grace so long that the gateway finances an abandoned generation.
func TestWithStreamGrace_Clamped(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want time.Duration
	}{
		{-time.Second, 0},
		{0, 0},
		{3 * time.Second, 3 * time.Second},
		{MaxStreamGrace, MaxStreamGrace},
		{time.Hour, MaxStreamGrace},
	}
	for _, tc := range cases {
		h := NewHandler(&fakeRouter{}, nil, nil, WithStreamGrace(tc.in))
		if h.streamGrace != tc.want {
			t.Errorf("WithStreamGrace(%v) → %v, want %v", tc.in, h.streamGrace, tc.want)
		}
	}
}

// TestStreamGraceConstantsInSync keeps the env-facing defaults in config and the
// handler-facing defaults here from drifting apart — they describe the same
// policy and a mismatch would make the chart lie about the running behaviour.
func TestStreamGraceConstantsInSync(t *testing.T) {
	if config.DefaultStreamGrace != DefaultStreamGrace {
		t.Errorf("config.DefaultStreamGrace = %v, llmproxy.DefaultStreamGrace = %v", config.DefaultStreamGrace, DefaultStreamGrace)
	}
	if config.MaxStreamGrace != MaxStreamGrace {
		t.Errorf("config.MaxStreamGrace = %v, llmproxy.MaxStreamGrace = %v", config.MaxStreamGrace, MaxStreamGrace)
	}
	if config.DefaultStreamDrainLimit != DefaultStreamDrainLimit {
		t.Errorf("config.DefaultStreamDrainLimit = %d, llmproxy.DefaultStreamDrainLimit = %d", config.DefaultStreamDrainLimit, DefaultStreamDrainLimit)
	}
}

// TestShutdown_RecoveredTrailerIsBilled is the regression test for the
// gateway-review blocker: a drain that HAS captured the authoritative usage
// trailer must not lose the spend when graceful shutdown starts.
//
// The original ordering (DrainBilling before srv.Shutdown) set billingClosing
// while SSE handlers were still live, so report() took the gotUsage branch,
// spawnBillingGoroutine refused, and — because gotUsage was true — no loss
// event was emitted either. The money vanished on every rolling deploy, which
// the probe reproduced as "updateCalls=0 cost=0 events=0".
//
// Phase 1 (StopStreamGrace) stops the drain waiting for trailers WITHOUT
// closing billing, which is what makes the recovered spend billable here.
func TestShutdown_RecoveredTrailerIsBilled(t *testing.T) {
	gate := allowGate()
	events := newRecordingEvents()
	calc := &fakeCostEstimator{inputRateNano: gateInputRateNano, outputRateNano: gateOutputRateNano}
	// The trailer arrives AFTER the disconnect, so billing depends on the DRAIN
	// surviving. The first version of this test used a router that had the
	// trailer queued already: the SSE loop calls observe() before the failing
	// write, so gotUsage was set inline and the drain never participated — it
	// passed with the entire grace mechanism disabled.
	router := &ctxHonouringRouter{
		preamble:     []*schemas.BifrostStreamChunk{chatDelta("c1", "hello "), chatDelta("c2", "world")},
		trailer:      chatTrailer(11, 22),
		trailerDelay: 20 * time.Millisecond,
		holdOpen:     true, // trailer delivered, but the provider has not unwound
	}
	h := NewHandler(router, nil, nil,
		WithBudgetGate(gate, calc),
		WithOpsEventPublisher(events),
		WithStreamGrace(MaxStreamGrace))

	h.Chat(&failAfterWriter{okWrites: 1}, chatReqWithProject(t, "30", true))

	// Let the trailer reach the drain, which then stays parked on the still-open
	// channel — exactly the state a SIGTERM finds it in.
	waitFor(t, func() bool { return router.trailerSent.Load() })

	// Shutdown, in the order the composition root uses.
	h.StopStreamGrace()
	h.DrainBilling()
	if got := gate.updateCalls.Load(); got != 1 {
		t.Fatalf("UpdateUsage calls = %d, want 1 — the trailer was recovered, so %d nano-USD of "+
			"provider-reported spend must be billed, not dropped on shutdown", got, gateWantCostNano)
	}
	if got := gate.getLastUpdateCostNano(); got != gateWantCostNano {
		t.Errorf("billed %d nano-USD, want %d", got, gateWantCostNano)
	}
}

// TestShutdown_RefusedBillingIsMetered is the backstop for the same blocker: if
// the increment IS refused (billing already closed — e.g. a stream that settles
// after phase 2 has begun), the loss must still be alarmable. Known spend that
// disappears into a single WARN line is exactly what the unbilled-stream event
// exists to prevent.
func TestShutdown_RefusedBillingIsMetered(t *testing.T) {
	gate := allowGate()
	events := newRecordingEvents()
	calc := &fakeCostEstimator{inputRateNano: gateInputRateNano, outputRateNano: gateOutputRateNano}
	h := NewHandler(&fakeRouter{}, nil, nil,
		WithBudgetGate(gate, calc),
		WithOpsEventPublisher(events))

	// Billing is already closed: the drain's settle races a completed shutdown.
	h.DrainBilling()

	req := chatReqWithProject(t, "30", true)
	ctx, sc, ok := h.newStreamContext(httptest.NewRecorder(), req)
	if !ok {
		t.Fatal("newStreamContext rejected the request")
	}
	defer sc.cancel()

	s := h.newChatSettler("streamOpenAI", ctx, sc, "openai", "gpt-4o", nil)
	s.in, s.out, s.gotUsage = 11, 22, true
	s.report(lossReasonWriteError, drainOutcomeShuttingDown)

	if got := gate.updateCalls.Load(); got != 0 {
		t.Fatalf("UpdateUsage calls = %d, want 0 (billing is closed)", got)
	}
	p := events.decodeUnbilled(t)
	if p.Reason != lossReasonBillingRefused {
		t.Errorf("event reason = %q, want %q — a refused increment on KNOWN spend is a gateway-side "+
			"loss and must be distinguishable from a provider that never reported usage",
			p.Reason, lossReasonBillingRefused)
	}
	if p.ProjectID != "30" {
		t.Errorf("event project_id = %q, want 30", p.ProjectID)
	}
}

// erroringRouter emits a preamble and then a mid-stream provider error chunk.
type erroringRouter struct {
	fakeRouter
	preamble []*schemas.BifrostStreamChunk
}

func (r *erroringRouter) ChatCompletionStreamRequest(ctx *schemas.BifrostContext, _ *schemas.BifrostChatRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	ch := make(chan *schemas.BifrostStreamChunk)
	go func() {
		defer close(ch)
		chunks := append(append([]*schemas.BifrostStreamChunk{}, r.preamble...),
			&schemas.BifrostStreamChunk{BifrostError: &schemas.BifrostError{
				Error: &schemas.ErrorField{Message: "upstream exploded"},
			}})
		for _, c := range chunks {
			select {
			case ch <- c:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

// closedNoUsageRouter completes cleanly but never sends a usage trailer.
type closedNoUsageRouter struct{ fakeRouter }

func (r *closedNoUsageRouter) ChatCompletionStreamRequest(_ *schemas.BifrostContext, _ *schemas.BifrostChatRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	return newChunkChan(chatDelta("c1", "hello")), nil
}

// nonFlusherWriter has no Flush method, so beginStream refuses the stream.
type nonFlusherWriter struct{ hdr http.Header }

func (w *nonFlusherWriter) Header() http.Header {
	if w.hdr == nil {
		w.hdr = http.Header{}
	}
	return w.hdr
}
func (w *nonFlusherWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *nonFlusherWriter) WriteHeader(int)             {}

// TestUnbilledStream_ReasonAndOutcomeDimensions exercises the loss dimensions
// that had no coverage. Each is a distinct operational story — "the provider
// failed", "we could not stream at all", "the provider never reported usage",
// "the operator turned the mechanism off" — and an unreachable or mislabelled
// dimension makes the loss event unalarmable, which is the whole point of it.
func TestUnbilledStream_ReasonAndOutcomeDimensions(t *testing.T) {
	cases := []struct {
		name        string
		router      LLMRouter
		writer      http.ResponseWriter
		grace       time.Duration
		wantReason  string
		wantOutcome string
	}{
		{
			name:        "mid-stream provider error",
			router:      &erroringRouter{preamble: []*schemas.BifrostStreamChunk{chatDelta("c1", "hi")}},
			writer:      &failAfterWriter{okWrites: 10},
			grace:       50 * time.Millisecond,
			wantReason:  lossReasonProviderError,
			wantOutcome: drainOutcomeChannelClosed,
		},
		{
			name:        "streaming unsupported by the writer",
			router:      newSilentRouter(chatDelta("c1", "hi")),
			writer:      &nonFlusherWriter{},
			grace:       50 * time.Millisecond,
			wantReason:  lossReasonBeginStreamFail,
			wantOutcome: drainOutcomeGraceExpired,
		},
		{
			name:        "clean close with no trailer",
			router:      &closedNoUsageRouter{},
			writer:      &failAfterWriter{okWrites: 10},
			grace:       50 * time.Millisecond,
			wantReason:  lossReasonCleanCloseNoTrai,
			wantOutcome: drainOutcomeInline,
		},
		{
			name:        "grace disabled",
			router:      newSilentRouter(chatDelta("c1", "hi"), chatDelta("c2", "there")),
			writer:      &failAfterWriter{okWrites: 1},
			grace:       0,
			wantReason:  lossReasonWriteError,
			wantOutcome: drainOutcomeDisabled,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate := allowGate()
			events := newRecordingEvents()
			h := NewHandler(tc.router, nil, nil,
				WithBudgetGate(gate, &fakeCostEstimator{totalNano: 1_000}),
				WithOpsEventPublisher(events),
				WithStreamGrace(tc.grace))

			h.Chat(tc.writer, chatReqWithProject(t, "30", true))
			events.waitForEvent(t)
			h.DrainBilling()

			if got := gate.updateCalls.Load(); got != 0 {
				t.Fatalf("UpdateUsage calls = %d, want 0 (no provider usage in any of these)", got)
			}
			p := events.decodeUnbilled(t)
			if p.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", p.Reason, tc.wantReason)
			}
			if p.DrainOutcome != tc.wantOutcome {
				t.Errorf("drain_outcome = %q, want %q", p.DrainOutcome, tc.wantOutcome)
			}
		})
	}
}

// TestUnbilledStream_ResponsesDialectCountsObservedBytes covers the second byte
// counter (responsesDeltaBytes), which no test referenced.
func TestUnbilledStream_ResponsesDialectCountsObservedBytes(t *testing.T) {
	gate := allowGate()
	events := newRecordingEvents()
	body := strings.Repeat("x", 4096)
	h := NewHandler(newSilentRouter(responsesDelta(body), responsesDelta(body)), nil, nil,
		WithBudgetGate(gate, &fakeCostEstimator{totalNano: 1_000}),
		WithOpsEventPublisher(events),
		WithStreamGrace(50*time.Millisecond))

	req := httptest.NewRequest(http.MethodPost, "/llm/v1/responses",
		strings.NewReader(`{"model":"openai/gpt-4o","input":"hi","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerProjectID, "30")
	h.Responses(&failAfterWriter{okWrites: 1}, req)

	events.waitForEvent(t)
	h.DrainBilling()

	if got := gate.updateCalls.Load(); got != 0 {
		t.Fatalf("UpdateUsage calls = %d, want 0", got)
	}
	if p := events.decodeUnbilled(t); p.ObservedOutputBytes < int64(len(body)) {
		t.Errorf("observed_output_bytes = %d, want >= %d (responsesDeltaBytes must count Delta payloads)",
			p.ObservedOutputBytes, len(body))
	}
}

// TestUnbilledStream_UnresolvableProjectIsNotPublished: the loss event carries a
// normalised project id, like every other money path. An unattributable loss is
// logged, not published with a raw header value in the subject.
func TestUnbilledStream_ProjectIDNormalisation(t *testing.T) {
	cases := []struct {
		name      string
		projectID string
		wantOut   string // "" = must not publish
	}{
		{"unparsable", "not-a-number", ""},
		{"empty", "", ""},
		{"zero is not a project", "0", ""},
		{"negative", "-7", ""},
		{"injection attempt in the subject token", "42.events.>", ""},
		{"plain", "42", "42"},
		{"normalised", "042", "42"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := newRecordingEvents()
			h := NewHandler(&fakeRouter{}, nil, nil, WithOpsEventPublisher(events))

			h.publishUnbilledStreamEvent(tc.projectID, "openai", "gpt-4o",
				lossReasonWriteError, drainOutcomeGraceExpired, 128)

			events.mu.Lock()
			defer events.mu.Unlock()
			if tc.wantOut == "" {
				if len(events.events) != 0 {
					t.Fatalf("published %d event(s) for project %q; want 0 — an unattributable loss "+
						"is logged, never published with a raw header value", len(events.events), tc.projectID)
				}
				return
			}
			if len(events.events) != 1 {
				t.Fatalf("published %d event(s), want 1", len(events.events))
			}
			var env softAlertEnvelope
			if err := json.Unmarshal(events.events[0], &env); err != nil {
				t.Fatalf("unmarshal envelope: %v", err)
			}
			var p unbilledStreamPayload
			if err := json.Unmarshal(env.Payload, &p); err != nil {
				t.Fatalf("unmarshal payload: %v", err)
			}
			if p.ProjectID != tc.wantOut {
				t.Errorf("project_id = %q, want %q (normalised through parseProjectID like every "+
					"sibling money path)", p.ProjectID, tc.wantOut)
			}
		})
	}
}

// TestDrainLimiter_PerProjectFairness pins the fairness half of the bound: one
// project saturating its own share must not consume the whole pool, or a single
// tenant's disconnect storm degrades every other tenant's billing to the short
// saturated grace.
func TestDrainLimiter_PerProjectFairness(t *testing.T) {
	l := newDrainLimiter(16) // per-project cap = 16/8 = 2

	for i := range 2 {
		if !l.acquire("noisy") {
			t.Fatalf("slot %d for a project must be granted below its per-project cap", i)
		}
	}
	if l.acquire("noisy") {
		t.Error("a third slot for the same project was granted — the per-project cap is not enforced")
	}
	if !l.acquire("quiet") {
		t.Error("another project was starved by the noisy one — the bound is not fair")
	}

	l.release("noisy")
	if !l.acquire("noisy") {
		t.Error("a released per-project slot was not reusable")
	}
}

// TestDrainLimiter_GlobalCap keeps the pod-level bound honest.
func TestDrainLimiter_GlobalCap(t *testing.T) {
	l := newDrainLimiter(3) // per-project cap floors at 1
	for i, p := range []string{"1", "2", "3"} {
		if !l.acquire(p) {
			t.Fatalf("slot %d denied below the global cap", i)
		}
	}
	if l.acquire("4") {
		t.Error("global cap exceeded")
	}
	l.release("2")
	if !l.acquire("4") {
		t.Error("a released global slot was not reusable")
	}
	if inUse, total, per := l.snapshot(); inUse != 3 || total != 3 || per != 1 {
		t.Errorf("snapshot = (%d, %d, %d), want (3, 3, 1)", inUse, total, per)
	}
}

// TestAnthropicDialect_PartialUsageIsNotAuthoritative is the regression guard
// for the silent-underbill defect found in review round 2.
//
// bifrost maps Anthropic's message_start to response.created, and that event
// already carries usage (input_tokens, output_tokens:1). Accepting it made
// gotUsage true from the FIRST chunk, so a mid-stream disconnect billed
// "input + 1 token" as authoritative AND suppressed the loss event — an
// invisible underbill across the whole dialect, strictly worse than the visible
// loss issue #9 set out to fix. Only the terminal event's usage counts.
func TestAnthropicDialect_PartialUsageIsNotAuthoritative(t *testing.T) {
	gate := allowGate()
	events := newRecordingEvents()
	calc := &fakeCostEstimator{inputRateNano: gateInputRateNano, outputRateNano: gateOutputRateNano}

	// message_start-shaped: usage present, but on a NON-terminal event.
	partial := &schemas.BifrostStreamChunk{BifrostResponsesStreamResponse: &schemas.BifrostResponsesStreamResponse{
		Type: schemas.ResponsesStreamResponseTypeCreated,
		Response: &schemas.BifrostResponsesResponse{
			ID:    strPtr("resp-start"),
			Usage: &schemas.ResponsesResponseUsage{InputTokens: 5000, OutputTokens: 1},
		},
	}}
	h := NewHandler(newSilentRouter(partial, responsesDelta("hello")), nil, nil,
		WithBudgetGate(gate, calc),
		WithOpsEventPublisher(events),
		WithStreamGrace(50*time.Millisecond))

	h.Messages(&failAfterWriter{okWrites: 1}, messagesStreamReqWithProject(t, "30"))
	events.waitForEvent(t)
	h.DrainBilling()

	if got := gate.updateCalls.Load(); got != 0 {
		t.Fatalf("UpdateUsage calls = %d, want 0 — mid-stream usage is NOT the authoritative trailer; "+
			"billing it understates output and hides that we did", got)
	}
	if p := events.decodeUnbilled(t); p.Reason != lossReasonWriteError {
		t.Errorf("reason = %q, want %q — the loss must stay visible, not be masked by partial usage",
			p.Reason, lossReasonWriteError)
	}
}

// TestAnthropicDialect_TerminalUsageStillBills is the other half: gating on the
// terminal event must not stop a real trailer from billing.
func TestAnthropicDialect_TerminalUsageStillBills(t *testing.T) {
	gate := allowGate()
	events := newRecordingEvents()
	calc := &fakeCostEstimator{inputRateNano: gateInputRateNano, outputRateNano: gateOutputRateNano}
	h := NewHandler(&closingResponsesRouter{chunks: []*schemas.BifrostStreamChunk{
		responsesDelta("hello"), responsesTrailer(11, 22),
	}}, nil, nil,
		WithBudgetGate(gate, calc),
		WithOpsEventPublisher(events),
		WithStreamGrace(2*time.Second))

	h.Messages(&failAfterWriter{okWrites: 1}, messagesStreamReqWithProject(t, "30"))
	gate.waitForUpdate(t)
	h.DrainBilling()

	if got := gate.updateCalls.Load(); got != 1 {
		t.Fatalf("UpdateUsage calls = %d, want 1", got)
	}
	if got := gate.getLastUpdateCostNano(); got != gateWantCostNano {
		t.Errorf("billed %d, want %d", got, gateWantCostNano)
	}
}

// closingResponsesRouter emits its chunks and closes, honouring ctx.Done().
type closingResponsesRouter struct {
	fakeRouter
	chunks []*schemas.BifrostStreamChunk
}

func (r *closingResponsesRouter) ResponsesStreamRequest(ctx *schemas.BifrostContext, _ *schemas.BifrostResponsesRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	ch := make(chan *schemas.BifrostStreamChunk)
	go func() {
		defer close(ch)
		for _, c := range r.chunks {
			select {
			case ch <- c:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

// TestBillingRefused_OnlyAlarmsOnRealDrops: a handler with no budget gate must
// not publish billing_refused on every clean stream. That false alarm would
// desensitise operators to the one signal that detects real issue-#9 loss.
func TestBillingRefused_OnlyAlarmsOnRealDrops(t *testing.T) {
	events := newRecordingEvents()
	// No WithBudgetGate: the composition root produces exactly this whenever
	// the pool is nil or governance assembly fails.
	h := NewHandler(&fakeRouter{}, nil, nil, WithOpsEventPublisher(events))

	req := chatReqWithProject(t, "30", true)
	ctx, sc, ok := h.newStreamContext(httptest.NewRecorder(), req)
	if !ok {
		t.Fatal("newStreamContext rejected the request")
	}
	defer sc.cancel()

	s := h.newChatSettler("streamOpenAI", ctx, sc, "openai", "gpt-4o", nil)
	s.in, s.out, s.gotUsage = 11, 22, true
	s.report(lossReasonCleanCloseNoTrai, drainOutcomeInline)

	events.mu.Lock()
	defer events.mu.Unlock()
	if len(events.events) != 0 {
		t.Errorf("published %d event(s) with no budget gate wired; want 0 — 'nothing to bill' is not "+
			"a dropped payment and must not raise the loss alarm", len(events.events))
	}
}

// TestShutdown_DisconnectDuringHTTPDrainIsBilled covers the window the
// composition root ACTUALLY creates, which the first version of this guard
// missed: StopStreamGrace fires first, and the stream then dies during
// srv.ShutdownHTTP's drain window. Every drain on the shutdown path is born
// here, so if tracking keys off the phase-1 flag they are all skipped by
// DrainBilling's wait and billing closes underneath them.
//
// The earlier test started the stream BEFORE phase 1 — the one ordering
// production never produces — so it passed against code that lost the money
// 5/5 runs in a probe. Ordering matters more than the assertion here.
func TestShutdown_DisconnectDuringHTTPDrainIsBilled(t *testing.T) {
	gate := allowGate()
	events := newRecordingEvents()
	calc := &fakeCostEstimator{inputRateNano: gateInputRateNano, outputRateNano: gateOutputRateNano}
	router := &ctxHonouringRouter{
		preamble:     []*schemas.BifrostStreamChunk{chatDelta("c1", "hello "), chatDelta("c2", "world")},
		trailer:      chatTrailer(11, 22),
		trailerDelay: 200 * time.Millisecond,
	}
	h := NewHandler(router, nil, nil,
		WithBudgetGate(gate, calc),
		WithOpsEventPublisher(events),
		WithStreamGrace(MaxStreamGrace))

	// srv.ShutdownHTTP is running: the HTTP surface is draining, but the grace
	// is still ARMED. A client that hangs up here must be billed exactly as on
	// a normal day — this is the ~150 s window of every rolling deploy.
	h.Chat(&failAfterWriter{okWrites: 1}, chatReqWithProject(t, "30", true))

	if router.tornDown.Load() {
		t.Fatal("the provider stream was cut during the HTTP drain window — disconnect billing is " +
			"disabled for the whole deploy, which is the issue-#9 bypass (StopStreamGrace must not " +
			"run before ShutdownHTTP)")
	}

	// The HTTP drain window is where the drain does its work: ShutdownHTTP runs
	// for as long as it takes (§9.5 ceiling 150 s) with the grace still armed.
	gate.waitForUpdate(t)

	// Only now does the sequence disarm the grace and drain billing.
	h.StopStreamGrace()
	h.DrainBilling()

	if got := gate.updateCalls.Load(); got != 1 {
		t.Fatalf("UpdateUsage calls = %d, want 1 — a stream that disconnects during the HTTP drain "+
			"still recovers its trailer, and that spend must be billed, not dropped", got)
	}
	if got := gate.getLastUpdateCostNano(); got != gateWantCostNano {
		t.Errorf("billed %d nano-USD, want %d", got, gateWantCostNano)
	}
}

// TestShutdown_GraceDisarmedOnlyAfterTheHTTPDrain is the direct guard for the
// round-3 regression: if StopStreamGrace is hoisted ahead of the HTTP drain,
// every disconnect in that window gets grace=0 and its trailer is destroyed.
// Reproduced 3/3 before the fix, with a clean control.
func TestShutdown_GraceDisarmedOnlyAfterTheHTTPDrain(t *testing.T) {
	h := NewHandler(&fakeRouter{}, nil, nil, WithStreamGrace(MaxStreamGrace))

	// Before the sequence disarms it, a fresh drain must still get a real grace.
	if got := h.effectiveDrainGrace(); got != MaxStreamGrace {
		t.Errorf("grace before StopStreamGrace = %v, want %v (the grace must stay armed for the "+
			"whole HTTP-drain window)", got, MaxStreamGrace)
	}

	h.StopStreamGrace()

	if got := h.effectiveDrainGrace(); got != 0 {
		t.Errorf("grace after StopStreamGrace = %v, want 0 (past this point the grace must not "+
			"extend the pod's termination window)", got)
	}
}

// waitFor polls cond until it holds or the test times out. Used where the
// property under test is "this happened before the next shutdown phase", which
// a fixed sleep models badly on a loaded CI box.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition never became true within 3s")
}

// The four tests below pin round-1 fixes that were mutation-verified as
// DELETABLE with the whole suite still green. A defensive fix with no failing
// input is indistinguishable from dead code to the next person editing it.

// TestNextChunk_ClosedChannelWinsOverClientGone: when the provider closes and
// the client hangs up at the same instant, a bare select would pick either at
// random — mislabelling a completed stream as a disconnect and burning a drain
// slot on an already-closed channel.
func TestNextChunk_ClosedChannelWinsOverClientGone(t *testing.T) {
	gone := make(chan struct{})
	close(gone)

	for i := range 200 {
		ch := make(chan *schemas.BifrostStreamChunk)
		close(ch)
		if _, exit := nextChunk(ch, gone); exit != streamExitChanDone {
			t.Fatalf("iteration %d: exit = %v, want streamExitChanDone — a closed provider channel is "+
				"a clean completion even when the client is also gone", i, exit)
		}
	}
	// A live channel with a departed client must still report the disconnect.
	live := make(chan *schemas.BifrostStreamChunk)
	if _, exit := nextChunk(live, gone); exit != streamExitClient {
		t.Errorf("exit = %v, want streamExitClient when only the client is gone", exit)
	}
}

// TestSSELoop_PanicDoesNotLeakTheProviderStream: with the context decoupled
// from the request, nothing else will ever cancel it, so a panic in the loop
// leaks the provider stream (and its socket) forever.
func TestSSELoop_PanicDoesNotLeakTheProviderStream(t *testing.T) {
	router := &panickingRouter{}
	h := NewHandler(router, nil, nil, WithStreamGrace(DefaultStreamGrace))

	req := chatReqWithProject(t, "30", true)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected the panic to propagate to the caller")
			}
		}()
		h.Chat(&panicOnWrite{}, req)
	}()

	select {
	case <-router.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("provider context was never cancelled after a panic — the stream is leaked for the " +
			"lifetime of the process")
	}
}

type panickingRouter struct {
	fakeRouter
	cancelled chan struct{}
	once      sync.Once
}

func (r *panickingRouter) ChatCompletionStreamRequest(ctx *schemas.BifrostContext, _ *schemas.BifrostChatRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	r.once.Do(func() { r.cancelled = make(chan struct{}) })
	ch := make(chan *schemas.BifrostStreamChunk, 1)
	ch <- chatDelta("c1", "boom")
	go func() {
		<-ctx.Done()
		close(r.cancelled)
	}()
	return ch, nil
}

type panicOnWrite struct{ hdr http.Header }

func (w *panicOnWrite) Header() http.Header {
	if w.hdr == nil {
		w.hdr = http.Header{}
	}
	return w.hdr
}
func (w *panicOnWrite) Write([]byte) (int, error) { panic("writer exploded") }
func (w *panicOnWrite) WriteHeader(int)           {}
func (w *panicOnWrite) Flush()                    {}

// TestCheckBudget_BoundedOnTheDetachedContext: the streaming path hands
// checkBudget a context.WithoutCancel-derived context, so without an explicit
// timeout a stalled budget store parks the handler goroutine forever — there is
// no client cancellation left to unwind it.
func TestCheckBudget_BoundedOnTheDetachedContext(t *testing.T) {
	if budgetGateTimeout <= 0 {
		t.Fatal("budgetGateTimeout must be positive or the gate is unbounded")
	}
	gate := &deadlineObservingChecker{seen: make(chan bool, 1)}
	h := NewHandler(&fakeRouter{}, nil, nil,
		WithBudgetGate(gate, &fakeCostEstimator{totalNano: 1}))

	req := chatReqWithProject(t, "30", true)
	ctx, sc, ok := h.newStreamContext(httptest.NewRecorder(), req)
	if !ok {
		t.Fatal("newStreamContext rejected the request")
	}
	defer sc.cancel()

	if ctxDeadline, has := ctx.Deadline(); has {
		t.Fatalf("the stream context itself must stay deadline-free (§9.5); got %v", ctxDeadline)
	}
	h.checkBudget(httptest.NewRecorder(), ctx, "gpt-4o")

	select {
	case hasDeadline := <-gate.seen:
		if !hasDeadline {
			t.Error("CheckBudget received a context with NO deadline — a stalled budget store would " +
				"park this streaming handler goroutine forever")
		}
	default:
		t.Fatal("CheckBudget was never called")
	}
}

type deadlineObservingChecker struct {
	fakeBudgetChecker
	seen chan bool
}

func (d *deadlineObservingChecker) CheckBudget(ctx context.Context, _ int, _, _ string, _, _ int64) (failmode.Decision, error) {
	_, has := ctx.Deadline()
	select {
	case d.seen <- has:
	default:
	}
	return failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy}, nil
}

// TestDrain_ShutdownShrinksTheHardBackstop: once shutdown starts, a wedged
// provider must cost ~drainShutdownTimeout of the pod's termination grace, not
// the full grace + drainHardTimeout.
func TestDrain_ShutdownShrinksTheHardBackstop(t *testing.T) {
	if drainShutdownTimeout >= drainHardTimeout {
		t.Fatalf("drainShutdownTimeout (%v) must be well under drainHardTimeout (%v) or shutdown "+
			"inherits the full backstop", drainShutdownTimeout, drainHardTimeout)
	}
	gate := allowGate()
	h := NewHandler(&wedgedRouter{}, nil, nil,
		WithBudgetGate(gate, &fakeCostEstimator{totalNano: 1_000}),
		WithStreamGrace(MaxStreamGrace))

	h.Chat(&failAfterWriter{okWrites: 1}, chatReqWithProject(t, "30", true))

	start := time.Now()
	h.DrainBilling()
	if elapsed := time.Since(start); elapsed > drainShutdownTimeout+2*time.Second {
		t.Errorf("DrainBilling took %v against a wedged provider; the hard backstop must shrink to "+
			"~%v once shutdown starts", elapsed, drainShutdownTimeout)
	}
}

// wedgedRouter never closes its channel and ignores cancellation entirely.
type wedgedRouter struct{ fakeRouter }

func (r *wedgedRouter) ChatCompletionStreamRequest(_ *schemas.BifrostContext, _ *schemas.BifrostChatRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	ch := make(chan *schemas.BifrostStreamChunk, 2)
	ch <- chatDelta("c1", "hello")
	ch <- chatDelta("c2", "world")
	return ch, nil // never closed, never cancelled
}
