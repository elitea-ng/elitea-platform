package llmproxy

// loopguard_handler_test.go — handler-level tests for circular-routing guard #2
// (spec §2.6): the per-(project_id, model) circuit breaker on /llm, and the
// budget.soft_alert event emission (spec §8.3).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

// TestLoopGuard_Handler_429OnBurst asserts the wired handler returns the spec
// §2.5 error shape (429 / rate_limit_error / rate_limit_exceeded) with a
// Retry-After header once the same (project, model) tuple bursts past the
// threshold, and that the provider is NOT called for rejected requests.
func TestLoopGuard_Handler_429OnBurst(t *testing.T) {
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{ID: "ok"}
	// Frozen clock: the whole burst lands at one instant, so "inside the 1 s
	// window" is a property of the test, not of how fast the CI box happens to
	// run. With the wall clock a loaded runner could spread the 5 requests past
	// the window and flake.
	clk := &testClock{t: time.Unix(1_700_000_000, 0)}
	h := newTestHandler(router, nil, nil, WithLoopBreakerClock(testLoopParams(), clk.now))

	// First threshold-1 requests pass.
	for i := 0; i < testLoopThreshold-1; i++ {
		rec := httptest.NewRecorder()
		h.Chat(rec, chatReqWithProject(t, "42", false))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 (below threshold)", i+1, rec.Code)
		}
	}
	// The threshold-th request inside the window trips the circuit.
	router.called.Store(false)
	rec := httptest.NewRecorder()
	h.Chat(rec, chatReqWithProject(t, "42", false))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("tripping request: status = %d, want 429", rec.Code)
	}
	if router.called.Load() {
		t.Error("provider was called for the tripped request — breaker must reject at admission")
	}

	var out openAIError
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal 429 body: %v", err)
	}
	if out.Error.Type != "rate_limit_error" {
		t.Errorf("error.type = %q, want rate_limit_error", out.Error.Type)
	}
	if out.Error.Code != "rate_limit_exceeded" {
		t.Errorf("error.code = %q, want rate_limit_exceeded", out.Error.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("Retry-After header missing on 429")
	}

	// A different project is unaffected while the circuit is open.
	rec2 := httptest.NewRecorder()
	h.Chat(rec2, chatReqWithProject(t, "43", false))
	if rec2.Code != http.StatusOK {
		t.Errorf("different project during open circuit: status = %d, want 200", rec2.Code)
	}
}

// TestLoopGuard_Handler_DisarmedByDefault asserts a Handler constructed WITHOUT
// WithLoopBreaker never rate-limits (unit-test compatibility; production arms
// it in main.go, guarded by TestMainWiring).
func TestLoopGuard_Handler_DisarmedByDefault(t *testing.T) {
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{ID: "ok"}
	h := newTestHandler(router, nil, nil)

	for i := 0; i < testLoopThreshold*2; i++ {
		rec := httptest.NewRecorder()
		h.Chat(rec, chatReqWithProject(t, "42", false))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 (breaker disarmed)", i+1, rec.Code)
		}
	}
}

// TestLoopGuard_Handler_AnonymousNotTracked asserts requests without a
// resolvable project bypass the breaker (no stable loop tuple).
func TestLoopGuard_Handler_AnonymousNotTracked(t *testing.T) {
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{ID: "ok"}
	// Frozen clock so the burst genuinely sits inside one window: a wall-clock
	// run could pass merely because the requests spread out (see 429OnBurst).
	clk := &testClock{t: time.Unix(1_700_000_000, 0)}
	h := newTestHandler(router, nil, nil, WithLoopBreakerClock(testLoopParams(), clk.now))

	for i := 0; i < testLoopThreshold*2; i++ {
		rec := httptest.NewRecorder()
		h.Chat(rec, chatReqWithProject(t, "", false))
		if rec.Code != http.StatusOK {
			t.Fatalf("anonymous request %d: status = %d, want 200", i+1, rec.Code)
		}
	}
}

// ── soft-alert event emission ────────────────────────────────────────────────

// fakeAlertPublisher records PublishSoftAlertEvent calls.
type fakeAlertPublisher struct {
	projectID   string
	event       []byte
	calls       int
	hadDeadline bool
}

func (f *fakeAlertPublisher) PublishSoftAlertEvent(ctx context.Context, projectID string, event []byte) error {
	f.calls++
	f.projectID = projectID
	f.event = event
	_, f.hadDeadline = ctx.Deadline()
	return nil
}

// TestSoftAlertEvent_EnvelopeShape asserts publishSoftAlertEvent emits the
// natsbus-compatible envelope {type, source, payload, timestamp} with the
// documented payload fields, under a bounded context.
func TestSoftAlertEvent_EnvelopeShape(t *testing.T) {
	pub := &fakeAlertPublisher{}
	h := newTestHandler(&trackingRouter{}, nil, nil, WithAlertEventPublisher(pub))

	h.publishSoftAlertEvent(context.Background(), h.budget().alerts, 42, budgetScopeProject, "42", 750_000, 1_750_000_000)

	if pub.calls != 1 {
		t.Fatalf("publisher calls = %d, want 1", pub.calls)
	}
	if pub.projectID != "42" {
		t.Errorf("projectID = %q, want 42", pub.projectID)
	}
	if !pub.hadDeadline {
		t.Error("publish context had no deadline — every NATS op must be bounded")
	}

	var env softAlertEnvelope
	if err := json.Unmarshal(pub.event, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Type != "budget.soft_alert" {
		t.Errorf("envelope.type = %q, want budget.soft_alert", env.Type)
	}
	if env.Source != "elitea-llm-gateway" {
		t.Errorf("envelope.source = %q, want elitea-llm-gateway", env.Source)
	}
	if env.Timestamp.IsZero() || time.Since(env.Timestamp) > time.Minute {
		t.Errorf("envelope.timestamp = %v, want recent UTC time", env.Timestamp)
	}

	var payload softAlertPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.ProjectID != "42" || payload.Scope != budgetScopeProject ||
		payload.PeriodStartUnix != 1_750_000_000 || payload.CostJustBilledNano != 750_000 {
		t.Errorf("payload = %+v, want {42 %s 1750000000 750000}", payload, budgetScopeProject)
	}
}

// TestSoftAlertEvent_NilPublisherNoOp asserts emission is a safe no-op when no
// publisher is wired.
func TestSoftAlertEvent_NilPublisherNoOp(t *testing.T) {
	h := newTestHandler(&trackingRouter{}, nil, nil)
	h.publishSoftAlertEvent(context.Background(), h.budget().alerts, 42, budgetScopeProject, "42", 1, 1) // must not panic
}

// TestChat_EmitsElapsedHeader asserts the unary chat path stamps X-Elapsed-Ms
// (the BFF.9d overhead gate's primary metric source) with a parseable
// millisecond float.
func TestChat_EmitsElapsedHeader(t *testing.T) {
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{ID: "ok"}
	h := newTestHandler(router, nil, nil)

	rec := httptest.NewRecorder()
	h.Chat(rec, chatReqWithProject(t, "42", false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	v := rec.Header().Get("X-Elapsed-Ms")
	if v == "" {
		t.Fatal("X-Elapsed-Ms header missing on unary chat response")
	}
	var ms float64
	if _, err := fmt.Sscanf(v, "%f", &ms); err != nil || ms < 0 {
		t.Fatalf("X-Elapsed-Ms = %q, want non-negative float", v)
	}
}
