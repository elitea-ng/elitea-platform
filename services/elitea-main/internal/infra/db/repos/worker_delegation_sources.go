package repos

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workerdelegation"
)

// The delegation policy (ADR-0044 decision 3, application/workerdelegation)
// reads the command's input through the same sources as its egress policy,
// and the root agent's declared data classes from its registry row.
var (
	_ workerdelegation.InputReader     = (*WorkerEgressSources)(nil)
	_ workerdelegation.DataClassReader = (*WorkerEgressSources)(nil)
)

// AgentDataClasses reads the declared data classes of one agent's registry
// row (elitea_lifecycle.agent_registry, shared 0204). No row, or a database
// without the registry, is none. It reads through the pool: no admission
// writes the registry.
func (s *WorkerEgressSources) AgentDataClasses(ctx context.Context, projectID, applicationID int64) ([]string, error) {
	var classes []string
	err := s.pool.QueryRow(ctx, `
SELECT data_classes FROM elitea_lifecycle.agent_registry
WHERE tenant_project_id = $1 AND application_id = $2`, projectID, applicationID).Scan(&classes)
	if errors.Is(err, pgx.ErrNoRows) || undefinedTable(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("worker delegation: read the agent registry: %w", err)
	}
	return classes, nil
}
