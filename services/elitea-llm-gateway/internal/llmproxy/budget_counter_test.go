package llmproxy

// budget_counter_test.go — issue #10 regression pin.
//
// The deleted mechanism was a per-project in-process "in-flight reservation"
// counter. Its increment was gated on a prompt-token estimate that every call
// site passed as 0, so the increment never fired while the billing path
// decremented unconditionally: the counter could only ever drift negative, and
// its sync.Map entries were never reaped.
//
// These tests pin the properties that must hold now that the mechanism is gone:
//
//  1. Every amount that reaches the authoritative counter through the remaining
//     billing path is STRICTLY POSITIVE — no exit path (success, provider
//     error, admission block, drain-skip) may apply a negative delta. The
//     running total therefore never dips below zero and, after N complete
//     lifecycles, equals exactly N × cost.
//  2. Admission never feeds an estimate into the budget arithmetic:
//     checkBudget passes reqCostNano=0 on every call. An estimate reaching the
//     gate is what the deleted code pretended to do; an estimate reaching a
//     BILLED amount is forbidden outright (DECISIONS.md, money path).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/failmode"
)

// accountingChecker is a BudgetChecker that behaves like the authoritative
// counter: UpdateUsage APPLIES the delta rather than merely recording that it
// was called. That distinction is the whole point — a fake that only counts
// calls cannot observe a negative drift.
type accountingChecker struct {
	verdict failmode.Decision

	mu sync.Mutex
	// total is the running counter, in nano-USD.
	total int64
	// minTotal is the lowest value total ever held.
	minTotal int64
	// deltas is every amount UpdateUsage was asked to apply, in order.
	deltas []int64
	// admissionCosts is every reqCostNano CheckBudget was called with from the
	// admission path AND the billing goroutine's snapshot/soft-alert reads.
	admissionCosts []int64
}

func (a *accountingChecker) CheckBudget(_ context.Context, _ int, _, _ string, _, reqCostNano int64) (failmode.Decision, error) {
	a.mu.Lock()
	a.admissionCosts = append(a.admissionCosts, reqCostNano)
	a.mu.Unlock()
	return a.verdict, nil
}

func (a *accountingChecker) UpdateUsage(_ context.Context, _ int, scope, _, _ string, costNano, _, _ int64, _ *failmode.UsageDimensions) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.deltas = append(a.deltas, costNano)
	a.total += costNano
	if a.total < a.minTotal {
		a.minTotal = a.total
	}
	return nil
}

func (a *accountingChecker) TryAlertCooldown(_ context.Context, _, _ string, _ int64) (bool, error) {
	return false, nil
}

func (a *accountingChecker) snapshot() (total, minTotal int64, deltas []int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.total, a.minTotal, append([]int64(nil), a.deltas...)
}

// firstAdmissionCost returns the reqCostNano of the very first CheckBudget call
// — the admission gate in checkBudget, before any billing goroutine runs.
func (a *accountingChecker) firstAdmissionCost(t *testing.T) int64 {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.admissionCosts) == 0 {
		t.Fatal("CheckBudget was never called — the admission gate did not run")
	}
	return a.admissionCosts[0]
}

// imageReqNoUsage builds an /llm/v1/images/generations request. The fake router
// answers with a response carrying NO Usage field, which routes billing through
// updateUsageDirect (the per-image fallback) rather than the token path.
func imageReqNoUsage(projectID string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/images/generations",
		strings.NewReader(`{"model":"dall-e-3","prompt":"a cat"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerProjectID, projectID)
	return req
}

// TestBudgetCounter_NeverNegative_AcrossEveryExitPath drives the four billing
// exit paths concurrently and asserts the authoritative counter only ever moves
// up, by exactly the billed amount.
func TestBudgetCounter_NeverNegative_AcrossEveryExitPath(t *testing.T) {
	const (
		perChatNano = 500_000
		lifecycles  = 8
	)

	gate := &accountingChecker{verdict: failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy}}

	// Path A: N concurrent unary successes against ONE shared gate. Concurrency
	// is the case the deleted reservation claimed to bound; with it gone the
	// counter must still land exactly on N × cost.
	//
	// Each goroutine gets its own Handler + router because the package's
	// fakeRouter records lastVK without synchronisation — a test-fixture
	// limitation, not a production one. The gate, which is what these
	// assertions read, is shared and mutex-guarded.
	chatHandlers := make([]*Handler, lifecycles)
	for i := range chatHandlers {
		router := &trackingRouter{}
		router.chatResp = &schemas.BifrostChatResponse{
			ID:    "resp-1",
			Usage: &schemas.BifrostLLMUsage{PromptTokens: 10, CompletionTokens: 20},
		}
		chatHandlers[i] = NewHandler(router, nil, nil, WithBudgetGate(gate, &fakeCostEstimator{totalNano: perChatNano}))
	}
	var wg sync.WaitGroup
	for _, ch := range chatHandlers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch.Chat(httptest.NewRecorder(), chatReqWithProject(t, "77", false))
		}()
	}
	wg.Wait()

	// Path B: provider error — the response is an error, so nothing is billed.
	errRouter := &trackingRouter{}
	errRouter.chatErr = &schemas.BifrostError{Error: &schemas.ErrorField{Message: "upstream boom"}}
	errH := NewHandler(errRouter, nil, nil, WithBudgetGate(gate, &fakeCostEstimator{totalNano: perChatNano}))
	errRec := httptest.NewRecorder()
	errH.Chat(errRec, chatReqWithProject(t, "77", false))
	if errRec.Code == http.StatusOK {
		t.Fatalf("provider-error path returned 200; the fake router did not fail the request")
	}

	// Path C: admission block — 402 before dispatch, nothing billed. The router
	// carries a BILLABLE response on purpose: if the gate ever stopped blocking,
	// the request would reach the provider and bill, and blockDeltas below would
	// catch it. A nil-response router would silently absorb that regression.
	blockGate := &accountingChecker{verdict: failmode.Decision{Verdict: failmode.Block402, State: failmode.StateNATSHealthy}}
	blockRouter := &trackingRouter{}
	blockRouter.chatResp = &schemas.BifrostChatResponse{
		ID:    "must-not-be-reached",
		Usage: &schemas.BifrostLLMUsage{PromptTokens: 10, CompletionTokens: 20},
	}
	blockH := NewHandler(blockRouter, nil, nil, WithBudgetGate(blockGate, &fakeCostEstimator{totalNano: perChatNano}))
	blockRec := httptest.NewRecorder()
	blockH.Chat(blockRec, chatReqWithProject(t, "77", false))
	if blockRec.Code != http.StatusPaymentRequired {
		t.Fatalf("admission-block path status = %d, want 402", blockRec.Code)
	}
	if blockRouter.called.Load() {
		t.Error("provider was dispatched despite a Block402 admission verdict")
	}

	// Path D: image response with no Usage → updateUsageDirect (fixed per-image
	// fallback), the one path that bills without the cost calculator.
	imgRouter := &trackingRouter{}
	imgRouter.imgResp = &schemas.BifrostImageGenerationResponse{
		Data: []schemas.ImageData{{URL: "https://example.invalid/a.png"}},
	}
	imgH := NewHandler(imgRouter, nil, nil, WithBudgetGate(gate, &fakeCostEstimator{totalNano: perChatNano}))
	imgRec := httptest.NewRecorder()
	imgH.ImageGeneration(imgRec, imageReqNoUsage("77"))
	if imgRec.Code != http.StatusOK {
		t.Fatalf("image path status = %d, want 200", imgRec.Code)
	}

	// Settle every billing goroutine on every handler before reading.
	for _, ch := range chatHandlers {
		ch.DrainBilling()
	}
	errH.DrainBilling()
	blockH.DrainBilling()
	imgH.DrainBilling()

	total, minTotal, deltas := gate.snapshot()
	blockTotal, blockMin, blockDeltas := blockGate.snapshot()

	// 1. No delta may be negative or zero. A negative delta is the exact shape
	//    of the deleted reservation's drift.
	for i, d := range deltas {
		if d <= 0 {
			t.Errorf("UpdateUsage delta #%d = %d; every billed amount must be strictly positive "+
				"(a negative delta drives the budget counter below zero)", i, d)
		}
	}
	if len(blockDeltas) != 0 {
		t.Errorf("blocked request produced %d UpdateUsage call(s) %v; an admission block must bill nothing",
			len(blockDeltas), blockDeltas)
	}

	// 2. The running counter never dipped below zero.
	if minTotal < 0 {
		t.Errorf("budget counter reached %d; it must never go negative", minTotal)
	}
	if blockMin < 0 || blockTotal != 0 {
		t.Errorf("blocked-path counter = %d (min %d); want 0/0", blockTotal, blockMin)
	}

	// 3. The counter equals exactly what the completed lifecycles cost:
	//    N chat successes + one image at the per-image fallback. The provider
	//    error and the block contribute nothing.
	wantTotal := int64(lifecycles)*perChatNano + perImageFallbackNano
	if total != wantTotal {
		t.Errorf("budget counter = %d, want %d (%d chats × %d + one image × %d); "+
			"a mismatch means some exit path applied an amount it should not have",
			total, wantTotal, lifecycles, perChatNano, perImageFallbackNano)
	}
}

// TestCheckBudget_AdmissionPassesNoEstimate pins that the admission gate hands
// CheckBudget reqCostNano=0. The deleted mechanism fed a prompt-token estimate
// (plus a cumulative reservation) into this argument; nothing may reintroduce
// an estimate here without a human amending DECISIONS.md, because the same
// estimate must never be able to leak onto the billed amount.
func TestCheckBudget_AdmissionPassesNoEstimate(t *testing.T) {
	gate := &accountingChecker{verdict: failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy}}
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{
		ID:    "resp-1",
		Usage: &schemas.BifrostLLMUsage{PromptTokens: 4_000, CompletionTokens: 4_000},
	}
	// A deliberately large per-call cost: if any estimate were wired into
	// admission, this is the value that would show up on the gate.
	h := NewHandler(router, nil, nil, WithBudgetGate(gate, &fakeCostEstimator{totalNano: 999_000_000}))

	h.Chat(httptest.NewRecorder(), chatReqWithProject(t, "77", false))
	h.DrainBilling()

	if got := gate.firstAdmissionCost(t); got != 0 {
		t.Errorf("admission CheckBudget reqCostNano = %d, want 0 — the gate must not be fed a "+
			"pre-flight estimate (issue #10; DECISIONS.md money path)", got)
	}
}

// TestBillingRefusedAfterDrain_AppliesNothing pins the drain-skip exit path.
// Once billing is closed the increment is refused and reported as billRefused —
// and, crucially, NO compensating negative amount is applied. The deleted code
// decremented the reservation on exactly this path.
func TestBillingRefusedAfterDrain_AppliesNothing(t *testing.T) {
	gate := &accountingChecker{verdict: failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy}}
	h := NewHandler(&trackingRouter{}, nil, nil, WithBudgetGate(gate, &fakeCostEstimator{totalNano: 250_000}))

	h.DrainBilling() // billing closed

	got := h.updateUsage(context.Background(), "openai", "gpt-4o", 10, 20, "77", "")
	if got != billRefused {
		t.Fatalf("updateUsage after drain = %v, want billRefused", got)
	}

	total, minTotal, deltas := gate.snapshot()
	if len(deltas) != 0 {
		t.Errorf("refused billing still applied %v to the counter; the drain-skip path must apply nothing", deltas)
	}
	if total != 0 || minTotal != 0 {
		t.Errorf("counter = %d (min %d) after a refused increment; want 0/0", total, minTotal)
	}
}

// compile-time guard: accountingChecker must satisfy the port the handler uses.
var _ BudgetChecker = (*accountingChecker)(nil)
