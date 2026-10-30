package repos

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workerapprovalpolicy"
)

// The approval policy (ADR-0041 decision 5, application/workerapprovalpolicy)
// reads the command's input through the same sources as its egress policy,
// and the root agent's risk tier from its registry row.
var _ workerapprovalpolicy.TierReader = (*WorkerEgressSources)(nil)

// AgentRiskTier reads the risk tier of one agent's registry row
// (elitea_lifecycle.agent_registry, shared 0204). No row, or a database
// without the registry, is the empty tier, which the policy reads as low. It
// reads through the pool: no admission writes the registry.
func (s *WorkerEgressSources) AgentRiskTier(ctx context.Context, projectID, applicationID int64) (string, error) {
	var tier string
	err := s.pool.QueryRow(ctx, `
SELECT risk_tier FROM elitea_lifecycle.agent_registry
WHERE tenant_project_id = $1 AND application_id = $2`, projectID, applicationID).Scan(&tier)
	if errors.Is(err, pgx.ErrNoRows) || undefinedTable(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("worker approval policy: read the agent registry: %w", err)
	}
	return tier, nil
}
