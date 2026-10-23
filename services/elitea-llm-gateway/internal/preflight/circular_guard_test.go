// Package preflight — circular-routing guard integration test (spec §2.6,
// pre-flight exit-gate item "circular-routing guard integration test passes").
//
// Guard #1 (SELF_REFERENTIAL_CREDENTIAL) is covered by unit tests in
// internal/account. This file covers guard #2 end-to-end through the mounted
// chi router: >= 5 requests for the same (project_id, model) tuple within 1 s
// open the circuit and the handler returns HTTP 429 with the spec §2.5 error
// shape (rate_limit_error / rate_limit_exceeded) for 30 s, WITHOUT invoking
// the LLM router — containing a runtime routing loop.
package preflight_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/failmode"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/llmproxy"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/preflight"
)

// TestCircularRoutingGuard_LoopContained is the §2.6 guard-#2 integration gate.
func TestCircularRoutingGuard_LoopContained(t *testing.T) {
	t.Parallel()

	secret := []byte("circular-guard-secret")
	const (
		projectID     = 9005
		projectIDStr  = "9005"
		userID        = "user-circular"
		tenantID      = "tenant-circular"
		hardLimitNano = int64(100) * failmode.NanoUSD // generous — budget must NOT be the blocker
	)

	router := preflight.NewMockRouter(preflight.MockRouterConfig{
		InputTokens:  10,
		OutputTokens: 5,
	})
	gov, _, _ := preflight.NewSeededGovernance(t, projectID, hardLimitNano, 0)
	// Arm guard #2 exactly as main.go does, but off a frozen clock: every
	// request below is meant to land inside the same 1 s sliding window and the
	// same 30 s cooldown. Reading the wall clock instead would make that a
	// property of CI load, so a slow runner could spread the burst past the
	// window and flake. The breaker logic under test is unchanged — only its
	// time source is.
	//
	// The numbers became operator-settable in issue #12 (the production default
	// is no longer 5/1s/30s). This gate still asserts the breaker's BEHAVIOUR
	// end to end through the mounted handler, so it pins them explicitly here
	// rather than tracking whatever the default happens to be.
	frozen := time.Unix(1_700_000_000, 0)
	breakerParams := llmproxy.LoopBreakerParams{Threshold: 5, Window: time.Second, OpenFor: 30 * time.Second}
	handler := preflight.MountedHandler(t, router, gov, secret,
		llmproxy.WithLoopBreakerClock(breakerParams, func() time.Time { return frozen }),
	)

	send := func() *httptest.ResponseRecorder {
		body := `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"loop"}]}`
		req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		preflight.SignRequest(req, secret, projectIDStr, userID, tenantID)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// A loop hammers the same tuple with no delay: the first 4 requests pass,
	// the 5th opens the circuit.
	for i := 0; i < 4; i++ {
		if rec := send(); rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 (below breaker threshold)\nbody: %s",
				i+1, rec.Code, rec.Body.String())
		}
	}

	rec := send()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("5th request within 1 s: status = %d, want 429 (circuit must open)\nbody: %s",
			rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 response missing Retry-After header")
	}

	var errBody struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("decode 429 body: %v\nraw: %s", err, rec.Body.String())
	}
	if errBody.Error.Type != "rate_limit_error" {
		t.Errorf("error.type = %q, want rate_limit_error (spec §2.5)", errBody.Error.Type)
	}
	if errBody.Error.Code != "rate_limit_exceeded" {
		t.Errorf("error.code = %q, want rate_limit_exceeded (spec §2.5)", errBody.Error.Code)
	}

	// While open, the loop's traffic keeps getting 429 and the provider is
	// never called again.
	router.Reset()
	for i := 0; i < 3; i++ {
		if rec := send(); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("request during open circuit: status = %d, want 429", rec.Code)
		}
	}
	if router.Called() {
		t.Error("provider was called during open circuit — loop NOT contained")
	}

	// A different project on the same model is unaffected (tuple isolation).
	otherBody := `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"innocent"}]}`
	otherReq := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", strings.NewReader(otherBody))
	otherReq.Header.Set("Content-Type", "application/json")
	preflight.SignRequest(otherReq, secret, "9006", userID, tenantID)
	otherRec := httptest.NewRecorder()
	handler.ServeHTTP(otherRec, otherReq)
	if otherRec.Code != http.StatusOK {
		t.Errorf("innocent project during open circuit: status = %d, want 200 (tuple isolation)", otherRec.Code)
	}
}
