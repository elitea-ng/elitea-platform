package llmproxy

// End-to-end tests for the throttles per agent and per user (ADR-0041
// decision 4): a request that carries the signed user and execution headers is
// refused with 429 by the throttle on its agent or its user, and another agent
// or user is not.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/policy"
)

// mapAgentLookup is a policy.AgentLookup over a fixed map.
type mapAgentLookup map[string]policy.AgentRef

func (m mapAgentLookup) LookupAgent(_ context.Context, id string) (policy.AgentRef, bool, error) {
	ref, ok := m[id]
	return ref, ok, nil
}

// failingCounter is a rate counter whose every operation fails.
type failingCounter struct{}

func (failingCounter) IncrRateLimit(context.Context, string, int64) (int64, error) {
	return 0, errors.New("nats down")
}

func (failingCounter) ReadRateLimit(context.Context, string) (int64, error) {
	return 0, errors.New("nats down")
}

func throttledHandler(t *testing.T, counter policy.RateCounter, agents policy.AgentLookup, rows ...policy.Row) *Handler {
	t.Helper()
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{ID: "ok"}
	limiter := policy.NewLimiter(policy.LimiterConfig{
		Counter: counter,
		Subject: func(kind, key string, window int64) string { return kind + key },
		Agents:  agents,
	})
	return NewHandler(router, nil, nil,
		WithBudgetGate(allowingGate(), &fakeCostEstimator{}),
		WithGovernancePolicy(fixedPolicy{snapshotOf(t, rows...)}, limiter, nil))
}

func throttledReq(t *testing.T, userID, executionID string) *http.Request {
	t.Helper()
	req := chatReqWithProject(t, "42", false)
	if userID != "" {
		req.Header.Set(headerUserID, userID)
	}
	if executionID != "" {
		req.Header.Set(headerExecutionID, executionID)
	}
	return req
}

func chatStatus(h *Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.Chat(rec, req)
	return rec
}

func TestAgentThrottleRefusesTheAgentButNotAnotherAgent(t *testing.T) {
	agents := mapAgentLookup{
		"run-a": {ProjectID: 42, ApplicationID: 3},
		"run-b": {ProjectID: 42, ApplicationID: 4},
	}
	h := throttledHandler(t, newStubCounter(), agents, govRow(policy.TypeRateLimit, "agent-cap", map[string]any{
		"scope":      map[string]any{"project_ids": []any{42.0}, "agent_ids": []any{3.0}},
		"rate_limit": map[string]any{"requests_per_min": 1.0},
	}))

	if rec := chatStatus(h, throttledReq(t, "", "run-a")); rec.Code != http.StatusOK {
		t.Fatalf("the agent's first call was refused: %d %s", rec.Code, rec.Body.String())
	}
	rec := chatStatus(h, throttledReq(t, "", "run-a"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the agent's second call = %d, want 429; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a throttle refusal carries no Retry-After")
	}
	if e := errorBody(t, rec); e.Type != "rate_limit_error" || e.Code != "rate_limit_exceeded" {
		t.Errorf("error envelope = %+v; a throttle uses the rate-limit shape", e)
	}
	for i := 0; i < 3; i++ {
		if rec := chatStatus(h, throttledReq(t, "", "run-b")); rec.Code != http.StatusOK {
			t.Fatalf("another agent's call %d was refused by the throttle: %d", i+1, rec.Code)
		}
		if rec := chatStatus(h, throttledReq(t, "", "")); rec.Code != http.StatusOK {
			t.Fatalf("a call made from no agent run was refused by the throttle: %d", rec.Code)
		}
	}
}

func TestUserThrottleRefusesTheUserButNotAnotherUser(t *testing.T) {
	h := throttledHandler(t, newStubCounter(), nil, govRow(policy.TypeRateLimit, "user-cap", map[string]any{
		"scope":      map[string]any{"user_ids": []any{7.0}},
		"rate_limit": map[string]any{"requests_per_min": 1.0},
	}))

	if rec := chatStatus(h, throttledReq(t, "7", "")); rec.Code != http.StatusOK {
		t.Fatalf("the user's first call was refused: %d", rec.Code)
	}
	rec := chatStatus(h, throttledReq(t, "7", ""))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("the user's second call = %d (Retry-After %q), want 429 with Retry-After",
			rec.Code, rec.Header().Get("Retry-After"))
	}
	for i := 0; i < 3; i++ {
		if rec := chatStatus(h, throttledReq(t, "8", "")); rec.Code != http.StatusOK {
			t.Fatalf("another user's call %d was refused by the throttle: %d", i+1, rec.Code)
		}
	}
}

func TestThrottleFailsOpenWhenTheCounterIsBroken(t *testing.T) {
	agents := mapAgentLookup{"run-a": {ProjectID: 42, ApplicationID: 3}}
	h := throttledHandler(t, failingCounter{}, agents,
		govRow(policy.TypeRateLimit, "user-cap", map[string]any{
			"scope":      map[string]any{"user_ids": []any{7.0}},
			"rate_limit": map[string]any{"requests_per_min": 1.0},
		}),
		govRow(policy.TypeRateLimit, "agent-cap", map[string]any{
			"scope":      map[string]any{"project_ids": []any{42.0}, "agent_ids": []any{3.0}},
			"rate_limit": map[string]any{"requests_per_min": 1.0},
		}))
	for i := 0; i < 3; i++ {
		if rec := chatStatus(h, throttledReq(t, "7", "run-a")); rec.Code != http.StatusOK {
			t.Fatalf("call %d was refused while the counter was broken: %d; throttles fail open", i+1, rec.Code)
		}
	}
	if h.rateLimiter.Degraded() == 0 {
		t.Error("the fail-open was not counted in Degraded")
	}
}
