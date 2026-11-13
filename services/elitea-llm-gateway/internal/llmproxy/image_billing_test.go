package llmproxy

// Tests for the image billing order and the image admission estimate
// (issue 1969). They cover each pricing path, the admission refusal at the
// boundary, a response with fewer images than requested, and the calls that
// must bill nothing.

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

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/failmode"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/requestlog"
)

// imageGate answers CheckBudget per scope and records the estimate the
// admission passed.
type imageGate struct {
	fakeBudgetChecker
	mu       sync.Mutex
	estimate map[string][]int64
	decide   func(scope string, estimate int64) failmode.Decision
}

func (g *imageGate) CheckBudget(_ context.Context, _ int, scope, _ string, _, estimate int64) (failmode.Decision, error) {
	g.mu.Lock()
	if g.estimate == nil {
		g.estimate = map[string][]int64{}
	}
	g.estimate[scope] = append(g.estimate[scope], estimate)
	g.mu.Unlock()
	return g.decide(scope, estimate), nil
}

func (g *imageGate) firstEstimate(scope string) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.estimate[scope]) == 0 {
		return -1
	}
	return g.estimate[scope][0]
}

func healthyAllow(spent, limit int64) func(string, int64) failmode.Decision {
	return func(string, int64) failmode.Decision {
		return failmode.Decision{
			Verdict: failmode.Allow, State: failmode.StateNATSHealthy, Budgeted: limit > 0,
			SpentNano: spent, HardLimitNano: limit,
		}
	}
}

func newImageGate(decide func(string, int64) failmode.Decision) *imageGate {
	return &imageGate{fakeBudgetChecker: fakeBudgetChecker{updated: make(chan struct{})}, decide: decide}
}

// imageCalc is a calculator with a catalog per-image price.
type imageCalc struct {
	fakeCostEstimator
	unit   int64
	priced bool
	mu     sync.Mutex
	asked  [][3]string
}

func (c *imageCalc) ImageUnitNano(_ context.Context, provider, model, size, quality string) (int64, bool) {
	c.mu.Lock()
	c.asked = append(c.asked, [3]string{model, size, quality})
	c.mu.Unlock()
	return c.unit, c.priced
}

func imageResponse(urls int) *schemas.BifrostImageGenerationResponse {
	resp := &schemas.BifrostImageGenerationResponse{}
	for range urls {
		resp.Data = append(resp.Data, schemas.ImageData{URL: "https://example.com/i.png"})
	}
	return resp
}

// serveImage posts a generation request through the request-log middleware and
// returns the response and the logged row.
func serveImage(t *testing.T, h *Handler, body string, headers map[string]string) (*httptest.ResponseRecorder, requestlog.Record) {
	t.Helper()
	sink := &logCaptureSink{}
	recorder := requestlog.New(sink, discardLogger())
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/images/generations", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerProjectID, "42")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	requestlog.Middleware(recorder)(h.route()).ServeHTTP(rec, req)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	recorder.Stop(ctx)
	rows := sink.all()
	if len(rows) != 1 {
		t.Fatalf("logged rows = %d, want 1", len(rows))
	}
	return rec, rows[0]
}

func TestImageBilling_CatalogPerImageBillsTheReturnedCount(t *testing.T) {
	gate := newImageGate(healthyAllow(0, 0))
	calc := &imageCalc{unit: 120_000_000, priced: true}
	// Three images requested, two returned: the bill is for two.
	router := &imageRouter{}
	router.imgResp = imageResponse(2)
	h := NewHandler(router, nil, nil, WithBudgetGate(gate, calc))

	rec, row := serveImage(t, h, `{"model":"openai/dall-e-3","prompt":"p","n":3,"size":"1792x1024","quality":"hd"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	gate.waitForUpdate(t)
	if got := gate.getLastUpdateCostNano(); got != 240_000_000 {
		t.Fatalf("billed %d nano-USD, want 240000000 (2 returned x 0.12 USD)", got)
	}
	if row.Image == nil || row.Image.Count != 2 || row.Image.Size != "1792x1024" || row.Image.Quality != "hd" ||
		row.Image.PricingSource != requestlog.PricingSourceCatalogPerImage {
		t.Fatalf("logged image fields = %+v", row.Image)
	}
	// The admission estimated the REQUESTED count: 3 x 0.12 USD.
	if got := gate.firstEstimate(budgetScopeProject); got != 360_000_000 {
		t.Fatalf("admission estimate = %d, want 360000000", got)
	}
	calc.mu.Lock()
	defer calc.mu.Unlock()
	if calc.asked[0] != [3]string{"dall-e-3", "1792x1024", "hd"} {
		t.Fatalf("the catalog was asked %v", calc.asked[0])
	}
}

func TestImageBilling_NoCatalogPriceBillsTheFlaggedEstimateAndWarnsOncePerModel(t *testing.T) {
	before := imageFallbackEstimate.Value()
	gate := newImageGate(healthyAllow(0, 0))
	router := &imageRouter{}
	router.imgResp = imageResponse(2)
	h := NewHandler(router, nil, nil, WithBudgetGate(gate, &imageCalc{priced: false}))

	_, row := serveImage(t, h, `{"model":"openai/some-new-image-model","prompt":"p"}`, nil)
	gate.waitForUpdate(t)
	if got := gate.getLastUpdateCostNano(); got != 2*perImageFallbackNano {
		t.Fatalf("billed %d, want %d (2 images at the estimate)", got, 2*perImageFallbackNano)
	}
	if row.Image == nil || row.Image.PricingSource != requestlog.PricingSourceFallbackEstimate {
		t.Fatalf("a flat-estimate bill must be flagged: %+v", row.Image)
	}
	if imageFallbackEstimate.Value() != before+1 {
		t.Fatalf("counter moved by %d, want 1", imageFallbackEstimate.Value()-before)
	}
	// A second request for the same model counts again but warns no more.
	serveImage(t, h, `{"model":"openai/some-new-image-model","prompt":"p"}`, nil)
	if imageFallbackEstimate.Value() != before+2 {
		t.Fatal("the counter must move on every estimated bill")
	}
	count := 0
	h.imageEstimateWarned.Range(func(_, _ any) bool { count++; return true })
	if count != 1 {
		t.Fatalf("%d models warned, want 1", count)
	}
}

func TestImageBilling_TokenUsageKeepsTheTokenPath(t *testing.T) {
	gate := newImageGate(healthyAllow(0, 0))
	calc := &imageCalc{unit: 999, priced: true, fakeCostEstimator: fakeCostEstimator{totalNano: 750_000}}
	router := &imageRouter{}
	router.imgResp = imageResponse(1)
	router.imgResp.Usage = &schemas.ImageUsage{InputTokens: 10, OutputTokens: 5}
	h := NewHandler(router, nil, nil, WithBudgetGate(gate, calc))

	_, row := serveImage(t, h, `{"model":"openai/gpt-image-1","prompt":"p"}`, nil)
	gate.waitForUpdate(t)
	if got := gate.getLastUpdateCostNano(); got != 750_000 {
		t.Fatalf("billed %d, want the token cost 750000", got)
	}
	if row.Image == nil || row.Image.PricingSource != requestlog.PricingSourceToken || row.Image.Count != 1 {
		t.Fatalf("logged image fields = %+v", row.Image)
	}
}

func TestImageBilling_UsageBlockWithZeroTokensStillBills(t *testing.T) {
	gate := newImageGate(healthyAllow(0, 0))
	router := &imageRouter{}
	router.imgResp = imageResponse(1)
	router.imgResp.Usage = &schemas.ImageUsage{}
	h := NewHandler(router, nil, nil, WithBudgetGate(gate, &imageCalc{unit: 40_000_000, priced: true}))

	serveImage(t, h, `{"model":"openai/dall-e-3","prompt":"p"}`, nil)
	gate.waitForUpdate(t)
	if got := gate.getLastUpdateCostNano(); got != 40_000_000 {
		t.Fatalf("billed %d: a usage block with no tokens must not bill zero", got)
	}
}

func TestImageBilling_Dalle3DefaultsAreAskedOfTheCatalog(t *testing.T) {
	gate := newImageGate(healthyAllow(0, 0))
	calc := &imageCalc{unit: 40_000_000, priced: true}
	router := &imageRouter{}
	router.imgResp = imageResponse(1)
	h := NewHandler(router, nil, nil, WithBudgetGate(gate, calc))

	serveImage(t, h, `{"model":"openai/dall-e-3","prompt":"p"}`, nil)
	calc.mu.Lock()
	defer calc.mu.Unlock()
	if calc.asked[0] != [3]string{"dall-e-3", "1024x1024", "standard"} {
		t.Fatalf("a request with no size or quality asked the catalog for %v", calc.asked[0])
	}
}

func TestImageAdmission_RefusesAnEstimateThatDoesNotFitAtTheBoundary(t *testing.T) {
	const limit, estimate = int64(1_000_000_000), int64(120_000_000)
	for _, tc := range []struct {
		name   string
		spent  int64
		wantOK bool
	}{
		{"fits exactly", limit - estimate, true},
		{"one nano over", limit - estimate + 1, false},
		{"far over", limit - 1, false},
		{"plenty left", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := newImageGate(healthyAllow(tc.spent, limit))
			router := &imageRouter{}
			router.imgResp = imageResponse(1)
			h := NewHandler(router, nil, nil, WithBudgetGate(gate, &imageCalc{unit: estimate, priced: true}))

			rec, _ := serveImage(t, h, `{"model":"openai/dall-e-3","prompt":"p","size":"1792x1024","quality":"hd"}`, nil)
			if tc.wantOK {
				if rec.Code != http.StatusOK || !router.imageCalled() {
					t.Fatalf("status %d, provider called %v; want admitted", rec.Code, router.imageCalled())
				}
				return
			}
			if router.imageCalled() {
				t.Fatal("a refused request must not reach the provider")
			}
			assertBudgetRefusal(t, rec, budgetCodeProject, budgetScopeFieldProject)
			gate.waitNoUpdate(t)
		})
	}
}

func TestImageAdmission_RefusesAtTheMemberCeiling(t *testing.T) {
	const estimate = int64(40_000_000)
	gate := newImageGate(func(scope string, _ int64) failmode.Decision {
		if scope == budgetScopeUser {
			return failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy, Budgeted: true,
				SpentNano: 990_000_000, HardLimitNano: 1_000_000_000}
		}
		return failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy}
	})
	router := &imageRouter{}
	router.imgResp = imageResponse(1)
	h := NewHandler(router, nil, nil, WithBudgetGate(gate, &imageCalc{unit: estimate, priced: true}))

	rec, _ := serveImage(t, h, `{"model":"openai/dall-e-3","prompt":"p"}`, map[string]string{headerUserID: "7"})
	assertBudgetRefusal(t, rec, budgetCodeMember, budgetScopeFieldMember)
	if router.imageCalled() {
		t.Fatal("the provider was called after a member refusal")
	}
}

func TestImageAdmission_NoEstimateForAnUnbudgetedProject(t *testing.T) {
	gate := newImageGate(healthyAllow(0, 0))
	calc := &imageCalc{unit: 1_000_000_000_000, priced: true}
	router := &imageRouter{}
	router.imgResp = imageResponse(1)
	h := NewHandler(router, nil, nil, WithBudgetGate(gate, calc))

	rec, _ := serveImage(t, h, `{"model":"openai/dall-e-3","prompt":"p"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("a project with no ceiling must not be refused on an estimate: %d", rec.Code)
	}
}

func TestImageAdmission_AnUnpricedModelIsEstimatedAtTheFlatFigure(t *testing.T) {
	gate := newImageGate(healthyAllow(0, 0))
	router := &imageRouter{}
	router.imgResp = imageResponse(1)
	h := NewHandler(router, nil, nil, WithBudgetGate(gate, &imageCalc{priced: false}))

	serveImage(t, h, `{"model":"openai/some-new-image-model","prompt":"p","n":4}`, nil)
	if got := gate.firstEstimate(budgetScopeProject); got != 4*perImageFallbackNano {
		t.Fatalf("admission estimate = %d, want %d", got, 4*perImageFallbackNano)
	}
}

func TestImageBilling_AFailedOrEmptyProviderCallBillsNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		router *imageRouter
	}{
		{"provider error", func() *imageRouter {
			r := &imageRouter{}
			r.imgErr = &schemas.BifrostError{StatusCode: intPtr(500), Error: &schemas.ErrorField{Message: "boom"}}
			return r
		}()},
		{"empty response", func() *imageRouter {
			r := &imageRouter{}
			r.imgResp = imageResponse(0)
			return r
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := newImageGate(healthyAllow(0, 0))
			h := NewHandler(tc.router, nil, nil, WithBudgetGate(gate, &imageCalc{unit: 40_000_000, priced: true}))
			_, row := serveImage(t, h, `{"model":"openai/dall-e-3","prompt":"p"}`, nil)
			gate.waitNoUpdate(t)
			if row.Image == nil || row.Image.Count != 0 || row.Image.PricingSource != "" {
				t.Fatalf("a request that billed nothing logged %+v", row.Image)
			}
		})
	}
}

func TestImageBilling_EditAndVariationUseTheSamePricing(t *testing.T) {
	for _, path := range []string{"/llm/v1/images/edits", "/llm/v1/images/variations"} {
		t.Run(path, func(t *testing.T) {
			gate := newImageGate(healthyAllow(0, 0))
			calc := &imageCalc{unit: 20_000_000, priced: true}
			router := &imageRouter{}
			router.imgResp = imageResponse(2)
			h := NewHandler(router, nil, nil, WithBudgetGate(gate, calc))

			body, ct := multipartBody(t,
				map[string]string{"model": "openai/dall-e-2", "prompt": "p", "size": "256x256", "n": "2"},
				map[string][]byte{"image[]": []byte("DATA")},
			)
			req := httptest.NewRequest(http.MethodPost, path, body)
			req.Header.Set("Content-Type", ct)
			req.Header.Set(headerProjectID, "42")
			rec := httptest.NewRecorder()
			h.route().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			gate.waitForUpdate(t)
			if got := gate.getLastUpdateCostNano(); got != 40_000_000 {
				t.Fatalf("billed %d, want 40000000 (2 x 0.02 USD)", got)
			}
			calc.mu.Lock()
			defer calc.mu.Unlock()
			if calc.asked[0][1] != "256x256" {
				t.Fatalf("the catalog was asked %v", calc.asked[0])
			}
		})
	}
}

func TestEstimateExceeds(t *testing.T) {
	d := failmode.Decision{SpentNano: 900, HardLimitNano: 1000}
	for est, want := range map[int64]bool{0: false, -5: false, 100: false, 101: true} {
		if got := estimateExceeds(d, est); got != want {
			t.Errorf("estimateExceeds(spent 900, limit 1000, %d) = %v, want %v", est, got, want)
		}
	}
	// No ceiling reported (unlimited, or NATS down): the FSM decides.
	if estimateExceeds(failmode.Decision{SpentNano: 900}, 1<<60) {
		t.Error("a decision with no limit must never refuse on an estimate")
	}
	// A huge estimate must not overflow.
	if !estimateExceeds(failmode.Decision{SpentNano: 1, HardLimitNano: 10}, 1<<62) {
		t.Error("an estimate far above the limit must refuse")
	}
}

func TestNewImageSpec(t *testing.T) {
	n, huge := 3, 1_000_000
	size, q := " 1024X1792 ", " HD "
	got := newImageSpec("dall-e-3", &n, &size, &q)
	if got != (imageSpec{n: 3, size: "1024x1792", quality: "hd"}) {
		t.Fatalf("spec = %+v", got)
	}
	if got := newImageSpec("gpt-image-1", &huge, nil, nil); got.n != maxImageEstimateCount || got.size != "" || got.quality != "" {
		t.Fatalf("spec = %+v; a huge n must be bounded and a non-DALL-E model keeps empty size and quality", got)
	}
	auto := "auto"
	if got := newImageSpec("dall-e-3", nil, &auto, &auto); got.size != "1024x1024" || got.quality != "standard" {
		t.Fatalf("auto on DALL-E 3 = %+v", got)
	}
	zero := 0
	if got := newImageSpec("dall-e-2", &zero, nil, nil); got.n != 1 || got.quality != "" {
		t.Fatalf("spec = %+v", got)
	}
}

func assertBudgetRefusal(t *testing.T, rec *httptest.ResponseRecorder, wantCode, wantScope string) {
	t.Helper()
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402; body=%s", rec.Code, rec.Body.String())
	}
	var body openAIError
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not the OpenAI error shape: %v", err)
	}
	if body.Error.Type != budgetErrorType || body.Error.Code != wantCode || body.Error.Scope != wantScope {
		t.Fatalf("refusal = %+v, want type %s code %s scope %s", body.Error, budgetErrorType, wantCode, wantScope)
	}
}

func (f *fakeBudgetChecker) waitNoUpdate(t *testing.T) {
	t.Helper()
	time.Sleep(60 * time.Millisecond)
	if f.updateCalls.Load() != 0 {
		t.Fatalf("UpdateUsage was called %d times, want none", f.updateCalls.Load())
	}
}
