package llmproxy

// End-to-end tests for the authored-governance plane (issue #218).
//
// They drive the real HTTP handlers, because that is the only level at which
// the question this feature answers can be asked: an operator saved a rule —
// does a request obey it? A unit test over Snapshot.CheckModel proves the
// decision function works and proves nothing about whether a request ever
// reaches it. The gap between those two is where this table sat unread for two
// releases.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/policy"
)

// fixedPolicy is a PolicySource over one compiled snapshot.
type fixedPolicy struct{ snap *policy.Snapshot }

func (f fixedPolicy) Current() *policy.Snapshot { return f.snap }

func snapshotOf(t *testing.T, rows ...policy.Row) *policy.Snapshot {
	t.Helper()
	snap := policy.Compile(rows, time.Now())
	if len(snap.Rejected) > 0 {
		t.Fatalf("test fixture rows were rejected: %+v", snap.Rejected)
	}
	return snap
}

func govRow(typ, name string, data map[string]any) policy.Row {
	return policy.Row{ID: name, Type: typ, Section: "governance", Name: name, Data: data, Enabled: true}
}

// allowingGate lives in audio_billing_test.go: a budget checker that admits
// every request, so a refusal in these tests can only come from the
// authored-governance plane.

// capturingRouter records the chat request it dispatched, which is how a
// routing rewrite is asserted: the rule's effect is only visible in what the
// PROVIDER was asked for.
type capturingRouter struct {
	trackingRouter
	mu   sync.Mutex
	last *schemas.BifrostChatRequest
}

func (c *capturingRouter) ChatCompletionRequest(
	ctx *schemas.BifrostContext, req *schemas.BifrostChatRequest,
) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	c.mu.Lock()
	c.last = req
	c.mu.Unlock()
	return c.trackingRouter.ChatCompletionRequest(ctx, req)
}

func (c *capturingRouter) lastRequest() *schemas.BifrostChatRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

// strptr is the *string the Responses-API response id field takes.
func strptr(s string) *string { return &s }

func errorBody(t *testing.T, rec *httptest.ResponseRecorder) openAIErrorFields {
	t.Helper()
	var body openAIError
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("response body is not an OpenAI error envelope: %v", err)
	}
	return body.Error
}

// --- model allowlist ------------------------------------------------------

func TestModelAllowlistRefusesAndDoesNotCallTheProvider(t *testing.T) {
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{ID: "should-not-reach"}
	snap := snapshotOf(t, govRow(policy.TypeModelConfig, "openai-only", map[string]any{
		"scope": map[string]any{"project_ids": []any{42.0}, "providers": []any{"openai"}},
	}))
	h := NewHandler(router, nil, nil,
		WithBudgetGate(allowingGate(), &fakeCostEstimator{}),
		WithGovernancePolicy(fixedPolicy{snap}, nil, nil))

	rec := httptest.NewRecorder()
	// The request names an anthropic model, which the row does not permit.
	h.Messages(rec, messagesReqWithProject(t, "42"))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if router.called.Load() {
		t.Error("the provider was called for a model the governance policy forbids")
	}
	e := errorBody(t, rec)
	if e.Code != "model_not_permitted" || e.Type != "permission_error" {
		t.Errorf("error envelope = %+v; a 404 would send the caller looking for a typo instead", e)
	}
}

func TestModelAllowlistAdmitsAPermittedModel(t *testing.T) {
	router := &trackingRouter{}
	router.respResp = &schemas.BifrostResponsesResponse{ID: strptr("ok")}
	snap := snapshotOf(t, govRow(policy.TypeModelConfig, "anthropic-ok", map[string]any{
		"scope": map[string]any{"project_ids": []any{42.0}, "providers": []any{"anthropic"}},
	}))
	h := NewHandler(router, nil, nil,
		WithBudgetGate(allowingGate(), &fakeCostEstimator{}),
		WithGovernancePolicy(fixedPolicy{snap}, nil, nil))

	rec := httptest.NewRecorder()
	h.Messages(rec, messagesReqWithProject(t, "42"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !router.called.Load() {
		t.Error("a permitted model did not reach the provider")
	}
}

// TestNoPolicyMeansNoRestriction is the nil-is-off promise every existing test
// depends on.
func TestNoPolicyMeansNoRestriction(t *testing.T) {
	router := &trackingRouter{}
	router.respResp = &schemas.BifrostResponsesResponse{ID: strptr("ok")}
	h := NewHandler(router, nil, nil, WithBudgetGate(allowingGate(), &fakeCostEstimator{}))

	rec := httptest.NewRecorder()
	h.Messages(rec, messagesReqWithProject(t, "42"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d with no policy wired, want 200", rec.Code)
	}
}

// --- MCP allowlist --------------------------------------------------------

func mcpChatReq(t *testing.T, projectID, serverLabel string) *http.Request {
	t.Helper()
	body := `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"mcp","server_label":"` + serverLabel + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerProjectID, projectID)
	return req
}

func TestMCPAllowlistRefusesAnUnlistedServer(t *testing.T) {
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{ID: "should-not-reach"}
	snap := snapshotOf(t, govRow(policy.TypeMCPAllowlist, "approved", map[string]any{
		"scope": map[string]any{"project_ids": []any{42.0}},
		"mcp":   map[string]any{"allowlist": []any{"github"}},
	}))
	h := NewHandler(router, nil, nil,
		WithBudgetGate(allowingGate(), &fakeCostEstimator{}),
		WithGovernancePolicy(fixedPolicy{snap}, nil, nil))

	rec := httptest.NewRecorder()
	h.Chat(rec, mcpChatReq(t, "42", "evil"))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if router.called.Load() {
		t.Error("the provider was called with an MCP server the policy forbids")
	}
	if e := errorBody(t, rec); e.Code != "mcp_server_not_permitted" {
		t.Errorf("error code = %q", e.Code)
	}

	// The allowlisted server goes through.
	router2 := &trackingRouter{}
	router2.chatResp = &schemas.BifrostChatResponse{ID: "ok"}
	h2 := NewHandler(router2, nil, nil,
		WithBudgetGate(allowingGate(), &fakeCostEstimator{}),
		WithGovernancePolicy(fixedPolicy{snap}, nil, nil))
	rec2 := httptest.NewRecorder()
	h2.Chat(rec2, mcpChatReq(t, "42", "github"))
	if rec2.Code != http.StatusOK {
		t.Fatalf("an allowlisted MCP server was refused: %d %s", rec2.Code, rec2.Body.String())
	}
}

// --- rate limits ----------------------------------------------------------

// stubCounter is a rate counter with no NATS behind it.
type stubCounter struct{ totals map[string]int64 }

func newStubCounter() *stubCounter { return &stubCounter{totals: map[string]int64{}} }

func (s *stubCounter) IncrRateLimit(_ context.Context, subject string, delta int64) (int64, error) {
	s.totals[subject] += delta
	return s.totals[subject], nil
}

func (s *stubCounter) ReadRateLimit(_ context.Context, subject string) (int64, error) {
	return s.totals[subject], nil
}

func TestRateLimitRefusesWithRetryAfter(t *testing.T) {
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{ID: "ok"}
	snap := snapshotOf(t, govRow(policy.TypeRateLimit, "cap", map[string]any{
		"scope":      map[string]any{"project_ids": []any{42.0}},
		"rate_limit": map[string]any{"requests_per_min": 1.0},
	}))
	limiter := policy.NewLimiter(policy.LimiterConfig{
		Counter: newStubCounter(),
		Subject: func(kind, key string, window int64) string { return kind + key },
	})
	h := NewHandler(router, nil, nil,
		WithBudgetGate(allowingGate(), &fakeCostEstimator{}),
		WithGovernancePolicy(fixedPolicy{snap}, limiter, nil))

	first := httptest.NewRecorder()
	h.Chat(first, chatReqWithProject(t, "42", false))
	if first.Code != http.StatusOK {
		t.Fatalf("the first request under a ceiling of 1 was refused: %d", first.Code)
	}

	second := httptest.NewRecorder()
	h.Chat(second, chatReqWithProject(t, "42", false))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body=%s", second.Code, second.Body.String())
	}
	if second.Header().Get("Retry-After") == "" {
		t.Error("a rate-limit refusal carries no Retry-After; the caller cannot know when to come back")
	}
	e := errorBody(t, second)
	if e.Type != "rate_limit_error" || e.Code != "rate_limit_exceeded" {
		t.Errorf("error envelope = %+v; spec §2.5 fixes both fields", e)
	}
}

// TestRateLimitOfAnotherProjectDoesNotApply pins that the bucket is per tenant.
func TestRateLimitOfAnotherProjectDoesNotApply(t *testing.T) {
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{ID: "ok"}
	snap := snapshotOf(t, govRow(policy.TypeRateLimit, "cap", map[string]any{
		"scope":      map[string]any{"project_ids": []any{42.0}},
		"rate_limit": map[string]any{"requests_per_min": 1.0},
	}))
	limiter := policy.NewLimiter(policy.LimiterConfig{
		Counter: newStubCounter(),
		Subject: func(kind, key string, window int64) string { return kind + key },
	})
	h := NewHandler(router, nil, nil,
		WithBudgetGate(allowingGate(), &fakeCostEstimator{}),
		WithGovernancePolicy(fixedPolicy{snap}, limiter, nil))

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h.Chat(rec, chatReqWithProject(t, "99", false))
		if rec.Code != http.StatusOK {
			t.Fatalf("project 99 request %d was refused by project 42's ceiling: %d", i, rec.Code)
		}
	}
}

// --- routing rules --------------------------------------------------------

func TestRoutingRuleRewritesTheDispatchedTarget(t *testing.T) {
	router := &capturingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{ID: "ok"}
	snap := snapshotOf(t, govRow(policy.TypeRoutingRule, "to-anthropic", map[string]any{
		"cel":      `provider == "openai"`,
		"priority": 10.0,
		"targets": []any{
			map[string]any{"provider": "anthropic", "model": "claude-sonnet", "weight": 1.0},
		},
	}))
	h := NewHandler(router, nil, nil,
		WithBudgetGate(allowingGate(), &fakeCostEstimator{}),
		WithGovernancePolicy(fixedPolicy{snap}, nil, nil),
		WithRoutingPick(func(total float64) float64 { return 0 }))

	rec := httptest.NewRecorder()
	h.Chat(rec, chatReqWithProject(t, "42", false))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	req := router.lastRequest()
	if req == nil {
		t.Fatal("the router recorded no request")
	}
	if string(req.Provider) != "anthropic" || req.Model != "claude-sonnet" {
		t.Errorf("dispatched to %s/%s, want anthropic/claude-sonnet", req.Provider, req.Model)
	}
}

// TestRoutingCannotEscapeTheModelAllowlist is the ordering guarantee. If the
// allowlist ran first, a rule could route a request to a model the operator
// forbade — turning a routing feature into a policy bypass.
func TestRoutingCannotEscapeTheModelAllowlist(t *testing.T) {
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{ID: "should-not-reach"}
	snap := snapshotOf(t,
		govRow(policy.TypeRoutingRule, "to-anthropic", map[string]any{
			"cel":      `provider == "openai"`,
			"priority": 10.0,
			"targets": []any{
				map[string]any{"provider": "anthropic", "model": "claude-sonnet", "weight": 1.0},
			},
		}),
		govRow(policy.TypeModelConfig, "openai-only", map[string]any{
			"scope": map[string]any{"project_ids": []any{42.0}, "providers": []any{"openai"}},
		}),
	)
	h := NewHandler(router, nil, nil,
		WithBudgetGate(allowingGate(), &fakeCostEstimator{}),
		WithGovernancePolicy(fixedPolicy{snap}, nil, nil),
		WithRoutingPick(func(total float64) float64 { return 0 }))

	rec := httptest.NewRecorder()
	h.Chat(rec, chatReqWithProject(t, "42", false))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — the routed target must be judged by the allowlist", rec.Code)
	}
	if router.called.Load() {
		t.Error("the provider was called with a routed target the allowlist forbids")
	}
}

// TestRoutingRuleWithAnUnknownProviderIsIgnored keeps a bad rule from making
// every matching request undispatchable.
func TestRoutingRuleWithAnUnknownProviderIsIgnored(t *testing.T) {
	router := &capturingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{ID: "ok"}
	snap := snapshotOf(t, govRow(policy.TypeRoutingRule, "bogus", map[string]any{
		"cel":      `true`,
		"priority": 10.0,
		"targets": []any{
			map[string]any{"provider": "not-a-real-provider", "model": "x", "weight": 1.0},
		},
	}))
	h := NewHandler(router, nil, nil,
		WithBudgetGate(allowingGate(), &fakeCostEstimator{}),
		WithGovernancePolicy(fixedPolicy{snap}, nil, nil))

	rec := httptest.NewRecorder()
	h.Chat(rec, chatReqWithProject(t, "42", false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the request keeps its original target", rec.Code)
	}
	if req := router.lastRequest(); req != nil && string(req.Provider) == "not-a-real-provider" {
		t.Error("the request was dispatched to a provider the gateway cannot serve")
	}
}

// --- credential rate policy ----------------------------------------------

// TestExcludedRatePolicyRecordsNoSpend is the whole point of the `excluded`
// treatment: the request runs and the counter does not move.
func TestExcludedRatePolicyRecordsNoSpend(t *testing.T) {
	gate := allowingGate()
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{
		ID:    "ok",
		Model: "openai/gpt-4o",
		Usage: &schemas.BifrostLLMUsage{PromptTokens: 10, CompletionTokens: 20},
	}
	snap := snapshotOf(t, govRow(policy.TypeCredentialPolicy, "internal", map[string]any{
		"scope":      map[string]any{"project_ids": []any{42.0}},
		"credential": map[string]any{"rate_policy": policy.RatePolicyExcluded},
	}))
	h := NewHandler(router, nil, nil,
		WithBudgetGate(gate, &fakeCostEstimator{totalNano: 1_500_000}),
		WithGovernancePolicy(fixedPolicy{snap}, nil, nil))

	rec := httptest.NewRecorder()
	h.Chat(rec, chatReqWithProject(t, "42", false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	h.DrainBilling()

	if gate.updateCalls.Load() != 0 {
		t.Errorf("UpdateUsage ran %d times for an excluded request", gate.updateCalls.Load())
	}
}

// TestBilledRatePolicyStillBills is the control for the test above. Without it,
// a change that broke billing entirely would make that test pass.
func TestBilledRatePolicyStillBills(t *testing.T) {
	gate := allowingGate()
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{
		ID:    "ok",
		Model: "openai/gpt-4o",
		Usage: &schemas.BifrostLLMUsage{PromptTokens: 10, CompletionTokens: 20},
	}
	snap := snapshotOf(t, govRow(policy.TypeCredentialPolicy, "normal", map[string]any{
		"scope":      map[string]any{"project_ids": []any{42.0}},
		"credential": map[string]any{"rate_policy": policy.RatePolicyBilled},
	}))
	h := NewHandler(router, nil, nil,
		WithBudgetGate(gate, &fakeCostEstimator{totalNano: 1_500_000}),
		WithGovernancePolicy(fixedPolicy{snap}, nil, nil))

	rec := httptest.NewRecorder()
	h.Chat(rec, chatReqWithProject(t, "42", false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	h.DrainBilling()

	if gate.updateCalls.Load() == 0 {
		t.Error("a billed request recorded no usage")
	}
}
