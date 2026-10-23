package configurations

import (
	"encoding/json"
	"strings"
)

// ModelPriceCurrency is the only currency the gateway prices in: its budget
// counter is nano-USD and gateway.gateway_models holds USD per 1M tokens.
const ModelPriceCurrency = "USD"

// ModelPriceRow is one gateway.gateway_models row as the catalogue reads it.
// Input and Output are the exact NUMERIC text of input_cost_per_1m_tokens and
// output_cost_per_1m_tokens; nil is SQL NULL.
type ModelPriceRow struct {
	Provider  string
	ModelName string
	Input     *string
	Output    *string
}

// ModelPrice is the per-1M-token USD price of one catalogue model, as the
// gateway's budget gate charges it. CachedInput equals Input: the gateway bills
// every prompt token, cached reads included, at the input price.
type ModelPrice struct {
	Input       json.Number
	Output      json.Number
	CachedInput json.Number
}

// ModelPriceLookupNames are the model_name values a price read must fetch for
// these catalogue names: each name itself and, for a `provider/model` name, the
// model part, so MatchModelPrices can try the exact (provider, model) row.
func ModelPriceLookupNames(names []string) []string {
	seen := make(map[string]struct{}, len(names)*2)
	lookup := make([]string, 0, len(names)*2)
	add := func(name string) {
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		lookup = append(lookup, name)
	}
	for _, name := range names {
		add(name)
		if _, model, ok := splitProviderModel(name); ok {
			add(model)
		}
	}
	return lookup
}

// MatchModelPrices resolves each catalogue name to a price, conservatively. A
// catalogue item carries only its model name, while the gateway keys a price by
// (provider, model_name), so the match never guesses:
//
//   - a `provider/model` name whose exact (provider, model) row exists takes
//     that row and nothing else;
//   - otherwise every row whose model_name equals the name is a candidate, and
//     the price is served only when all candidates agree on (input, output).
//
// A row with a NULL input or output price is not a price (the gateway falls
// back to its built-in defaults for it, which are never served), so it makes
// the name unknown rather than being skipped. A name with no row is unknown.
// Unknown names are absent from the result.
func MatchModelPrices(names []string, rows []ModelPriceRow) map[string]ModelPrice {
	type key struct{ provider, model string }
	exact := make(map[key]ModelPriceRow, len(rows))
	byName := make(map[string][]ModelPriceRow, len(rows))
	for _, row := range rows {
		exact[key{row.Provider, row.ModelName}] = row
		byName[row.ModelName] = append(byName[row.ModelName], row)
	}

	prices := make(map[string]ModelPrice, len(names))
	for _, name := range names {
		if provider, model, ok := splitProviderModel(name); ok {
			if row, found := exact[key{provider, model}]; found {
				if price, priced := rowPrice(row); priced {
					prices[name] = price
				}
				continue
			}
		}
		candidates := byName[name]
		if len(candidates) == 0 {
			continue
		}
		price, priced := rowPrice(candidates[0])
		for _, row := range candidates[1:] {
			other, otherPriced := rowPrice(row)
			if !otherPriced || other.Input != price.Input || other.Output != price.Output {
				priced = false
				break
			}
		}
		if priced {
			prices[name] = price
		}
	}
	return prices
}

func splitProviderModel(name string) (string, string, bool) {
	provider, model, ok := strings.Cut(name, "/")
	if !ok || provider == "" || model == "" {
		return "", "", false
	}
	return provider, model, true
}

func rowPrice(row ModelPriceRow) (ModelPrice, bool) {
	if row.Input == nil || row.Output == nil {
		return ModelPrice{}, false
	}
	input := canonicalDecimal(*row.Input)
	output := canonicalDecimal(*row.Output)
	if input == "" || output == "" {
		return ModelPrice{}, false
	}
	return ModelPrice{
		Input:       json.Number(input),
		Output:      json.Number(output),
		CachedInput: json.Number(input),
	}, true
}

// canonicalDecimal trims the NUMERIC(20,8) scale padding ("2.50000000" →
// "2.5", "3.00000000" → "3"), so rows that hold the same price compare equal
// and the wire carries the plain number. It returns "" for anything that is
// not a finite plain decimal (NUMERIC admits NaN and Infinity; JSON does not).
func canonicalDecimal(text string) string {
	text = strings.TrimSpace(text)
	digits := strings.TrimPrefix(text, "-")
	if digits == "" {
		return ""
	}
	dots := 0
	for _, r := range digits {
		switch {
		case r == '.':
			dots++
		case r < '0' || r > '9':
			return ""
		}
	}
	if dots > 1 || digits[0] == '.' || digits[len(digits)-1] == '.' {
		return ""
	}
	if dots == 1 {
		text = strings.TrimRight(text, "0")
		text = strings.TrimSuffix(text, ".")
	}
	if text == "-0" {
		return "0"
	}
	return text
}

// ApplyModelPrices stamps each priced item's cost fields. Items without a
// price are left untouched, so their wire shape is unchanged.
func ApplyModelPrices(items []CurrentModelCatalogItem, prices map[string]ModelPrice) {
	for index := range items {
		price, ok := prices[items[index].Name]
		if !ok {
			continue
		}
		input, output, cached := price.Input, price.Output, price.CachedInput
		currency := ModelPriceCurrency
		items[index].InputCostPer1MTokens = &input
		items[index].OutputCostPer1MTokens = &output
		items[index].CachedInputCostPer1MTokens = &cached
		items[index].Currency = &currency
	}
}
