package llmproxy

// metered_test.go — the metered-call API (#1238, metered.go).
//
// The properties under test: a refusal is the LLM refusal byte for byte and
// comes before any provider I/O (the gateway calls no provider here; the
// worker calls none until admit answers 200); an unpriced operation is refused
// before the budget gate is asked; a settlement bills once per caller event id,
// with a derived member id; and an admission cannot be spent by anyone but the
// identity it was issued to.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/cost"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/failmode"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/policy"
)

// fakeMeteredPricer prices from a fixed table and counts lookups.
type fakeMeteredPricer struct {
	prices  map[string]cost.MeteredPrice
	lookups atomic.Int64
}

func (f *fakeMeteredPricer) MeteredPrice(_ context.Context, provider, operation string) (cost.MeteredPrice, bool) {
	f.lookups.Add(1)
	price, ok := f.prices[provider+"/"+operation]
	return price, ok
}

func defaultMeteredPrices() *fakeMeteredPricer {
	return &fakeMeteredPricer{prices: map[string]cost.MeteredPrice{
		"tavily/web_search":      {Unit: cost.MeteredUnitCredit, NanoUSDPerUnit: 8_000_000},
		"firecrawl/web_fetch":    {Unit: cost.MeteredUnitPage, NanoUSDPerUnit: 3_800_000},
		"ddg_instant/web_search": {Unit: cost.MeteredUnitRequest, NanoUSDPerUnit: 0},
	}}
}

func newMeteredHandler(gate BudgetChecker, pricer MeteredPricer, opts ...HandlerOption) *Handler {
	opts = append([]HandlerOption{WithBudgetGate(gate, &fakeCostEstimator{}), WithMeteredPrices(pricer)}, opts...)
	return NewHandler(&trackingRouter{}, nil, nil, opts...)
}

func meteredRequest(t *testing.T, path, project, user, execution string, body any) *http.Request {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if project != "" {
		req.Header.Set(headerProjectID, project)
	}
	if user != "" {
		req.Header.Set(headerUserID, user)
	}
	if execution != "" {
		req.Header.Set(headerExecutionID, execution)
	}
	return req
}

func admitBody(provider, operation string) map[string]any {
	return map[string]any{"operation": operation, "provider": provider, "estimate": map[string]any{"credits": 1}}
}

// admit runs one admission as project 42, member 7, execution exec-1.
func admit(t *testing.T, h *Handler, provider, operation string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.MeteredAdmit(rec, meteredRequest(t, "/llm/v1/metered/admit", "42", "7", "exec-1", admitBody(provider, operation)))
	return rec
}

func admissionID(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("admit status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp meteredAdmitResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.AdmissionID
}

func settleBody(admission, eventID, provider, operation, outcome string, units map[string]any) map[string]any {
	return map[string]any{
		"admission_id": admission, "event_id": eventID, "operation": operation, "provider": provider,
		"units": units, "outcome": outcome,
	}
}

func settle(t *testing.T, h *Handler, project, user, execution string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.MeteredSettle(rec, meteredRequest(t, "/llm/v1/metered/settle", project, user, execution, body))
	return rec
}

// llmRefusalBody is the body the same gate writes for a chat call.
func llmRefusalBody(t *testing.T, gate BudgetChecker) []byte {
	t.Helper()
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{ID: "unreached"}
	h := NewHandler(router, nil, nil, WithBudgetGate(gate, &fakeCostEstimator{}))
	rec := httptest.NewRecorder()
	h.Chat(rec, memberChatRequest(t, "42", "7"))
	return rec.Body.Bytes()
}

func TestMeteredAdmitRefusesExhaustedProject(t *testing.T) {
	gate := newScopedChecker(block402(), allow())
	pricer := defaultMeteredPrices()
	rec := admit(t, newMeteredHandler(gate, pricer), "tavily", "web_search")
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", rec.Code)
	}
	if want := llmRefusalBody(t, newScopedChecker(block402(), allow())); !bytes.Equal(rec.Body.Bytes(), want) {
		t.Fatalf("refusal body\n got %s\nwant %s (the LLM refusal)", rec.Body.Bytes(), want)
	}
	if len(gate.billedCalls()) != 0 {
		t.Fatal("a refused admission billed")
	}
}

func TestMeteredAdmitRefusesExhaustedMember(t *testing.T) {
	gate := newScopedChecker(allow(), block402())
	rec := admit(t, newMeteredHandler(gate, defaultMeteredPrices()), "tavily", "web_search")
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", rec.Code)
	}
	if want := llmRefusalBody(t, newScopedChecker(allow(), block402())); !bytes.Equal(rec.Body.Bytes(), want) {
		t.Fatalf("refusal body\n got %s\nwant %s (the LLM refusal)", rec.Body.Bytes(), want)
	}
	if _, code := decodeErrorTypeAndCode(t, rec.Body.Bytes()); code != budgetCodeMember {
		t.Fatalf("code = %q, want %q", code, budgetCodeMember)
	}
}

func TestMeteredAdmitFailClosed503(t *testing.T) {
	gate := newScopedChecker(failmode.Decision{Verdict: failmode.Block503, State: failmode.StateDownPGStale}, allow())
	rec := admit(t, newMeteredHandler(gate, defaultMeteredPrices()), "tavily", "web_search")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if _, code := decodeErrorTypeAndCode(t, rec.Body.Bytes()); code != "nats_unavailable" {
		t.Fatalf("code = %q, want nats_unavailable", code)
	}
}

// An operation with no price row is refused before the budget gate is asked:
// there is no default price and no silent zero.
func TestMeteredAdmitUnpricedRefused(t *testing.T) {
	gate := newScopedChecker(allow(), allow())
	rec := admit(t, newMeteredHandler(gate, defaultMeteredPrices()), "brave", "web_search")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if _, code := decodeErrorTypeAndCode(t, rec.Body.Bytes()); code != "metered_operation_unpriced" {
		t.Fatalf("code = %q", code)
	}
	if len(gate.checkedScopes()) != 0 {
		t.Fatalf("the budget gate was asked %v for an unpriced operation", gate.checkedScopes())
	}
	// A gateway with no pricer at all refuses every operation the same way.
	bare := NewHandler(&trackingRouter{}, nil, nil, WithBudgetGate(gate, &fakeCostEstimator{}))
	if rec := admit(t, bare, "tavily", "web_search"); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("no pricer: status = %d, want 422", rec.Code)
	}
}

// The authored rate ceilings run on a metered admission as on a model call.
func TestMeteredAdmitRunsRateCeiling(t *testing.T) {
	snap := snapshotOf(t, govRow(policy.TypeRateLimit, "cap", map[string]any{
		"scope":      map[string]any{"project_ids": []any{42.0}},
		"rate_limit": map[string]any{"requests_per_min": 1.0},
	}))
	limiter := policy.NewLimiter(policy.LimiterConfig{
		Counter: newStubCounter(),
		Subject: func(kind, key string, window int64) string { return kind + key },
	})
	h := newMeteredHandler(newScopedChecker(allow(), allow()), defaultMeteredPrices(),
		WithGovernancePolicy(fixedPolicy{snap}, limiter, nil))
	if rec := admit(t, h, "tavily", "web_search"); rec.Code != http.StatusOK {
		t.Fatalf("first admission = %d, want 200", rec.Code)
	}
	second := admit(t, h, "tavily", "web_search")
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second admission = %d, want 429; body %s", second.Code, second.Body.String())
	}
	if second.Header().Get("Retry-After") == "" {
		t.Fatal("a rate refusal carries no Retry-After")
	}
}

// A metered call with no execution has no run to land on.
func TestMeteredAdmitNeedsAnExecution(t *testing.T) {
	h := newMeteredHandler(newScopedChecker(allow(), allow()), defaultMeteredPrices())
	rec := httptest.NewRecorder()
	h.MeteredAdmit(rec, meteredRequest(t, "/llm/v1/metered/admit", "42", "7", "", admitBody("tavily", "web_search")))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestMeteredAdmitAnswersPriceAndEstimate(t *testing.T) {
	rec := admit(t, newMeteredHandler(newScopedChecker(allow(), allow()), defaultMeteredPrices()), "tavily", "web_search")
	var resp meteredAdmitResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.AdmissionID == "" || resp.Price.Unit != "credit" || resp.Price.NanoUSDPerUnit != 8_000_000 || resp.EstimateNanoUSD != 8_000_000 {
		t.Fatalf("admission = %+v", resp)
	}
}

// Settling the same event id twice hands the SAME project and member ids to
// the counter both times, so the NATS dedup window, processed_event_ids and
// the ledger key collapse the retry.
func TestMeteredSettleIdempotent(t *testing.T) {
	gate := newScopedChecker(allow(), allow())
	h := newMeteredHandler(gate, defaultMeteredPrices())
	admission := admissionID(t, admit(t, h, "tavily", "web_search"))
	eventID := uuid.NewString()
	body := settleBody(admission, eventID, "tavily", "web_search", "ok", map[string]any{"requests": 1, "credits": 1})
	for range 2 {
		if rec := settle(t, h, "42", "7", "exec-1", body); rec.Code != http.StatusOK {
			t.Fatalf("settle = %d, body %s", rec.Code, rec.Body.String())
		}
	}
	h.DrainBilling()
	bills := gate.billedCalls()
	if len(bills) != 4 {
		t.Fatalf("bills = %d, want 4 (project and member, twice)", len(bills))
	}
	ids := map[string]map[string]bool{}
	for _, bill := range bills {
		if ids[bill.scope] == nil {
			ids[bill.scope] = map[string]bool{}
		}
		ids[bill.scope][bill.eventID] = true
	}
	if len(ids[failmode.ScopeProject]) != 1 || !ids[failmode.ScopeProject][eventID] {
		t.Fatalf("project event ids = %v, want only %s", ids[failmode.ScopeProject], eventID)
	}
	if len(ids[failmode.ScopeUser]) != 1 {
		t.Fatalf("member event ids = %v, want one stable id", ids[failmode.ScopeUser])
	}
}

// The member scope is billed under UUIDv5(event_id, "member"): stable for a
// retry, and never the project's id (processed_event_ids would read a shared
// id as a redelivery and the member accumulator would never move). Only the
// project delta carries dimensions, so the ledger gets one row.
func TestMeteredSettleMemberDerivedID(t *testing.T) {
	gate := newScopedChecker(allow(), allow())
	h := newMeteredHandler(gate, defaultMeteredPrices())
	admission := admissionID(t, admit(t, h, "tavily", "web_search"))
	eventID := uuid.NewString()
	rec := settle(t, h, "42", "7", "exec-1",
		settleBody(admission, eventID, "tavily", "web_search", "ok", map[string]any{"requests": 1, "credits": 2}))
	if rec.Code != http.StatusOK {
		t.Fatalf("settle = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp meteredSettleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.BilledNanoUSD != 16_000_000 {
		t.Fatalf("settle answer = %s (%v), want billed 16000000", rec.Body.String(), err)
	}
	gate.waitForBothBilled(t)
	want := uuid.NewSHA1(uuid.MustParse(eventID), []byte("member")).String()
	for _, bill := range gate.billedCalls() {
		switch bill.scope {
		case failmode.ScopeProject:
			if bill.eventID != eventID || bill.cost != 16_000_000 || bill.dims == nil {
				t.Fatalf("project bill = %+v", bill)
			}
			d := bill.dims
			if d.Operation != failmode.UsageOperationTool || d.Units != 2 || d.Unit != "credit" ||
				d.Provider != "tavily" || d.Model != "web_search" || d.ExecutionID != "exec-1" {
				t.Fatalf("project dims = %+v", d)
			}
		case failmode.ScopeUser:
			if bill.eventID != want || bill.eventID == eventID || bill.dims != nil {
				t.Fatalf("member bill = %+v, want event %s and no dimensions", bill, want)
			}
		}
	}
}

func TestMeteredSettleRejectsForeignAdmission(t *testing.T) {
	gate := newScopedChecker(allow(), allow())
	h := newMeteredHandler(gate, defaultMeteredPrices())
	admission := admissionID(t, admit(t, h, "tavily", "web_search"))
	other := newMeteredHandler(gate, defaultMeteredPrices()) // another key: no identity secret
	tampered := admission[:len(admission)-2] + "AA"
	cases := map[string]struct {
		h                           *Handler
		project, user, execution    string
		admission, provider, opname string
	}{
		"another project":      {h, "43", "7", "exec-1", admission, "tavily", "web_search"},
		"another member":       {h, "42", "8", "exec-1", admission, "tavily", "web_search"},
		"another execution":    {h, "42", "7", "exec-2", admission, "tavily", "web_search"},
		"another operation":    {h, "42", "7", "exec-1", admission, "tavily", "web_fetch"},
		"another provider":     {h, "42", "7", "exec-1", admission, "firecrawl", "web_search"},
		"a tampered signature": {h, "42", "7", "exec-1", tampered, "tavily", "web_search"},
		"another key":          {other, "42", "7", "exec-1", admission, "tavily", "web_search"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := settle(t, tc.h, tc.project, tc.user, tc.execution,
				settleBody(tc.admission, uuid.NewString(), tc.provider, tc.opname, "ok", map[string]any{"credits": 1}))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body %s", rec.Code, rec.Body.String())
			}
			if _, code := decodeErrorTypeAndCode(t, rec.Body.Bytes()); code != "metered_admission_invalid" {
				t.Fatalf("code = %q", code)
			}
		})
	}
	h.DrainBilling()
	if len(gate.billedCalls()) != 0 {
		t.Fatalf("a refused settlement billed %v", gate.billedCalls())
	}
}

// An admission expires: a settle after the TTL is refused.
func TestMeteredSettleRefusesExpiredAdmission(t *testing.T) {
	h := newMeteredHandler(newScopedChecker(allow(), allow()), defaultMeteredPrices())
	admission := h.signAdmission(meteredAdmission{
		Version: 1, ProjectID: "42", UserID: "7", ExecutionID: "exec-1", Provider: "tavily",
		Operation: "web_search", Unit: "credit", NanoPerUnit: 1, ExpiresUnix: time.Now().Add(-time.Second).Unix(),
	})
	rec := settle(t, h, "42", "7", "exec-1",
		settleBody(admission, uuid.NewString(), "tavily", "web_search", "ok", map[string]any{"credits": 1}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// A $0 operation still bills: it writes its ledger row and moves the counter
// by zero, so free operations appear in Usage and analytics.
func TestMeteredZeroPriceStillWritesLedgerRow(t *testing.T) {
	gate := newScopedChecker(allow(), allow())
	h := newMeteredHandler(gate, defaultMeteredPrices())
	admission := admissionID(t, admit(t, h, "ddg_instant", "web_search"))
	eventID := uuid.NewString()
	rec := settle(t, h, "42", "7", "exec-1",
		settleBody(admission, eventID, "ddg_instant", "web_search", "ok", map[string]any{"requests": 1}))
	if rec.Code != http.StatusOK {
		t.Fatalf("settle = %d, body %s", rec.Code, rec.Body.String())
	}
	gate.waitForBothBilled(t)
	var project *billCall
	for _, bill := range gate.billedCalls() {
		if bill.scope == failmode.ScopeProject {
			project = &bill
		}
	}
	if project == nil || project.cost != 0 || project.dims == nil || project.dims.Units != 1 ||
		project.dims.Unit != "request" || project.dims.Operation != failmode.UsageOperationTool {
		t.Fatalf("project bill = %+v, want a $0 tool row with 1 request", project)
	}
}

// Firecrawl charges a page for a scrape that answered 403 or 404. A provider
// error with no reported units bills that charge; any other provider's error
// bills nothing.
func TestMeteredSettleBillsTheDocumentedFailureCharge(t *testing.T) {
	cases := map[string]struct {
		provider, operation string
		want                int64
	}{
		"firecrawl fetch": {"firecrawl", "web_fetch", 3_800_000},
		"tavily search":   {"tavily", "web_search", 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newMeteredHandler(newScopedChecker(allow(), allow()), defaultMeteredPrices())
			admission := admissionID(t, admit(t, h, tc.provider, tc.operation))
			rec := settle(t, h, "42", "7", "exec-1",
				settleBody(admission, uuid.NewString(), tc.provider, tc.operation, "provider_error", map[string]any{}))
			var resp meteredSettleResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.BilledNanoUSD != tc.want {
				t.Fatalf("settle = %d %s, want billed %d", rec.Code, rec.Body.String(), tc.want)
			}
			h.DrainBilling()
		})
	}
}

func TestMeteredSettleRefusesANonUUIDEventID(t *testing.T) {
	h := newMeteredHandler(newScopedChecker(allow(), allow()), defaultMeteredPrices())
	admission := admissionID(t, admit(t, h, "tavily", "web_search"))
	rec := settle(t, h, "42", "7", "exec-1",
		settleBody(admission, "not-a-uuid", "tavily", "web_search", "ok", map[string]any{"credits": 1}))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "event_id") {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
}
