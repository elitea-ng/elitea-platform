package main

// governance_defaults.go — the adapter that lets the budget COUNTER engine read
// an AUTHORED budget row.
//
// The two planes are otherwise independent: internal/governance knows nothing
// about gateway.governance_config, and internal/policy knows nothing about NATS
// counters. This adapter is the one seam between them, and it lives in the
// composition root so neither package acquires a dependency on the other.
//
// It answers only for a project with NO gateway.project_budget row. A project
// with its own row keeps it — CheckBudget never reaches this adapter in that
// case — so an authored global ceiling cannot override a per-project one.

import (
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/policy"
)

// policyBudgetDefaults adapts the definition plane to the counter engine's
// BudgetDefaults port.
type policyBudgetDefaults struct {
	store *policy.Store
}

// DefaultBudgetNano returns the authored ceiling for projectID in nano-USD.
//
// ok is false when the store is absent, when no budget row selects the project,
// or when the row is unlimited. Every one of those restores the previous
// behaviour exactly: no row means no ceiling.
func (p policyBudgetDefaults) DefaultBudgetNano(projectID int) (int64, int, string, bool) {
	if p.store == nil {
		return 0, 0, "", false
	}
	def, ok := p.store.Current().DefaultBudget(projectID)
	if !ok {
		return 0, 0, "", false
	}
	limitNano, ok := def.LimitNanoUSD()
	if !ok {
		return 0, 0, "", false
	}
	return limitNano, def.SoftAlertPct, def.NATSFailMode, true
}
