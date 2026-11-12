package llmproxy

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/cost"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/requestlog"
)

// tokenCalc is a calculator whose catalog sells the model by the token.
type tokenCalc struct {
	imageCalc
	token bool
}

func (c *tokenCalc) TokenPriced(context.Context, string, string) bool { return c.token }

// flakyCalc prices the first lookup and fails every later one, like a database
// that blips between admission and settle.
type flakyCalc struct {
	imageCalc
	calls atomic.Int64
}

func (c *flakyCalc) ImageUnitNano(ctx context.Context, p, m, s, q string) (int64, bool) {
	if c.calls.Add(1) > 1 {
		return 0, false
	}
	return c.imageCalc.ImageUnitNano(ctx, p, m, s, q)
}

func TestImageAdmission_TokenPricedModelWithoutAnImagePriceKeepsTheOldRule(t *testing.T) {
	const limit = int64(1_000_000_000)
	// Only 1 nano is left. A flat 0.04 USD estimate would refuse; the old rule
	// (refuse only a scope already over its ceiling) admits.
	gate := newImageGate(healthyAllow(limit-1, limit))
	router := &imageRouter{}
	router.imgResp = imageResponse(1)
	h := NewHandler(router, nil, nil, WithBudgetGate(gate, &tokenCalc{token: true}))
	rec, _ := serveImage(t, h, `{"model":"openai/gpt-image-1","prompt":"p"}`, nil)
	if rec.Code != http.StatusOK || !router.imageCalled() {
		t.Fatalf("a token-priced image model must not be refused on the flat estimate: %d", rec.Code)
	}
	if got := gate.firstEstimate(budgetScopeProject); got != 0 {
		t.Fatalf("estimate = %d, want 0 for a token-priced model", got)
	}

	// A model the catalog does not sell by the token keeps the flat estimate.
	gate = newImageGate(healthyAllow(limit-1, limit))
	router = &imageRouter{}
	router.imgResp = imageResponse(1)
	h = NewHandler(router, nil, nil, WithBudgetGate(gate, &tokenCalc{token: false}))
	rec, _ = serveImage(t, h, `{"model":"openai/some-new-image-model","prompt":"p"}`, nil)
	assertBudgetRefusal(t, rec, budgetCodeProject, budgetScopeFieldProject)
}

func TestImageBilling_SettleUsesTheAdmissionQuote(t *testing.T) {
	gate := newImageGate(healthyAllow(0, 0))
	calc := &flakyCalc{imageCalc: imageCalc{unit: 120_000_000, priced: true}}
	router := &imageRouter{}
	router.imgResp = imageResponse(1)
	h := NewHandler(router, nil, nil, WithBudgetGate(gate, calc))

	_, row := serveImage(t, h, `{"model":"openai/dall-e-3","prompt":"p","size":"1792x1024","quality":"hd"}`, nil)
	gate.waitForUpdate(t)
	if got := gate.getLastUpdateCostNano(); got != 120_000_000 {
		t.Fatalf("billed %d, want the admission price 120000000 and not the cheaper estimate", got)
	}
	if calc.calls.Load() != 1 {
		t.Fatalf("%d lookups, want 1 (settle must not look up again)", calc.calls.Load())
	}
	if row.Image == nil || row.Image.PricingSource != requestlog.PricingSourceCatalogPerImage {
		t.Fatalf("logged %+v", row.Image)
	}
}

func TestImageBilling_ATypedNilCalculatorPricesNothing(t *testing.T) {
	var calc *cost.Calculator
	gate := newImageGate(healthyAllow(0, 0))
	router := &imageRouter{}
	router.imgResp = imageResponse(1)
	h := NewHandler(router, nil, nil, WithBudgetGate(gate, calc))

	_, row := serveImage(t, h, `{"model":"openai/dall-e-3","prompt":"p"}`, nil)
	gate.waitForUpdate(t)
	if got := gate.getLastUpdateCostNano(); got != perImageFallbackNano {
		t.Fatalf("billed %d, want the flat estimate", got)
	}
	if row.Image == nil || row.Image.PricingSource != requestlog.PricingSourceFallbackEstimate {
		t.Fatalf("logged %+v", row.Image)
	}
}

func TestNewImageSpec_AutoIsUnspecifiedForEveryModel(t *testing.T) {
	auto := "auto"
	got := newImageSpec("gpt-image-1", nil, &auto, &auto)
	if got.size != "" || got.quality != "" {
		t.Fatalf("auto on gpt-image-1 = %+v, want empty size and quality", got)
	}
}
