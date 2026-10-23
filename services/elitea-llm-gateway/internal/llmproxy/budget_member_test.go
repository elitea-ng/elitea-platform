package llmproxy

// budget_member_test.go — per-member budget admission and billing (issue #321).
//
// The defect these cover: gateway.user_budget was authored, served and
// rendered, and the gateway's admission check knew a single project scope, so a
// member cap never refused anything. A test that only proves the refusal would
// not have caught it either — a gate that refuses EVERY member also passes
// "over-cap member is refused". Both directions are asserted here, against the
// same handler and the same fake, with only the member's verdict differing.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/failmode"
)

// scopedBudgetChecker answers CheckBudget differently per scope and records
// every UpdateUsage call, so a test can assert both which scopes were consulted
// and which were billed.
type scopedBudgetChecker struct {
	projectVerdict failmode.Decision
	memberVerdict  failmode.Decision
	memberErr      error

	mu       sync.Mutex
	checked  []scopeCall
	billed   []billCall
	billWait chan struct{}
	once     sync.Once
}

type scopeCall struct {
	scope   string
	scopeID string
}

type billCall struct {
	scope   string
	scopeID string
	eventID string
	cost    int64
	dims    *failmode.UsageDimensions
}

func newScopedChecker(project, member failmode.Decision) *scopedBudgetChecker {
	return &scopedBudgetChecker{
		projectVerdict: project,
		memberVerdict:  member,
		billWait:       make(chan struct{}),
	}
}

func (s *scopedBudgetChecker) CheckBudget(_ context.Context, _ int, scope, scopeID string, _, _ int64) (failmode.Decision, error) {
	s.mu.Lock()
	s.checked = append(s.checked, scopeCall{scope: scope, scopeID: scopeID})
	s.mu.Unlock()
	if scope == failmode.ScopeUser {
		return s.memberVerdict, s.memberErr
	}
	return s.projectVerdict, nil
}

func (s *scopedBudgetChecker) UpdateUsage(_ context.Context, _ int, scope, scopeID, eventID string, costNano, _, _ int64, dims *failmode.UsageDimensions) error {
	s.mu.Lock()
	s.billed = append(s.billed, billCall{
		scope: scope, scopeID: scopeID, eventID: eventID, cost: costNano, dims: dims,
	})
	billedScopes := len(s.billed)
	s.mu.Unlock()
	// Unblock once both scopes have been billed, so a test never reads a
	// half-finished billing goroutine.
	if billedScopes >= 2 {
		s.once.Do(func() { close(s.billWait) })
	}
	return nil
}

func (s *scopedBudgetChecker) TryAlertCooldown(_ context.Context, _, _ string, _ int64) (bool, error) {
	return false, nil
}

func (s *scopedBudgetChecker) checkedScopes() []scopeCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]scopeCall(nil), s.checked...)
}

func (s *scopedBudgetChecker) billedCalls() []billCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]billCall(nil), s.billed...)
}

func (s *scopedBudgetChecker) waitForBothBilled(t *testing.T) {
	t.Helper()
	select {
	case <-s.billWait:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for both scopes to be billed")
	}
}

// memberChatRequest builds a chat request carrying both identity headers.
func memberChatRequest(t *testing.T, projectID, userID string) *http.Request {
	t.Helper()
	req := chatReqWithProject(t, projectID, false)
	if userID != "" {
		req.Header.Set(headerUserID, userID)
	}
	return req
}

func allow() failmode.Decision {
	return failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy}
}

func block402() failmode.Decision {
	return failmode.Decision{Verdict: failmode.Block402, State: failmode.StateNATSHealthy}
}

// decodeErrorTypeAndCode pulls BOTH OpenAI-shaped error fields out of a refusal
// body. Both are load-bearing and they carry different things: the type says
// "this is a budget refusal" and the code says which budget. A helper that
// collapsed them into one string could not tell a project refusal from a member
// one once both carry the same type — which is exactly the shape the SDK reads.
func decodeErrorTypeAndCode(t *testing.T, body []byte) (string, string) {
	t.Helper()
	var payload struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	return payload.Error.Type, payload.Error.Code
}

// TestMemberBudget_WithinCapProceeds is the NEGATIVE control for the refusal
// test below. Without it, a member gate that refused every request would look
// like a working per-member budget.
func TestMemberBudget_WithinCapProceeds(t *testing.T) {
	gate := newScopedChecker(allow(), allow())
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{ID: "served"}
	h := newHandlerWithScopedGate(router, gate, 500_000)

	rec := httptest.NewRecorder()
	h.Chat(rec, memberChatRequest(t, "42", "7"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if !router.called.Load() {
		t.Fatal("provider was not called for a member inside their cap")
	}

	// The member scope must actually have been consulted; a 200 that never
	// asked would pass this test for the wrong reason.
	var askedMember bool
	for _, call := range gate.checkedScopes() {
		if call.scope == failmode.ScopeUser {
			askedMember = true
			if call.scopeID != "42:7" {
				t.Fatalf("member scope_id = %q, want %q", call.scopeID, "42:7")
			}
		}
	}
	if !askedMember {
		t.Fatal("the member scope was never consulted; the cap is not being read")
	}
}

// TestMemberBudget_OverCapRefused is the defect in #321: before the member
// scope existed this returned 200 and the provider was called.
func TestMemberBudget_OverCapRefused(t *testing.T) {
	// The PROJECT is under budget. Only the member is over — so a refusal here
	// can only come from the member cap.
	gate := newScopedChecker(allow(), block402())
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{ID: "should-not-reach"}
	h := newHandlerWithScopedGate(router, gate, 500_000)

	rec := httptest.NewRecorder()
	h.Chat(rec, memberChatRequest(t, "42", "7"))

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402; body %s", rec.Code, rec.Body.String())
	}
	if router.called.Load() {
		t.Fatal("provider was called for a member over their cap")
	}
	// The SDK contract: the TYPE marks it as a budget refusal and the CODE
	// carries the scope. A member refusal that puts the scope in the type is
	// not recognised as a budget refusal at all (see budgetErrorType).
	errType, code := decodeErrorTypeAndCode(t, rec.Body.Bytes())
	if errType != "budget_exceeded" {
		t.Fatalf("error type = %q, want budget_exceeded", errType)
	}
	if code != "member_budget_exceeded" {
		t.Fatalf("error code = %q, want member_budget_exceeded", code)
	}
}

// TestMemberBudget_NoMemberIDSkipsMemberScope keeps token-authenticated
// integrations working: they carry no member to charge and must not be refused
// by a cap that cannot apply to them.
func TestMemberBudget_NoMemberIDSkipsMemberScope(t *testing.T) {
	// The member verdict is Block402, which must NOT be reached.
	gate := newScopedChecker(allow(), block402())
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{ID: "served"}
	h := newHandlerWithScopedGate(router, gate, 500_000)

	rec := httptest.NewRecorder()
	h.Chat(rec, memberChatRequest(t, "42", ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	for _, call := range gate.checkedScopes() {
		if call.scope == failmode.ScopeUser {
			t.Fatal("the member scope was consulted for a request with no member id")
		}
	}
}

// TestMemberBudget_ProjectCeilingTakesPrecedence pins which of the two
// refusals a client sees when both apply. The project ceiling is the outer one,
// and its error code deep-links somewhere a member can act on.
func TestMemberBudget_ProjectCeilingTakesPrecedence(t *testing.T) {
	gate := newScopedChecker(block402(), block402())
	router := &trackingRouter{}
	h := newHandlerWithScopedGate(router, gate, 500_000)

	rec := httptest.NewRecorder()
	h.Chat(rec, memberChatRequest(t, "42", "7"))

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", rec.Code)
	}
	errType, code := decodeErrorTypeAndCode(t, rec.Body.Bytes())
	if errType != "budget_exceeded" {
		t.Fatalf("error type = %q, want budget_exceeded", errType)
	}
	if code != "insufficient_quota" {
		t.Fatalf("error code = %q, want insufficient_quota (the project ceiling)", code)
	}
}

// TestMemberBudget_BillsBothScopesWithDistinctEventIDs is the half of #321 that
// makes the cap converge: a member accumulator that is never incremented would
// admit forever no matter how correct the admission read is.
//
// The distinct event ids are load-bearing. gateway.processed_event_ids has
// event_id as its primary key, so a member delta reusing the project delta's id
// is discarded by the write-back consumer as an already-applied redelivery.
func TestMemberBudget_BillsBothScopesWithDistinctEventIDs(t *testing.T) {
	gate := newScopedChecker(allow(), allow())
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{
		ID:    "served",
		Usage: &schemas.BifrostLLMUsage{PromptTokens: 11, CompletionTokens: 22},
	}
	h := newHandlerWithScopedGate(router, gate, 500_000)

	rec := httptest.NewRecorder()
	h.Chat(rec, memberChatRequest(t, "42", "7"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	gate.waitForBothBilled(t)

	calls := gate.billedCalls()
	if len(calls) != 2 {
		t.Fatalf("billed %d scopes, want 2 (project and member)", len(calls))
	}

	byScope := map[string]billCall{}
	for _, call := range calls {
		byScope[call.scope] = call
	}
	project, okProject := byScope[failmode.ScopeProject]
	member, okMember := byScope[failmode.ScopeUser]
	if !okProject || !okMember {
		t.Fatalf("billed scopes = %+v, want one project and one member", calls)
	}
	if project.eventID == member.eventID {
		t.Fatalf("both scopes billed under event id %q; the member delta would dedup away",
			project.eventID)
	}
	if member.scopeID != "42:7" {
		t.Fatalf("member scope_id = %q, want %q", member.scopeID, "42:7")
	}
	if project.cost != member.cost {
		t.Fatalf("project billed %d and member billed %d; one request costs one amount",
			project.cost, member.cost)
	}

	// Only the project delta carries the usage dimensions. Two dimension
	// objects for one request would double every token and request count the
	// per-model table reports (issue #320).
	if project.dims == nil {
		t.Fatal("the project delta carried no usage dimensions; the ledger row would be missing")
	}
	if member.dims != nil {
		t.Fatal("the member delta carried usage dimensions; the ledger would double-count")
	}
	if project.dims.UserID == nil || *project.dims.UserID != 7 {
		t.Fatalf("ledger user_id = %v, want 7", project.dims.UserID)
	}
	if project.dims.PromptTokens != 11 || project.dims.CompletionTokens != 22 {
		t.Fatalf("ledger tokens = (%d, %d), want (11, 22)",
			project.dims.PromptTokens, project.dims.CompletionTokens)
	}
	if project.dims.Model != "gpt-4o" || project.dims.Provider != "openai" {
		t.Fatalf("ledger model/provider = (%q, %q), want (gpt-4o, openai)",
			project.dims.Model, project.dims.Provider)
	}
}

// TestMemberBudget_NoMemberIDBillsProjectOnly proves the ledger records "no
// member" as absent rather than as member 0.
func TestMemberBudget_NoMemberIDBillsProjectOnly(t *testing.T) {
	gate := newScopedChecker(allow(), allow())
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{
		ID:    "served",
		Usage: &schemas.BifrostLLMUsage{PromptTokens: 3, CompletionTokens: 4},
	}
	h := newHandlerWithScopedGate(router, gate, 500_000)

	rec := httptest.NewRecorder()
	h.Chat(rec, memberChatRequest(t, "42", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	waitFor(t, func() bool { return len(gate.billedCalls()) >= 1 })
	// Give any second (wrong) billing goroutine a chance to record itself.
	time.Sleep(50 * time.Millisecond)

	calls := gate.billedCalls()
	if len(calls) != 1 {
		t.Fatalf("billed %d scopes, want 1 (project only)", len(calls))
	}
	if calls[0].scope != failmode.ScopeProject {
		t.Fatalf("billed scope = %q, want project", calls[0].scope)
	}
	if calls[0].dims == nil || calls[0].dims.UserID != nil {
		t.Fatalf("ledger user_id = %v, want nil for a call with no member", calls[0].dims)
	}
}

// TestMemberBudget_GateErrorRefuses keeps the member half fail-closed: an
// unreadable member cap must not become an admitted request.
func TestMemberBudget_GateErrorRefuses(t *testing.T) {
	gate := newScopedChecker(allow(), failmode.Decision{})
	gate.memberErr = context.DeadlineExceeded
	router := &trackingRouter{}
	h := newHandlerWithScopedGate(router, gate, 500_000)

	rec := httptest.NewRecorder()
	h.Chat(rec, memberChatRequest(t, "42", "7"))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if router.called.Load() {
		t.Fatal("provider was called although the member cap could not be read")
	}
}

// newHandlerWithScopedGate mirrors newBudgetHandler for the scoped fake.
func newHandlerWithScopedGate(router *trackingRouter, gate *scopedBudgetChecker, costNano int64) *Handler {
	return NewHandler(router, nil, nil, WithBudgetGate(gate, &fakeCostEstimator{totalNano: costNano}))
}

// compile-time assertion that the scoped fake still satisfies the port.
var _ BudgetChecker = (*scopedBudgetChecker)(nil)
