package llmproxy

// coverage_boost_test.go — targeted tests to lift package coverage to ≥85%.
//
// Covers:
//   - identity.go:86  SignIdentityHeaders (0 %)
//   - models_static.go NewStaticModelResolver / Query / Next / Scan / Err / Close (0 %)
//   - budget_gate.go   providerModelFrom*Req extractors (66.7 %)
//   - budget_gate.go   usageFromImageResponse — Usage!=nil and Usage==nil branches (60 %)
//   - budget_gate.go   trySoftAlert — not-crossed / crossed / cooldown branches (38.5 %)
//   - budget_gate.go   updateUsageDirect (0 %)
//   - handler.go       ImageGeneration — image-count fallback billing path (72.2 %)

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/failmode"
)

// ── SignIdentityHeaders ───────────────────────────────────────────────────────

// TestSignIdentityHeaders_WithSecret verifies that SignIdentityHeaders sets all
// three identity headers and that the resulting signature is accepted by
// verifySignature (round-trip check).
func TestSignIdentityHeaders_WithSecret(t *testing.T) {
	secret := []byte("test-secret")
	h := http.Header{}
	SignIdentityHeaders(h, secret, "proj-42", "user-7", "tenant-1", "")

	if got := h.Get(headerProjectID); got != "proj-42" {
		t.Errorf("X-Elitea-Project-Id = %q, want proj-42", got)
	}
	if got := h.Get(headerUserID); got != "user-7" {
		t.Errorf("X-Elitea-User-Id = %q, want user-7", got)
	}
	if got := h.Get(headerTenantID); got != "tenant-1" {
		t.Errorf("X-Elitea-Tenant-Id = %q, want tenant-1", got)
	}
	if got := h.Get(headerSignature); got == "" {
		t.Error("X-Elitea-Identity-Signature must be set when secret is non-empty")
	}
	if !verifySignature(h, secret) {
		t.Error("verifySignature must accept headers written by SignIdentityHeaders")
	}
}

// TestSignIdentityHeaders_NoSecret verifies that an empty secret omits the
// signature header (and verifySignature still returns true for empty secrets).
func TestSignIdentityHeaders_NoSecret(t *testing.T) {
	h := http.Header{}
	SignIdentityHeaders(h, nil, "proj-1", "user-1", "tenant-1", "")

	if got := h.Get(headerSignature); got != "" {
		t.Errorf("signature header must be absent when secret is empty; got %q", got)
	}
	if !verifySignature(h, nil) {
		t.Error("verifySignature must return true for empty secret")
	}
}

// TestSignIdentityHeaders_EmptyFieldsOmitHeaders verifies that empty field
// values do not produce empty header values (the header is simply not set).
func TestSignIdentityHeaders_EmptyFieldsOmitHeaders(t *testing.T) {
	h := http.Header{}
	SignIdentityHeaders(h, nil, "", "", "", "")

	if got := h.Get(headerProjectID); got != "" {
		t.Errorf("empty projectID should not set header; got %q", got)
	}
	if got := h.Get(headerUserID); got != "" {
		t.Errorf("empty userID should not set header; got %q", got)
	}
	if got := h.Get(headerTenantID); got != "" {
		t.Errorf("empty tenantID should not set header; got %q", got)
	}
}

// TestSignIdentityHeaders_WrongSecretRejected verifies that headers signed with
// one secret are rejected when verified with a different secret.
func TestSignIdentityHeaders_WrongSecretRejected(t *testing.T) {
	h := http.Header{}
	SignIdentityHeaders(h, []byte("real"), "p", "u", "t", "")
	if verifySignature(h, []byte("fake")) {
		t.Error("headers signed with 'real' must not verify under 'fake'")
	}
}

// ── NewStaticModelResolver / staticModelRowsIter ──────────────────────────────

// TestNewStaticModelResolver_ReturnsConfiguredIDs verifies that a static resolver
// constructed with a known list of IDs returns exactly those IDs via the normal
// resolver path (List → query → rows).
func TestNewStaticModelResolver_ReturnsConfiguredIDs(t *testing.T) {
	ids := []string{"openai/gpt-4o", "anthropic/claude-3-5-sonnet", "dall-e-3"}
	r := NewStaticModelResolver(ids)

	got := r.List(context.Background(), "99")
	if len(got) != len(ids) {
		t.Fatalf("List() returned %d models, want %d", len(got), len(ids))
	}
	gotIDs := modelIDs(got)
	for i, want := range ids {
		if gotIDs[i] != want {
			t.Errorf("model[%d] = %q, want %q", i, gotIDs[i], want)
		}
	}
}

// TestNewStaticModelResolver_Empty verifies that an empty ids slice produces an
// empty model list.
func TestNewStaticModelResolver_Empty(t *testing.T) {
	r := NewStaticModelResolver(nil)
	got := r.List(context.Background(), "1")
	if len(got) != 0 {
		t.Errorf("empty ids: got %d models, want 0", len(got))
	}
}

// TestStaticModelRowsIter_ScanArityError verifies that Scan returns an error
// when called with the wrong number of destinations (exercises the guard).
func TestStaticModelRowsIter_ScanArityError(t *testing.T) {
	it := &staticModelRowsIter{rows: []staticModelRow{{title: "x"}}}
	it.Next()                   // advance to the first row
	err := it.Scan(new(string)) // only 1 dest — must error (expects 2)
	if err == nil {
		t.Error("Scan with 1 destination should return an error; got nil")
	}
}

// TestStaticModelRowsIter_NextAndScan exercises the full Next/Scan/Err/Close
// sequence directly so those methods register as covered.
func TestStaticModelRowsIter_NextAndScan(t *testing.T) {
	rows := []staticModelRow{{title: "m1"}, {title: "m2"}}
	it := &staticModelRowsIter{rows: rows}

	// First row
	if !it.Next() {
		t.Fatal("Next() on non-empty iterator should return true")
	}
	var title string
	var data []byte
	var shared bool
	if err := it.Scan(&title, &data, &shared); err != nil {
		t.Fatalf("Scan error: %v", err)
	}
	if title != "m1" {
		t.Errorf("title = %q, want m1", title)
	}
	if data != nil {
		t.Errorf("data = %v, want nil (title-only rows)", data)
	}

	// Second row
	if !it.Next() {
		t.Fatal("Next() should return true for second row")
	}
	if err := it.Scan(&title, &data, &shared); err != nil {
		t.Fatalf("Scan error: %v", err)
	}
	if title != "m2" {
		t.Errorf("title = %q, want m2", title)
	}

	// Exhausted
	if it.Next() {
		t.Error("Next() should return false after all rows consumed")
	}

	// Err and Close are no-ops — just verify they don't panic.
	if err := it.Err(); err != nil {
		t.Errorf("Err() = %v, want nil", err)
	}
	it.Close() // must not panic
}

// TestStaticModelQuerier_Query exercises the Query method of staticModelQuerier
// directly to ensure it is covered.
func TestStaticModelQuerier_Query(t *testing.T) {
	q := &staticModelQuerier{rows: []staticModelRow{{title: "a"}, {title: "b"}}}
	rows, err := q.Query(context.Background(), "ignored sql", "ignored args")
	if err != nil {
		t.Fatalf("Query error: %v", err)
	}
	var count int
	for rows.Next() {
		count++
		var title string
		var data []byte
		var shared bool
		if scanErr := rows.Scan(&title, &data, &shared); scanErr != nil {
			t.Fatalf("Scan error: %v", scanErr)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err() = %v", err)
	}
	rows.Close()
	if count != 2 {
		t.Errorf("row count = %d, want 2", count)
	}
}

// ── providerModelFrom*Req extractors ─────────────────────────────────────────

// TestProviderModelExtractors exercises all seven extractor functions with
// nil input and a populated request.  The nil case exercises the early-return
// branch; the populated case exercises the happy path.

func TestProviderModelFromChatReq(t *testing.T) {
	// nil input
	p, m := providerModelFromChatReq(nil)
	if p != "" || m != "" {
		t.Errorf("nil: got (%q, %q), want (\"\", \"\")", p, m)
	}
	// populated
	req := &schemas.BifrostChatRequest{Provider: "openai", Model: "gpt-4o"}
	p, m = providerModelFromChatReq(req)
	if p != "openai" || m != "gpt-4o" {
		t.Errorf("populated: got (%q, %q), want (openai, gpt-4o)", p, m)
	}
}

func TestProviderModelFromResponsesReq(t *testing.T) {
	p, m := providerModelFromResponsesReq(nil)
	if p != "" || m != "" {
		t.Errorf("nil: got (%q, %q)", p, m)
	}
	req := &schemas.BifrostResponsesRequest{Provider: "anthropic", Model: "claude-3-5-sonnet"}
	p, m = providerModelFromResponsesReq(req)
	if p != "anthropic" || m != "claude-3-5-sonnet" {
		t.Errorf("populated: got (%q, %q), want (anthropic, claude-3-5-sonnet)", p, m)
	}
}

func TestProviderModelFromTextReq(t *testing.T) {
	p, m := providerModelFromTextReq(nil)
	if p != "" || m != "" {
		t.Errorf("nil: got (%q, %q)", p, m)
	}
	req := &schemas.BifrostTextCompletionRequest{Provider: "openai", Model: "gpt-3.5-turbo-instruct"}
	p, m = providerModelFromTextReq(req)
	if p != "openai" || m != "gpt-3.5-turbo-instruct" {
		t.Errorf("populated: got (%q, %q)", p, m)
	}
}

func TestProviderModelFromEmbeddingReq(t *testing.T) {
	p, m := providerModelFromEmbeddingReq(nil)
	if p != "" || m != "" {
		t.Errorf("nil: got (%q, %q)", p, m)
	}
	req := &schemas.BifrostEmbeddingRequest{Provider: "openai", Model: "text-embedding-3-small"}
	p, m = providerModelFromEmbeddingReq(req)
	if p != "openai" || m != "text-embedding-3-small" {
		t.Errorf("populated: got (%q, %q)", p, m)
	}
}

func TestProviderModelFromImageGenReq(t *testing.T) {
	p, m := providerModelFromImageGenReq(nil)
	if p != "" || m != "" {
		t.Errorf("nil: got (%q, %q)", p, m)
	}
	req := &schemas.BifrostImageGenerationRequest{Provider: "openai", Model: "dall-e-3"}
	p, m = providerModelFromImageGenReq(req)
	if p != "openai" || m != "dall-e-3" {
		t.Errorf("populated: got (%q, %q)", p, m)
	}
}

func TestProviderModelFromImageEditReq(t *testing.T) {
	p, m := providerModelFromImageEditReq(nil)
	if p != "" || m != "" {
		t.Errorf("nil: got (%q, %q)", p, m)
	}
	req := &schemas.BifrostImageEditRequest{Provider: "openai", Model: "dall-e-2"}
	p, m = providerModelFromImageEditReq(req)
	if p != "openai" || m != "dall-e-2" {
		t.Errorf("populated: got (%q, %q)", p, m)
	}
}

func TestProviderModelFromImageVariationReq(t *testing.T) {
	p, m := providerModelFromImageVariationReq(nil)
	if p != "" || m != "" {
		t.Errorf("nil: got (%q, %q)", p, m)
	}
	req := &schemas.BifrostImageVariationRequest{Provider: "openai", Model: "dall-e-2"}
	p, m = providerModelFromImageVariationReq(req)
	if p != "openai" || m != "dall-e-2" {
		t.Errorf("populated: got (%q, %q)", p, m)
	}
}

// ── usageFromImageResponse ────────────────────────────────────────────────────

// TestUsageFromImageResponse_NilResponse verifies the nil guard.
func TestUsageFromImageResponse_NilResponse(t *testing.T) {
	in, out, count := usageFromImageResponse(nil)
	if in != 0 || out != 0 || count != 0 {
		t.Errorf("nil: got (%d,%d,%d), want (0,0,0)", in, out, count)
	}
}

// TestUsageFromImageResponse_WithUsage verifies that a response carrying a
// non-nil Usage returns token counts and imageCount=0 (token-based billing path).
func TestUsageFromImageResponse_WithUsage(t *testing.T) {
	resp := &schemas.BifrostImageGenerationResponse{
		Usage: &schemas.ImageUsage{InputTokens: 5, OutputTokens: 10},
	}
	in, out, count := usageFromImageResponse(resp)
	if in != 5 {
		t.Errorf("inputTokens = %d, want 5", in)
	}
	if out != 10 {
		t.Errorf("outputTokens = %d, want 10", out)
	}
	if count != 0 {
		t.Errorf("imageCount = %d, want 0 (token-based path)", count)
	}
}

// TestUsageFromImageResponse_UsageNilFallback verifies that a response WITHOUT
// Usage returns imageCount = len(Data) (per-image fallback billing path).
func TestUsageFromImageResponse_UsageNilFallback(t *testing.T) {
	resp := &schemas.BifrostImageGenerationResponse{
		Data: []schemas.ImageData{
			{URL: "https://example.com/img1.png"},
			{URL: "https://example.com/img2.png"},
			{URL: "https://example.com/img3.png"},
		},
	}
	in, out, count := usageFromImageResponse(resp)
	if in != 0 || out != 0 {
		t.Errorf("no-Usage path: tokens = (%d,%d), want (0,0)", in, out)
	}
	if count != 3 {
		t.Errorf("imageCount = %d, want 3", count)
	}
}

// ── trySoftAlert ─────────────────────────────────────────────────────────────
//
// trySoftAlert is called deep inside spawnBillingGoroutine after UpdateUsage
// succeeds.  The call chain is:
//
//   checkBudget (call 1) → Allow → handler calls provider →
//   updateUsage → spawnBillingGoroutine → [detached goroutine:
//   CheckBudget (call 2, pre-snapshot) → UpdateUsage → trySoftAlert →
//   CheckBudget (call 3, post-increment)]
//
// FIX #27 (github issue #15) moved call 2 off the request goroutine and into
// the detached billing goroutine — it used to run synchronously in
// updateUsage, adding up to billingCtxTimeout of client-visible latency.
//
// The tests below call h.trySoftAlert directly (package-level access) so the
// exact verdict returned by each CheckBudget call can be controlled without
// wiring through the full admission path.

// softAlertChecker implements BudgetChecker for trySoftAlert unit tests.
// CheckBudget returns checkResult; TryAlertCooldown returns alertFired/alertErr
// and records each invocation.
type softAlertChecker struct {
	checkResult failmode.Decision
	checkErr    error
	alertFired  bool
	alertErr    error
	alertCalled atomic.Int64
}

func (s *softAlertChecker) CheckBudget(_ context.Context, _ int, _, _ string, _, _ int64) (failmode.Decision, error) {
	return s.checkResult, s.checkErr
}

func (s *softAlertChecker) UpdateUsage(_ context.Context, _ int, _, _, _ string, _, _, _ int64, _ *failmode.UsageDimensions) error {
	return nil
}

func (s *softAlertChecker) TryAlertCooldown(_ context.Context, _, _ string, _ int64) (bool, error) {
	s.alertCalled.Add(1)
	return s.alertFired, s.alertErr
}

// TestTrySoftAlert_NotCrossed verifies that trySoftAlert does NOT call
// TryAlertCooldown when the post-increment CheckBudget still shows
// SoftThresholdNear=false (no threshold crossing detected).
func TestTrySoftAlert_NotCrossed(t *testing.T) {
	checker := &softAlertChecker{
		checkResult: failmode.Decision{
			Verdict:           failmode.Allow,
			State:             failmode.StateNATSHealthy,
			SoftThresholdNear: false, // still below 80% after increment
		},
	}
	h := NewHandler(nil, nil, nil, WithBudgetGate(checker, &fakeCostEstimator{}))

	// preDec: the pre-increment snapshot (SoftThresholdNear=false — was below).
	preDec := failmode.Decision{
		Verdict:           failmode.Allow,
		State:             failmode.StateNATSHealthy,
		SoftThresholdNear: false,
	}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	h.trySoftAlert(ctx, h.budget(), 1, budgetScopeProject, "1", 0, 500_000, preDec)

	if checker.alertCalled.Load() != 0 {
		t.Errorf("TryAlertCooldown called %d times, want 0 (threshold not crossed)", checker.alertCalled.Load())
	}
}

// TestTrySoftAlert_CrossedNATSHealthy verifies that trySoftAlert fires the
// alert when the NATS_HEALTHY path transitions from SoftThresholdNear=false
// (pre-increment) to SoftThresholdNear=true (post-increment).
func TestTrySoftAlert_CrossedNATSHealthy(t *testing.T) {
	checker := &softAlertChecker{
		// Post-increment state: just crossed 80%.
		checkResult: failmode.Decision{
			Verdict:           failmode.Allow,
			State:             failmode.StateNATSHealthy,
			SoftThresholdNear: true,
		},
		alertFired: true,
	}
	h := NewHandler(nil, nil, nil, WithBudgetGate(checker, &fakeCostEstimator{}))

	preDec := failmode.Decision{
		Verdict:           failmode.Allow,
		State:             failmode.StateNATSHealthy,
		SoftThresholdNear: false, // was below threshold before this billing increment
	}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	h.trySoftAlert(ctx, h.budget(), 2, budgetScopeProject, "2", 0, 500_000, preDec)

	if checker.alertCalled.Load() == 0 {
		t.Error("TryAlertCooldown must be called when NATS_HEALTHY crosses SoftThresholdNear")
	}
}

// TestTrySoftAlert_CrossedBlock402 verifies that trySoftAlert fires the alert
// when the post-increment CheckBudget returns Block402 (hard limit just crossed).
func TestTrySoftAlert_CrossedBlock402(t *testing.T) {
	checker := &softAlertChecker{
		checkResult: failmode.Decision{
			Verdict: failmode.Block402,
			State:   failmode.StateNATSHealthy,
		},
		alertFired: true,
	}
	h := NewHandler(nil, nil, nil, WithBudgetGate(checker, &fakeCostEstimator{}))

	preDec := failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	h.trySoftAlert(ctx, h.budget(), 3, budgetScopeProject, "3", 0, 500_000, preDec)

	if checker.alertCalled.Load() == 0 {
		t.Error("TryAlertCooldown must be called when post-increment Block402 is detected")
	}
}

// TestTrySoftAlert_CrossedDownPGFreshNear verifies the degraded-path branch:
// StateDownPGFreshNear means the per-replica near-cap was entered.
func TestTrySoftAlert_CrossedDownPGFreshNear(t *testing.T) {
	checker := &softAlertChecker{
		checkResult: failmode.Decision{
			Verdict: failmode.Allow,
			State:   failmode.StateDownPGFreshNear,
		},
		alertFired: true,
	}
	h := NewHandler(nil, nil, nil, WithBudgetGate(checker, &fakeCostEstimator{}))

	preDec := failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	h.trySoftAlert(ctx, h.budget(), 4, budgetScopeProject, "4", 0, 500_000, preDec)

	if checker.alertCalled.Load() == 0 {
		t.Error("TryAlertCooldown must be called for StateDownPGFreshNear")
	}
}

// TestTrySoftAlert_CooldownSuppresses verifies that when TryAlertCooldown
// returns fired=false (cooldown active) the function handles it gracefully
// (no panic; TryAlertCooldown IS still called — the cooldown check itself fires).
func TestTrySoftAlert_CooldownSuppresses(t *testing.T) {
	checker := &softAlertChecker{
		checkResult: failmode.Decision{
			Verdict:           failmode.Allow,
			State:             failmode.StateNATSHealthy,
			SoftThresholdNear: true,
		},
		alertFired: false, // cooldown window active — suppress
	}
	h := NewHandler(nil, nil, nil, WithBudgetGate(checker, &fakeCostEstimator{}))

	preDec := failmode.Decision{
		Verdict:           failmode.Allow,
		State:             failmode.StateNATSHealthy,
		SoftThresholdNear: false,
	}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	h.trySoftAlert(ctx, h.budget(), 5, budgetScopeProject, "5", 0, 500_000, preDec)

	// TryAlertCooldown IS reached (crossing detected) even though it returns false.
	if checker.alertCalled.Load() == 0 {
		t.Error("TryAlertCooldown must be called even when cooldown suppresses the alert")
	}
}

// TestTrySoftAlert_PostCheckError verifies that an error from the post-increment
// CheckBudget inside trySoftAlert is handled gracefully (logged, no panic, no
// subsequent TryAlertCooldown call).
func TestTrySoftAlert_PostCheckError(t *testing.T) {
	checker := &softAlertChecker{
		checkResult: failmode.Decision{},
		checkErr:    fmt.Errorf("simulated post-increment CheckBudget error"),
	}
	h := NewHandler(nil, nil, nil, WithBudgetGate(checker, &fakeCostEstimator{}))

	preDec := failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

	// Must not panic; error is logged and the function returns early.
	h.trySoftAlert(ctx, h.budget(), 6, budgetScopeProject, "6", 0, 500_000, preDec)

	if checker.alertCalled.Load() != 0 {
		t.Error("TryAlertCooldown must not be called when post-increment CheckBudget errors")
	}
}

// TestTrySoftAlert_AlertCooldownError verifies that a TryAlertCooldown error
// is handled gracefully (logged, no panic).
func TestTrySoftAlert_AlertCooldownError(t *testing.T) {
	checker := &softAlertChecker{
		checkResult: failmode.Decision{
			Verdict:           failmode.Allow,
			State:             failmode.StateNATSHealthy,
			SoftThresholdNear: true, // crossing detected
		},
		alertFired: false,
		alertErr:   fmt.Errorf("simulated TryAlertCooldown error"),
	}
	h := NewHandler(nil, nil, nil, WithBudgetGate(checker, &fakeCostEstimator{}))

	preDec := failmode.Decision{
		Verdict:           failmode.Allow,
		State:             failmode.StateNATSHealthy,
		SoftThresholdNear: false,
	}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

	// Must not panic.
	h.trySoftAlert(ctx, h.budget(), 7, budgetScopeProject, "7", 0, 500_000, preDec)

	if checker.alertCalled.Load() == 0 {
		t.Error("TryAlertCooldown must be called even when it returns an error")
	}
}

// ── updateUsageDirect ─────────────────────────────────────────────────────────

// TestUpdateUsageDirect_BillsImageCost verifies that updateUsageDirect forwards
// a pre-computed nano cost to UpdateUsage (rather than through the cost
// calculator). This exercises the path taken for image responses with no Usage.
func TestUpdateUsageDirect_BillsImageCost(t *testing.T) {
	const wantCostNano = perImageFallbackNano * 2 // 2 images

	gate := &fakeBudgetChecker{
		checkVerdict: failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy},
		updated:      make(chan struct{}),
	}
	calc := &fakeCostEstimator{totalNano: 0} // token cost = 0 so direct path is taken

	h := NewHandler(nil, nil, nil, WithBudgetGate(gate, calc))

	// Call updateUsageDirect directly with a known cost.
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyVirtualKey, "42")
	h.updateUsageDirect(ctx, "42", "", "openai", "dall-e-3", wantCostNano)

	gate.waitForUpdate(t)
	if gate.updateCalls.Load() == 0 {
		t.Error("UpdateUsage was not called by updateUsageDirect")
	}
	if got := gate.getLastUpdateCostNano(); got != wantCostNano {
		t.Errorf("UpdateUsage costNano = %d, want %d", got, wantCostNano)
	}
	if got := gate.getLastUpdateProjectID(); got != 42 {
		t.Errorf("UpdateUsage projectID = %d, want 42", got)
	}
}

// TestUpdateUsageDirect_ZeroCostNoOp verifies that updateUsageDirect is a no-op
// when costNano <= 0.
func TestUpdateUsageDirect_ZeroCostNoOp(t *testing.T) {
	gate := &fakeBudgetChecker{
		checkVerdict: failmode.Decision{Verdict: failmode.Allow},
		updated:      make(chan struct{}),
	}
	calc := &fakeCostEstimator{totalNano: 0}
	h := NewHandler(nil, nil, nil, WithBudgetGate(gate, calc))

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyVirtualKey, "42")
	h.updateUsageDirect(ctx, "42", "", "openai", "dall-e-3", 0)

	// Give a brief window; UpdateUsage must NOT be called.
	time.Sleep(30 * time.Millisecond)
	if gate.updateCalls.Load() != 0 {
		t.Error("UpdateUsage must not be called when costNano=0")
	}
}

// TestUpdateUsageDirect_NilGateNoOp verifies that updateUsageDirect is a no-op
// when budgetGate is nil.
func TestUpdateUsageDirect_NilGateNoOp(t *testing.T) {
	// Handler with no budget gate.
	h := NewHandler(nil, nil, nil)

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyVirtualKey, "42")
	// Must not panic.
	h.updateUsageDirect(ctx, "42", "", "openai", "dall-e-3", perImageFallbackNano)
}

// ── ImageGeneration handler — per-image fallback billing path ─────────────────

// TestImageGeneration_PerImageFallbackBilling verifies the full handler path for
// an image response that carries no Usage field: the handler must bill via
// updateUsageDirect with imgCount * perImageFallbackNano.
func TestImageGeneration_PerImageFallbackBilling(t *testing.T) {
	const numImages = 2
	wantCostNano := int64(numImages) * perImageFallbackNano

	gate := &fakeBudgetChecker{
		checkVerdict: failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy},
		updated:      make(chan struct{}),
	}
	calc := &fakeCostEstimator{totalNano: 0} // cost calculator not used for direct billing

	router := &trackingRouter{}
	router.imgResp = &schemas.BifrostImageGenerationResponse{
		Data: []schemas.ImageData{
			{URL: "https://example.com/img1.png"},
			{URL: "https://example.com/img2.png"},
		},
		// No Usage field → fallback billing path
	}

	h := NewHandler(router, nil, nil, WithBudgetGate(gate, calc))

	body := `{"model":"openai/dall-e-3","prompt":"a cat"}`
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/images/generations", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerProjectID, "42")

	rec := httptest.NewRecorder()
	h.route().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	gate.waitForUpdate(t)
	if gate.updateCalls.Load() == 0 {
		t.Error("UpdateUsage not called for image fallback billing")
	}
	if got := gate.getLastUpdateCostNano(); got != wantCostNano {
		t.Errorf("UpdateUsage costNano = %d, want %d (%d images × %d nano)", got, wantCostNano, numImages, perImageFallbackNano)
	}
}

// TestImageGeneration_TokenBillingPath verifies that when the image response
// carries a non-nil Usage the normal token billing path (updateUsage via
// costCalc) is taken rather than the per-image fallback path.
func TestImageGeneration_TokenBillingPath(t *testing.T) {
	const wantCostNano = int64(750_000)

	gate := &fakeBudgetChecker{
		checkVerdict: failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy},
		updated:      make(chan struct{}),
	}
	calc := &fakeCostEstimator{totalNano: wantCostNano}

	router := &trackingRouter{}
	router.imgResp = &schemas.BifrostImageGenerationResponse{
		Data: []schemas.ImageData{{URL: "https://example.com/img.png"}},
		Usage: &schemas.ImageUsage{
			InputTokens:  10,
			OutputTokens: 5,
		},
	}

	h := NewHandler(router, nil, nil, WithBudgetGate(gate, calc))

	body := `{"model":"openai/gpt-image-1","prompt":"a dog"}`
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/images/generations", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerProjectID, "55")

	rec := httptest.NewRecorder()
	h.route().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	gate.waitForUpdate(t)
	if gate.updateCalls.Load() == 0 {
		t.Error("UpdateUsage not called for token-based image billing")
	}
	if got := gate.getLastUpdateCostNano(); got != wantCostNano {
		t.Errorf("UpdateUsage costNano = %d, want %d", got, wantCostNano)
	}
}

// TestImageGeneration_Block402_ProviderNotCalled verifies that the budget gate
// blocks image generation and the image provider is not called.
func TestImageGeneration_Block402_ProviderNotCalled(t *testing.T) {
	gate := &fakeBudgetChecker{
		checkVerdict: failmode.Decision{Verdict: failmode.Block402, State: failmode.StateNATSHealthy},
		updated:      make(chan struct{}),
	}
	router := &trackingRouter{}
	router.imgResp = &schemas.BifrostImageGenerationResponse{
		Data: []schemas.ImageData{{URL: "should-not-be-called"}},
	}

	h := NewHandler(router, nil, nil, WithBudgetGate(gate, &fakeCostEstimator{}))

	body := `{"model":"openai/dall-e-3","prompt":"a cat"}`
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/images/generations", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerProjectID, "77")

	rec := httptest.NewRecorder()
	h.route().ServeHTTP(rec, req)

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402 (image gen must enforce budget gate)", rec.Code)
	}
	if router.called.Load() {
		t.Error("image provider was called despite Block402 verdict")
	}
}
