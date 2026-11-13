package pricesync

import (
	"context"
	"math"
	"testing"
)

// imagePriceSheet holds the shapes the upstream sheet gives per-image prices
// (issue 1969): the size and the quality live in the KEY, and the price is a
// flat per-image price, a per-pixel price, or an image-token price.
const imagePriceSheet = `{
	"1024-x-1024/dall-e-2": {"litellm_provider": "openai", "mode": "image_generation", "output_cost_per_image": 0.02},
	"256-x-256/dall-e-2":   {"litellm_provider": "openai", "mode": "image_generation", "output_cost_per_image": 0.016},
	"standard/1024-x-1024/dall-e-3": {"litellm_provider": "openai", "mode": "image_generation", "output_cost_per_image": 0.04},
	"hd/1792-x-1024/dall-e-3":       {"litellm_provider": "openai", "mode": "image_generation", "output_cost_per_image": 0.12},
	"azure/hd/1024-x-1792/dall-e-3": {"litellm_provider": "azure", "mode": "image_generation", "input_cost_per_pixel": 7.19343e-08, "output_cost_per_token": 0.0},
	"azure/standard/1024-x-1024/dall-e-2": {"litellm_provider": "azure", "mode": "image_generation", "input_cost_per_pixel": 0.0},
	"vertex_ai/imagen-4.0-generate-001": {"litellm_provider": "vertex_ai-image-models", "mode": "image_generation", "output_cost_per_image": 0.04},
	"fal_ai/fal-ai/flux/schnell": {"litellm_provider": "fal_ai", "mode": "image_generation", "output_cost_per_image": 0.003, "output_cost_per_pixel": 2.86102294921875e-09},
	"azure_ai/FLUX.2-flex": {"litellm_provider": "azure_ai", "mode": "image_generation", "input_cost_per_pixel": 5e-08},
	"low/1024-x-1024/gpt-image-1.5": {"litellm_provider": "openai", "mode": "image_generation", "input_cost_per_image": 0.009},
	"gemini/gemini-2.5-flash-image": {"litellm_provider": "gemini", "mode": "image_generation", "input_cost_per_image": 0.0011, "output_cost_per_image_token": 0.00003, "input_cost_per_token": 3e-07},
	"1024-x-1024/50-steps/bedrock/amazon.nova-canvas-v1:0": {"litellm_provider": "bedrock", "mode": "image_generation", "output_cost_per_image": 0.06},
	"free-image": {"litellm_provider": "openai", "mode": "image_generation", "output_cost_per_image": 0},
	"gpt-image-1": {"litellm_provider": "openai", "mode": "image_generation", "input_cost_per_image_token": 0.00001, "output_cost_per_image_token": 0.00004},
	"some-chat": {"litellm_provider": "openai", "mode": "chat", "output_cost_per_image": 0.5}
}`

func imageByKey(t *testing.T) map[string]RawImagePrice {
	t.Helper()
	got, err := parseLiteLLMImagePrices([]byte(imagePriceSheet))
	if err != nil {
		t.Fatalf("parseLiteLLMImagePrices: %v", err)
	}
	out := map[string]RawImagePrice{}
	for _, r := range got {
		if r.Image == nil {
			t.Fatalf("row %q has no Image", r.ModelName)
		}
		out[r.Provider+"|"+r.ModelName+"|"+r.Image.Size+"|"+r.Image.Quality] = *r.Image
	}
	return out
}

func TestParseImagePricesSplitsSizeAndQualityFromTheKey(t *testing.T) {
	got := imageByKey(t)
	for key, want := range map[string]float64{
		"openai|dall-e-2|1024x1024|":         0.02,
		"openai|dall-e-2|256x256|":           0.016,
		"openai|dall-e-3|1024x1024|standard": 0.04,
		"openai|dall-e-3|1792x1024|hd":       0.12,
		"vertex|imagen-4.0-generate-001||":   0.04,
		"openai|gpt-image-1.5|1024x1024|low": 0.009,
		"gemini|gemini-2.5-flash-image||":    0, // input_cost_per_image of a sizeless entry is NOT the image price
	} {
		price, ok := got[key]
		if want == 0 {
			if ok {
				t.Errorf("%s must not be priced per image, got %+v", key, price)
			}
			continue
		}
		if !ok || price.PerImage == nil || math.Abs(*price.PerImage-want) > 1e-12 {
			t.Errorf("%s = %+v, want %v per image", key, price, want)
		}
	}
}

func TestParseImagePricesFlatPriceBeatsPixelPrice(t *testing.T) {
	got := imageByKey(t)
	price := got["fal_ai|fal-ai/flux/schnell||"]
	if price.PerImage == nil || *price.PerImage != 0.003 || price.PerPixel != nil {
		t.Fatalf("flux/schnell = %+v, want 0.003 per image and no per-pixel price", price)
	}
}

func TestParseImagePricesPerPixelWithSizeBecomesPerImage(t *testing.T) {
	got := imageByKey(t)
	price := got["azure|dall-e-3|1024x1792|hd"]
	// 7.19343e-08 USD * 1024 * 1792 pixels = 0.13199...
	if price.PerImage == nil || math.Abs(*price.PerImage-0.1319999) > 1e-4 || price.PerPixel != nil {
		t.Fatalf("azure hd 1024x1792 = %+v, want ~0.132 per image", price)
	}
}

func TestParseImagePricesPerPixelWithoutSizeStaysPerPixel(t *testing.T) {
	got := imageByKey(t)
	price := got["azure|FLUX.2-flex||"]
	if price.PerPixel == nil || *price.PerPixel != 5e-08 || price.PerImage != nil {
		t.Fatalf("FLUX.2-flex = %+v, want a per-pixel price only", price)
	}
	norm, err := Normalizer{}.Normalize(RawModelPrice{Provider: "azure", ModelName: "FLUX.2-flex", Image: &price}, PerToken, "litellm")
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if norm.Image.UsdPer1MPixels == nil || math.Abs(*norm.Image.UsdPer1MPixels-0.05) > 1e-12 {
		t.Fatalf("per 1M pixels = %v, want 0.05 (5e-08 USD * 1e6)", norm.Image.UsdPer1MPixels)
	}
}

func TestParseImagePricesNormalizerDoesNotScalePerImage(t *testing.T) {
	per := 0.04
	norm, err := Normalizer{}.Normalize(RawModelPrice{Provider: "openai", ModelName: "dall-e-3", Image: &RawImagePrice{PerImage: &per}}, PerToken, "litellm")
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if norm.Image.UsdPerImage == nil || *norm.Image.UsdPerImage != 0.04 {
		t.Fatalf("per image = %v, want 0.04 unscaled", norm.Image.UsdPerImage)
	}
}

func TestParseImagePricesSkipsWhatItCannotPriceHonestly(t *testing.T) {
	got := imageByKey(t)
	for _, key := range []string{
		"azure|dall-e-2|1024x1024|standard",          // price 0 would bill the image at nothing
		"openai|free-image||",                        // flat zero
		"bedrock|amazon.nova-canvas-v1:0|1024x1024|", // step-count variant
		"openai|gpt-image-1||",                       // token priced: the token path pays it
		"openai|some-chat||",                         // not an image_generation entry
	} {
		if _, ok := got[key]; ok {
			t.Errorf("%s must not be stored as a per-image price", key)
		}
	}
}

func TestParseImagePricesLeavesTheTokenRowsAlone(t *testing.T) {
	models, err := parseLiteLLM([]byte(imagePriceSheet))
	if err != nil {
		t.Fatalf("parseLiteLLM: %v", err)
	}
	for _, m := range models {
		if m.Image != nil {
			t.Errorf("parseLiteLLM must not return image rows: %+v", m)
		}
	}
	var found bool
	for _, m := range models {
		if m.ModelName == "gpt-image-1" {
			found = approxEqual(m.OutputCost, 0.00004)
		}
	}
	if !found {
		t.Error("a token-priced gpt-image model must keep its token row")
	}
}

func TestSyncWritesImagePricesToTheirOwnTable(t *testing.T) {
	flat, px := 0.12, 5e-08
	src := &fakeSource{name: "litellm", denom: PerToken, raws: []RawModelPrice{
		{Provider: "openai", ModelName: "gpt-4o", InputCost: fptr(0.0000025)},
		{Provider: "openai", ModelName: "dall-e-3", Image: &RawImagePrice{Size: "1792x1024", Quality: "hd", PerImage: &flat}},
		{Provider: "azure", ModelName: "FLUX.2-flex", Image: &RawImagePrice{PerPixel: &px}},
	}}
	tx := &fakeTx{locked: true}
	s := NewSyncer(&fakeDB{tx: tx}, []PriceSource{src}, quietLogger())
	n, err := s.Sync(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if n != 3 || len(tx.execSQL) != 3 {
		t.Fatalf("offered %d, %d statements; want 3 and 3", n, len(tx.execSQL))
	}
	img := tx.execSQL[1]
	if !contains(img, "gateway.gateway_model_image_prices") || !contains(img, "WHERE NOT gateway_model_image_prices.price_overridden") {
		t.Errorf("image statement does not target the image table with the override guard:\n%s", img)
	}
	args := tx.execArgs[1]
	if args[0] != "openai" || args[1] != "dall-e-3" || args[2] != "1792x1024" || args[3] != "hd" {
		t.Errorf("image args = %v", args)
	}
	if got, ok := args[4].(*float64); !ok || *got != 0.12 {
		t.Errorf("usd_per_image arg = %v, want 0.12", args[4])
	}
	if got, ok := tx.execArgs[2][5].(*float64); !ok || math.Abs(*got-0.05) > 1e-12 {
		t.Errorf("usd_per_1m_pixels arg = %v, want 0.05", tx.execArgs[2][5])
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
