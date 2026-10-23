package configurations

import (
	"reflect"
	"testing"
)

func priceRow(provider, model string, input, output *string) ModelPriceRow {
	return ModelPriceRow{Provider: provider, ModelName: model, Input: input, Output: output}
}

func priceText(value string) *string { return &value }

func TestMatchModelPrices(t *testing.T) {
	rows := []ModelPriceRow{
		// Exact (provider, model) row for "openai/gpt-x", plus a disagreeing
		// row under the same model_name that the exact match must ignore.
		priceRow("openai", "gpt-x", priceText("2.50000000"), priceText("10.00000000")),
		priceRow("azure", "gpt-x", priceText("3.00000000"), priceText("12.00000000")),
		// Unanimous by name: two providers, one price.
		priceRow("openai", "shared-model", priceText("1.00000000"), priceText("4.00000000")),
		priceRow("azure", "shared-model", priceText("1.0"), priceText("4")),
		// Disagreeing by name.
		priceRow("openai", "split-model", priceText("1.00000000"), priceText("4.00000000")),
		priceRow("azure", "split-model", priceText("1.00000000"), priceText("5.00000000")),
		// A NULL price is not a price, alone or beside a real one.
		priceRow("openai", "null-model", nil, priceText("1.00000000")),
		priceRow("openai", "half-null", priceText("1.00000000"), priceText("2.00000000")),
		priceRow("azure", "half-null", priceText("1.00000000"), nil),
		// Exact row with a NULL price: unknown, no fallback to the name rows.
		priceRow("vertex", "gem", nil, nil),
		priceRow("google", "gem", priceText("0.1"), priceText("0.4")),
		// NUMERIC admits NaN; JSON does not.
		priceRow("openai", "nan-model", priceText("NaN"), priceText("1")),
		// A row stored under the full name.
		priceRow("openai", "acme/full", priceText("0.00000000"), priceText("0.75000000")),
	}
	names := []string{
		"openai/gpt-x", "gpt-x", "shared-model", "split-model", "null-model",
		"half-null", "vertex/gem", "nan-model", "acme/full", "unknown",
		"other/shared-model",
	}

	got := MatchModelPrices(names, rows)
	want := map[string]ModelPrice{
		"openai/gpt-x": {Input: "2.5", Output: "10", CachedInput: "2.5"},
		"shared-model": {Input: "1", Output: "4", CachedInput: "1"},
		// No exact (acme, full) row, so the model_name = name rows decide.
		"acme/full": {Input: "0", Output: "0.75", CachedInput: "0"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("prices = %#v\nwant   %#v", got, want)
	}
}

func TestModelPriceLookupNames(t *testing.T) {
	got := ModelPriceLookupNames([]string{"openai/gpt-x", "gpt-x", "plain", "", "plain", "/bad"})
	want := []string{"openai/gpt-x", "gpt-x", "plain", "/bad"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lookup = %#v, want %#v", got, want)
	}
}

func TestApplyModelPricesLeavesUnpricedItemsUntouched(t *testing.T) {
	items := []CurrentModelCatalogItem{{Name: "priced"}, {Name: "unpriced"}}
	ApplyModelPrices(items, map[string]ModelPrice{"priced": {Input: "1", Output: "2", CachedInput: "1"}})
	if items[0].InputCostPer1MTokens == nil || *items[0].InputCostPer1MTokens != "1" ||
		*items[0].OutputCostPer1MTokens != "2" || *items[0].CachedInputCostPer1MTokens != "1" ||
		*items[0].Currency != "USD" {
		t.Fatalf("priced item = %+v", items[0])
	}
	if !reflect.DeepEqual(items[1], CurrentModelCatalogItem{Name: "unpriced"}) {
		t.Fatalf("unpriced item changed: %+v", items[1])
	}
}
