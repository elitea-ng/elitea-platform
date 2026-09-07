package repos

import (
	"context"
	"errors"
	"fmt"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CurrentAgentTaskStatusRepository reads the durable state of ONE agent
// execution through the response message it is bound to. It is the read twin
// of CurrentAgentCancelRepository and reuses that repository's exact ownership
// predicate; see internal/db/queries/agent_task_status.sql.
type CurrentAgentTaskStatusRepository struct {
	projects projectStore
}

func NewCurrentAgentTaskStatusRepository(pool *pgxpool.Pool) (*CurrentAgentTaskStatusRepository, error) {
	projects, err := newPostgresProjectStore(pool)
	if err != nil {
		return nil, err
	}
	return newCurrentAgentTaskStatusRepository(projects)
}

func newCurrentAgentTaskStatusRepository(projects projectStore) (*CurrentAgentTaskStatusRepository, error) {
	if projects == nil {
		return nil, errors.New("current agent task status project database is required")
	}
	return &CurrentAgentTaskStatusRepository{projects: projects}, nil
}

type currentAgentTaskStatusQuerier interface {
	GetCurrentAgentTaskStatus(
		context.Context,
		sqlcgen.GetCurrentAgentTaskStatusParams,
	) (sqlcgen.GetCurrentAgentTaskStatusRow, error)
}

func (repository *CurrentAgentTaskStatusRepository) ReadCurrentAgentTaskState(
	ctx context.Context,
	request agentexecutionapp.CurrentAgentTaskStatusRequest,
) (agentexecutionapp.CurrentAgentTaskState, error) {
	if repository == nil || repository.projects == nil || request.Validate() != nil {
		return agentexecutionapp.CurrentAgentTaskState{}, agentexecutionapp.ErrInvalidCurrentAgentTaskStatus
	}
	projectID, valid := currentAgentDatabaseID(request.ProjectID)
	if !valid {
		return agentexecutionapp.CurrentAgentTaskState{}, agentexecutionapp.ErrInvalidCurrentAgentTaskStatus
	}
	responseMessageID, err := currentPGUUID(request.ResponseMessageID)
	if err != nil {
		return agentexecutionapp.CurrentAgentTaskState{}, agentexecutionapp.ErrInvalidCurrentAgentTaskStatus
	}

	var state agentexecutionapp.CurrentAgentTaskState
	err = repository.projects.WithinProjectTx(
		ctx,
		request.ProjectID,
		// Read-only. The status poll must never be able to move a job, and a
		// read-write transaction on this path would give a poller the same
		// lock footprint as the canceller.
		pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly},
		func(tx sqlExecutor) error {
			queries, ok := tx.(currentAgentTaskStatusQuerier)
			if !ok {
				return errors.New("current agent task status query is unavailable")
			}
			row, queryErr := queries.GetCurrentAgentTaskStatus(
				ctx,
				sqlcgen.GetCurrentAgentTaskStatusParams{
					ResponseMessageID: responseMessageID,
					ProjectID:         projectID,
					ActorUserID:       request.ActorUserID,
				},
			)
			if errors.Is(queryErr, pgx.ErrNoRows) {
				// Absent, bound to another project, or owned by another actor.
				// The three are deliberately one outcome.
				return agentexecutionapp.ErrCurrentAgentTaskStatusNotFound
			}
			if queryErr != nil {
				return fmt.Errorf("read current agent task status: %w", queryErr)
			}
			state.State = row.State
			state.DesiredState = row.DesiredState
			if row.SettledAt.Valid {
				settledAt := row.SettledAt.Time.UTC()
				state.SettledAt = &settledAt
			}
			if row.TerminalErrorCode != nil {
				state.TerminalErrorCode = *row.TerminalErrorCode
			}
			return nil
		},
	)
	if err != nil {
		return agentexecutionapp.CurrentAgentTaskState{}, err
	}
	return state, nil
}

var _ agentexecutionapp.CurrentAgentTaskStatusStore = (*CurrentAgentTaskStatusRepository)(nil)
