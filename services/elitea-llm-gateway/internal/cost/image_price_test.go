package cost

import (
	"context"
	"testing"
)

// The gpt-image-2 call from issue #6719: 101 input tokens and 5488 output
// tokens. The provider reports the generated image as output tokens.
const (
	imageCallInputTokens  = 101
	imageCallOutputTokens = 5488
)

// TestCost_GptImage2PricesOutputAtTheImageTokenRate is the gateway half of
// issue #6719. The price sync now stores the per-image-token output rate
// (0.00003 USD per token = 30 USD per 1M) in the output column. The output
// tokens must be billed at that rate, not at the input x 3 estimate.
func TestCost_GptImage2PricesOutputAtTheImageTokenRate(t *testing.T) {
	db := &fakeDB{rows: map[string]fakeRow{
		"openai:gpt-image-2": {srcInput: ptr(usdToNano(5.00)), srcOutput: ptr(usdToNano(30.00))},
	}}
	c := New(Config{DB: db})
	got := c.Cost(context.Background(), "openai", "gpt-image-2", imageCallInputTokens, imageCallOutputTokens)

	// 101 * 5 / 1e6 USD = 0.000505 USD = 505,000 nano-USD.
	if got.InputNanoUSD != 505_000 {
		t.Errorf("input = %d nano-USD, want 505000", got.InputNanoUSD)
	}
	// 5488 * 30 / 1e6 USD = 0.16464 USD = 164,640,000 nano-USD. The legacy
	// runtime billed 0 for these tokens; input x 3 would bill 82,320,000.
	if got.OutputNanoUSD != 164_640_000 {
		t.Errorf("output = %d nano-USD, want 164640000 (the image-token rate)", got.OutputNanoUSD)
	}
	if got.TotalNanoUSD != 165_145_000 {
		t.Errorf("total = %d nano-USD, want 165145000", got.TotalNanoUSD)
	}
	if got.OutputDerived {
		t.Error("OutputDerived = true for a row with a catalog output price")
	}
	if got.Source != SourceCatalog {
		t.Errorf("source = %q, want catalog", got.Source)
	}
}

// TestCost_ARowWithoutAnOutputPriceIsMarkedDerived is the state before the sync
// fix: an input price and a NULL output price. The input x 3 estimate still
// pays, as it always has, and the cost now says that it is an estimate.
func TestCost_ARowWithoutAnOutputPriceIsMarkedDerived(t *testing.T) {
	db := &fakeDB{rows: map[string]fakeRow{
		"openai:gpt-image-2": {srcInput: ptr(usdToNano(5.00)), srcOutput: nil},
	}}
	c := New(Config{DB: db})
	got := c.Cost(context.Background(), "openai", "gpt-image-2", imageCallInputTokens, imageCallOutputTokens)
	if got.OutputNanoUSD != 82_320_000 {
		t.Errorf("output = %d nano-USD, want 82320000 (input x 3)", got.OutputNanoUSD)
	}
	if !got.OutputDerived {
		t.Error("OutputDerived = false, want true: the output price is the input x 3 estimate")
	}

	// A request with no output tokens did not use the estimate.
	embedding := c.Cost(context.Background(), "openai", "gpt-image-2", 10, 0)
	if embedding.OutputDerived {
		t.Error("OutputDerived = true for a request with no output tokens")
	}
}

// TestCost_DefaultTablePriceIsNotMarkedDerived keeps the flag to its meaning.
// The default table publishes both prices, so nothing is derived from input.
func TestCost_DefaultTablePriceIsNotMarkedDerived(t *testing.T) {
	c := New(Config{})
	if got := c.Cost(context.Background(), "openai", "gpt-4o", 10, 10); got.OutputDerived {
		t.Errorf("OutputDerived = true for a default-table price: %+v", got)
	}
}
