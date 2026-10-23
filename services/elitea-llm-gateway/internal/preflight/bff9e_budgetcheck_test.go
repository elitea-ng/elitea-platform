// Package preflight — BFF.9e hermetic test: budget hard-block and soft-alert gate.
//
// Proves two enforcement properties of the GovernanceStore + handler layer:
//
//  1. hard-block: a request to an over-budget project receives HTTP 402 with
//     error.type=budget_exceeded and error.code=insufficient_quota; the mock
//     LLM router is never invoked (gate fires at pre-admission).
//
//  2. soft-alert: when a billing increment causes the running spend to cross the
//     80% soft-alert threshold, the soft-alert path fires (observable via the
//     FakeNATS delta / counter — see note below on observability).
//
//  3. under-budget (control): a request to a project well below its limit
//     returns HTTP 200 and the router IS called.
//
// Observability note for subtest 2:
// FakeNATS.TryAlertCooldown always returns (fired=true, nil) — it does not
// record that it was called. The proxy observable used here is that after the
// 80% crossing, UpdateUsage runs to completion: the NATS counter is incremented
// AND a write-behind delta is published (DeltaCount increases). This proves the
// full updateUsage→trySoftAlert chain was executed. In a production scenario the
// alert is additionally visible as a slog.Warn entry; in this hermetic test the
// delta count is the observable.
//
// To make the 80% threshold crossable with a single request, this test uses a
// small hard limit expressed in nano-USD (not the full NanoUSD = 1e9 scale). The
// unit is nano-USD throughout; the choice of a small limit keeps the arithmetic
// trivial and avoids needing enormous MockRouter token counts.
//
// All calls are in-process (httptest.NewRecorder); no live NATS, Postgres, or
// external provider is required.
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

// bff9eErrorBody is the OpenAI-shaped error envelope we assert against.
type bff9eErrorBody struct {
	Error struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// TestBFF9E_BudgetHardBlockAndSoftAlert is the hermetic BFF.9e pre-flight gate.
func TestBFF9E_BudgetHardBlockAndSoftAlert(t *testing.T) {
	t.Parallel()

	secret := []byte("bff9e-budget-secret")

	const (
		userID   = "user-bff9e"
		tenantID = "tenant-bff9e"
	)

	// ── subtest 1: hard-block ─────────────────────────────────────────────────
	//
	// Seed a project at exactly its hard limit → over-budget → Block402.
	// Assert:
	//   (a) HTTP 402.
	//   (b) error.type == "budget_exceeded".
	//   (c) error.code == "insufficient_quota".
	//   (d) MockRouter was NOT called (gate blocks at admission).
	t.Run("hard-block", func(t *testing.T) {
		t.Parallel()

		const (
			projectID     = 9001
			projectIDStr  = "9001"
			hardLimitNano = int64(100) * failmode.NanoUSD // 100 USD
			spentNano     = int64(100) * failmode.NanoUSD // 100 USD spent — exactly at limit → Block402
		)

		router := preflight.NewMockRouter(preflight.MockRouterConfig{
			InputTokens:  100,
			OutputTokens: 50,
		})
		gov, _, _ := preflight.NewSeededGovernance(t, projectID, hardLimitNano, spentNano)
		handler := preflight.MountedHandler(t, router, gov, secret)

		body := `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"budget test"}]}`
		req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		preflight.SignRequest(req, secret, projectIDStr, userID, tenantID)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		// (a) HTTP 402.
		if rec.Code != http.StatusPaymentRequired {
			t.Fatalf("hard-block: status = %d, want 402 (budget_exceeded)\nbody: %s",
				rec.Code, rec.Body.String())
		}

		// (b)+(c) Error body shape.
		var errBody bff9eErrorBody
		if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
			t.Fatalf("hard-block: decode error body: %v\nraw: %s", err, rec.Body.String())
		}
		if errBody.Error.Type != "budget_exceeded" {
			t.Errorf("hard-block: error.type = %q, want \"budget_exceeded\"", errBody.Error.Type)
		}
		if errBody.Error.Code != "insufficient_quota" {
			t.Errorf("hard-block: error.code = %q, want \"insufficient_quota\"", errBody.Error.Code)
		}

		// (d) Router must NOT have been called.
		if router.Called() {
			t.Error("hard-block: MockRouter.Called() = true — LLM provider was invoked despite over-budget verdict (gate must block at admission)")
		}

		t.Logf("hard-block: HTTP 402 with type=%q code=%q; router not called — PASS",
			errBody.Error.Type, errBody.Error.Code)
	})

	// ── subtest 2: soft-alert ─────────────────────────────────────────────────
	//
	// Design:
	//   hard limit = 1,000,000 nano-USD  (a small but valid limit)
	//   spent      =   790,000 nano-USD  (79% of limit — just under the 80% threshold)
	//   soft alert pct = 80
	//
	// MockRouter uses InputTokens=100, OutputTokens=50 and the request uses
	// openai/gpt-4o ($2.50/$10.00 per 1M tokens). The gateway cost calculator
	// bills:
	//   input:  100 tok × 2,500,000,000 nano / 1,000,000 = 250,000 nano-USD
	//   output:  50 tok × 10,000,000,000 nano / 1,000,000 = 500,000 nano-USD
	//   total:  750,000 nano-USD
	//
	// Post-billing counter = 790,000 + 750,000 = 1,540,000 > 800,000 (80% of 1M).
	//
	// Assert:
	//   (a) HTTP 200 (request is under limit at admission time).
	//   (b) Router WAS called.
	//   (c) After completion, DeltaCount() > 0 (UpdateUsage ran through the full
	//       updateUsage→trySoftAlert chain, including TryAlertCooldown).
	//   (d) The NATS counter total for the project's subject was incremented
	//       above the soft-alert threshold (counter > 800,000 nano-USD).
	t.Run("soft-alert", func(t *testing.T) {
		t.Parallel()

		const (
			projectID    = 9002
			projectIDStr = "9002"

			// A small limit in nano-USD (NOT multiplied by failmode.NanoUSD) so one
			// request's billing can cross the 80% threshold.
			hardLimitNano = int64(1_000_000) // 1 milli-USD in nano-USD
			spentNano     = int64(790_000)   // 79% of 1M nano-USD

			softAlertPct = 80
		)

		router := preflight.NewMockRouter(preflight.MockRouterConfig{
			InputTokens:  100,
			OutputTokens: 50,
		})
		gov, nc, _ := preflight.NewSeededGovernance(
			t, projectID, hardLimitNano, spentNano,
			preflight.WithSoftAlertPct(softAlertPct),
		)
		// Wire the FakeNATS as the alert-event publisher so the spec §8.3
		// observable — "a soft-alert is recorded on gateway.events.*" — is
		// asserted directly (assertion (e) below).
		handler := preflight.MountedHandler(t, router, gov, secret,
			llmproxy.WithAlertEventPublisher(nc),
		)

		body := `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"soft alert test"}]}`
		req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		preflight.SignRequest(req, secret, projectIDStr, userID, tenantID)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		// (a) HTTP 200 — project was under limit at admission.
		if rec.Code != http.StatusOK {
			t.Fatalf("soft-alert: status = %d, want 200 (project is under hard limit at admission)\nbody: %s",
				rec.Code, rec.Body.String())
		}

		// (b) Router was called.
		if !router.Called() {
			t.Error("soft-alert: MockRouter.Called() = false — provider was not invoked for under-budget request")
		}

		// UpdateUsage is called asynchronously via a goroutine in the handler after
		// the response completes. Wait for the first write-behind delta to be
		// published before making assertions about billing correctness.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && nc.DeltaCount() == 0 {
			time.Sleep(5 * time.Millisecond)
		}

		// (c) At least one delta published — proves UpdateUsage ran through the
		//     full chain: IncrBudgetIdempotent incremented the counter AND
		//     PublishDelta wrote a write-behind record. Both must succeed for the
		//     soft-alert path (trySoftAlert) to have been reachable at all.
		if nc.DeltaCount() == 0 {
			t.Error("soft-alert: FakeNATS.DeltaCount() = 0 — UpdateUsage did not run; soft-alert chain was not triggered")
		}

		// (c2) Verify the NATS counter subject for this project was incremented
		//      by a positive amount. GetTotal is mutex-safe (unlike reading
		//      Deltas directly). We wait until DeltaCount() > 0 above, which
		//      implies IncrBudgetIdempotent already ran and committed the delta to
		//      Totals. If TryAlertCooldown was called (the observable we care about)
		//      it happens AFTER UpdateUsage completes, so the counter increment
		//      is a strict lower-bound prerequisite for TryAlertCooldown being
		//      reachable. A counter that equals the initial seed (no increment)
		//      proves UpdateUsage was NOT called, directly falsifying the claim.
		now := time.Now().UTC()
		y, m, _ := now.Date()
		periodStart := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC).Unix()
		subjectKey := "gateway.budget.counter.project." + projectIDStr + "." + itoa64(periodStart)

		// Expected: counter has grown beyond the seed (spentNano) by at least 1.
		currentTotal := nc.GetTotal(subjectKey)
		if currentTotal <= spentNano {
			t.Errorf("soft-alert: NATS counter at subject %q = %d nano-USD after billing, "+
				"want > %d (the seeded spend) — IncrBudgetIdempotent was not called or had no effect",
				subjectKey, currentTotal, spentNano)
		}

		// (d) The post-billing counter must have crossed the soft-alert threshold
		//     (80%% of hard limit = 800,000 nano-USD). This is the condition that
		//     gates TryAlertCooldown being called: if the counter has not crossed
		//     the threshold, trySoftAlert never reaches TryAlertCooldown at all.
		softThreshold := hardLimitNano * int64(softAlertPct) / 100 // 800,000 nano-USD
		if currentTotal <= softThreshold {
			t.Errorf("soft-alert: NATS counter at subject %q = %d nano-USD, "+
				"want > %d (80%% of hard limit %d); soft-alert threshold was not crossed",
				subjectKey, currentTotal, softThreshold, hardLimitNano)
		} else {
			t.Logf("soft-alert: NATS counter crossed 80%% threshold: %d > %d nano-USD — PASS",
				currentTotal, softThreshold)
		}

		// (e) The budget.soft_alert event was published on the gateway.events.*
		//     surface (spec §8.3 / BFF.9e observable). FakeNATS.TryAlertCooldown
		//     always grants the claim, so once the threshold crossing is proven
		//     ((c2)+(d) above) the fired path MUST have emitted exactly one
		//     event for this project, carrying the natsbus envelope shape.
		alertDeadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(alertDeadline) && nc.AlertEventCount() == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		if nc.AlertEventCount() == 0 {
			t.Fatal("soft-alert: no budget.soft_alert event published on gateway.events.* — the alert is not externally observable")
		}
		evt := nc.AlertEvents[0]
		if evt.ProjectID != projectIDStr {
			t.Errorf("soft-alert event projectID = %q, want %q", evt.ProjectID, projectIDStr)
		}
		var env struct {
			Type   string `json:"type"`
			Source string `json:"source"`
		}
		if err := json.Unmarshal(evt.Event, &env); err != nil {
			t.Fatalf("soft-alert event envelope decode: %v\nraw: %s", err, evt.Event)
		}
		if env.Type != "budget.soft_alert" {
			t.Errorf("soft-alert event type = %q, want budget.soft_alert", env.Type)
		}
		if env.Source != "elitea-llm-gateway" {
			t.Errorf("soft-alert event source = %q, want elitea-llm-gateway", env.Source)
		}

		t.Logf("soft-alert: HTTP 200, router called, DeltaCount=%d, threshold crossed, event published — PASS",
			nc.DeltaCount())
	})

	// ── subtest 3: under-budget allowed ──────────────────────────────────────
	//
	// Control case: project well below its limit (10 USD of 100 USD) → 200.
	t.Run("under-budget-allowed", func(t *testing.T) {
		t.Parallel()

		const (
			projectID     = 9003
			projectIDStr  = "9003"
			hardLimitNano = int64(100) * failmode.NanoUSD // 100 USD
			spentNano     = int64(10) * failmode.NanoUSD  // 10 USD — well under limit
		)

		router := preflight.NewMockRouter(preflight.MockRouterConfig{
			InputTokens:  100,
			OutputTokens: 50,
		})
		gov, _, _ := preflight.NewSeededGovernance(t, projectID, hardLimitNano, spentNano)
		handler := preflight.MountedHandler(t, router, gov, secret)

		body := `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"allowed test"}]}`
		req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		preflight.SignRequest(req, secret, projectIDStr, userID, tenantID)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		// HTTP 200.
		if rec.Code != http.StatusOK {
			t.Fatalf("under-budget-allowed: status = %d, want 200\nbody: %s",
				rec.Code, rec.Body.String())
		}

		// Router was called.
		if !router.Called() {
			t.Error("under-budget-allowed: MockRouter.Called() = false — provider not invoked despite under-budget allow")
		}

		t.Log("under-budget-allowed: HTTP 200, router called — PASS")
	})
}

// itoa64 converts int64 to string without importing strconv in the test file.
// Used to reconstruct the NATS budget subject key.
func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	buf := [20]byte{}
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
