package llmproxy

// budget_plane_test.go — issue #315: budget enforcement must be installable on
// a gateway that already serves traffic, and the install must not race the
// money path.
//
// RUN THESE WITH -race. Without it TestInstallBudgetEnforcement_UnderLoad
// measures nothing: the install it performs was a plain field write before this
// change, and a plain field write produces correct-looking output on every run.
// The detector is the assertion.

import (
	"context"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/failmode"
)

// allowDecision is the verdict an under-budget project gets.
func allowDecision() failmode.Decision {
	return failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy}
}

// The fakes in this file are SAFE FOR CONCURRENT USE, and the neighbouring
// files' fakes are not. fakeRouter records the last virtual key in a plain
// field, and fakeAlertPublisher counts with a plain int; both are correct for a
// single-goroutine test and both report a data race under this file's load.
// A race in a fake would hide the race this file exists to detect.

// concurrentRouter answers every chat request with a FRESH response that
// carries a usage trailer, so the billing half of the money path runs on every
// request. It allocates per call rather than returning one shared response,
// because the handler reads the value while other workers are inside the same
// method.
type concurrentRouter struct {
	fakeRouter
	calls atomic.Int64
}

func (r *concurrentRouter) ChatCompletionRequest(_ *schemas.BifrostContext, _ *schemas.BifrostChatRequest) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	r.calls.Add(1)
	return &schemas.BifrostChatResponse{
		ID:    "cmpl-ok",
		Model: "openai/gpt-4o",
		Usage: &schemas.BifrostLLMUsage{PromptTokens: 10, CompletionTokens: 20},
	}, nil
}

// countingAlertPublisher is the tenant-facing soft-alert publisher, counted
// atomically.
type countingAlertPublisher struct{ calls atomic.Int64 }

func (p *countingAlertPublisher) PublishSoftAlertEvent(_ context.Context, _ string, _ []byte) error {
	p.calls.Add(1)
	return nil
}

// countingOpsPublisher is the operator-only publisher, counted atomically.
type countingOpsPublisher struct{ calls atomic.Int64 }

func (p *countingOpsPublisher) PublishOpsEvent(_ context.Context, _ []byte) error {
	p.calls.Add(1)
	return nil
}

// budgetLoad drives the money path of a handler from several goroutines until
// it is stopped. It exists so an install can be measured AGAINST live traffic
// rather than against an idle handler.
type budgetLoad struct {
	stop     chan struct{}
	wg       sync.WaitGroup
	requests atomic.Int64
}

// startBudgetLoad hammers every request-path read of the budget plane:
//
//   - h.Chat            admission (CheckBudget) and billing (CostUnits,
//     UpdateUsage, the soft-alert re-read).
//   - publishUnbilledStreamEvent   the operator-only ops publisher.
//   - publishSoftAlertEvent        the tenant-facing alert publisher.
//   - budgetFractionFunc           the CEL budget_used reader.
//
// Each one loads the plane. Before issue #315 each one read a plain struct
// field instead, so an install concurrent with this load is the data race the
// issue is about.
func startBudgetLoad(t *testing.T, h *Handler, workers int) *budgetLoad {
	t.Helper()
	l := &budgetLoad{stop: make(chan struct{})}
	for i := 0; i < workers; i++ {
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			ctx := context.Background()
			for {
				select {
				case <-l.stop:
					return
				default:
				}
				rec := httptest.NewRecorder()
				h.Chat(rec, chatReqWithProject(t, "42", false))
				h.publishUnbilledStreamEvent("42", "openai", "gpt-4o", "test", "test", 0)
				h.publishSoftAlertEvent(ctx, h.budget().alerts, 42, budgetScopeProject, "42", 1, 1)
				_ = h.budgetFractionFunc(ctx, 42)
				l.requests.Add(1)
			}
		}()
	}
	return l
}

// waitForRequests blocks until the load has completed at least n requests, so
// an install really lands on a path that is in use.
func (l *budgetLoad) waitForRequests(t *testing.T, n int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for l.requests.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("load produced %d requests in 5 s, want at least %d", l.requests.Load(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func (l *budgetLoad) done() {
	close(l.stop)
	l.wg.Wait()
}

// TestInstallBudgetEnforcement_UnderLoad is the issue #315 regression test.
//
// It reproduces the recovery the issue asks for: a gateway starts with NO
// budget gate (NATS was unreachable at boot), serves traffic, and then has
// enforcement installed while that traffic is in flight.
//
// The test asserts two different things, and both are needed:
//
//  1. Under -race, the install does not race the request path. This is the part
//     that fails on the pre-#315 shape, where the gate was a plain field that
//     the money path read with no synchronisation.
//  2. The gate is really in force afterwards — it is CALLED by requests that
//     arrive after the install, and it BILLS them. "The field became non-nil"
//     is not the property that matters.
func TestInstallBudgetEnforcement_UnderLoad(t *testing.T) {
	// The NATS-less boot posture: no gate, no calculator, no publishers.
	h := NewHandler(&concurrentRouter{}, nil, nil)
	if h.BudgetEnforcementInstalled() {
		t.Fatal("a handler built with no budget options reports enforcement installed")
	}

	load := startBudgetLoad(t, h, 8)
	// Let the load reach a steady state, so the install below lands on a
	// handler that is really serving and not on an idle one.
	load.waitForRequests(t, 16)

	gate := &fakeBudgetChecker{checkVerdict: allowDecision(), updated: make(chan struct{})}
	ok := h.InstallBudgetEnforcement(BudgetEnforcement{
		Gate:   gate,
		Calc:   &fakeCostEstimator{totalNano: 500_000},
		Alerts: &countingAlertPublisher{},
		Ops:    &countingOpsPublisher{},
	})
	if !ok {
		t.Fatal("InstallBudgetEnforcement refused the first install")
	}

	// Keep the traffic running across the install, then wait for the installed
	// gate to be exercised by BOTH halves of the money path.
	deadline := time.Now().Add(5 * time.Second)
	for gate.checkCalls.Load() == 0 || gate.updateCalls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("installed gate saw %d CheckBudget and %d UpdateUsage calls in 5 s; enforcement did not take effect",
				gate.checkCalls.Load(), gate.updateCalls.Load())
		}
		time.Sleep(time.Millisecond)
	}
	load.done()

	if !h.BudgetEnforcementInstalled() {
		t.Error("enforcement reports not installed after a successful install")
	}
	// The billing goroutines are detached; drain them so the race detector sees
	// every read of the plane before the test ends.
	h.DrainBilling()
}

// TestInstallBudgetEnforcement_RefusesUnderLoad covers the two refusals while
// traffic runs, because both of them return early and a refusal that still
// wrote the plane would be the same data race.
func TestInstallBudgetEnforcement_RefusesUnderLoad(t *testing.T) {
	first := &fakeBudgetChecker{checkVerdict: allowDecision(), updated: make(chan struct{})}
	h := NewHandler(&concurrentRouter{}, nil, nil,
		WithBudgetGate(first, &fakeCostEstimator{totalNano: 500_000}))

	load := startBudgetLoad(t, h, 4)
	load.waitForRequests(t, 8)

	// A nil gate never removes enforcement. Turning the gate off on a running
	// gateway is a fail-open policy change, not something a transport error may
	// decide.
	if h.InstallBudgetEnforcement(BudgetEnforcement{Gate: nil}) {
		t.Error("InstallBudgetEnforcement accepted a nil gate")
	}
	// A second install never swaps the gate under in-flight requests.
	second := &fakeBudgetChecker{checkVerdict: allowDecision(), updated: make(chan struct{})}
	if h.InstallBudgetEnforcement(BudgetEnforcement{Gate: second, Calc: &fakeCostEstimator{totalNano: 1}}) {
		t.Error("InstallBudgetEnforcement replaced a gate that was already installed")
	}

	load.done()
	h.DrainBilling()

	if h.budget().gate != BudgetChecker(first) {
		t.Error("the published gate is not the one installed first")
	}
	if second.checkCalls.Load() != 0 {
		t.Errorf("the refused gate was called %d times", second.checkCalls.Load())
	}
	if first.checkCalls.Load() == 0 {
		t.Error("the installed gate was never called")
	}
}

// TestInstallBudgetEnforcement_RefusesAfterInstall proves a runtime install
// really gates: a request that arrives after the install is refused by the
// installed gate, with the budget refusal contract the SDK matches on.
func TestInstallBudgetEnforcement_RefusesAfterInstall(t *testing.T) {
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{
		ID:    "cmpl-ok",
		Model: "openai/gpt-4o",
		Usage: &schemas.BifrostLLMUsage{PromptTokens: 10, CompletionTokens: 20},
	}
	h := NewHandler(router, nil, nil)

	// Before the install the gateway meters nothing and the provider runs.
	rec := httptest.NewRecorder()
	h.Chat(rec, chatReqWithProject(t, "42", false))
	if rec.Code != 200 {
		t.Fatalf("pre-install status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	gate := &fakeBudgetChecker{
		checkVerdict: failmode.Decision{Verdict: failmode.Block402, State: failmode.StateNATSHealthy},
		updated:      make(chan struct{}),
	}
	if !h.InstallBudgetEnforcement(BudgetEnforcement{Gate: gate, Calc: &fakeCostEstimator{totalNano: 1}}) {
		t.Fatal("InstallBudgetEnforcement refused the install")
	}

	router.called.Store(false)
	rec = httptest.NewRecorder()
	h.Chat(rec, chatReqWithProject(t, "42", false))
	if rec.Code != 402 {
		t.Fatalf("post-install status = %d, want 402 — the installed gate did not refuse", rec.Code)
	}
	if router.called.Load() {
		t.Error("the provider was called after an installed gate refused the request")
	}
	h.DrainBilling()
}

// TestBudgetPlaneSnapshotIsImmutable pins the property the read sites depend
// on: a snapshot a request already holds never changes under it. Without this,
// "load the plane once per operation" would be a convention with nothing
// enforcing it.
func TestBudgetPlaneSnapshotIsImmutable(t *testing.T) {
	h := NewHandler(&concurrentRouter{}, nil, nil)
	before := h.budget()

	gate := &fakeBudgetChecker{checkVerdict: allowDecision(), updated: make(chan struct{})}
	if !h.InstallBudgetEnforcement(BudgetEnforcement{Gate: gate, Calc: &fakeCostEstimator{totalNano: 1}}) {
		t.Fatal("InstallBudgetEnforcement refused the install")
	}

	if before.gate != nil {
		t.Error("the install mutated a snapshot an earlier reader already held")
	}
	if h.budget() == before {
		t.Error("the install republished the same pointer instead of a new value")
	}
}
