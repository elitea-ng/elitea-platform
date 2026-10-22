package pricesync

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"context"
)

// seedModelsJSON is the bundled static price seed ported from the legacy centry
// gateway_access 002_seed_gateway_models migration. It is ALREADY per-1M (it
// mirrors the gateway_models schema), so SeedSource declares Per1M and the
// Normalizer applies a ×1 no-op convert. It exists as an airgapped / pinned
// fallback and as a lowest-precedence gap-filler behind the live sources (§8.8).
//
//go:embed seed_models.json
var seedModelsJSON []byte

type seedEntry struct {
	Provider        string   `json:"provider"`
	ModelName       string   `json:"model_name"`
	InputCostPer1M  *float64 `json:"input_cost_per_1m"`
	OutputCostPer1M *float64 `json:"output_cost_per_1m"`
}

// SeedSource serves the bundled static price seed. Denomination is Per1M.
type SeedSource struct {
	entries []seedEntry
	err     error
}

// NewSeedSource parses the embedded seed once at construction so a malformed
// bundle surfaces immediately rather than on first Fetch.
func NewSeedSource() *SeedSource {
	var entries []seedEntry
	err := json.Unmarshal(seedModelsJSON, &entries)
	return &SeedSource{entries: entries, err: err}
}

// Name implements PriceSource.
func (s *SeedSource) Name() string { return "seed" }

// Denomination implements PriceSource: the seed is authored per-1M already.
func (s *SeedSource) Denomination() Denomination { return Per1M }

// Fetch implements PriceSource. It never touches the network.
func (s *SeedSource) Fetch(_ context.Context) ([]RawModelPrice, error) {
	if s.err != nil {
		return nil, fmt.Errorf("seed: parse bundled models: %w", s.err)
	}
	out := make([]RawModelPrice, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, RawModelPrice{
			Provider:   e.Provider,
			ModelName:  e.ModelName,
			InputCost:  e.InputCostPer1M,
			OutputCost: e.OutputCostPer1M,
		})
	}
	return out, nil
}
