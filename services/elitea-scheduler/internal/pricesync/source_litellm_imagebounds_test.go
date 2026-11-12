package pricesync

import (
	"strings"
	"testing"
)

func TestParseImagePricesSkipsARowTheTableWouldRefuse(t *testing.T) {
	long := strings.Repeat("m", 129)
	sheet := `{
		"` + long + `": {"litellm_provider": "openai", "mode": "image_generation", "output_cost_per_image": 0.04},
		"huge-price": {"litellm_provider": "openai", "mode": "image_generation", "output_cost_per_image": 100001},
		"huge-pixel": {"litellm_provider": "openai", "mode": "image_generation", "input_cost_per_pixel": 2},
		"fine": {"litellm_provider": "openai", "mode": "image_generation", "output_cost_per_image": 0.04}
	}`
	got, err := parseLiteLLMImagePrices([]byte(sheet))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ModelName != "fine" {
		t.Fatalf("rows = %+v, want only the one the table accepts", got)
	}
}
