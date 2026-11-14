package llmproxy

// guardrail_warn_test.go: the warn action and the personal_data detector on
// the /llm path (ADR-0043 decision 1, issue 2012).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/guardrail"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/policy"
)

func personalDataRow(stage, action string) policy.Row {
	return guardRow("pii-"+action, map[string]any{
		"stage":     stage,
		"detectors": []any{map[string]any{"name": "pii", "type": "personal_data", "action": action, "rules": []any{"email", "payment-card"}}},
	})
}

func personalDataRequest(t *testing.T) *http.Request {
	t.Helper()
	body := `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"mail jane.doe@corp-mail.io card 4111 1111 1111 1111"}]}`
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", strings.NewReader(body))
	req.Header.Set(headerProjectID, "42")
	return req
}

// TestGuardrailWarnLetsTheRequestThroughUnchangedAndRecordsWarned: a warn
// detector blocks nothing and masks nothing; the verdict has the outcome
// `warned` and the finding labels, and no value.
func TestGuardrailWarnLetsTheRequestThroughUnchangedAndRecordsWarned(t *testing.T) {
	router := &capturingRouter{}
	h := NewHandler(router, nil, nil,
		WithBudgetGate(allowingGate(), &fakeCostEstimator{}),
		WithGovernancePolicy(fixedPolicy{snapshotOf(t, personalDataRow("input", "warn"))}, nil, nil))
	resp, verdicts := serveLogged(t, h.Chat, personalDataRequest(t))
	router.mu.Lock()
	got := router.last
	router.mu.Unlock()
	if got == nil || got.Input[0].Content == nil || got.Input[0].Content.ContentStr == nil {
		t.Fatalf("no dispatched request: %d %s", resp.Code, resp.Body.String())
	}
	if want := "mail jane.doe@corp-mail.io card 4111 1111 1111 1111"; *got.Input[0].Content.ContentStr != want {
		t.Fatalf("the provider received %q; a warn must not change the request", *got.Input[0].Content.ContentStr)
	}
	if len(verdicts) != 1 {
		t.Fatalf("verdicts = %+v", verdicts)
	}
	v := verdicts[0]
	if v.Outcome != guardrail.OutcomeWarned || v.Action != guardrail.ActionWarn || v.DetectorType != guardrail.TypePersonalData ||
		strings.Join(v.Labels, ",") != "email,payment-card" || v.Findings != 2 {
		t.Fatalf("verdict = %+v", v)
	}
	if strings.Contains(strings.Join(v.Labels, ""), "corp-mail") || strings.Contains(strings.Join(v.Labels, ""), "4111") {
		t.Fatal("a verdict label carries the value")
	}
}

// TestGuardrailPersonalDataMaskReachesTheProviderRedacted: the same rows with
// mask send markers, and block refuses with the same typed error as a secret.
func TestGuardrailPersonalDataMaskReachesTheProviderRedacted(t *testing.T) {
	router := &capturingRouter{}
	h := NewHandler(router, nil, nil,
		WithBudgetGate(allowingGate(), &fakeCostEstimator{}),
		WithGovernancePolicy(fixedPolicy{snapshotOf(t, personalDataRow("input", "mask"))}, nil, nil))
	rec := httptest.NewRecorder()
	h.Chat(rec, personalDataRequest(t))
	router.mu.Lock()
	got := router.last
	router.mu.Unlock()
	if got == nil || got.Input[0].Content == nil || got.Input[0].Content.ContentStr == nil {
		t.Fatalf("no dispatched request: %d %s", rec.Code, rec.Body.String())
	}
	if want := "mail [REDACTED:email] card [REDACTED:payment-card]"; *got.Input[0].Content.ContentStr != want {
		t.Fatalf("the provider received %q, want %q", *got.Input[0].Content.ContentStr, want)
	}

	blocker := &trackingRouter{}
	hb := guardHandler(t, blocker, personalDataRow("input", "block"))
	resp, verdicts := serveLogged(t, hb.Chat, personalDataRequest(t))
	if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), guardrailCodeBlocked) {
		t.Fatalf("block = %d %s", resp.Code, resp.Body.String())
	}
	if blocker.called.Load() {
		t.Fatal("the provider was called for a blocked request")
	}
	if strings.Contains(resp.Body.String(), "jane.doe") || strings.Contains(resp.Body.String(), "4111") {
		t.Fatalf("the refusal carries the value: %s", resp.Body.String())
	}
	if len(verdicts) != 1 || verdicts[0].Outcome != guardrail.OutcomeBlocked {
		t.Fatalf("verdicts = %+v", verdicts)
	}
}
