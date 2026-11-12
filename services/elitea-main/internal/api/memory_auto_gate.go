package api

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workerapprovalpolicy"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	dbrepos "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// memoryAutoGate is the agent save's memory write limit (ADR-0050 decision
// 1b, issue 1660): the limit from the agent lifecycle section and the
// agent's registry tier. The limit is read on each save that asks for auto:
// it is a rare, human-paced write.
type memoryAutoGate struct {
	pool  *pgxpool.Pool
	tiers workerapprovalpolicy.TierReader
	// err is why the gate could not be built. A gate that is not built
	// refuses every save that asks for auto (fail closed): returning no gate
	// would silently accept auto for every tier.
	err error
}

// newMemoryAutoGate is nil without a pool, which leaves the check off (a
// composition with no database has no registry to ask). A pool whose tier
// reader cannot be built answers a gate that refuses auto.
func newMemoryAutoGate(pool *pgxpool.Pool) *memoryAutoGate {
	if pool == nil {
		return nil
	}
	tiers, err := dbrepos.NewWorkerEgressSources(pool)
	if err != nil {
		slog.Error("agent memory: the auto write limit cannot read agent tiers; saves with write auto are refused", "error", err)
		return &memoryAutoGate{pool: pool, err: err}
	}
	return &memoryAutoGate{pool: pool, tiers: tiers}
}

func (g *memoryAutoGate) MaxTier(ctx context.Context) (agentlifecycle.Tier, error) {
	if g.err != nil {
		return "", g.err
	}
	return platformconfig.LoadMemoryAutoMaxTier(ctx, g.pool)
}

func (g *memoryAutoGate) AgentTier(ctx context.Context, projectID, applicationID int64) (agentlifecycle.Tier, error) {
	if g.err != nil {
		return "", g.err
	}
	return workerapprovalpolicy.ReadTier(ctx, g.tiers, projectID, applicationID)
}
