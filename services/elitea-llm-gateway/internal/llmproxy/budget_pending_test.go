package llmproxy

// budget_pending_test.go — live 4.45.1: a gateway that booted while NATS was
// unreachable logged "READINESS GATED" and, ~15 s later, "BUDGET ENFORCEMENT
// RECOVERED". In between, /readyz said not ready and the /llm routes served
// every request with no budget check, no rate limit and no billing: readiness
// never kept traffic out (the default chart probes the mTLS gateway with
// tcpSocket). A handler marked pending now refuses every spend route until
// the gate is installed, and the install lifts the refusal atomically.
//
// RUN WITH -race: TestBudgetPending_InstallUnderLoadServesNothingUnenforced
// installs the gate while requests are in flight.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/failmode"
)

// assertPendingRefusal checks the 503 body: the OpenAI shape every /llm route
// uses, and the same type/code as the gate's own outage refusal.
func assertPendingRefusal(t *testing.T, label string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("%s: status = %d, want 503; body=%s", label, rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: body %s: %v", label, rec.Body.String(), err)
	}
	if body.Error.Type != "service_unavailable" || body.Error.Code != "nats_unavailable" {
		t.Fatalf("%s: error = %+v, want service_unavailable/nats_unavailable", label, body.Error)
	}
}

func TestBudgetPending_RefusesEverySpendRouteBeforeTheProvider(t *testing.T) {
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{ID: "should-not-be-served"}
	h := NewHandler(router, nil, nil, WithBudgetEnforcementPending())
	if !h.BudgetEnforcementPending() || h.BudgetEnforcementInstalled() {
		t.Fatal("setup: the handler is not pending")
	}

	embeddings := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/llm/v1/embeddings",
			strings.NewReader(`{"model":"openai/text-embedding-3-small","input":"hi"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(headerProjectID, "42")
		return req
	}
	for _, tc := range []struct {
		label string
		serve func(http.ResponseWriter, *http.Request)
		req   *http.Request
	}{
		{"chat", h.Chat, chatReqWithProject(t, "42", false)},
		{"chat stream", h.Chat, chatReqWithProject(t, "42", true)},
		// No project: nothing can tell whether it has a cap without the gate.
		{"chat anonymous", h.Chat, chatReqWithProject(t, "", false)},
		{"messages", h.Messages, messagesReqWithProject(t, "42")},
		{"messages stream", h.Messages, messagesStreamReqWithProject(t, "42")},
		{"embeddings", h.Embeddings, embeddings()},
	} {
		rec := httptest.NewRecorder()
		tc.serve(rec, tc.req)
		assertPendingRefusal(t, tc.label, rec)
	}
	if router.called.Load() {
		t.Fatal("a pending handler dispatched a request to the provider: it was served unenforced")
	}
}

// A handler NOT marked pending (no NATS configured, or the operator's =off)
// keeps the documented unmetered posture.
func TestBudgetPending_NotPendingKeepsTheUnmeteredPosture(t *testing.T) {
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{ID: "ok"}
	h := NewHandler(router, nil, nil)
	if h.BudgetEnforcementPending() {
		t.Fatal("a handler built without the option is pending")
	}
	rec := httptest.NewRecorder()
	h.Chat(rec, chatReqWithProject(t, "42", false))
	if rec.Code != http.StatusOK || !router.called.Load() {
		t.Fatalf("status = %d, provider called = %v; want 200 and true", rec.Code, router.called.Load())
	}
}

// The option never marks a handler that already has a gate.
func TestBudgetPending_OptionIsInertWhenAGateIsWired(t *testing.T) {
	gate := &fakeBudgetChecker{checkVerdict: allowDecision(), updated: make(chan struct{})}
	h := NewHandler(&trackingRouter{}, nil, nil,
		WithBudgetGate(gate, &fakeCostEstimator{totalNano: 1}), WithBudgetEnforcementPending())
	if h.BudgetEnforcementPending() {
		t.Fatal("a handler with a wired gate is pending")
	}
}

// pendingRouter counts provider dispatches and, for each, whether a gate had
// been installed when the request was admitted. It is safe for concurrent use.
type pendingRouter struct {
	concurrentRouter
	h          *Handler
	unenforced atomic.Int64
}

func (r *pendingRouter) ChatCompletionRequest(ctx *schemas.BifrostContext, req *schemas.BifrostChatRequest) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	// The plane is monotonic: a dispatch with no gate installed at THIS point
	// had none at admission either, so it was served unenforced.
	if !r.h.BudgetEnforcementInstalled() {
		r.unenforced.Add(1)
	}
	return r.concurrentRouter.ChatCompletionRequest(ctx, req)
}

// TestBudgetPending_InstallUnderLoadServesNothingUnenforced is the gap itself:
// traffic arrives while enforcement is pending, the recovery loop installs the
// gate mid-traffic, and no request is ever dispatched without the gate. After
// the install, requests are admitted by the gate and billed.
func TestBudgetPending_InstallUnderLoadServesNothingUnenforced(t *testing.T) {
	router := &pendingRouter{}
	h := NewHandler(router, nil, nil, WithBudgetEnforcementPending())
	router.h = h

	load := startBudgetLoad(t, h, 8)
	load.waitForRequests(t, 32)
	if served := router.calls.Load(); served != 0 {
		load.done()
		t.Fatalf("%d requests reached the provider while enforcement was pending", served)
	}

	gate := &fakeBudgetChecker{checkVerdict: allowDecision(), updated: make(chan struct{})}
	if !h.InstallBudgetEnforcement(BudgetEnforcement{
		Gate: gate, Calc: &fakeCostEstimator{totalNano: 500_000},
		Alerts: &countingAlertPublisher{}, Ops: &countingOpsPublisher{},
	}) {
		load.done()
		t.Fatal("InstallBudgetEnforcement refused the first install")
	}
	if h.BudgetEnforcementPending() {
		load.done()
		t.Fatal("the install did not lift the pending refusal")
	}

	deadline := time.Now().Add(5 * time.Second)
	for router.calls.Load() == 0 || gate.updateCalls.Load() == 0 {
		if time.Now().After(deadline) {
			load.done()
			t.Fatalf("after the install: %d dispatches, %d billed; want both > 0",
				router.calls.Load(), gate.updateCalls.Load())
		}
		time.Sleep(time.Millisecond)
	}
	load.done()
	h.DrainBilling()

	if n := router.unenforced.Load(); n != 0 {
		t.Fatalf("%d requests were served with no budget gate", n)
	}
	if checks, served := gate.checkCalls.Load(), router.calls.Load(); checks < served {
		t.Fatalf("%d dispatches but only %d budget checks: a request bypassed the gate", served, checks)
	}
}

// TestBudgetPending_InstalledGateDecidesEveryLaterRequest is the version of the
// install-under-load test that CAN fail: the installed gate refuses every
// request (Block402). Traffic runs across the install. Every response is a 503
// before it and a 402 after it, nothing else, and no request ever reaches the
// provider. A request admitted by the pending window, or one that skipped the
// gate after the install, would be a 200 and a provider call.
func TestBudgetPending_InstalledGateDecidesEveryLaterRequest(t *testing.T) {
	router := &concurrentRouter{}
	h := NewHandler(router, nil, nil, WithBudgetEnforcementPending())
	gate := &fakeBudgetChecker{
		checkVerdict: failmode.Decision{Verdict: failmode.Block402, State: failmode.StateNATSHealthy},
		updated:      make(chan struct{}),
	}

	var (
		pending, refused, other atomic.Int64
		installed               atomic.Bool
		afterInstall            atomic.Int64
	)
	stop := make(chan struct{})
	done := make(chan struct{})
	const workers = 8
	for range workers {
		go func() {
			defer func() { done <- struct{}{} }()
			for {
				select {
				case <-stop:
					return
				default:
				}
				wasInstalled := installed.Load()
				rec := httptest.NewRecorder()
				h.Chat(rec, chatReqWithProject(t, "42", false))
				switch rec.Code {
				case http.StatusServiceUnavailable:
					pending.Add(1)
					if wasInstalled {
						// Started after the install yet refused as pending.
						other.Add(1)
					}
				case http.StatusPaymentRequired:
					refused.Add(1)
					if wasInstalled {
						afterInstall.Add(1)
					}
				default:
					other.Add(1)
				}
			}
		}()
	}
	stopAll := func() {
		close(stop)
		for range workers {
			<-done
		}
	}
	wait := func(what string, cond func() bool) {
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				stopAll()
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(time.Millisecond)
		}
	}
	wait("pending refusals", func() bool { return pending.Load() >= 32 })
	if !h.InstallBudgetEnforcement(BudgetEnforcement{Gate: gate, Calc: &fakeCostEstimator{totalNano: 1}}) {
		stopAll()
		t.Fatal("InstallBudgetEnforcement refused the first install")
	}
	installed.Store(true)
	wait("gate refusals after the install", func() bool { return afterInstall.Load() >= 32 })
	stopAll()

	if n := router.calls.Load(); n != 0 {
		t.Fatalf("%d requests reached the provider; every one must be refused (503 pending, 402 gate)", n)
	}
	if n := other.Load(); n != 0 {
		t.Fatalf("%d responses were neither the pending 503 nor the gate's 402", n)
	}
	if checks := gate.checkCalls.Load(); checks != refused.Load() {
		t.Fatalf("the gate decided %d requests but %d were refused with 402", checks, refused.Load())
	}
	if n := gate.updateCalls.Load(); n != 0 {
		t.Fatalf("%d refused requests were billed", n)
	}
}
