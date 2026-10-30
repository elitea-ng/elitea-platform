package platformconfig

import (
	"math"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runcaps"
)

// The per-tier run caps of SectionRunCaps (ADR-0041 decision 3, #1498).
// Each tier has four keys, "<tier>_<cap>", for example
// "high_max_tool_calls". Every value is the most ONE run of an agent of that
// tier may use; 0 is no cap. An agent's author may lower a cap for the agent
// (version meta `run_caps`) and never raise it.
const (
	RunCapMaxToolCalls   = runcaps.MaxToolCalls
	RunCapMaxModelTurns  = runcaps.MaxModelTurns
	RunCapMaxSpend       = runcaps.MaxSpend
	RunCapMaxRecordsRead = runcaps.MaxRecordsRead
)

// RunCapNames lists the per-tier caps in display order.
var RunCapNames = []string{RunCapMaxToolCalls, RunCapMaxModelTurns, RunCapMaxSpend, RunCapMaxRecordsRead}

// DefaultRunMaxToolCalls is the ADR default of max_tool_calls at every tier.
// The worker applies the same default to a command without caps.
const DefaultRunMaxToolCalls = 200

// The largest value of each cap. A larger value is no cap in practice; the
// bound keeps a hand-written row inside the worker's integer range.
const (
	MaxRunCapToolCalls   = 100_000
	MaxRunCapModelTurns  = 100_000
	MaxRunCapRecordsRead = 1_000_000_000
	// MaxRunCapSpend is in USD, the currency of the price catalogue and the
	// project budgets.
	MaxRunCapSpend = 1_000_000
)

// TierRunCaps is the caps of one tier, or the effective caps of one run.
// 0 is no cap.
type TierRunCaps struct {
	MaxToolCalls   uint32
	MaxModelTurns  uint32
	MaxRecordsRead uint64
	// MaxSpend is USD. Main enforces it (the worker cannot price tokens).
	MaxSpend float64
}

// DefaultTierRunCaps is the ADR default of every tier: 200 tool calls, no
// other cap.
func DefaultTierRunCaps() TierRunCaps {
	return TierRunCaps{MaxToolCalls: DefaultRunMaxToolCalls}
}

// Lower answers caps with each cap the lower of the two; 0 (no cap) loses
// to any cap.
func (c TierRunCaps) Lower(other TierRunCaps) TierRunCaps {
	return TierRunCaps{
		MaxToolCalls:   lowerCap(c.MaxToolCalls, other.MaxToolCalls),
		MaxModelTurns:  lowerCap(c.MaxModelTurns, other.MaxModelTurns),
		MaxRecordsRead: lowerCap(c.MaxRecordsRead, other.MaxRecordsRead),
		MaxSpend:       lowerCap(c.MaxSpend, other.MaxSpend),
	}
}

// Value answers one cap by name as a float64 (spend has decimals).
func (c TierRunCaps) Value(name string) float64 {
	switch name {
	case RunCapMaxToolCalls:
		return float64(c.MaxToolCalls)
	case RunCapMaxModelTurns:
		return float64(c.MaxModelTurns)
	case RunCapMaxRecordsRead:
		return float64(c.MaxRecordsRead)
	case RunCapMaxSpend:
		return c.MaxSpend
	default:
		return 0
	}
}

// Raised answers caps with one cap doubled, for the one continuation a
// safety.override_cap holder grants past it. A cap that is off stays off.
func (c TierRunCaps) Raised(name string) TierRunCaps {
	switch name {
	case RunCapMaxToolCalls:
		c.MaxToolCalls = uint32(min(uint64(c.MaxToolCalls)*2, math.MaxUint32))
	case RunCapMaxModelTurns:
		c.MaxModelTurns = uint32(min(uint64(c.MaxModelTurns)*2, math.MaxUint32))
	case RunCapMaxRecordsRead:
		c.MaxRecordsRead = min(c.MaxRecordsRead, math.MaxUint64/2) * 2
	case RunCapMaxSpend:
		c.MaxSpend *= 2
	}
	return c
}

func lowerCap[T uint32 | uint64 | float64](a, b T) T {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	default:
		return min(a, b)
	}
}

// RunCapKey is the key of one tier's cap.
func RunCapKey(tier agentlifecycle.Tier, name string) string {
	return string(tier) + "_" + name
}

// RunCapLimit is the largest value of one cap.
func RunCapLimit(name string) float64 {
	switch name {
	case RunCapMaxToolCalls:
		return MaxRunCapToolCalls
	case RunCapMaxModelTurns:
		return MaxRunCapModelTurns
	case RunCapMaxRecordsRead:
		return MaxRunCapRecordsRead
	case RunCapMaxSpend:
		return MaxRunCapSpend
	default:
		return 0
	}
}

// ValidRunCap reports whether a value is 0 or within the cap's range: a
// whole number for a count, at most four decimals for spend.
func ValidRunCap(name string, raw any) bool {
	number, ok := raw.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 || number > RunCapLimit(name) {
		return false
	}
	if name == RunCapMaxSpend {
		scaled := number * 10_000
		return math.Abs(scaled-math.Round(scaled)) < 1e-6
	}
	return number == math.Trunc(number)
}

// tierRunCapsFromValues overlays the saved per-tier keys on the defaults. A
// present value that does not read is the tightest cap (1, or 0.0001 USD),
// never "no cap": a hand-written row can tighten a cap and never silently
// remove it. A value past the cap's range is clamped to it.
func tierRunCapsFromValues(values Values) (map[agentlifecycle.Tier]TierRunCaps, bool) {
	tiers := make(map[agentlifecycle.Tier]TierRunCaps, len(agentlifecycle.Tiers))
	stored := false
	for _, tier := range agentlifecycle.Tiers {
		caps := DefaultTierRunCaps()
		for _, name := range RunCapNames {
			raw, present := values[RunCapKey(tier, name)]
			if !present {
				continue
			}
			stored = true
			value := readRunCap(name, raw)
			switch name {
			case RunCapMaxToolCalls:
				caps.MaxToolCalls = uint32(value)
			case RunCapMaxModelTurns:
				caps.MaxModelTurns = uint32(value)
			case RunCapMaxRecordsRead:
				caps.MaxRecordsRead = uint64(value)
			case RunCapMaxSpend:
				caps.MaxSpend = value
			}
		}
		tiers[tier] = caps
	}
	return tiers, stored
}

func readRunCap(name string, raw any) float64 {
	number, ok := raw.(float64)
	tightest := 1.0
	if name == RunCapMaxSpend {
		tightest = 0.0001
	}
	if !ok || math.IsNaN(number) || number < 0 {
		return tightest
	}
	if number > RunCapLimit(name) {
		return RunCapLimit(name)
	}
	if name != RunCapMaxSpend && number > 0 {
		number = max(math.Trunc(number), 1)
	}
	return number
}
