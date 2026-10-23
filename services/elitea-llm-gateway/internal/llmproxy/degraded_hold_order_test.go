package llmproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/cost"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/failmode"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/governance"
)

// heldAtBillGate wraps the real store and records, at the moment the PROJECT
// bill is written, how much this replica still holds. A hold released before
// the bill would read 0 there: the window in which neither the reservation nor
// the cost is counted.
type heldAtBillGate struct {
	*governance.GovernanceStore
	degraded *failmode.DegradedCounters
	heldAt   atomic.Int64
	billed   atomic.Bool
}

func (g *heldAtBillGate) UpdateUsage(ctx context.Context, projectID int, scope, scopeID, eventID string, costNano int64, periodStartUnix, periodEndUnix int64, dims *failmode.UsageDimensions) error {
	if scope == "project" {
		g.heldAt.Store(g.degraded.Held(degradedSubject()))
		g.billed.Store(true)
	}
	return g.GovernanceStore.UpdateUsage(ctx, projectID, scope, scopeID, eventID, costNano, periodStartUnix, periodEndUnix, dims)
}

// TestStreamSettler_AdoptionSurvivesTheCancel is the deterministic half of
// the ordering rule. The context-done backstop runs on its own goroutine as
// soon as the stream context is cancelled, and a settler cancels BEFORE it
// bills. Without adoption the backstop releases the hold in that gap.
func TestStreamSettler_AdoptionSurvivesTheCancel(t *testing.T) {
	for _, adopt := range []bool{true, false} {
		h := NewHandler(&fakeRouter{}, nil, nil)
		req := chatReqWithProject(t, integProjectIDStr, true)
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
		if adopt {
			s.adoptHolds()
		}
		sc.cancel()
		time.Sleep(50 * time.Millisecond) // let the AfterFunc goroutine run

		got := d.Held("k")
		if adopt && got != 5 {
			t.Fatalf("adopted: the backstop released the hold at the cancel (held %d)", got)
		}
		if !adopt && got != 0 {
			t.Fatalf("control: without adoption the backstop must release at the cancel (held %d)", got)
		}
		if adopt {
			s.report(lossReasonCleanCloseNoTrai, drainOutcomeInline) // no usage: nothing to bill
			if d.Held("k") != 0 {
				t.Fatal("report did not release the adopted hold")
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
		h.Chat(rec, chatReqWithProject(t, integProjectIDStr, stream))
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
