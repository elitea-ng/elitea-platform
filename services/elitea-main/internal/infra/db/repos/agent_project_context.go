package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenant"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CurrentAgentProjectContextRepository serves the already authorized admission
// and claim-bound nested version reads. The tenant and actor come from Main.
type CurrentAgentProjectContextRepository struct{ tenants *tenant.Executor }

func NewCurrentAgentProjectContextRepository(pool *pgxpool.Pool) *CurrentAgentProjectContextRepository {
	return &CurrentAgentProjectContextRepository{tenants: tenant.NewExecutor(pool)}
}

func (repository *CurrentAgentProjectContextRepository) ResolveCurrentAgentProjectContext(ctx context.Context, projectID, actorID int32) (agentexecutionapp.CurrentAgentProjectContext, error) {
	var result agentexecutionapp.CurrentAgentProjectContext
	if repository == nil || repository.tenants == nil || ctx == nil || projectID <= 0 || actorID <= 0 {
		return result, errors.New("project context admission identity is required")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := repository.tenants.WithinTx(ctx, tenant.Project{ID: int64(projectID)}, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var data []byte
		err := tx.QueryRow(ctx, `SELECT id, data FROM configuration WHERE project_id=$1 AND type='project_context' ORDER BY id LIMIT 1`, projectID).Scan(&result.ID, &data)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read project context snapshot: %w", err)
		}
		source := struct {
			Content               string `json:"content"`
			Enabled               bool   `json:"enabled"`
			ActivationDescription string `json:"activation_description"`
		}{Enabled: true}
		if err := json.Unmarshal(data, &source); err != nil {
			return fmt.Errorf("decode project context snapshot: %w", err)
		}
		result.Content, result.Enabled, result.ActivationDescription = source.Content, source.Enabled, source.ActivationDescription
		return nil
	})
	return result, err
}
