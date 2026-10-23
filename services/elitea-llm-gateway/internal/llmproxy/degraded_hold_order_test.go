package llmproxy

// degraded_hold_order_test.go — THE RULE of failmode.Holds: a degraded-mode
// reservation goes only AFTER the request's cost is on the counter, or when
// nothing will be billed. Each test below has a release point that came
// before the bill in an earlier design: a context cancel (client disconnect,
// a stream settle), and a handler that returns while its bill is in flight.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/cost"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/failmode"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/governance"
)

// heldAtBillGate wraps the real store and records, at the moment the PROJECT
// bill is written, how much this replica still holds. A hold released before
// the bill would read 0 there: the window in which neither the reservation nor
// the cost is counted. When block is set, the project bill waits for it.
type heldAtBillGate struct {
	*governance.GovernanceStore
	degraded *failmode.DegradedCounters
	heldAt   atomic.Int64
	billed   atomic.Bool
	entered  chan struct{}
	block    chan struct{}
}

func (g *heldAtBillGate) UpdateUsage(ctx context.Context, projectID int, scope, scopeID, eventID string, costNano int64, periodStartUnix, periodEndUnix int64, dims *failmode.UsageDimensions) error {
	if scope == "project" {
		if g.entered != nil {
			close(g.entered)
		}
		if g.block != nil {
			<-g.block
		}
		g.heldAt.Store(g.degraded.Held(degradedSubject()))
		g.billed.Store(true)
	}
	return g.GovernanceStore.UpdateUsage(ctx, projectID, scope, scopeID, eventID, costNano, periodStartUnix, periodEndUnix, dims)
}

// TestStreamSettler_PinOutlivesTheHandler is the deterministic half of the
// stream rule. A settler can bill after the handler returned (a drain
// goroutine), so it pins the holds while the handler still runs. With the pin,
// the handler's return and the stream cancel release nothing; report() drops
// the pin. The control shows that without the pin the handler's return would
// release the hold before the settle.
func TestStreamSettler_PinOutlivesTheHandler(t *testing.T) {
	for _, pin := range []bool{true, false} {
		h := NewHandler(&fakeRouter{}, nil, nil)
		holds := failmode.NewHolds()
		req := chatReqWithProject(t, integProjectIDStr, true)
		req = req.WithContext(failmode.WithHolds(req.Context(), holds))
		ctx, sc, ok := h.buildContext(httptest.NewRecorder(), req, true)
		if !ok {
			t.Fatal("buildContext refused")
		}
		d := failmode.NewDegradedCounters()
		hold, ok := d.TryReserve("k", 5, 10)
		if !ok {
			t.Fatal("reserve")
		}
		failmode.HoldsFrom(ctx).Add(hold)

		s := h.newChatSettler("test", ctx, sc, "openai", "gpt-4o", nil)
		if pin {
			s.pinHolds()
		}
		sc.cancel()
		holds.End() // the handler returns

		got := d.Held("k")
		if pin && got != 5 {
			t.Fatalf("pinned: the hold went at the handler's return (held %d)", got)
		}
		if !pin && got != 0 {
			t.Fatalf("control: without a pin the handler's return must release (held %d)", got)
		}
		if pin {
			s.report(lossReasonCleanCloseNoTrai, drainOutcomeInline) // no usage: nothing to bill
			if d.Held("k") != 0 {
				t.Fatal("report did not drop the settle's pin")
			}
		}
	}
}

// TestDegraded_HoldOutlivesTheStreamCancel: the stream context is cancelled
// before the settler bills. The reservation must still be held when the bill
// is written, on the unary and on the streamed path.
func TestDegraded_HoldOutlivesTheStreamCancel(t *testing.T) {
	for _, stream := range []bool{false, true} {
		gs, degraded := buildDegradedStore(t, 1, failmode.NanoUSD)
		gate := &heldAtBillGate{GovernanceStore: gs, degraded: degraded}
		router := &fakeRouter{
			chatResp: &schemas.BifrostChatResponse{ID: "c", Model: "gpt-4o", Usage: degradedUsage},
			streamChan: newChunkChan(
				&schemas.BifrostStreamChunk{BifrostChatResponse: &schemas.BifrostChatResponse{ID: "c1", Object: "chat.completion.chunk"}},
				usageChunk(1000, 500),
			),
		}
		h := NewHandler(router, nil, nil, WithBudgetGate(gate, cost.New(cost.Config{})))
		rec := httptest.NewRecorder()
		serveScoped(h, h.Chat, rec, chatReqWithProject(t, integProjectIDStr, stream))
		if rec.Code != http.StatusOK {
			t.Fatalf("stream=%v: status = %d; body=%s", stream, rec.Code, rec.Body.String())
		}
		h.drainWg.Wait()
		h.billingWg.Wait()
		if !gate.billed.Load() {
			t.Fatalf("stream=%v: the request was never billed", stream)
		}
		if got := gate.heldAt.Load(); got != failmode.NanoUSD {
			t.Fatalf("stream=%v: held %d while the bill was written; want the 1 USD reservation still held", stream, got)
		}
		waitHeld(t, degraded, 0)
	}
}

// disconnectingRouter answers a unary chat with a billable response, but
// first cancels the CLIENT's request context, as a client that disconnects
// just before the answer does. It then waits, so a release tied to that
// cancel has time to run before the handler bills.
type disconnectingRouter struct {
	fakeRouter
	disconnect context.CancelFunc
}

func (r *disconnectingRouter) ChatCompletionRequest(_ *schemas.BifrostContext, _ *schemas.BifrostChatRequest) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	r.disconnect()
	time.Sleep(50 * time.Millisecond)
	return &schemas.BifrostChatResponse{ID: "c", Model: "gpt-4o", Usage: degradedUsage}, nil
}

// TestDegraded_ClientDisconnectDoesNotReleaseBeforeTheBill: a unary call
// (and a stream with no grace) inherits the client's cancellation. The
// disconnect must not release the reservation: the handler still bills the
// response the provider already produced.
func TestDegraded_ClientDisconnectDoesNotReleaseBeforeTheBill(t *testing.T) {
	gs, degraded := buildDegradedStore(t, 1, failmode.NanoUSD)
	gate := &heldAtBillGate{GovernanceStore: gs, degraded: degraded}
	clientCtx, disconnect := context.WithCancel(context.Background())
	router := &disconnectingRouter{disconnect: disconnect}
	h := NewHandler(router, nil, nil, WithBudgetGate(gate, cost.New(cost.Config{})))

	req := chatReqWithProject(t, integProjectIDStr, false).WithContext(clientCtx)
	serveScoped(h, h.Chat, httptest.NewRecorder(), req)
	h.billingWg.Wait()
	if !gate.billed.Load() {
		t.Fatal("the response was never billed, so the test proves nothing about the order")
	}
	if got := gate.heldAt.Load(); got != failmode.NanoUSD {
		t.Fatalf("held %d when the bill was written; the client disconnect released the reservation first", got)
	}
	waitHeld(t, degraded, 0)
}

// TestDegraded_HoldStaysWhileTheBillIsInFlight: the handler returns (the
// request scope drops its pin) while the billing goroutine is still writing.
// The realtime session end has the same shape: the session ends while its
// last turn's bill runs. The reservation must stay until the bill lands.
func TestDegraded_HoldStaysWhileTheBillIsInFlight(t *testing.T) {
	gs, degraded := buildDegradedStore(t, 1, failmode.NanoUSD)
	gate := &heldAtBillGate{
		GovernanceStore: gs, degraded: degraded,
		entered: make(chan struct{}), block: make(chan struct{}),
	}
	router := &fakeRouter{chatResp: &schemas.BifrostChatResponse{ID: "c", Model: "gpt-4o", Usage: degradedUsage}}
	h := NewHandler(router, nil, nil, WithBudgetGate(gate, cost.New(cost.Config{})))

	rec := httptest.NewRecorder()
	serveScoped(h, h.Chat, rec, chatReqWithProject(t, integProjectIDStr, false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the billing goroutine never reached the bill")
	}
	// The handler returned; the bill is blocked in flight.
	time.Sleep(20 * time.Millisecond)
	if got := degraded.Held(degradedSubject()); got != failmode.NanoUSD {
		t.Fatalf("held = %d after the handler returned with the bill in flight; want the reservation kept", got)
	}
	close(gate.block)
	h.billingWg.Wait()
	if got := gate.heldAt.Load(); got != failmode.NanoUSD {
		t.Fatalf("held %d when the bill was written, want the reservation", got)
	}
	waitHeld(t, degraded, 0)
}
