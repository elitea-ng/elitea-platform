package llmproxy

// stream_partial_bill_test.go — issue #79: when the gateway cuts a provider
// stream itself (grace expired, graceful shutdown, drain-pool saturation), the
// usage the provider had ALREADY accumulated must still reach the money path.
//
// The first attempt at this was reverted because it was wrong three ways, and
// each way has a test here:
//
//  1. it billed a count that is indistinguishable from Anthropic's
//     message_start placeholder (output_tokens: 1) →
//     TestStreamCut_PlaceholderAccumulatedCount_IsNotBilled;
//  2. it suppressed budget.unbilled_stream on a partial bill, turning a visible
//     loss into an invisible ~89% underbill →
//     TestStreamCut_PartialBill_StillReportsTheGap;
//  3. it read the count off the cancellation CHUNK, whose delivery is a coin
//     flip → TestStreamCut_AccumulatedUsageIsBilledDeterministically.
//
// The producer below models bifrost verbatim on the three points that matter:
// it registers the accumulated-usage handle on the context and mutates it in
// place, it delivers the cancellation chunk with GateSendChunk's
// already-cancelled select, and it closes the channel LAST.

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

// accumulatingRouter is a fake provider that reports usage the way a real
// streaming provider does: through the in-place handle on the context
// (schemas.BifrostContextKeyStreamAccumulatedUsage), not only through a
// trailer chunk.
//
// Its cancellation path is the load-bearing part. bifrost's
// HandleStreamCancellation attaches a snapshot of the handle to a cancellation
// chunk and hands it to GateSendChunk:
//
//	select {
//	case responseChan <- chunk:
//	case <-ctx.Done():
//	}
//
// The context is ALREADY cancelled at that point — cancelling it is what
// produced the chunk — so both cases are ready and Go picks at random. A test
// whose producer waits on a timer instead cannot observe that; the previous
// attempt's test did exactly that and could not see the race it named.
type accumulatingRouter struct {
	fakeRouter

	preamble []*schemas.BifrostStreamChunk
	// accIn / accOut are the counts the provider has accumulated by the time
	// it goes quiet — the state of the handle when the gateway cuts it.
	accIn, accOut int
	// trailer, when set, is delivered after trailerDelay, so a test can check
	// that a recovered trailer still outranks the accumulated count.
	trailer      *schemas.BifrostStreamChunk
	trailerDelay time.Duration

	// cancelChunkDelivered counts the cancellation chunks that actually won
	// their select. It is reported on failure so a chunk-sourced regression is
	// diagnosable at a glance.
	cancelChunkDelivered atomic.Int64
}

func (r *accumulatingRouter) stream(ctx *schemas.BifrostContext) chan *schemas.BifrostStreamChunk {
	ch := make(chan *schemas.BifrostStreamChunk)
	// bifrost: every streaming provider registers this handle ONCE, up front,
	// and mutates it in place as usage arrives.
	usage := &schemas.BifrostLLMUsage{}
	ctx.SetValue(schemas.BifrostContextKeyStreamAccumulatedUsage, usage)

	go func() {
		// Registered first, so it runs LAST — bifrost closes the channel after
		// the cancellation handler, from this same goroutine. That ordering is
		// the happens-before edge the gateway relies on to read the handle.
		defer close(ch)
		defer func() {
			if ctx.Err() == nil {
				return
			}
			// providerUtils.HandleStreamCancellation → GateSendChunk, verbatim.
			cancelChunk := &schemas.BifrostStreamChunk{BifrostError: &schemas.BifrostError{
				Error: &schemas.ErrorField{Message: "Request cancelled: client disconnected"},
			}}
			cancelChunk.BifrostError.ExtraFields.BilledUsage = &schemas.BifrostLLMUsage{
				PromptTokens:     usage.PromptTokens,
				CompletionTokens: usage.CompletionTokens,
			}
			select {
			case ch <- cancelChunk:
				r.cancelChunkDelivered.Add(1)
			case <-ctx.Done():
			}
		}()

		for _, c := range r.preamble {
			select {
			case ch <- c:
			case <-ctx.Done():
				return
			}
		}
		// Generation carries on after the last delta the client saw, and the
		// provider keeps counting into the handle.
		usage.PromptTokens, usage.CompletionTokens = r.accIn, r.accOut
		usage.TotalTokens = r.accIn + r.accOut

		if r.trailer != nil {
			select {
			case <-time.After(r.trailerDelay):
			case <-ctx.Done():
				return
			}
			select {
			case ch <- r.trailer:
			case <-ctx.Done():
			}
			return
		}
		<-ctx.Done() // no trailer, ever
	}()
	return ch
}

func (r *accumulatingRouter) ChatCompletionStreamRequest(ctx *schemas.BifrostContext, _ *schemas.BifrostChatRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	return r.stream(ctx), nil
}

func (r *accumulatingRouter) ResponsesStreamRequest(ctx *schemas.BifrostContext, _ *schemas.BifrostResponsesRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	return r.stream(ctx), nil
}

// TestStreamCut_AccumulatedUsageIsBilledDeterministically is the issue #79
// acceptance gate.
//
// The client hangs up, the grace expires, the gateway cuts the provider — and
// the provider had already processed 11 input and 22 output tokens. Every one
// of those cut streams must be billed, on every run.
//
// MUST FAIL against the reverted state: nothing there reads the accumulated
// count at all, so 0 of 40 streams are billed. It also fails against a
// chunk-sourced fix, because the cancellation chunk wins its select only about
// half the time.
func TestStreamCut_AccumulatedUsageIsBilledDeterministically(t *testing.T) {
	const runs = 40

	billed := 0
	router := &accumulatingRouter{}
	for i := 0; i < runs; i++ {
		gate := allowGate()
		calc := &fakeCostEstimator{inputRateNano: gateInputRateNano, outputRateNano: gateOutputRateNano}
		router.preamble = []*schemas.BifrostStreamChunk{chatDelta("c1", "hello "), chatDelta("c2", "world")}
		router.accIn, router.accOut = 11, 22

		h := NewHandler(router, nil, nil,
			WithBudgetGate(gate, calc),
			WithOpsEventPublisher(newRecordingEvents()),
			WithStreamGrace(20*time.Millisecond))

		// A caller disconnect (issue #1201) cancels the provider at once and
		// reads the same handle for its estimate, so this run covers both.
		h.Chat(&failAfterWriter{okWrites: 1}, chatReqWithProject(t, "30", true))
		h.DrainBilling()

		if gate.updateCalls.Load() == 0 {
			continue
		}
		billed++
		if got := gate.getLastUpdateCostNano(); got != gateWantCostNano {
			t.Fatalf("run %d billed %d nano-USD, want %d — the provider's accumulated "+
				"count (11 in, 22 out) must reach the money path unchanged", i, got, gateWantCostNano)
		}
	}

	if billed != runs {
		t.Fatalf("billed %d of %d cut streams (cancellation chunk delivered %d times) — "+
			"provider-accumulated usage must be read from the context handle the gateway "+
			"owns, which is deterministic, NOT from the cancellation chunk, whose delivery "+
			"races the cancelled context that produced it",
			billed, runs, router.cancelChunkDelivered.Load())
	}
}

// TestStreamCut_PlaceholderAccumulatedCount_IsNotBilled is the guard the first
// attempt lacked.
//
// On Anthropic (and Bedrock-Anthropic, on both dialects) the accumulator is
// MAX-merged and the real output_tokens arrives exactly once, at the end, in
// message_delta. A mid-stream cut therefore sees the message_start placeholder:
// input_tokens plus output_tokens: 1. That measures no work, so billing it
// reports an ~89% underbill as a success. The correct outcome is the one issue
// #9 already defines: bill nothing, and meter the loss.
func TestStreamCut_PlaceholderAccumulatedCount_IsNotBilled(t *testing.T) {
	gate := allowGate()
	events := newRecordingEvents()
	calc := &fakeCostEstimator{inputRateNano: gateInputRateNano, outputRateNano: gateOutputRateNano}
	router := &accumulatingRouter{
		preamble: []*schemas.BifrostStreamChunk{responsesDelta("hello "), responsesDelta("world")},
		// Exactly what message_start leaves on the handle.
		accIn: 5000, accOut: 1,
	}
	h := NewHandler(router, nil, nil,
		WithBudgetGate(gate, calc),
		WithOpsEventPublisher(events),
		WithStreamGrace(20*time.Millisecond))

	h.Chat(&nonFlusherWriter{}, messagesStreamReqWithProject(t, "30"))
	events.waitForEvent(t)
	h.DrainBilling()

	if got := gate.updateCalls.Load(); got != 0 {
		t.Fatalf("UpdateUsage calls = %d, want 0 — output_tokens: 1 is Anthropic's message_start "+
			"placeholder, not an accumulation, and billing it turns a visible loss into an "+
			"invisible ~89%% underbill", got)
	}
	if in, out := calc.getLastTokens(); in != 0 || out != 0 {
		t.Fatalf("cost calculator saw (%d, %d) tokens — a placeholder count must never be priced", in, out)
	}
	p := events.decodeUnbilled(t)
	if p.Reason != lossReasonBeginStreamFail {
		t.Errorf("event reason = %q, want %q — a rejected placeholder is a plain unbilled stream, "+
			"not a partial bill", p.Reason, lossReasonBeginStreamFail)
	}
	if p.PartialInputTokens != 0 || p.PartialOutputTokens != 0 {
		t.Errorf("event carries partial tokens (%d, %d) but nothing was billed",
			p.PartialInputTokens, p.PartialOutputTokens)
	}
}

// TestStreamCut_PartialBill_StillReportsTheGap: billing something is not the
// same as billing correctly. An accumulated count is a FLOOR on the real usage,
// so the remainder is an underbill of unknown size — and the only thing that
// keeps it visible is the event. The reverted attempt set gotUsage on the
// fallback, which suppressed the event entirely.
func TestStreamCut_PartialBill_StillReportsTheGap(t *testing.T) {
	gate := allowGate()
	events := newRecordingEvents()
	calc := &fakeCostEstimator{inputRateNano: gateInputRateNano, outputRateNano: gateOutputRateNano}
	router := &accumulatingRouter{
		preamble: []*schemas.BifrostStreamChunk{chatDelta("c1", "hello "), chatDelta("c2", "world")},
		accIn:    11, accOut: 22,
	}
	h := NewHandler(router, nil, nil,
		WithBudgetGate(gate, calc),
		WithOpsEventPublisher(events),
		WithStreamGrace(20*time.Millisecond))

	h.Chat(&nonFlusherWriter{}, chatReqWithProject(t, "30", true))
	gate.waitForUpdate(t)
	events.waitForEvent(t)
	h.DrainBilling()

	if got := gate.updateCalls.Load(); got == 0 {
		t.Fatal("the cut stream was not billed at all")
	}
	p := events.decodeUnbilled(t)
	if p.Reason != lossReasonPartialProviderUsage {
		t.Fatalf("event reason = %q, want %q — a stream billed from a partial count MUST still "+
			"publish, or the gap between the floor and the true total is invisible",
			p.Reason, lossReasonPartialProviderUsage)
	}
	if p.ExitReason != lossReasonBeginStreamFail {
		t.Errorf("event exit_reason = %q, want %q — the trigger that ended the stream must survive",
			p.ExitReason, lossReasonBeginStreamFail)
	}
	if p.PartialInputTokens != 11 || p.PartialOutputTokens != 22 {
		t.Errorf("event partial tokens = (%d, %d), want (11, 22) — the floor that was billed must "+
			"be on the event so the size of the gap is bounded",
			p.PartialInputTokens, p.PartialOutputTokens)
	}
	if p.ProjectID != "30" {
		t.Errorf("event project_id = %q, want 30", p.ProjectID)
	}
}

// TestStreamCut_TerminalTrailerOutranksAccumulatedCount pins the precedence:
// terminal trailer > partial > nothing. A recovered trailer is authoritative
// and must never be overwritten downward by the (smaller, earlier) accumulated
// count — and a stream billed from the trailer is not a partial bill, so it
// must not publish the partial event either.
func TestStreamCut_TerminalTrailerOutranksAccumulatedCount(t *testing.T) {
	gate := allowGate()
	events := newRecordingEvents()
	calc := &fakeCostEstimator{inputRateNano: gateInputRateNano, outputRateNano: gateOutputRateNano}
	router := &accumulatingRouter{
		preamble: []*schemas.BifrostStreamChunk{chatDelta("c1", "hello "), chatDelta("c2", "world")},
		// The handle holds a partial count all along; the trailer lands inside
		// the grace and reports the real totals.
		accIn: 3, accOut: 4,
		trailer:      chatTrailer(11, 22),
		trailerDelay: 20 * time.Millisecond,
	}
	h := NewHandler(router, nil, nil,
		WithBudgetGate(gate, calc),
		WithOpsEventPublisher(events),
		WithStreamGrace(2*time.Second))

	h.Chat(&nonFlusherWriter{}, chatReqWithProject(t, "30", true))
	gate.waitForUpdate(t)
	h.DrainBilling()

	if got := gate.getLastUpdateCostNano(); got != gateWantCostNano {
		t.Fatalf("billed %d nano-USD, want %d — the recovered trailer is authoritative and must "+
			"never be replaced by the smaller accumulated count", got, gateWantCostNano)
	}
	if in, out := calc.getLastTokens(); in != 11 || out != 22 {
		t.Fatalf("cost calculator saw (%d, %d) tokens, want (11, 22)", in, out)
	}
	events.mu.Lock()
	n := len(events.events)
	events.mu.Unlock()
	if n != 0 {
		t.Errorf("a stream billed from its terminal trailer published %d event(s); it is neither "+
			"a loss nor a partial bill", n)
	}
}

// TestStreamCut_AccumulatedUsageSurvivesDrainSaturation: the accumulated count
// is the LAST line of defence, so it must still work on the paths that cut the
// stream hardest — a saturated drain pool falling back to the short grace.
func TestStreamCut_AccumulatedUsageSurvivesDrainSaturation(t *testing.T) {
	gate := allowGate()
	events := newRecordingEvents()
	calc := &fakeCostEstimator{inputRateNano: gateInputRateNano, outputRateNano: gateOutputRateNano}
	router := &accumulatingRouter{
		preamble: []*schemas.BifrostStreamChunk{chatDelta("c1", "hello")},
		accIn:    11, accOut: 22,
	}
	h := NewHandler(router, nil, nil,
		WithBudgetGate(gate, calc),
		WithOpsEventPublisher(events),
		WithStreamGrace(5*time.Second),
		WithStreamDrainLimit(1))

	if !h.drainLimit.acquire("99") {
		t.Fatal("could not take the single drain slot")
	}

	h.Chat(&nonFlusherWriter{}, chatReqWithProject(t, "30", true))
	gate.waitForUpdate(t)
	h.DrainBilling()

	if got := gate.getLastUpdateCostNano(); got != gateWantCostNano {
		t.Fatalf("billed %d nano-USD, want %d", got, gateWantCostNano)
	}
	p := events.decodeUnbilled(t)
	if p.Reason != lossReasonPartialProviderUsage {
		t.Errorf("event reason = %q, want %q", p.Reason, lossReasonPartialProviderUsage)
	}
	if !strings.HasPrefix(p.DrainOutcome, drainOutcomeSaturated) {
		t.Errorf("drain_outcome = %q, want a %q outcome", p.DrainOutcome, drainOutcomeSaturated)
	}
}

// TestStreamCut_AccumulatedCountIsReadOnlyAfterTheChannelCloses guards the
// synchronisation rule.
//
// The handle is a pointer the provider goroutine mutates IN PLACE. The only
// happens-before edge the gateway has is the channel close, which every bifrost
// provider performs from that same goroutine after its last write. A settler
// whose channel never closed (a wedged provider, cut by the hard backstop) is
// therefore reading memory another goroutine still owns, so it must read
// nothing at all — a data race on the money path is worse than an unbilled
// stream.
func TestStreamCut_AccumulatedCountIsReadOnlyAfterTheChannelCloses(t *testing.T) {
	gate := allowGate()
	calc := &fakeCostEstimator{inputRateNano: gateInputRateNano, outputRateNano: gateOutputRateNano}
	h := NewHandler(&fakeRouter{}, nil, nil,
		WithBudgetGate(gate, calc),
		WithOpsEventPublisher(newRecordingEvents()))

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyVirtualKey, "30")
	ctx.SetValue(schemas.BifrostContextKeyStreamAccumulatedUsage,
		&schemas.BifrostLLMUsage{PromptTokens: 11, CompletionTokens: 22})

	s := h.newChatSettler("test", ctx, nil, "openai", "gpt-4o", nil)
	// The provider never unwound, so the channel never closed.
	s.report(lossReasonWriteError, drainOutcomeHardTimeout)
	h.DrainBilling()

	if got := gate.updateCalls.Load(); got != 0 {
		t.Fatalf("UpdateUsage calls = %d, want 0 — the accumulated handle may only be read after "+
			"the provider closed the channel, which is the sole happens-before edge for it", got)
	}
}
