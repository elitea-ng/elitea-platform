package preflight_test

// harness_test.go — smoke test for the preflight harness itself.
//
// Proves the harness works end-to-end by:
//   1. Building a MockRouter + a seeded-under-budget GovernanceStore.
//   2. Mounting the handler via MountedHandler.
//   3. Signing a POST /llm/v1/chat/completions request with SignRequest.
//   4. Asserting HTTP 200 and at least 2 SSE frames (proving stream flushing
//      + the harness identity signature round-trips correctly).

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/failmode"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/preflight"
)

func TestHarnessSmoke_StreamReturns200AndAtLeast2Frames(t *testing.T) {
	const (
		projectID     = 42
		hardLimitNano = int64(100) * failmode.NanoUSD // 100 USD
		spentNano     = int64(10) * failmode.NanoUSD  // 10 USD — well under limit
		projectIDStr  = "42"
	)

	secret := []byte("smoke-test-secret")

	// 1. Build a MockRouter that emits 3 content chunks + 1 final usage chunk
	//    (4 total stream messages → 4 "data: " SSE frames + 1 "[DONE]" frame).
	router := preflight.NewMockRouter(preflight.MockRouterConfig{
		Chunks:       3,
		InputTokens:  200,
		OutputTokens: 100,
	})

	// 2. Seed the GovernanceStore: project 42 at 10 USD of 100 USD limit.
	gov, nc, _ := preflight.NewSeededGovernance(t, projectID, hardLimitNano, spentNano)

	// 3. Mount the fully-wired handler.
	handler := preflight.MountedHandler(t, router, gov, secret)

	// 4. Build a signed streaming chat request.
	body := `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hello"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	preflight.SignRequest(req, secret, projectIDStr, "user-1", "tenant-1")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// ── assertions ──────────────────────────────────────────────────────────

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}

	ct := rec.Header().Get("Content-Type")
	if ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	// Count SSE data frames in the response body.
	var dataFrames int
	scanner := bufio.NewScanner(strings.NewReader(rec.Body.String()))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			dataFrames++
		}
	}

	// 3 content chunks + 1 usage chunk + 1 [DONE] = 5 frames minimum.
	// The assertion is >=2 per spec so even a MockRouter with Chunks=1 passes.
	if dataFrames < 2 {
		t.Errorf("SSE data frame count = %d, want >= 2\nfull body:\n%s", dataFrames, rec.Body.String())
	}

	// The router must have been called (under-budget allow path).
	if !router.Called() {
		t.Error("MockRouter.Called() is false — provider was not invoked despite under-budget Allow")
	}

	// FIX: billing is now async (off the HTTP critical path), so we must poll
	// until the goroutine completes rather than reading DeltaCount immediately.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && nc.DeltaCount() == 0 {
		time.Sleep(5 * time.Millisecond)
	}

	// After a successful stream the NATS counter must have been incremented
	// (UpdateUsage fired through the real GovernanceStore).
	// Check that at least one delta was published to the write-behind stream.
	if nc.DeltaCount() == 0 {
		t.Error("FakeNATS: no write-behind delta published after billed streaming completion")
	}
}

// TestHarnessSmoke_OverBudget verifies that a project seeded at its hard limit
// receives a 402 and the MockRouter is never called.
func TestHarnessSmoke_OverBudget_Returns402_RouterNotCalled(t *testing.T) {
	const (
		projectID     = 99
		hardLimitNano = int64(100) * failmode.NanoUSD
		spentNano     = int64(100) * failmode.NanoUSD // exactly at the limit → Block402
		projectIDStr  = "99"
	)

	secret := []byte("smoke-test-secret-2")

	router := preflight.NewMockRouter(preflight.MockRouterConfig{})
	gov, _, _ := preflight.NewSeededGovernance(t, projectID, hardLimitNano, spentNano)
	handler := preflight.MountedHandler(t, router, gov, secret)

	body := `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	preflight.SignRequest(req, secret, projectIDStr, "user-2", "tenant-1")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402 (budget_exceeded); body:\n%s", rec.Code, rec.Body.String())
	}
	if router.Called() {
		t.Error("MockRouter.Called() is true — provider was invoked despite over-budget verdict")
	}
}

// TestHarnessSmoke_InvalidSignatureReturns403 verifies that a request signed
// with the wrong secret is rejected with 403.
func TestHarnessSmoke_InvalidSignatureReturns403(t *testing.T) {
	secret := []byte("correct-secret")
	wrongSecret := []byte("wrong-secret")

	router := preflight.NewMockRouter(preflight.MockRouterConfig{})
	gov, _, _ := preflight.NewSeededGovernance(t, 1, 100*failmode.NanoUSD, 0)
	handler := preflight.MountedHandler(t, router, gov, secret)

	body := `{"model":"openai/gpt-4o","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	preflight.SignRequest(req, wrongSecret, "1", "", "") // signed with wrong key

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (invalid identity signature)", rec.Code)
	}
}
