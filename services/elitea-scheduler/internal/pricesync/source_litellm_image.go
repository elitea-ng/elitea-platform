package pricesync

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Per-image prices (issue 1969, shared migration 0243).
//
// The sheet prices an image model in one of three ways, and the catalog's
// per-1M-token columns hold none of them:
//
//  1. A flat price per image: output_cost_per_image (Imagen, Bedrock, fal.ai,
//     stability, gemini image models).
//  2. A price per pixel: input_cost_per_pixel or output_cost_per_pixel
//     (the azure DALL-E 3 and gpt-image-1 entries, some fal.ai entries).
//  3. A token price (gpt-image-*, gemini-*-image). That one is NOT read here:
//     the provider reports the tokens and tokenRates already stores the rate.
//
// SIZE AND QUALITY LIVE IN THE KEY. The sheet does not give DALL-E a size
// field. It lists one entry per size and quality and encodes both in the key:
// "hd/1792-x-1024/dall-e-3", "azure/standard/1024-x-1024/dall-e-3",
// "low/1024-x-1536/gpt-image-1.5". splitImageKey takes the key apart.
//
// PER-PIXEL PRICES. A per-pixel price whose key names a size is converted here
// to the price of one image of that size, because the result is the number a
// price list prints and the gateway then needs no pixel arithmetic. A
// per-pixel price with no size is kept per pixel (scaled to per-1M by the
// Normalizer) and the gateway multiplies it by the pixels of the request.
//
// WHAT IS NOT READ, ON PURPOSE:
//   - A price of zero or less. A zero price would bill the image at nothing,
//     and a call billed at nothing is the defect this work removes. The model
//     stays without a price and the gateway bills the flagged estimate.
//   - The resolution-tier fields (output_cost_per_image_1K, _2K, _4K, _512 ...).
//     They need a size-to-tier map this sync does not have. The flat price of
//     the entry is stored for any size.
//   - An entry whose key carries a step count ("50-steps", "max-steps"). The
//     request does not name a step count, so the gateway cannot choose a row.
//   - input_cost_per_image of an entry that names no size and no quality. For
//     a gemini entry that is the price of an image SENT to the model, not of
//     one that comes back. When the key names a size or a quality
//     (gpt-image-1.5), the sheet uses that field for the image price itself.

var (
	imageSizeSegment  = regexp.MustCompile(`^(\d+)-x-(\d+)$`)
	imageStepsSegment = regexp.MustCompile(`^(\d+|max)-steps$`)
)

// imageQualitySegments are the quality words the sheet puts in a key.
var imageQualitySegments = map[string]bool{
	"standard": true, "hd": true, "low": true, "medium": true, "high": true, "auto": true,
}

// splitImageKey takes a sheet key apart into the bare model name, the size
// ("1792x1024") and the quality. ok is false for a key this sync does not
// price (a step-count variant, or a key with no model left).
//
// A leading segment equal to the entry's provider is dropped first
// ("azure/", "fal_ai/", "gemini/"). The segments after it that name a quality,
// a size or a step count are the variant; the rest is the model, which may
// itself hold slashes ("fal-ai/flux-pro/v1.1").
func splitImageKey(key, litellmProvider string) (model, size, quality string, ok bool) {
	parts := strings.Split(key, "/")
	if len(parts) > 1 && isProviderSegment(parts[0], litellmProvider) {
		parts = parts[1:]
	}
	i := 0
	for ; i < len(parts)-1; i++ {
		seg := parts[i]
		switch {
		case imageQualitySegments[seg]:
			quality = seg
		case imageSizeSegment.MatchString(seg):
			m := imageSizeSegment.FindStringSubmatch(seg)
			size = m[1] + "x" + m[2]
		case seg == "max-x-max":
			return "", "", "", false
		case imageStepsSegment.MatchString(seg):
			return "", "", "", false
		default:
			model = strings.Join(parts[i:], "/")
			return model, size, quality, model != ""
		}
	}
	model = parts[len(parts)-1]
	return model, size, quality, model != ""
}

// isProviderSegment reports whether the first key segment names the entry's
// provider, by the raw LiteLLM name or by the alias the catalog uses.
func isProviderSegment(segment, litellmProvider string) bool {
	if segment == litellmProvider {
		return true
	}
	return providerAliasKnown(segment) &&
		normalizeProvider(segment) == normalizeProvider(litellmProvider)
}

// providerAliasKnown reports whether the name is a LiteLLM provider this
// package maps (azure, vertex_ai, ...), so that "vertex_ai/imagen-4" is split
// while an unrelated first segment is left in the model name.
func providerAliasKnown(name string) bool {
	_, ok := providerAlias[strings.ToLower(name)]
	return ok
}

// parseSizePixels returns the pixel count of "WxH".
func parseSizePixels(size string) (float64, bool) {
	w, h, ok := strings.Cut(size, "x")
	if !ok {
		return 0, false
	}
	wi, err1 := strconv.Atoi(w)
	hi, err2 := strconv.Atoi(h)
	if err1 != nil || err2 != nil || wi <= 0 || hi <= 0 {
		return 0, false
	}
	return float64(wi) * float64(hi), true
}

// positive returns v when it is set and above zero.
func positive(v *float64) *float64 {
	if v == nil || *v <= 0 {
		return nil
	}
	return v
}

// roundUSD rounds a USD price to 10 decimals, the scale of the catalog column,
// so a product of floats does not store its binary noise.
func roundUSD(v float64) float64 {
	return math.Round(v*1e10) / 1e10
}

// imagePrice reads the per-image price of one image_generation entry.
func (e litellmEntry) imagePrice(size, quality string) (perImage, perPixel *float64) {
	if p := positive(e.OutputCostPerImage); p != nil {
		return p, nil
	}
	if size != "" || quality != "" {
		if p := positive(e.InputCostPerImage); p != nil {
			return p, nil
		}
	}
	pixel := positive(e.InputCostPerPixel)
	if pixel == nil {
		pixel = positive(e.OutputCostPerPixel)
	}
	if pixel == nil {
		return nil, nil
	}
	if pixels, ok := parseSizePixels(size); ok {
		v := roundUSD(*pixel * pixels)
		return &v, nil
	}
	return nil, pixel
}

// The limits of gateway.gateway_model_image_prices (shared migration 0243).
const (
	maxImageModelLen   = 128
	maxImageSizeLen    = 16
	maxImageQualityLen = 32
	maxUSDPerImage     = 100000.0
	// maxUSDPerPixel is the per-pixel form of the 1,000,000 USD per 1M pixels
	// bound.
	maxUSDPerPixel = 1.0
)

// imageRowProblem names why a row does not fit the table, or "" when it fits.
func imageRowProblem(model, size, quality string, perImage, perPixel *float64) string {
	switch {
	case len(model) > maxImageModelLen:
		return "model name longer than the column"
	case len(size) > maxImageSizeLen:
		return "size longer than the column"
	case len(quality) > maxImageQualityLen:
		return "quality longer than the column"
	case perImage != nil && *perImage > maxUSDPerImage:
		return "price per image above the allowed range"
	case perPixel != nil && *perPixel > maxUSDPerPixel:
		return "price per pixel above the allowed range"
	}
	return ""
}

// parseLiteLLMImagePrices decodes the per-image prices of the sheet into
// image RawModelPrices, in a fixed order. Split from parseLiteLLM because the
// two feed different tables.
func parseLiteLLMImagePrices(body []byte) ([]RawModelPrice, error) {
	var raw map[string]litellmEntry
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("litellm: decode: %w", err)
	}
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []RawModelPrice
	for _, name := range names {
		e := raw[name]
		if name == "sample_spec" || e.LiteLLMProvider == "" || e.Mode != imageGenerationMode {
			continue
		}
		model, size, quality, ok := splitImageKey(name, e.LiteLLMProvider)
		if !ok {
			continue
		}
		perImage, perPixel := e.imagePrice(size, quality)
		if perImage == nil && perPixel == nil {
			continue
		}
		// One row the table refuses (a value over a column length or over the
		// range CHECK of shared migration 0243) would roll back the whole
		// image pass, so it is skipped here and named in the log.
		if reason := imageRowProblem(model, size, quality, perImage, perPixel); reason != "" {
			slog.Warn("pricesync: skipping an image price the catalog cannot store",
				"key", name, "reason", reason)
			continue
		}
		out = append(out, RawModelPrice{
			Provider:  normalizeProvider(e.LiteLLMProvider),
			ModelName: model,
			Image: &RawImagePrice{
				Size: size, Quality: quality,
				PerImage: perImage, PerPixel: perPixel,
			},
		})
	}
	return out, nil
}
