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
// DALL-E 3 renders "standard" quality. Other models keep an empty value, which
// matches any catalog row (the highest price wins, so the estimate never runs
// low).
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
	lower := strings.ToLower(model)
	if strings.Contains(lower, "dall-e") && (spec.size == "" || spec.size == "auto") {
		spec.size = "1024x1024"
	}
	if strings.Contains(lower, "dall-e-3") && (spec.quality == "" || spec.quality == "auto") {
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

// catalogImageUnit asks the calculator behind bp for the price of one image.
func catalogImageUnit(ctx context.Context, bp *budgetPlane, provider, model string, spec imageSpec) (int64, bool) {
	pricer, ok := bp.calc.(ImagePricer)
	if !ok || pricer == nil {
		return 0, false
	}
	unit, ok := pricer.ImageUnitNano(ctx, provider, model, spec.size, spec.quality)
	return unit, ok && unit > 0
}

// imageEstimate is the admission estimate of the request, in nano-USD.
func imageEstimate(ctx context.Context, bp *budgetPlane, provider, model string, spec imageSpec) int64 {
	unit, ok := catalogImageUnit(ctx, bp, provider, model, spec)
	if !ok {
		unit = perImageFallbackNano
	}
	return spec.n * unit
}

// checkImageBudget is checkBudget plus the estimate. It reports the request to
// the log as an image request first, so a refused request still has its row
// marked.
func (h *Handler) checkImageBudget(w http.ResponseWriter, ctx context.Context, provider, model string, spec imageSpec) bool {
	requestlog.FromContext(ctx).SetImage(requestlog.ImageFields{Size: spec.size, Quality: spec.quality})
	v := h.admissionVerdictEstimate(ctx, model, admissionArrival, func(bp *budgetPlane) int64 {
		return imageEstimate(ctx, bp, provider, model, spec)
	})
	if v.allow {
		return true
	}
	if v.scope != "" {
		imageBudgetRefused.Add(1)
	}
	if v.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.FormatInt(int64(v.retryAfter/time.Second)+1, 10))
	}
	writeBudgetRefusal(w, v)
	return false
}

// billImageResponse bills the images a successful image response carries, in
// the order the file comment gives, and records the outcome on the request
// log. It bills nothing for a nil or empty response.
func (h *Handler) billImageResponse(ctx context.Context, provider, model string, spec imageSpec, resp *schemas.BifrostImageGenerationResponse) {
	if resp == nil {
		return
	}
	returned := int64(len(resp.Data))
	fields := requestlog.ImageFields{Count: returned, Size: spec.size, Quality: spec.quality}
	project, user := identityProjectFromCtx(ctx), identityUserFromCtx(ctx)

	in, out, _ := usageFromImageResponse(resp)
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
	unit, priced := catalogImageUnit(ctx, bp, provider, model, spec)
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
