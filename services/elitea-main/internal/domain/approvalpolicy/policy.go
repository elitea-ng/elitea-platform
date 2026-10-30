// Package approvalpolicy is the approval policy of ADR-0041 decision 5
// (#1498): which tool categories need a human approval at each risk tier,
// and at which tiers a pipeline must pass a `hitl` node on every path to an
// `external_send` or `payment` tool node.
//
// The platform's `sensitive_action_tools` (guardrails) stay one input of the
// policy: a tool needs an approval when that list names it OR when the
// policy row of the run's tier names its category. Both are administrator
// rules. An author cannot remove either one; the worker applies them to
// every call. The tool categories are domain/toolcategory.
package approvalpolicy

import (
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/toolcategory"
)

// Policy is the administrator's approval policy.
type Policy struct {
	// Categories maps a tier to the categories that need an approval.
	Categories map[agentlifecycle.Tier][]toolcategory.Category
	// RequireHITL lists the tiers whose pipelines must pass a `hitl` node
	// on every path to an external_send or payment tool node.
	RequireHITL []agentlifecycle.Tier
}

// DefaultCategories are the ADR defaults: delete, external_send and payment
// need an approval at the high and critical tiers.
func DefaultCategories(tier agentlifecycle.Tier) []toolcategory.Category {
	switch tier {
	case agentlifecycle.TierHigh, agentlifecycle.TierCritical:
		return []toolcategory.Category{toolcategory.Delete, toolcategory.ExternalSend, toolcategory.Payment}
	default:
		return nil
	}
}

// DefaultPolicy is the policy of a deployment that never saved the section.
// No tier requires a hitl node: the ADR says a tier policy CAN require one,
// and the hardening checklist recommends it for high and critical.
func DefaultPolicy() Policy {
	policy := Policy{Categories: map[agentlifecycle.Tier][]toolcategory.Category{}}
	for _, tier := range agentlifecycle.Tiers {
		policy.Categories[tier] = DefaultCategories(tier)
	}
	return policy
}

// CategoriesFor answers the categories that need an approval at tier, in the
// ADR's order and each once. An unknown tier is the lowest tier.
func (p Policy) CategoriesFor(tier agentlifecycle.Tier) []toolcategory.Category {
	if !tier.Valid() {
		tier = agentlifecycle.TierLow
	}
	return Normalize(p.Categories[tier])
}

// Requires reports whether a call of a tool of category needs an approval
// at tier.
func (p Policy) Requires(tier agentlifecycle.Tier, category toolcategory.Category) bool {
	for _, candidate := range p.CategoriesFor(tier) {
		if candidate == category {
			return true
		}
	}
	return false
}

// RequiresHITL reports whether pipelines of tier must pass a hitl node on
// every path to an external_send or payment tool node.
func (p Policy) RequiresHITL(tier agentlifecycle.Tier) bool {
	for _, required := range p.RequireHITL {
		if required == tier {
			return true
		}
	}
	return false
}

// Normalize keeps the known categories, each once, in the ADR's order.
func Normalize(categories []toolcategory.Category) []toolcategory.Category {
	seen := map[toolcategory.Category]bool{}
	for _, category := range categories {
		seen[category] = true
	}
	out := []toolcategory.Category{}
	for _, category := range toolcategory.All {
		if seen[category] {
			out = append(out, category)
		}
	}
	return out
}

// GatedByHITL reports whether a category is one the hitl rule covers.
func GatedByHITL(category toolcategory.Category) bool {
	return category == toolcategory.ExternalSend || category == toolcategory.Payment
}
