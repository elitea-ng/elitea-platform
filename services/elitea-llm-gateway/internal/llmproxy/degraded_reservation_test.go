package llmproxy

// degraded_reservation_test.go — the NATS-down reservation through the REAL
// handler and the REAL GovernanceStore (human decision, DECISIONS.md
// 2026-10-22). Admission reserves against the replica's share; the holds go
// when the last pin drops: the request's own (its handler returned) and each
// bill's (the cost is on the counter).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/cost"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/failmode"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/governance"
	natspkg "github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/infra/nats"
)

// buildDegradedStore is buildIntStore with NATS down, a configurable replica
// count and reservation, and the counters exposed.
func buildDegradedStore(t *testing.T, replicas int, reservationNano int64) (*governance.GovernanceStore, *failmode.DegradedCounters) {
	t.Helper()
	nc := newIntegNATS()
	nc.readErr = natspkg.ErrUnavailable
	nc.incrErr = natspkg.ErrUnavailable
	db := &integDB{snap: failmode.Snapshot{
		HardLimitNano:   integLimitNano, // 100 USD; the default cap is 10 USD
		AccumulatedNano: 10 * failmode.NanoUSD,
		SoftAlertPct:    80,
		Found:           true,
		Age:             time.Second,
	}}
	degraded := failmode.NewDegradedCounters()
	rec := failmode.NewReconciler(db, &nc2integCounter{nc: nc}, degraded, nil)
	gs := governance.NewGovernanceStore(nc, failmode.NewStore(db), degraded, rec, failmode.Params{
		Mode:                    failmode.ModeTieredHybrid,
		PGFreshness:             5 * time.Minute,
		ExpectedReplicas:        replicas,
		DegradedReservationNano: reservationNano,
	}, nil)
	gs.Start(context.Background())
	t.Cleanup(gs.Drain)
	return gs, degraded
}

func degradedSubject() string {
	return natspkg.BudgetSubject("project", integProjectIDStr, billingPeriodStart(time.Now()))
}

// waitHeld polls until the held amount reaches want. The last pin can drop
// in a billing or drain goroutine, so it is not synchronous with the
// handler's return.
func waitHeld(t *testing.T, d *failmode.DegradedCounters, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := d.Held(degradedSubject())
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("held = %d, want %d", got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// serveScoped serves req through hf inside the request scope, as the router
// root does in production (DegradedHoldsScope).
func serveScoped(h *Handler, hf http.HandlerFunc, w http.ResponseWriter, req *http.Request) {
	h.DegradedHoldsScope(hf).ServeHTTP(w, req)
}

var degradedUsage = &schemas.BifrostLLMUsage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}

// TestDegraded_UnaryReleasesAfterBilling: the hold is gone after the request,
// and the real cost is on the degraded counter.
func TestDegraded_UnaryReleasesAfterBilling(t *testing.T) {
	gs, degraded := buildDegradedStore(t, 1, failmode.NanoUSD)
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{ID: "c", Model: "gpt-4o", Usage: degradedUsage}
	h := NewHandler(router, nil, nil, WithBudgetGate(gs, cost.New(cost.Config{})))

	rec := httptest.NewRecorder()
	serveScoped(h, h.Chat, rec, chatReqWithProject(t, integProjectIDStr, false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	h.billingWg.Wait()
	waitHeld(t, degraded, 0)
	if got := degraded.Get(degradedSubject()); got <= 0 {
		t.Fatalf("degraded spend = %d, want the billed cost", got)
	}
}

// TestDegraded_UnbilledRequestReleasesWhenTheHandlerReturns: a request that
// was admitted (and so reserved) but billed nothing — a provider error — gives
// the reservation back when its handler returns.
func TestDegraded_UnbilledRequestReleasesWhenTheHandlerReturns(t *testing.T) {
	gs, degraded := buildDegradedStore(t, 1, failmode.NanoUSD)
	router := &trackingRouter{}
	router.chatErr = &schemas.BifrostError{Error: &schemas.ErrorField{Message: "upstream down"}}
	h := NewHandler(router, nil, nil, WithBudgetGate(gs, cost.New(cost.Config{})))

	rec := httptest.NewRecorder()
	serveScoped(h, h.Chat, rec, chatReqWithProject(t, integProjectIDStr, false))
	if !router.called.Load() {
		t.Fatal("the request was not admitted, so it proves nothing about the release")
	}
	waitHeld(t, degraded, 0)
	if got := degraded.Get(degradedSubject()); got != 0 {
		t.Fatalf("degraded spend = %d, want 0 for an unbilled request", got)
	}
}

// TestDegraded_StreamReleasesAfterBilling: a stream's context is cancelled
// BEFORE the settler bills; the settler pinned the holds, so they are released
// only after the bill, and nothing is left held.
func TestDegraded_StreamReleasesAfterBilling(t *testing.T) {
	gs, degraded := buildDegradedStore(t, 1, failmode.NanoUSD)
	chunks := newChunkChan(
		&schemas.BifrostStreamChunk{BifrostChatResponse: &schemas.BifrostChatResponse{ID: "c1", Object: "chat.completion.chunk"}},
		usageChunk(1000, 500),
	)
	h := NewHandler(&fakeRouter{streamChan: chunks}, nil, nil, WithBudgetGate(gs, cost.New(cost.Config{})))

	rec := httptest.NewRecorder()
	serveScoped(h, h.Chat, rec, chatReqWithProject(t, integProjectIDStr, true))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	h.drainWg.Wait()
	h.billingWg.Wait()
	waitHeld(t, degraded, 0)
	if got := degraded.Get(degradedSubject()); got <= 0 {
		t.Fatalf("degraded spend = %d, want the streamed cost", got)
	}
}

// blockingRouter holds every unary chat until release is closed, so many
// requests are admitted and in flight at the same time.
type blockingRouter struct {
	fakeRouter
	calls   atomic.Int64
	release chan struct{}
}

func (b *blockingRouter) ChatCompletionRequest(_ *schemas.BifrostContext, _ *schemas.BifrostChatRequest) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	b.calls.Add(1)
	<-b.release
	return &schemas.BifrostChatResponse{ID: "c", Model: "gpt-4o", Usage: degradedUsage}, nil
}

// TestDegraded_ConcurrentRequestsAreBoundedByTheShare is the review's HIGH
// finding at the handler: 60 concurrent requests on a zero degraded spend.
// Before the reservation all 60 reached the provider. Now at most
// share/reservation do; the rest are refused with 402.
func TestDegraded_ConcurrentRequestsAreBoundedByTheShare(t *testing.T) {
	const reservation = failmode.NanoUSD // share 10 USD / 2 replicas = 5 USD → 5 in flight
	gs, degraded := buildDegradedStore(t, 2, reservation)
	router := &blockingRouter{release: make(chan struct{})}
	h := NewHandler(router, nil, nil, WithBudgetGate(gs, cost.New(cost.Config{})))

	const n = 60
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			serveScoped(h, h.Chat, rec, chatReqWithProject(t, integProjectIDStr, false))
			codes[i] = rec.Code
		}(i)
	}
	// Wait until every request that will be admitted has reached the provider
	// and every other one has been refused.
	deadline := time.Now().Add(5 * time.Second)
	for router.calls.Load() < 5 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if got := router.calls.Load(); got != 5 {
		t.Fatalf("%d requests reached the provider while degraded; want exactly 5 (share 5 USD / reservation 1 USD)", got)
	}
	if got := degraded.Held(degradedSubject()); got != 5*failmode.NanoUSD {
		t.Fatalf("held = %d, want the whole share", got)
	}
	close(router.release)
	wg.Wait()
	h.billingWg.Wait()

	ok, refused := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusPaymentRequired:
			refused++
		default:
			t.Errorf("unexpected status %d", c)
		}
	}
	if ok != 5 || refused != n-5 {
		t.Fatalf("ok=%d refused=%d, want 5 and %d", ok, refused, n-5)
	}
	waitHeld(t, degraded, 0)
}
