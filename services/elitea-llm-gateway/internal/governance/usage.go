package governance

// usage.go — the read-only budget-consumption view the authored-governance
// plane needs for the CEL `budget_used` variable (internal/policy).
//
// It is deliberately NOT part of CheckBudget. CheckBudget is an admission
// decision with a fail-mode FSM behind it; this is an observation, it is
// consulted before dispatch on a request that has not been admitted yet, and it
// must never refuse anything. Keeping them apart stops a routing rule from
// acquiring the power to block a request through the back door.

import (
	"context"
	"errors"
	"log/slog"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/failmode"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/infra/nats"
)

// BudgetFraction returns the fraction of the project's ceiling consumed in the
// current period, and whether a ceiling exists to divide by.
//
// ok is false — and the caller renders 0 — when the project is unlimited, has
// no budget row, or the snapshot cannot be read. A routing rule must not fail a
// request, so every unreadable state resolves to "nothing consumed" rather than
// to an error. That direction is safe here BECAUSE this value only ever selects
// a route: the spend control is CheckBudget, which fails closed on the same
// unreadable snapshot.
//
// The returned value is a float64 and is the ONLY float on this path. It is a
// RATIO, not money: both operands stay int64 nano-USD and the division happens
// once, at the boundary, to produce a CEL input. No money value is derived back
// out of it.
func (g *GovernanceStore) BudgetFraction(
	ctx context.Context,
	projectID int,
	scope, scopeID string,
	periodStartUnix int64,
) (float64, bool) {
	if g == nil {
		return 0, false
	}
	snap, err := g.store.ReadSnapshot(ctx, projectID, scope, scopeID, periodStartUnix)
	if err != nil {
		if !errors.Is(err, failmode.ErrNoBudgetRow) {
			g.log.Debug("governance: budget fraction unavailable; routing sees budget_used = 0",
				slog.Int("project_id", projectID), slog.Any("err", err))
		}
		return 0, false
	}
	if snap.IsUnlimited || snap.HardLimitNano <= 0 {
		return 0, false
	}

	// Prefer the authoritative counter; fall back to the durable snapshot when
	// NATS is unreachable. The fallback UNDER-reports (it misses spend not yet
	// written behind), which is the right direction for a routing input: a rule
	// that sends heavy spenders to a cheaper model should not fire on a number
	// the gateway cannot stand behind.
	usedNano := snap.AccumulatedNano
	if authoritative, natsErr := g.nc.ReadBudget(ctx, nats.BudgetSubject(scope, scopeID, periodStartUnix)); natsErr == nil {
		usedNano = authoritative
	}
	if usedNano < 0 {
		usedNano = 0
	}
	return float64(usedNano) / float64(snap.HardLimitNano), true
}
