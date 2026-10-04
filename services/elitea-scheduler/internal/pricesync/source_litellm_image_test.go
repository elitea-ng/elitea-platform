package pricesync

import (
	"math"
	"testing"
)

// gptImage2Sheet is the shape the upstream price sheet gives gpt-image-2 and
// its azure variant (issue #6719): a text input price, an image input price, an
// image OUTPUT price, and NO output_cost_per_token. The fal.ai entry carries only
// a flat per-image price, which has no catalog column.
const gptImage2Sheet = `{
	"gpt-image-2": {
		"litellm_provider": "openai",
		"mode": "image_generation",
		"input_cost_per_token": 0.000005,
		"input_cost_per_image_token": 0.000008,
		"output_cost_per_image_token": 0.00003,
		"cache_read_input_token_cost": 0.00000125
	},
	"azure/gpt-image-2": {
		"litellm_provider": "azure",
		"mode": "image_generation",
		"input_cost_per_token": 0.000005,
		"output_cost_per_image_token": 0.00003
	},
	"image-only-input": {
		"litellm_provider": "openai",
		"mode": "image_generation",
		"input_cost_per_image_token": 0.00001
	},
	"gemini-2.5-flash-image": {
		"litellm_provider": "vertex_ai",
		"mode": "image_generation",
		"input_cost_per_token": 0.0000003,
		"output_cost_per_token": 0.0000025,
		"output_cost_per_image_token": 0.00003
	},
	"fal_ai/gpt-image-2.5/text-to-image": {
		"litellm_provider": "fal_ai",
		"mode": "image_generation",
		"output_cost_per_image": 0.04116
	}
}`

func approxEqual(got *float64, want float64) bool {
	return got != nil && math.Abs(*got-want) < 1e-12
}

func TestParseLiteLLMMapsTheImageTokenOutputPrice(t *testing.T) {
	got, err := parseLiteLLM([]byte(gptImage2Sheet))
	if err != nil {
		t.Fatalf("parseLiteLLM: %v", err)
	}
	byModel := map[string]RawModelPrice{}
	for _, r := range got {
		byModel[r.ModelName] = r
	}

	image := byModel["gpt-image-2"]
	if !approxEqual(image.OutputCost, 0.00003) {
		t.Fatalf("gpt-image-2 output price = %v, want the image-token rate 0.00003 (issue #6719)", image.OutputCost)
	}
	// The text input price wins over the image input price: a generation
	// request sends text in.
	if !approxEqual(image.InputCost, 0.000005) {
		t.Errorf("gpt-image-2 input price = %v, want the per-token rate 0.000005", image.InputCost)
	}

	azure := byModel["azure/gpt-image-2"]
	if azure.Provider != "azure" || !approxEqual(azure.OutputCost, 0.00003) {
		t.Errorf("azure/gpt-image-2 = %+v, want provider azure and output 0.00003", azure)
	}

	inputOnly, ok := byModel["image-only-input"]
	if !ok {
		t.Fatal("an entry whose only price is an image-token price must be admitted")
	}
	if !approxEqual(inputOnly.InputCost, 0.00001) || inputOnly.OutputCost != nil {
		t.Errorf("image-only-input = in %v out %v, want in 0.00001 and out nil", inputOnly.InputCost, inputOnly.OutputCost)
	}

	// A per-token output price stays the output price. The image rate fills
	// only an ABSENT per-token price.
	gemini := byModel["gemini-2.5-flash-image"]
	if !approxEqual(gemini.OutputCost, 0.0000025) {
		t.Errorf("gemini-2.5-flash-image output = %v, want its per-token rate 0.0000025", gemini.OutputCost)
	}

	// A flat per-image price has no token column, so the entry has nothing the
	// catalog can store and is skipped rather than mispriced.
	if _, ok := byModel["fal_ai/gpt-image-2.5/text-to-image"]; ok {
		t.Error("a flat per-image price must not be stored in a per-token column")
	}
	if len(got) != 4 {
		t.Fatalf("expected 4 admitted models, got %d (%+v)", len(got), got)
	}
}

// TestNormalizedImagePriceIsPer1M pins the denomination after normalisation.
// 0.00003 USD per token is 30 USD per 1M tokens. The gateway multiplies the
// provider's output_tokens by this rate, so 5488 output tokens cost
// 5488 * 30 / 1e6 = 0.16464 USD, where the old row billed nothing for them.
func TestNormalizedImagePriceIsPer1M(t *testing.T) {
	got, err := parseLiteLLM([]byte(gptImage2Sheet))
	if err != nil {
		t.Fatalf("parseLiteLLM: %v", err)
	}
	for _, raw := range got {
		if raw.ModelName != "gpt-image-2" {
			continue
		}
		norm, err := Normalizer{}.Normalize(raw, PerToken, "litellm")
		if err != nil {
			t.Fatalf("Normalize: %v", err)
		}
		if !approxEqual(norm.OutputCostPer1M, 30) {
			t.Fatalf("output per 1M = %v, want 30", norm.OutputCostPer1M)
		}
		if cost := 5488 * *norm.OutputCostPer1M / 1e6; math.Abs(cost-0.16464) > 1e-9 {
			t.Fatalf("5488 output tokens cost %v USD, want 0.16464", cost)
		}
		return
	}
	t.Fatal("gpt-image-2 missing from the parsed sheet")
}
