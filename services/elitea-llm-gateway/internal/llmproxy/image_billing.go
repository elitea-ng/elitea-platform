package llmproxy

// image_billing.go — admission and billing of the three image routes
// (/images/generations, /images/edits, /images/variations), issue 1969.
//
// BILLING ORDER, applied to the images the provider actually RETURNED:
//
//  1. The provider reported token usage (gpt-image-*, gemini-*-image): the
//     token rates of the catalog pay it. Unchanged. Source "token".
//  2. Else the catalog holds a price per image for the request's model, size
//     and quality (gateway.gateway_model_image_prices, shared migration 0243):
//     returned x that price. Source "catalog_per_image".
//  3. Else the flat per-image ESTIMATE (perImageFallbackNano), recorded as
//     "fallback_estimate" on the request-log row, counted on
//     gateway_image_fallback_estimate_total and logged once per model. It is an
//     estimate, not a price: an operator adds a price to replace it. A call is
//     never billed zero because the catalog is silent.
//
// ADMISSION. Before the provider call the gateway estimates n x unit price
// (the catalog price for the requested size and quality, else the flat
// estimate) and refuses a request that does not fit in what is left of the
// project or member ceiling, in the existing budget error shape
// (budget_gate.go, estimateExceeds). The old check refused only a scope that
// was already over its ceiling, so one n=10 HD call could overshoot a nearly
// spent budget by ten images. A model that reports tokens is estimated with
// the same flat figure: there is no token count before the call, and the
// estimate is a floor, not a cap. The bill that settles is the real one.
//
// WHAT IS NEVER BILLED. A provider error, a refused call and an empty
// response bill nothing: billing runs only for a response with images in it.

import (
	"context"
	"expvar"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/cost"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/requestlog"
)

// MetricImageFallbackEstimate counts image requests billed on the flat
// per-image estimate.
const MetricImageFallbackEstimate = "gateway_image_fallback_estimate_total"

// MetricImageBudgetRefused counts image requests refused at admission because
// their estimate did not fit in the remaining budget.
const MetricImageBudgetRefused = "gateway_image_budget_refused_total"

var (
	imageFallbackEstimate = expvar.NewInt(MetricImageFallbackEstimate)
	imageBudgetRefused    = expvar.NewInt(MetricImageBudgetRefused)
)

// ImageMetricNames returns the names of this file's counters for the
// composition root's /metrics allowlist (an expvar variable that is not listed
// has no route, issue #465).
func ImageMetricNames() []string {
	return []string{MetricImageFallbackEstimate, MetricImageBudgetRefused}
}

// maxImageEstimateCount bounds n in the admission estimate. Providers accept
// at most 10 images; a larger n is refused by the provider and must not make
// the estimate overflow.
const maxImageEstimateCount = 100

// ImagePricer prices ONE image from the catalog. *cost.Calculator satisfies it.
// ok is false when the catalog has no usable price: the caller then uses the
// flagged estimate.
type ImagePricer interface {
	ImageUnitNano(ctx context.Context, provider, model, size, quality string) (int64, bool)
}

// imageSpec is what the request asks for.
type imageSpec struct {
	// n is the requested image count, at least 1.
	n       int64
	size    string
	quality string
}

// newImageSpec reads count, size and quality from a request's parameters and
// applies the documented provider defaults: DALL-E renders 1024x1024, and
// DALL-E 3 renders "standard" quality. "auto" means unspecified for every
// model. Other models keep an empty value, which matches any catalog row (the
// highest price wins, so the estimate never runs low).
func newImageSpec(model string, n *int, size, quality *string) imageSpec {
	spec := imageSpec{n: 1}
	if n != nil && *n > 1 {
		spec.n = int64(min(*n, maxImageEstimateCount))
	}
	if size != nil {
		spec.size = strings.ToLower(strings.TrimSpace(*size))
	}
	if quality != nil {
		spec.quality = strings.ToLower(strings.TrimSpace(*quality))
	}
	if spec.size == "auto" {
		spec.size = ""
	}
	if spec.quality == "auto" {
		spec.quality = ""
	}
	lower := strings.ToLower(model)
	if strings.Contains(lower, "dall-e") && spec.size == "" {
		spec.size = "1024x1024"
	}
	if strings.Contains(lower, "dall-e-3") && spec.quality == "" {
		spec.quality = "standard"
	}
	return spec
}

func imageSpecOfGeneration(req *schemas.BifrostImageGenerationRequest) imageSpec {
	if req == nil || req.Params == nil {
		return newImageSpec(modelOf(req), nil, nil, nil)
	}
	return newImageSpec(req.Model, req.Params.N, req.Params.Size, req.Params.Quality)
}

func modelOf(req *schemas.BifrostImageGenerationRequest) string {
	if req == nil {
		return ""
	}
	return req.Model
}

func imageSpecOfEdit(req *schemas.BifrostImageEditRequest) imageSpec {
	if req == nil || req.Params == nil {
		return newImageSpec("", nil, nil, nil)
	}
	return newImageSpec(req.Model, req.Params.N, req.Params.Size, req.Params.Quality)
}

func imageSpecOfVariation(req *schemas.BifrostImageVariationRequest) imageSpec {
	if req == nil || req.Params == nil {
		return newImageSpec("", nil, nil, nil)
	}
	return newImageSpec(req.Model, req.Params.N, req.Params.Size, nil)
}

// TokenPricer reports that the catalog sells a model by the token.
// *cost.Calculator satisfies it.
type TokenPricer interface {
	TokenPriced(ctx context.Context, provider, model string) bool
}

// imageQuote is the price of one image as the admission found it. Settle reuses
// it, so a second lookup at settle time cannot fail differently from the first
// and bill the cheaper estimate (looked is false when no admission ran).
type imageQuote struct {
	unit   int64
	priced bool
	looked bool
}

// typedNilCalc reports a nil *cost.Calculator held in the interface.
func typedNilCalc(calc CostEstimator) bool {
	c, isCalc := calc.(*cost.Calculator)
	return isCalc && c == nil
}

// catalogImageUnit asks the calculator behind bp for the price of one image,
// under its own deadline. A typed-nil calculator in the interface prices
// nothing.
func catalogImageUnit(ctx context.Context, bp *budgetPlane, provider, model string, spec imageSpec) (int64, bool) {
	if typedNilCalc(bp.calc) {
		return 0, false
	}
	pricer, ok := bp.calc.(ImagePricer)
	if !ok || pricer == nil {
		return 0, false
	}
	lookupCtx, cancel := context.WithTimeout(ctx, budgetGateTimeout)
	defer cancel()
	unit, ok := pricer.ImageUnitNano(lookupCtx, provider, model, spec.size, spec.quality)
	return unit, ok && unit > 0
}

// imageEstimate is the admission estimate of the request, in nano-USD, and the
// quote it was made from.
//
// Without a catalog price per image, a model the catalog sells by the TOKEN
// (gpt-image-*, gemini-*-image) is estimated at 0: its real cost is unknown
// before the call, and a flat guess would refuse a request that fits. For such
// a model admission keeps the pre-1969 rule: refuse only a scope that is
// already over its ceiling. Every other model without a price is estimated at
// the flat figure.
func imageEstimate(ctx context.Context, bp *budgetPlane, provider, model string, spec imageSpec) (int64, imageQuote) {
	unit, ok := catalogImageUnit(ctx, bp, provider, model, spec)
	quote := imageQuote{unit: unit, priced: ok, looked: true}
	if ok {
		return spec.n * unit, quote
	}
	if tp, isTP := bp.calc.(TokenPricer); isTP && !typedNilCalc(bp.calc) {
		lookupCtx, cancel := context.WithTimeout(ctx, budgetGateTimeout)
		defer cancel()
		if tp.TokenPriced(lookupCtx, provider, model) {
			return 0, quote
		}
	}
	return spec.n * perImageFallbackNano, quote
}

// checkImageBudget is checkBudget plus the estimate. It reports the request to
// the log as an image request first, so a refused request still has its row
// marked. The quote it returns is what settle bills from.
func (h *Handler) checkImageBudget(w http.ResponseWriter, ctx context.Context, provider, model string, spec imageSpec) (*imageQuote, bool) {
	requestlog.FromContext(ctx).SetImage(requestlog.ImageFields{Size: spec.size, Quality: spec.quality})
	quote := &imageQuote{}
	v := h.admissionVerdictEstimate(ctx, model, admissionArrival, func(bp *budgetPlane) int64 {
		est, q := imageEstimate(ctx, bp, provider, model, spec)
		*quote = q
		return est
	})
	if v.allow {
		return quote, true
	}
	if v.scope != "" {
		imageBudgetRefused.Add(1)
	}
	if v.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.FormatInt(int64(v.retryAfter/time.Second)+1, 10))
	}
	writeBudgetRefusal(w, v)
	return quote, false
}

// billImageResponse bills the images a successful image response carries, in
// the order the file comment gives, and records the outcome on the request
// log. It bills nothing for a nil or empty response.
func (h *Handler) billImageResponse(ctx context.Context, provider, model string, spec imageSpec, quote *imageQuote, resp *schemas.BifrostImageGenerationResponse) {
	if resp == nil {
		return
	}
	returned := int64(len(resp.Data))
	fields := requestlog.ImageFields{Count: returned, Size: spec.size, Quality: spec.quality}
	project, user := identityProjectFromCtx(ctx), identityUserFromCtx(ctx)

	in, out := usageFromImageResponse(resp)
	if in > 0 || out > 0 {
		fields.PricingSource = requestlog.PricingSourceToken
		requestlog.FromContext(ctx).SetImage(fields)
		h.updateUsage(ctx, provider, model, in, out, project, user)
		return
	}
	if returned <= 0 {
		requestlog.FromContext(ctx).SetImage(fields)
		return
	}

	// One snapshot prices and bills, so the calculator and the gate are the
	// pair that shipped together (budget_plane.go).
	bp := h.budget()
	unit, priced := int64(0), false
	if quote != nil && quote.looked {
		unit, priced = quote.unit, quote.priced
	} else {
		// No admission quote: look up under a context that outlives a client
		// disconnect, so a hang-up cannot turn an HD price into the estimate.
		billCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), billingCtxTimeout)
		unit, priced = catalogImageUnit(billCtx, bp, provider, model, spec)
		cancel()
	}
	if priced {
		fields.PricingSource = requestlog.PricingSourceCatalogPerImage
	} else {
		unit = perImageFallbackNano
		fields.PricingSource = requestlog.PricingSourceFallbackEstimate
		h.noteImageEstimate(provider, model)
	}
	requestlog.FromContext(ctx).SetImage(fields)
	h.updateUsageDirectWith(ctx, bp, project, user, provider, model, returned*unit)
}

// noteImageEstimate counts a request billed on the estimate and logs the model
// once, so an operator can add a price for it.
func (h *Handler) noteImageEstimate(provider, model string) {
	imageFallbackEstimate.Add(1)
	if _, seen := h.imageEstimateWarned.LoadOrStore(provider+"\x00"+model, struct{}{}); seen {
		return
	}
	h.logger.Warn("image billing: no per-image price in the catalog; billing the flat per-image estimate. "+
		"Add a price to gateway.gateway_model_image_prices for this model",
		"provider", provider, "model", model,
		"estimate_nano_per_image", perImageFallbackNano, "metric", MetricImageFallbackEstimate)
}
