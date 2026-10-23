package llmproxy

// budget_gate_test.go — tests for the pre-LLM budget admission gate wired into
// the /llm handlers (design §8.5, BF0.9b).
//
// Test matrix:
//   - over-budget (Block402) → HTTP 402, provider NOT called.
//   - NATS unavailable (Block503) → HTTP 503, provider NOT called.
//   - under-budget (Allow) → provider called, UpdateUsage invoked.
//   - gate nil (disabled) → provider called as normal.
//   - anonymous project (no project-id header) → provider called (no budget row = unlimited).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/cost"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/failmode"
)

// ── fakes ────────────────────────────────────────────────────────────────────

// fakeBudgetChecker implements BudgetChecker for tests. It records all calls
// and returns configured verdicts.
//
// Since updateUsage now spawns a goroutine (FIX #18), tests must call
// waitForUpdate() before reading lastUpdateCostNano / lastUpdateProjectID.
type fakeBudgetChecker struct {
	// checkVerdict is the failmode.Decision returned by CheckBudget.
	checkVerdict failmode.Decision
	// checkErr is the error returned by CheckBudget (nil normally).
	checkErr error

	// updateErr is the error returned by UpdateUsage (nil normally).
	updateErr error

	// atomic counters so tests can assert call counts without a mutex.
	checkCalls  atomic.Int64
	updateCalls atomic.Int64

	// mu protects lastUpdate* fields which are written by the billing goroutine.
	mu                  sync.Mutex
	lastUpdateCostNano  int64
	lastUpdateProjectID int

	// updated is closed by the first UpdateUsage call so tests can wait for
	// the async billing goroutine without a busy-poll or sleep. It is
	// initialised in the struct literal to avoid the double-once.Do pattern
	// that required updateChan() as an initialisation trampoline (Fix round-3 #13).
	once    sync.Once
	updated chan struct{}
}

// waitForUpdate blocks until the first UpdateUsage call completes (or 2 s
// elapses). Tests call this before reading lastUpdateCostNano / lastUpdateProjectID.
func (f *fakeBudgetChecker) waitForUpdate(t *testing.T) {
	t.Helper()
	select {
	case <-f.updated:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for async UpdateUsage call")
	}
}

func (f *fakeBudgetChecker) CheckBudget(_ context.Context, projectID int, _, _ string, _, _ int64) (failmode.Decision, error) {
	f.checkCalls.Add(1)
	return f.checkVerdict, f.checkErr
}

func (f *fakeBudgetChecker) UpdateUsage(_ context.Context, projectID int, scope, _, _ string, costNano, _, _ int64, dims *failmode.UsageDimensions) error {
	f.updateCalls.Add(1)
	f.mu.Lock()
	f.lastUpdateProjectID = projectID
	f.lastUpdateCostNano = costNano
	f.mu.Unlock()
	// Close updated exactly once so waitForUpdate unblocks regardless of how
	// many concurrent billing goroutines fire (Fix round-3 #13).
	f.once.Do(func() { close(f.updated) })
	return f.updateErr
}

func (f *fakeBudgetChecker) TryAlertCooldown(_ context.Context, _, _ string, _ int64) (bool, error) {
	return false, nil
}

// blockingBudgetChecker implements BudgetChecker for
// TestUpdateUsage_CheckBudgetOffCriticalPath (FIX #27, github issue #15). The
// FIRST CheckBudget call — the request-path admission gate in h.checkBudget —
// returns Allow immediately, so the provider is dispatched normally. Every
// SUBSEQUENT CheckBudget call — the billing goroutine's pre-increment
// snapshot and trySoftAlert's post-increment read — blocks on unblock until
// the test closes it, proving neither can delay the HTTP handler.
type blockingBudgetChecker struct {
	checkCalls  atomic.Int64
	updateCalls atomic.Int64
	unblock     chan struct{}
	updated     chan struct{}
	once        sync.Once
}

func (b *blockingBudgetChecker) CheckBudget(_ context.Context, _ int, _, _ string, _, _ int64) (failmode.Decision, error) {
	if b.checkCalls.Add(1) == 1 {
		return failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy}, nil
	}
	<-b.unblock
	return failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy}, nil
}

func (b *blockingBudgetChecker) UpdateUsage(_ context.Context, _ int, _, _, _ string, _, _, _ int64, _ *failmode.UsageDimensions) error {
	b.updateCalls.Add(1)
	b.once.Do(func() { close(b.updated) })
	return nil
}

func (b *blockingBudgetChecker) TryAlertCooldown(_ context.Context, _, _ string, _ int64) (bool, error) {
	return false, nil
}

// getLastUpdateCostNano returns lastUpdateCostNano under the mutex.
func (f *fakeBudgetChecker) getLastUpdateCostNano() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastUpdateCostNano
}

// getLastUpdateProjectID returns lastUpdateProjectID under the mutex.
func (f *fakeBudgetChecker) getLastUpdateProjectID() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastUpdateProjectID
}

// fakeCostEstimator returns a cost that is a function of the actual token counts
// it receives, so a bug in token extraction (e.g. swapped prompt/completion) is
// detectable.
//
// When inputRateNano or outputRateNano is non-zero the estimator computes:
//
//	TotalNanoUSD = inputTokens*inputRateNano + outputTokens*outputRateNano
//
// When both rates are zero it falls back to the fixed totalNano value, which
// preserves backward-compatibility for tests that supply a canned cost.
type fakeCostEstimator struct {
	// totalNano is the fixed cost returned when both rates are zero.
	totalNano int64
	// inputRateNano / outputRateNano: nano-USD per token (token-aware mode).
	inputRateNano  int64
	outputRateNano int64

	// lastInput / lastOutput record the token counts actually passed to Cost so
	// tests can assert that token extraction was correct end-to-end.
	mu         sync.Mutex
	lastInput  int64
	lastOutput int64
	// lastUnits records the whole Units value CostUnits received, so an audio
	// test can assert the BASIS the handler chose and not only the counts.
	lastUnits cost.Units

	// source is the provenance the fake reports on Cost.Source. It defaults to
	// the CATALOG, because that is what almost every test means by "a price":
	// leaving it empty would make every token-billed audio request look like one
	// the gateway priced from a fabricated default, and trip
	// MetricAudioDefaultPriced across the suite. A test that wants the
	// fabricated case sets cost.SourceFallback explicitly.
	source string
}

// costSource returns the provenance to stamp on a returned Cost.
func (f *fakeCostEstimator) costSource() string {
	if f.source == "" {
		return cost.SourceCatalog
	}
	return f.source
}

func (f *fakeCostEstimator) Cost(_ context.Context, _, _ string, inputTokens, outputTokens int64) cost.Cost {
	f.mu.Lock()
	f.lastInput = inputTokens
	f.lastOutput = outputTokens
	f.mu.Unlock()
	if f.inputRateNano != 0 || f.outputRateNano != 0 {
		return cost.Cost{
			TotalNanoUSD: inputTokens*f.inputRateNano + outputTokens*f.outputRateNano,
			Basis:        cost.BasisTokens,
			Source:       f.costSource(),
		}
	}
	return cost.Cost{TotalNanoUSD: f.totalNano, Basis: cost.BasisTokens, Source: f.costSource()}
}

// CostUnits is the audio-capable form (issue #323). The fake prices seconds and
// characters at the SAME per-unit rates it prices tokens at, and it records the
// units it received, so a test can assert both the basis the handler chose and
// the quantity it passed. A real rate table is the Calculator's job, not this
// stub's.
//
// It answers a non-token basis with cost.Basis set, so the handler's
// non-token accounting runs. audioUnpricedEstimator below is the stub for the
// opposite case: units the catalog cannot price.
func (f *fakeCostEstimator) CostUnits(ctx context.Context, provider, model string, u cost.Units) cost.Cost {
	f.mu.Lock()
	f.lastUnits = u
	f.mu.Unlock()

	basis := u.Basis()
	if basis == cost.BasisTokens {
		return f.Cost(ctx, provider, model, u.InputTokens, u.OutputTokens)
	}
	in, out := u.InputChars, u.OutputChars
	if basis == cost.BasisSeconds {
		in, out = u.InputMillis, u.OutputMillis
	}
	total := f.totalNano
	if f.inputRateNano != 0 || f.outputRateNano != 0 {
		total = in*f.inputRateNano + out*f.outputRateNano
	}
	return cost.Cost{TotalNanoUSD: total, Basis: basis, Source: f.costSource()}
}

func (f *fakeCostEstimator) getLastUnits() cost.Units {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastUnits
}

// audioUnpricedEstimator answers every non-token basis with an UNPRICED cost:
// an empty Basis and a zero total. It stands for the catalog that carries no
// per-second or per-character rate for the model, which is the state every
// model is in until the price sync writes one.
type audioUnpricedEstimator struct{ fakeCostEstimator }

func (a *audioUnpricedEstimator) CostUnits(ctx context.Context, provider, model string, u cost.Units) cost.Cost {
	if u.Basis() == cost.BasisTokens {
		return a.fakeCostEstimator.CostUnits(ctx, provider, model, u)
	}
	a.mu.Lock()
	a.lastUnits = u
	a.mu.Unlock()
	return cost.Cost{}
}

func (f *fakeCostEstimator) getLastTokens() (input, output int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastInput, f.lastOutput
}

// trackingRouter wraps fakeRouter and records whether any LLM method was called.
type trackingRouter struct {
	fakeRouter
	called atomic.Bool
}

func (t *trackingRouter) ChatCompletionRequest(ctx *schemas.BifrostContext, req *schemas.BifrostChatRequest) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	t.called.Store(true)
	return t.fakeRouter.ChatCompletionRequest(ctx, req)
}

func (t *trackingRouter) ChatCompletionStreamRequest(ctx *schemas.BifrostContext, req *schemas.BifrostChatRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	t.called.Store(true)
	return t.fakeRouter.ChatCompletionStreamRequest(ctx, req)
}

func (t *trackingRouter) ResponsesRequest(ctx *schemas.BifrostContext, req *schemas.BifrostResponsesRequest) (*schemas.BifrostResponsesResponse, *schemas.BifrostError) {
	t.called.Store(true)
	return t.fakeRouter.ResponsesRequest(ctx, req)
}

func (t *trackingRouter) TextCompletionRequest(ctx *schemas.BifrostContext, req *schemas.BifrostTextCompletionRequest) (*schemas.BifrostTextCompletionResponse, *schemas.BifrostError) {
	t.called.Store(true)
	return t.fakeRouter.TextCompletionRequest(ctx, req)
}

func (t *trackingRouter) EmbeddingRequest(ctx *schemas.BifrostContext, req *schemas.BifrostEmbeddingRequest) (*schemas.BifrostEmbeddingResponse, *schemas.BifrostError) {
	t.called.Store(true)
	return t.fakeRouter.EmbeddingRequest(ctx, req)
}

func (t *trackingRouter) ResponsesStreamRequest(ctx *schemas.BifrostContext, req *schemas.BifrostResponsesRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	t.called.Store(true)
	return t.fakeRouter.ResponsesStreamRequest(ctx, req)
}

func (t *trackingRouter) CountTokensRequest(ctx *schemas.BifrostContext, req *schemas.BifrostResponsesRequest) (*schemas.BifrostCountTokensResponse, *schemas.BifrostError) {
	t.called.Store(true)
	return t.fakeRouter.CountTokensRequest(ctx, req)
}

// ── helpers ───────────────────────────────────────────────────────────────────

// chatReqWithProject builds an httptest.Request for /llm/v1/chat/completions
// with the given project-id header set. A zero projectID omits the header.
func chatReqWithProject(t *testing.T, projectID string, stream bool) *http.Request {
	t.Helper()
	streamVal := "false"
	if stream {
		streamVal = "true"
	}
	body := fmt.Sprintf(`{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":%s}`, streamVal)
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if projectID != "" {
		req.Header.Set(headerProjectID, projectID)
	}
	return req
}

// messagesReqWithProject builds an httptest.Request for /llm/v1/messages.
func messagesReqWithProject(t *testing.T, projectID string) *http.Request {
	t.Helper()
	body := `{"model":"anthropic/claude-3-5-sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if projectID != "" {
		req.Header.Set(headerProjectID, projectID)
	}
	return req
}

// messagesStreamReqWithProject builds a streaming /llm/v1/messages request.
func messagesStreamReqWithProject(t *testing.T, projectID string) *http.Request {
	t.Helper()
	body := `{"model":"anthropic/claude-3-5-sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if projectID != "" {
		req.Header.Set(headerProjectID, projectID)
	}
	return req
}

// newBudgetHandler builds a Handler with the given tracking router and budget
// fake wired via WithBudgetGate. The fakeCostEstimator is fixed-cost (totalNano),
// preserving backward-compatibility for tests that care about the cost value but
// not about which tokens were counted.
func newBudgetHandler(router *trackingRouter, gate *fakeBudgetChecker, costNano int64) *Handler {
	calc := &fakeCostEstimator{totalNano: costNano}
	return NewHandler(router, nil, nil, WithBudgetGate(gate, calc))
}

// newTokenAwareBudgetHandler builds a Handler whose cost estimator multiplies
// each token by the supplied per-token rates, making billed cost sensitive to
// the token counts extracted from the LLM response. Returns the estimator so
// tests can inspect lastInput/lastOutput after billing completes.
func newTokenAwareBudgetHandler(router *trackingRouter, gate *fakeBudgetChecker, inputRate, outputRate int64) (*Handler, *fakeCostEstimator) {
	calc := &fakeCostEstimator{inputRateNano: inputRate, outputRateNano: outputRate}
	return NewHandler(router, nil, nil, WithBudgetGate(gate, calc)), calc
}

// ── test cases ────────────────────────────────────────────────────────────────

// TestBudgetGate_Block402_ProviderNotCalled verifies that an over-budget project
// (Block402 verdict) receives HTTP 402 and the provider is never invoked.
func TestBudgetGate_Block402_ProviderNotCalled(t *testing.T) {
	gate := &fakeBudgetChecker{checkVerdict: failmode.Decision{Verdict: failmode.Block402, State: failmode.StateNATSHealthy}, updated: make(chan struct{})}
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{ID: "should-not-reach"}
	h := newBudgetHandler(router, gate, 500_000)

	rec := httptest.NewRecorder()
	h.Chat(rec, chatReqWithProject(t, "42", false))

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402 (budget_exceeded)", rec.Code)
	}
	if router.called.Load() {
		t.Error("provider was called despite Block402 verdict — gate did not block")
	}
	// Verify error body shape.
	var out openAIError
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal error body: %v", err)
	}
	if out.Error.Type != "budget_exceeded" {
		t.Errorf("error.type = %q, want budget_exceeded", out.Error.Type)
	}
	if out.Error.Code != "insufficient_quota" {
		t.Errorf("error.code = %q, want insufficient_quota", out.Error.Code)
	}
}

// TestBudgetGate_Block503_ProviderNotCalled verifies that an infrastructure
// failure (Block503 verdict) receives HTTP 503 and the provider is never called.
func TestBudgetGate_Block503_ProviderNotCalled(t *testing.T) {
	gate := &fakeBudgetChecker{checkVerdict: failmode.Decision{Verdict: failmode.Block503, State: failmode.StateDownPGStale, Degraded: true}, updated: make(chan struct{})}
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{ID: "should-not-reach"}
	h := newBudgetHandler(router, gate, 0)

	rec := httptest.NewRecorder()
	h.Chat(rec, chatReqWithProject(t, "99", false))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (service_unavailable)", rec.Code)
	}
	if router.called.Load() {
		t.Error("provider was called despite Block503 verdict")
	}
	var out openAIError
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal error body: %v", err)
	}
	if out.Error.Type != "service_unavailable" {
		t.Errorf("error.type = %q, want service_unavailable", out.Error.Type)
	}
	if out.Error.Code != "nats_unavailable" {
		t.Errorf("error.code = %q, want nats_unavailable", out.Error.Code)
	}
}

// TestBudgetGate_Allow_ProviderCalled_UpdateUsageInvoked is the sunny-path test:
// an under-budget project (Allow verdict) causes the provider to be called and
// UpdateUsage is invoked with the actual response cost.
func TestBudgetGate_Allow_ProviderCalled_UpdateUsageInvoked(t *testing.T) {
	const wantCostNano = 1_500_000 // what fakeCostEstimator returns

	gate := &fakeBudgetChecker{checkVerdict: failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy}, updated: make(chan struct{})}
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{
		ID:    "cmpl-ok",
		Model: "openai/gpt-4o",
		Usage: &schemas.BifrostLLMUsage{PromptTokens: 10, CompletionTokens: 20},
	}
	h := newBudgetHandler(router, gate, wantCostNano)

	rec := httptest.NewRecorder()
	h.Chat(rec, chatReqWithProject(t, "42", false))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !router.called.Load() {
		t.Error("provider was NOT called despite Allow verdict")
	}
	// updateUsage is async (FIX #18) — wait for the billing goroutine before asserting.
	gate.waitForUpdate(t)
	if gate.updateCalls.Load() == 0 {
		t.Error("UpdateUsage was NOT called after a successful completion")
	}
	if got := gate.getLastUpdateCostNano(); got != wantCostNano {
		t.Errorf("UpdateUsage costNano = %d, want %d", got, wantCostNano)
	}
	if got := gate.getLastUpdateProjectID(); got != 42 {
		t.Errorf("UpdateUsage projectID = %d, want 42", got)
	}
}

// TestUpdateUsage_CheckBudgetOffCriticalPath is the regression test for FIX
// #27 (github issue #15): "Synchronous budget read on the request goroutine
// adds up to 10s client-visible latency". Before this fix, updateUsage called
// budgetGate.CheckBudget SYNCHRONOUSLY on the request goroutine — a
// pre-increment snapshot needed only for the soft-alert crossing check —
// bounded solely by billingCtxTimeout (10s). Because writeJSON never sets
// Content-Length, the chunked-encoding terminator is withheld until the
// handler returns, so a slow CheckBudget was fully client-visible latency
// even though FIX #18 claimed billing was already off the critical path.
//
// This test injects a BudgetChecker whose CheckBudget call blocks forever
// after the first (admission) call, and asserts the HTTP handler still
// returns promptly. It then releases the block and asserts billing
// (UpdateUsage) still eventually happens.
func TestUpdateUsage_CheckBudgetOffCriticalPath(t *testing.T) {
	gate := &blockingBudgetChecker{unblock: make(chan struct{}), updated: make(chan struct{})}
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{
		ID:    "cmpl-ok",
		Model: "openai/gpt-4o",
		Usage: &schemas.BifrostLLMUsage{PromptTokens: 10, CompletionTokens: 20},
	}
	calc := &fakeCostEstimator{totalNano: 1_500_000}
	h := NewHandler(router, nil, nil, WithBudgetGate(gate, calc))

	rec := httptest.NewRecorder()
	handlerDone := make(chan struct{})
	go func() {
		h.Chat(rec, chatReqWithProject(t, "42", false))
		close(handlerDone)
	}()

	select {
	case <-handlerDone:
	case <-time.After(1 * time.Second):
		t.Fatal("handler did not return within 1s — CheckBudget appears to be blocking the request goroutine")
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !router.called.Load() {
		t.Error("provider was NOT called despite Allow verdict")
	}
	if gate.updateCalls.Load() != 0 {
		t.Error("UpdateUsage ran before CheckBudget was unblocked — billing must not have completed synchronously")
	}

	// Release the blocked pre-increment CheckBudget call; billing should proceed
	// on the detached goroutine.
	close(gate.unblock)

	select {
	case <-gate.updated:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for billing (UpdateUsage) to complete after CheckBudget unblocked")
	}
	if got := gate.updateCalls.Load(); got != 1 {
		t.Errorf("UpdateUsage called %d times, want 1", got)
	}

	// Drain fully so the trySoftAlert goroutine (which also calls the
	// now-unblocked CheckBudget) finishes before the test exits.
	h.DrainBilling()
}

// deadlineCapturingBudgetChecker implements BudgetChecker for
// TestUpdateUsage_PreSnapshotDoesNotShrinkUpdateUsageDeadline. It records the
// context deadline observed by the pre-increment CheckBudget call (call #2 —
// call #1 is the fast request-path admission check) and by UpdateUsage, and
// sleeps for delay before returning from call #2 to simulate a slow/degraded
// budget-store read.
type deadlineCapturingBudgetChecker struct {
	delay      time.Duration
	checkCalls atomic.Int64

	mu                  sync.Mutex
	preSnapshotDeadline time.Time
	updateUsageDeadline time.Time

	once    sync.Once
	updated chan struct{}
}

func (d *deadlineCapturingBudgetChecker) CheckBudget(ctx context.Context, _ int, _, _ string, _, _ int64) (failmode.Decision, error) {
	n := d.checkCalls.Add(1)
	if n == 1 {
		// Request-path admission check — return immediately.
		return failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy}, nil
	}
	if n == 2 {
		// Pre-increment snapshot: simulate a slow/degraded budget-store read.
		time.Sleep(d.delay)
		dl, _ := ctx.Deadline()
		d.mu.Lock()
		d.preSnapshotDeadline = dl
		d.mu.Unlock()
	}
	return failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy}, nil
}

func (d *deadlineCapturingBudgetChecker) UpdateUsage(ctx context.Context, _ int, _, _, _ string, _, _, _ int64, _ *failmode.UsageDimensions) error {
	dl, _ := ctx.Deadline()
	d.mu.Lock()
	d.updateUsageDeadline = dl
	d.mu.Unlock()
	d.once.Do(func() { close(d.updated) })
	return nil
}

func (d *deadlineCapturingBudgetChecker) TryAlertCooldown(_ context.Context, _, _ string, _ int64) (bool, error) {
	return false, nil
}

// TestUpdateUsage_PreSnapshotDoesNotShrinkUpdateUsageDeadline guards against a
// regression introduced (and fixed) alongside FIX #27: spawnBillingGoroutine
// must give the pre-increment CheckBudget snapshot its OWN timeout budget,
// separate from the one passed to UpdateUsage. If the two shared a single
// context, a slow/degraded pre-snapshot read would eat into the money-
// critical UpdateUsage call's deadline, silently under-billing under DB/NATS
// degradation.
//
// The test injects a checker that sleeps `delay` inside the pre-snapshot
// CheckBudget call and records the deadline of every context it observes. If
// UpdateUsage's context were the SAME one CheckBudget used (bug), both
// deadlines would be set at the same moment and therefore be (near-)equal
// regardless of the sleep. With independent budgets (fix), UpdateUsage's
// context is created AFTER the sleep, so its deadline is later than the
// pre-snapshot's by approximately `delay`.
func TestUpdateUsage_PreSnapshotDoesNotShrinkUpdateUsageDeadline(t *testing.T) {
	const delay = 300 * time.Millisecond

	gate := &deadlineCapturingBudgetChecker{delay: delay, updated: make(chan struct{})}
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{
		ID:    "cmpl-ok",
		Model: "openai/gpt-4o",
		Usage: &schemas.BifrostLLMUsage{PromptTokens: 10, CompletionTokens: 20},
	}
	calc := &fakeCostEstimator{totalNano: 1_500_000}
	h := NewHandler(router, nil, nil, WithBudgetGate(gate, calc))

	rec := httptest.NewRecorder()
	h.Chat(rec, chatReqWithProject(t, "42", false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	select {
	case <-gate.updated:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for UpdateUsage")
	}
	h.DrainBilling()

	gate.mu.Lock()
	preDL, updDL := gate.preSnapshotDeadline, gate.updateUsageDeadline
	gate.mu.Unlock()

	if preDL.IsZero() {
		t.Fatal("pre-snapshot CheckBudget deadline was not captured")
	}
	if updDL.IsZero() {
		t.Fatal("UpdateUsage deadline was not captured")
	}

	gap := updDL.Sub(preDL)
	if gap < delay/2 {
		t.Errorf("UpdateUsage deadline is only %v after the pre-snapshot's (delay injected was %v); "+
			"want UpdateUsage to have its own fresh budget starting after the slow pre-snapshot read, "+
			"not a deadline shared with (and shrunk by) it", gap, delay)
	}
}

// TestBudgetGate_Allow_TokenAwareCost verifies that the cost billed to
// UpdateUsage reflects the ACTUAL token counts extracted from the LLM response.
// It uses a token-aware fakeCostEstimator so that a bug in
// usageFromChatResponse (e.g. swapping prompt/completion tokens) would produce
// a different cost than expected, causing this test to fail.
//
// Router response: 7 prompt tokens, 13 completion tokens.
// Rates: 100 nano/input token, 200 nano/output token.
// Expected cost: 7*100 + 13*200 = 700 + 2600 = 3300 nano-USD.
func TestBudgetGate_Allow_TokenAwareCost(t *testing.T) {
	const (
		inputRate  = int64(100) // 100 nano-USD per input token
		outputRate = int64(200) // 200 nano-USD per output token
		promptToks = 7
		compToks   = 13
		wantCost   = promptToks*inputRate + compToks*outputRate // 3300
	)

	gate := &fakeBudgetChecker{checkVerdict: failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy}, updated: make(chan struct{})}
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{
		ID:    "cmpl-token-aware",
		Model: "openai/gpt-4o",
		Usage: &schemas.BifrostLLMUsage{PromptTokens: promptToks, CompletionTokens: compToks},
	}
	h, calc := newTokenAwareBudgetHandler(router, gate, inputRate, outputRate)

	rec := httptest.NewRecorder()
	h.Chat(rec, chatReqWithProject(t, "77", false))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// Wait for the async billing goroutine to complete.
	gate.waitForUpdate(t)
	if gate.updateCalls.Load() == 0 {
		t.Error("UpdateUsage was NOT called")
	}
	// Assert that the BILLED cost matches the expected per-token calculation.
	// A wrong token extraction (e.g. swapped prompt/completion) would yield
	// 13*100 + 7*200 = 2700, not 3300, causing this assertion to fail.
	if got := gate.getLastUpdateCostNano(); got != wantCost {
		t.Errorf("UpdateUsage costNano = %d, want %d "+
			"(input=%d*%d + output=%d*%d); got input=%d output=%d from estimator",
			got, wantCost,
			promptToks, inputRate, compToks, outputRate,
			func() int64 { in, _ := calc.getLastTokens(); return in }(),
			func() int64 { _, out := calc.getLastTokens(); return out }())
	}
	// Additionally, assert the estimator received the correct per-role counts.
	gotIn, gotOut := calc.getLastTokens()
	if gotIn != promptToks {
		t.Errorf("estimator received inputTokens = %d, want %d "+
			"(usageFromChatResponse may have swapped prompt/completion)", gotIn, promptToks)
	}
	if gotOut != compToks {
		t.Errorf("estimator received outputTokens = %d, want %d "+
			"(usageFromChatResponse may have swapped prompt/completion)", gotOut, compToks)
	}
}

// TestBudgetGate_Disabled_NoInterference verifies that a Handler built without
// WithBudgetGate (nil gate) calls the provider and never touches the gate.
func TestBudgetGate_Disabled_NoInterference(t *testing.T) {
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{ID: "ok"}
	// No budget gate wired.
	h := NewHandler(router, nil, nil)

	rec := httptest.NewRecorder()
	h.Chat(rec, chatReqWithProject(t, "42", false))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !router.called.Load() {
		t.Error("provider was NOT called when gate is disabled")
	}
}

// TestBudgetGate_AnonymousProject_Allowed verifies that a request with no
// project-id header is allowed when the gate is wired (no ID ⇒ no budget row ⇒
// unlimited — consistent with GovernanceStore's ErrNoBudgetRow path).
func TestBudgetGate_AnonymousProject_Allowed(t *testing.T) {
	gate := &fakeBudgetChecker{checkVerdict: failmode.Decision{Verdict: failmode.Block402}, updated: make(chan struct{})} // would block if called
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{ID: "anon"}
	h := newBudgetHandler(router, gate, 0)

	rec := httptest.NewRecorder()
	// No project-id header.
	h.Chat(rec, chatReqWithProject(t, "", false))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (anonymous project bypasses gate)", rec.Code)
	}
	if router.called.Load() == false {
		t.Error("provider was NOT called for anonymous project")
	}
	// Gate's CheckBudget must NOT have been called — the handler skips it for anonymous.
	if gate.checkCalls.Load() > 0 {
		t.Error("CheckBudget was called for an anonymous project — should be skipped")
	}
}

// TestBudgetGate_Messages_Block402 tests the Anthropic /v1/messages path for
// over-budget → 402 with the Anthropic error format, provider not called.
func TestBudgetGate_Messages_Block402_AnthropicDialect(t *testing.T) {
	gate := &fakeBudgetChecker{checkVerdict: failmode.Decision{Verdict: failmode.Block402, State: failmode.StateNATSHealthy}, updated: make(chan struct{})}
	router := &trackingRouter{}
	router.respResp = &schemas.BifrostResponsesResponse{ID: strPtr("should-not-reach")}
	h := newBudgetHandler(router, gate, 0)

	rec := httptest.NewRecorder()
	h.Messages(rec, messagesReqWithProject(t, "7"))

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", rec.Code)
	}
	if router.called.Load() {
		t.Error("provider was called despite Block402 verdict on /messages path")
	}
	// The Messages handler writes OpenAI-shaped errors for its pre-route errors
	// (the gate writes before ToAnthropicResponsesResponse runs).
	var out openAIError
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal error body: %v", err)
	}
	if out.Error.Type != "budget_exceeded" {
		t.Errorf("error.type = %q, want budget_exceeded", out.Error.Type)
	}
}

// TestBudgetGate_Messages_Allow_UpdateUsageInvoked is the sunny-path for the
// Anthropic /v1/messages unary path.
func TestBudgetGate_Messages_Allow_UpdateUsageInvoked(t *testing.T) {
	const wantCostNano = 2_000_000

	gate := &fakeBudgetChecker{checkVerdict: failmode.Decision{Verdict: failmode.Allow}, updated: make(chan struct{})}
	router := &trackingRouter{}
	router.respResp = &schemas.BifrostResponsesResponse{
		ID:    strPtr("resp-ok"),
		Model: "anthropic/claude-3-5-sonnet",
		Usage: &schemas.ResponsesResponseUsage{InputTokens: 15, OutputTokens: 25},
	}
	h := newBudgetHandler(router, gate, wantCostNano)

	rec := httptest.NewRecorder()
	h.Messages(rec, messagesReqWithProject(t, "55"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !router.called.Load() {
		t.Error("provider NOT called despite Allow verdict")
	}
	gate.waitForUpdate(t)
	if gate.updateCalls.Load() == 0 {
		t.Error("UpdateUsage NOT called after successful /messages completion")
	}
	if got := gate.getLastUpdateProjectID(); got != 55 {
		t.Errorf("UpdateUsage projectID = %d, want 55", got)
	}
}

// TestBudgetGate_ChatStream_Block402_ProviderNotCalled verifies that a streaming
// Chat request is blocked at 402 before the provider is ever called.
func TestBudgetGate_ChatStream_Block402_ProviderNotCalled(t *testing.T) {
	gate := &fakeBudgetChecker{checkVerdict: failmode.Decision{Verdict: failmode.Block402}, updated: make(chan struct{})}
	router := &trackingRouter{}
	router.streamChan = newChunkChan()
	h := newBudgetHandler(router, gate, 0)

	rec := httptest.NewRecorder()
	h.Chat(rec, chatReqWithProject(t, "11", true /* stream */))

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402 (streaming blocked before provider)", rec.Code)
	}
	if router.called.Load() {
		t.Error("streaming provider was called despite Block402 verdict")
	}
}

// TestBudgetGate_CheckBudgetError_Returns503 verifies that a hard error from
// CheckBudget (unexpected infra failure) causes a 503 and blocks the provider.
func TestBudgetGate_CheckBudgetError_Returns503(t *testing.T) {
	gate := &fakeBudgetChecker{checkErr: fmt.Errorf("simulated gate panic"), updated: make(chan struct{})}
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{ID: "should-not-reach"}
	h := newBudgetHandler(router, gate, 0)

	rec := httptest.NewRecorder()
	h.Chat(rec, chatReqWithProject(t, "1", false))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 on gate error", rec.Code)
	}
	if router.called.Load() {
		t.Error("provider was called after a gate error")
	}
}

// TestBudgetGate_ZeroCost_SkipsUpdateUsage verifies that when the cost
// Calculator returns 0 (e.g. for a response with no usage tokens), UpdateUsage
// is NOT called — there is nothing to bill.
func TestBudgetGate_ZeroCost_SkipsUpdateUsage(t *testing.T) {
	gate := &fakeBudgetChecker{checkVerdict: failmode.Decision{Verdict: failmode.Allow}, updated: make(chan struct{})}
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{
		ID:    "zero-usage",
		Usage: &schemas.BifrostLLMUsage{PromptTokens: 0, CompletionTokens: 0},
	}
	// costNano=0 means the calculator always returns 0 total.
	h := newBudgetHandler(router, gate, 0)

	rec := httptest.NewRecorder()
	h.Chat(rec, chatReqWithProject(t, "3", false))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gate.updateCalls.Load() > 0 {
		t.Error("UpdateUsage called despite zero-cost completion — should be skipped")
	}
}

// ── FIX #3: Responses handler budget gate ────────────────────────────────────

// responsesReqWithProject builds a POST /llm/v1/responses httptest request.
func responsesReqWithProject(t *testing.T, projectID string) *http.Request {
	t.Helper()
	body := `{"model":"openai/gpt-4o","input":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if projectID != "" {
		req.Header.Set(headerProjectID, projectID)
	}
	return req
}

// TestBudgetGate_Responses_Block402_ProviderNotCalled verifies that the
// Responses handler enforces the gate and returns 402 before calling the provider.
func TestBudgetGate_Responses_Block402_ProviderNotCalled(t *testing.T) {
	gate := &fakeBudgetChecker{checkVerdict: failmode.Decision{Verdict: failmode.Block402, State: failmode.StateNATSHealthy}, updated: make(chan struct{})}
	router := &trackingRouter{}
	router.respResp = &schemas.BifrostResponsesResponse{ID: strPtr("should-not-reach")}
	h := newBudgetHandler(router, gate, 0)

	rec := httptest.NewRecorder()
	h.Responses(rec, responsesReqWithProject(t, "10"))

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402 (Responses handler must gate)", rec.Code)
	}
	if router.called.Load() {
		t.Error("provider was called despite Block402 on /responses path")
	}
	var out openAIError
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal error body: %v", err)
	}
	if out.Error.Type != "budget_exceeded" {
		t.Errorf("error.type = %q, want budget_exceeded", out.Error.Type)
	}
}

// TestBudgetGate_Responses_Allow_UpdateUsageInvoked is the unary sunny-path for
// the OpenAI Responses (/v1/responses) handler.
func TestBudgetGate_Responses_Allow_UpdateUsageInvoked(t *testing.T) {
	const wantCostNano = 3_000_000
	gate := &fakeBudgetChecker{checkVerdict: failmode.Decision{Verdict: failmode.Allow}, updated: make(chan struct{})}
	router := &trackingRouter{}
	router.respResp = &schemas.BifrostResponsesResponse{
		ID:    strPtr("resp-responses-ok"),
		Model: "openai/gpt-4o",
		Usage: &schemas.ResponsesResponseUsage{InputTokens: 20, OutputTokens: 30},
	}
	h := newBudgetHandler(router, gate, wantCostNano)

	rec := httptest.NewRecorder()
	h.Responses(rec, responsesReqWithProject(t, "20"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !router.called.Load() {
		t.Error("provider NOT called despite Allow verdict")
	}
	gate.waitForUpdate(t)
	if gate.updateCalls.Load() == 0 {
		t.Error("UpdateUsage NOT called after /responses completion")
	}
	if got := gate.getLastUpdateProjectID(); got != 20 {
		t.Errorf("UpdateUsage projectID = %d, want 20", got)
	}
}

// ── FIX #4: TextCompletion and Embeddings budget gate ────────────────────────

// TestBudgetGate_TextCompletion_Block402_ProviderNotCalled verifies the legacy
// completions handler gates at 402 before calling the provider.
func TestBudgetGate_TextCompletion_Block402_ProviderNotCalled(t *testing.T) {
	gate := &fakeBudgetChecker{checkVerdict: failmode.Decision{Verdict: failmode.Block402}, updated: make(chan struct{})}
	router := &trackingRouter{}
	router.textResp = &schemas.BifrostTextCompletionResponse{ID: "should-not-reach"}
	h := newBudgetHandler(router, gate, 0)

	body := `{"model":"openai/gpt-3.5-turbo-instruct","prompt":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerProjectID, "5")

	rec := httptest.NewRecorder()
	h.TextCompletion(rec, req)

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402 (TextCompletion must gate)", rec.Code)
	}
	if router.called.Load() {
		t.Error("provider was called despite Block402 on /completions path")
	}
}

// TestBudgetGate_TextCompletion_Allow_UpdateUsageInvoked is the sunny-path for
// the legacy completions handler.
func TestBudgetGate_TextCompletion_Allow_UpdateUsageInvoked(t *testing.T) {
	const wantCostNano = 900_000
	gate := &fakeBudgetChecker{checkVerdict: failmode.Decision{Verdict: failmode.Allow}, updated: make(chan struct{})}
	router := &trackingRouter{}
	router.textResp = &schemas.BifrostTextCompletionResponse{
		ID:    "txt-ok",
		Usage: &schemas.BifrostLLMUsage{PromptTokens: 5, CompletionTokens: 10},
	}
	h := newBudgetHandler(router, gate, wantCostNano)

	body := `{"model":"openai/gpt-3.5-turbo-instruct","prompt":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerProjectID, "6")

	rec := httptest.NewRecorder()
	h.TextCompletion(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	gate.waitForUpdate(t)
	if gate.updateCalls.Load() == 0 {
		t.Error("UpdateUsage NOT called after /completions success")
	}
	if got := gate.getLastUpdateProjectID(); got != 6 {
		t.Errorf("UpdateUsage projectID = %d, want 6", got)
	}
}

// TestBudgetGate_Embeddings_Block402_ProviderNotCalled verifies the embeddings
// handler gates at 402 before calling the provider.
func TestBudgetGate_Embeddings_Block402_ProviderNotCalled(t *testing.T) {
	gate := &fakeBudgetChecker{checkVerdict: failmode.Decision{Verdict: failmode.Block402}, updated: make(chan struct{})}
	router := &trackingRouter{}
	router.embResp = &schemas.BifrostEmbeddingResponse{}
	h := newBudgetHandler(router, gate, 0)

	body := `{"model":"openai/text-embedding-3-small","input":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/embeddings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerProjectID, "7")

	rec := httptest.NewRecorder()
	h.Embeddings(rec, req)

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402 (Embeddings must gate)", rec.Code)
	}
	if router.called.Load() {
		t.Error("provider was called despite Block402 on /embeddings path")
	}
}

// TestBudgetGate_Embeddings_Allow_UpdateUsageInvoked is the sunny-path for
// the embeddings handler.
func TestBudgetGate_Embeddings_Allow_UpdateUsageInvoked(t *testing.T) {
	const wantCostNano = 50_000
	gate := &fakeBudgetChecker{checkVerdict: failmode.Decision{Verdict: failmode.Allow}, updated: make(chan struct{})}
	router := &trackingRouter{}
	router.embResp = &schemas.BifrostEmbeddingResponse{
		Usage: &schemas.BifrostLLMUsage{PromptTokens: 8, CompletionTokens: 0},
	}
	h := newBudgetHandler(router, gate, wantCostNano)

	body := `{"model":"openai/text-embedding-3-small","input":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/embeddings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerProjectID, "8")

	rec := httptest.NewRecorder()
	h.Embeddings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	gate.waitForUpdate(t)
	if gate.updateCalls.Load() == 0 {
		t.Error("UpdateUsage NOT called after /embeddings success")
	}
	if got := gate.getLastUpdateProjectID(); got != 8 {
		t.Errorf("UpdateUsage projectID = %d, want 8", got)
	}
}

// ── FIX #5: streaming billing from final usage chunk ─────────────────────────

// TestBudgetGate_ChatStream_Allow_UpdateUsageFromFinalChunk verifies that
// a streaming Chat request bills via the usage-carrying final chunk.
func TestBudgetGate_ChatStream_Allow_UpdateUsageFromFinalChunk(t *testing.T) {
	const wantCostNano = 1_200_000
	gate := &fakeBudgetChecker{checkVerdict: failmode.Decision{Verdict: failmode.Allow}, updated: make(chan struct{})}
	router := &trackingRouter{}
	// Two chunks: a delta + a final usage chunk (providers send usage in the
	// last chunk with Usage populated).
	router.streamChan = newChunkChan(
		&schemas.BifrostStreamChunk{BifrostChatResponse: &schemas.BifrostChatResponse{
			ID:     "c-delta",
			Object: "chat.completion.chunk",
			// No Usage — normal mid-stream delta.
		}},
		&schemas.BifrostStreamChunk{BifrostChatResponse: &schemas.BifrostChatResponse{
			ID:     "c-final",
			Object: "chat.completion.chunk",
			Usage:  &schemas.BifrostLLMUsage{PromptTokens: 10, CompletionTokens: 20},
		}},
	)
	h := newBudgetHandler(router, gate, wantCostNano)

	rec := httptest.NewRecorder()
	h.Chat(rec, chatReqWithProject(t, "30", true /* stream */))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// Streaming billing is still async (goroutine spawned after channel drains).
	gate.waitForUpdate(t)
	if gate.updateCalls.Load() == 0 {
		t.Error("UpdateUsage NOT called after streaming Chat completion")
	}
	if got := gate.getLastUpdateCostNano(); got != wantCostNano {
		t.Errorf("UpdateUsage costNano = %d, want %d", got, wantCostNano)
	}
}

// TestBudgetGate_ChatStream_NoUsageChunk_Unbilled verifies that when no usage
// chunk arrives the stream completes without calling UpdateUsage (warn, don't bill).
func TestBudgetGate_ChatStream_NoUsageChunk_Unbilled(t *testing.T) {
	gate := &fakeBudgetChecker{checkVerdict: failmode.Decision{Verdict: failmode.Allow}, updated: make(chan struct{})}
	router := &trackingRouter{}
	// Stream with no usage on any chunk.
	router.streamChan = newChunkChan(
		&schemas.BifrostStreamChunk{BifrostChatResponse: &schemas.BifrostChatResponse{
			ID: "c-no-usage",
		}},
	)
	h := newBudgetHandler(router, gate, 500_000)

	rec := httptest.NewRecorder()
	h.Chat(rec, chatReqWithProject(t, "31", true /* stream */))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gate.updateCalls.Load() > 0 {
		t.Error("UpdateUsage should NOT be called when no usage chunk is present")
	}
}

// TestBudgetGate_MessagesStream_Allow_UpdateUsageFromFinalChunk verifies that
// streaming Anthropic /messages bills from the response.completed usage.
func TestBudgetGate_MessagesStream_Allow_UpdateUsageFromFinalChunk(t *testing.T) {
	const wantCostNano = 800_000
	gate := &fakeBudgetChecker{checkVerdict: failmode.Decision{Verdict: failmode.Allow}, updated: make(chan struct{})}
	router := &trackingRouter{}
	// Send a response.completed chunk which carries Response.Usage.
	completedChunk := &schemas.BifrostStreamChunk{
		BifrostResponsesStreamResponse: &schemas.BifrostResponsesStreamResponse{
			Type: schemas.ResponsesStreamResponseTypeCompleted,
			Response: &schemas.BifrostResponsesResponse{
				ID:    strPtr("resp-completed"),
				Model: "anthropic/claude-3-5-sonnet",
				Usage: &schemas.ResponsesResponseUsage{InputTokens: 12, OutputTokens: 18},
			},
		},
	}
	router.streamChan = newChunkChan(completedChunk)
	h := newBudgetHandler(router, gate, wantCostNano)

	rec := httptest.NewRecorder()
	h.Messages(rec, messagesStreamReqWithProject(t, "40"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	gate.waitForUpdate(t)
	if gate.updateCalls.Load() == 0 {
		t.Error("UpdateUsage NOT called after streaming Messages completion")
	}
}

// TestBudgetGate_ResponsesStream_Allow_UpdateUsageFromFinalChunk verifies that
// streaming /v1/responses bills from the response.completed usage.
func TestBudgetGate_ResponsesStream_Allow_UpdateUsageFromFinalChunk(t *testing.T) {
	const wantCostNano = 650_000
	gate := &fakeBudgetChecker{checkVerdict: failmode.Decision{Verdict: failmode.Allow}, updated: make(chan struct{})}
	router := &trackingRouter{}
	completedChunk := &schemas.BifrostStreamChunk{
		BifrostResponsesStreamResponse: &schemas.BifrostResponsesStreamResponse{
			Type: schemas.ResponsesStreamResponseTypeCompleted,
			Response: &schemas.BifrostResponsesResponse{
				ID:    strPtr("resp-r-completed"),
				Model: "openai/gpt-4o",
				Usage: &schemas.ResponsesResponseUsage{InputTokens: 8, OutputTokens: 12},
			},
		},
	}
	router.streamChan = newChunkChan(completedChunk)
	h := newBudgetHandler(router, gate, wantCostNano)

	body := `{"model":"openai/gpt-4o","input":"hi","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerProjectID, "50")

	rec := httptest.NewRecorder()
	h.Responses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	gate.waitForUpdate(t)
	if gate.updateCalls.Load() == 0 {
		t.Error("UpdateUsage NOT called after streaming /responses completion")
	}
}

// ── FIX #24: BifrostContextKeyUserID propagation ─────────────────────────────

// TestUserIDPropagatedToContext verifies that the X-Elitea-User-Id header is
// stored on the BifrostContext under BifrostContextKeyUserID.
func TestUserIDPropagatedToContext(t *testing.T) {
	// A router that captures the *BifrostContext actually handed to the core
	// call, so we can assert the user id was propagated onto it. Asserting only
	// HTTP 200 would pass even if propagation broke — the whole point is to
	// inspect what reached the router.
	capture := &ctxCapturingRouter{
		fakeRouter: fakeRouter{chatResp: &schemas.BifrostChatResponse{ID: "ok"}},
	}
	capture.chatResp.Model = "gpt-4o"
	h := NewHandler(capture, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions",
		strings.NewReader(`{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerProjectID, "99")
	req.Header.Set(headerUserID, "user-abc")

	rec := httptest.NewRecorder()
	h.Chat(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if capture.gotCtx == nil {
		t.Fatal("router never received a context (ChatCompletionRequest not called)")
	}
	got, ok := capture.gotCtx.Value(schemas.BifrostContextKeyUserID).(string)
	if !ok || got != "user-abc" {
		t.Fatalf("BifrostContextKeyUserID on context = %q (ok=%v), want %q — user id was not propagated",
			got, ok, "user-abc")
	}
}

// ctxCapturingRouter records the context passed to ChatCompletionRequest so a
// test can assert what the handler propagated (identity, etc.).
type ctxCapturingRouter struct {
	fakeRouter
	gotCtx *schemas.BifrostContext
}

func (r *ctxCapturingRouter) ChatCompletionRequest(ctx *schemas.BifrostContext, req *schemas.BifrostChatRequest) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	r.gotCtx = ctx
	return r.fakeRouter.ChatCompletionRequest(ctx, req)
}

// TestBillingPeriodHelpers verifies that billingPeriodStart / billingPeriodEnd
// produce correct Unix timestamps for a known date.
func TestBillingPeriodHelpers(t *testing.T) {
	// 2026-07-15 14:30:00 UTC — mid-month.
	mid := time.Date(2026, 7, 15, 14, 30, 0, 0, time.UTC)
	wantStart := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC).Unix()
	wantEnd := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC).Unix() - 1

	if got := billingPeriodStart(mid); got != wantStart {
		t.Errorf("billingPeriodStart = %d, want %d", got, wantStart)
	}
	if got := billingPeriodEnd(mid); got != wantEnd {
		t.Errorf("billingPeriodEnd = %d, want %d", got, wantEnd)
	}
}

// ── Fix round-3 #3: shutdown / DrainBilling integration test ─────────────────

// slowFakeBudgetChecker is a fakeBudgetChecker whose UpdateUsage blocks until
// the caller signals `proceed`. Used by the shutdown integration test to hold
// the billing goroutine in-flight so DrainBilling can be called while billing
// is still running.
type slowFakeBudgetChecker struct {
	fakeBudgetChecker
	// proceed is closed by the test when UpdateUsage should unblock and return.
	proceed chan struct{}
}

func (s *slowFakeBudgetChecker) UpdateUsage(ctx context.Context, projectID int, scope, scopeID, eventID string, costNano, periodStart, periodEnd int64, dims *failmode.UsageDimensions) error {
	// Block until the test says to continue (simulates a slow NATS round-trip).
	select {
	case <-s.proceed:
	case <-ctx.Done():
	}
	return s.fakeBudgetChecker.UpdateUsage(ctx, projectID, scope, scopeID, eventID, costNano, periodStart, periodEnd, dims)
}

// TestDrainBilling_WaitsForInFlightGoroutine is the Fix round-3 #3 integration
// test. It exercises the full handler→DrainBilling shutdown path:
//
//  1. Build a Handler with a slow budget checker (UpdateUsage blocks).
//  2. Fire a request that succeeds → billing goroutine is spawned and blocks.
//  3. Call DrainBilling() in a goroutine (should block until billing finishes).
//  4. Release the billing goroutine (close proceed).
//  5. Assert DrainBilling returned (billing completed, not dropped).
//  6. Assert UpdateUsage was called (spend was not silently discarded).
//
// Running under -race catches any Add-after-Wait panic: if DrainBilling sets
// billingClosing BEFORE billingWg.Wait() and a goroutine calls billingWg.Add
// after Wait, the race detector fires before the panic.
func TestDrainBilling_WaitsForInFlightGoroutine(t *testing.T) {
	const wantCostNano = 1_000_000

	proceed := make(chan struct{})
	slow := &slowFakeBudgetChecker{
		fakeBudgetChecker: fakeBudgetChecker{
			checkVerdict: failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy},
			updated:      make(chan struct{}),
		},
		proceed: proceed,
	}

	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{
		ID:    "cmpl-drain-test",
		Model: "openai/gpt-4o",
		Usage: &schemas.BifrostLLMUsage{PromptTokens: 5, CompletionTokens: 10},
	}
	// Build the handler pointing to the slow wrapper so UpdateUsage calls are
	// intercepted by slowFakeBudgetChecker.UpdateUsage (blocking on proceed).
	h := NewHandler(router, nil, nil, WithBudgetGate(slow, &fakeCostEstimator{totalNano: wantCostNano}))

	// Fire the request. The response is written immediately; the billing
	// goroutine is spawned and blocks inside slow.UpdateUsage.
	rec := httptest.NewRecorder()
	h.Chat(rec, chatReqWithProject(t, "42", false))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// DrainBilling() should block until billing completes. Run it concurrently
	// so we can release the goroutine from this goroutine.
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		h.DrainBilling()
	}()

	// Give the goroutine time to reach DrainBilling's billingWg.Wait().
	// 10 ms is enough on any modern machine; the test is not time-sensitive.
	time.Sleep(10 * time.Millisecond)

	// Verify DrainBilling has NOT returned yet (billing goroutine still blocked).
	select {
	case <-drainDone:
		t.Fatal("DrainBilling returned before billing goroutine completed — spend was dropped")
	default:
		// expected: DrainBilling is still waiting
	}

	// Release the billing goroutine.
	close(proceed)

	// DrainBilling must now unblock within 2 s.
	select {
	case <-drainDone:
		// expected
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for DrainBilling to return after billing goroutine was released")
	}

	// Billing must have been recorded (not dropped).
	if slow.updateCalls.Load() == 0 {
		t.Error("UpdateUsage was NOT called — billing was silently dropped during drain")
	}
	if got := slow.getLastUpdateCostNano(); got != wantCostNano {
		t.Errorf("UpdateUsage costNano = %d, want %d — wrong spend recorded", got, wantCostNano)
	}
}

// ── CountTokens budget gate ───────────────────────────────────────────────────

// countTokensReqWithProject builds a POST /llm/v1/messages/count_tokens request
// in Anthropic count-tokens format with the given project-id header.
func countTokensReqWithProject(t *testing.T, projectID string) *http.Request {
	t.Helper()
	body := `{"model":"anthropic/claude-3-5-sonnet","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/messages/count_tokens", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if projectID != "" {
		req.Header.Set(headerProjectID, projectID)
	}
	return req
}

// TestBudgetGate_CountTokens_Block402_ProviderNotCalled verifies that an
// over-budget project receives HTTP 402 on the /messages/count_tokens path and
// that the router's CountTokensRequest is never invoked.  Also verifies the
// Allow (under-budget) case: CountTokensRequest IS called and 200 is returned.
func TestBudgetGate_CountTokens_Block402_ProviderNotCalled(t *testing.T) {
	t.Run("block402 → 402, CountTokensRequest not called", func(t *testing.T) {
		gate := &fakeBudgetChecker{
			checkVerdict: failmode.Decision{Verdict: failmode.Block402, State: failmode.StateNATSHealthy},
			updated:      make(chan struct{}),
		}
		router := &trackingRouter{}
		router.countResp = &schemas.BifrostCountTokensResponse{}
		h := newBudgetHandler(router, gate, 0)

		rec := httptest.NewRecorder()
		h.CountTokens(rec, countTokensReqWithProject(t, "42"))

		if rec.Code != http.StatusPaymentRequired {
			t.Fatalf("status = %d, want 402 (count_tokens must be gated)", rec.Code)
		}
		if router.called.Load() {
			t.Error("CountTokensRequest was called despite Block402 verdict — gate did not block")
		}
		var out openAIError
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("unmarshal error body: %v", err)
		}
		if out.Error.Type != "budget_exceeded" {
			t.Errorf("error.type = %q, want budget_exceeded", out.Error.Type)
		}
		if out.Error.Code != "insufficient_quota" {
			t.Errorf("error.code = %q, want insufficient_quota", out.Error.Code)
		}
	})

	t.Run("allow → CountTokensRequest called, 200", func(t *testing.T) {
		gate := &fakeBudgetChecker{
			checkVerdict: failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy},
			updated:      make(chan struct{}),
		}
		router := &trackingRouter{}
		router.countResp = &schemas.BifrostCountTokensResponse{}
		h := newBudgetHandler(router, gate, 0)

		rec := httptest.NewRecorder()
		h.CountTokens(rec, countTokensReqWithProject(t, "42"))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if !router.called.Load() {
			t.Error("CountTokensRequest was NOT called despite Allow verdict")
		}
	})
}

// TestDrainBilling_NewRequestsRejectedAfterDrain verifies that billing goroutines
// spawned AFTER DrainBilling() sets billingClosing are not added to the
// WaitGroup (which would panic). The test fires a request after DrainBilling
// returns and asserts the billing goroutine was skipped (not panicked) and
// a warning was logged via the updateCalls counter staying zero.
func TestDrainBilling_NewRequestsRejectedAfterDrain(t *testing.T) {
	const costNano = 500_000

	gate := &fakeBudgetChecker{
		checkVerdict: failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy},
		updated:      make(chan struct{}),
	}
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{
		ID:    "cmpl-post-drain",
		Model: "openai/gpt-4o",
		Usage: &schemas.BifrostLLMUsage{PromptTokens: 5, CompletionTokens: 5},
	}
	h := NewHandler(router, nil, nil, WithBudgetGate(gate, &fakeCostEstimator{totalNano: costNano}))

	// Drain first (no in-flight billing goroutines yet).
	h.DrainBilling()

	// Fire a request AFTER drain. The billing goroutine must be skipped
	// without panicking (Add-after-Wait guard).
	rec := httptest.NewRecorder()
	h.Chat(rec, chatReqWithProject(t, "10", false))

	// Provider was called (the HTTP path is not affected by drain state).
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Billing must have been skipped (not added after Wait).
	// Give a brief window for any goroutine that might have been spawned anyway.
	time.Sleep(50 * time.Millisecond)
	if gate.updateCalls.Load() != 0 {
		t.Error("UpdateUsage was called after DrainBilling — billing goroutine was added after Wait")
	}
}

// TestRecheckVerdict_ObservesTheBackstopWithoutFeedingIt pins the one difference
// between an ARRIVAL and a RE-CHECK.
//
// THE DEFECT. A live realtime session re-asks this verdict on a ticker, and the
// verdict recorded a hit in the per-(project, model) amplification backstop each
// time. The gateway's own gating work therefore arrived in that window as if it
// were tenant traffic, and enough long sessions on one pair opened the circuit
// for that project's REAL /llm requests.
//
// The re-check must still RESPECT an open circuit. Dropping the read would leave
// a live session outside a control every other path obeys.
func TestRecheckVerdict_ObservesTheBackstopWithoutFeedingIt(t *testing.T) {
	const threshold = 3
	h := NewHandler(newDispatchSpy(), nil, nil,
		WithLoopBreakerParams(LoopBreakerParams{
			Threshold: threshold,
			Window:    time.Minute,
			OpenFor:   time.Minute,
		}))
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyVirtualKey, "42")

	// Many more re-checks than the threshold, on one tuple.
	for i := 0; i < 50; i++ {
		if v := h.recheckVerdict(ctx, "gpt-4o"); !v.allow {
			t.Fatalf("re-check #%d was refused with status %d; a re-check must not open the circuit it reads",
				i, v.status)
		}
	}
	// An arriving request is still admitted, which is the property that matters
	// to a tenant: the sliding window holds none of the re-checks.
	if v := h.admissionVerdict(ctx, "gpt-4o"); !v.allow {
		t.Fatal("an arriving request was refused after 50 budget re-checks; the re-checks filled the window")
	}

	// A circuit that ARRIVALS open is still reported to a re-check.
	for i := 0; i < threshold; i++ {
		h.admissionVerdict(ctx, "gpt-4o")
	}
	v := h.recheckVerdict(ctx, "gpt-4o")
	if v.allow {
		t.Fatal("a re-check was admitted while the tuple's circuit was open; the read must not be dropped")
	}
	if v.status != http.StatusTooManyRequests || v.code != "rate_limit_exceeded" {
		t.Fatalf("re-check verdict = status %d code %q, want 429 rate_limit_exceeded", v.status, v.code)
	}
}
