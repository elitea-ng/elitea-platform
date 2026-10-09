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

type CurrentAgentCancelRepository struct {
	projects projectStore
}

func NewCurrentAgentCancelRepository(pool *pgxpool.Pool) (*CurrentAgentCancelRepository, error) {
	projects, err := newPostgresProjectStore(pool)
	if err != nil {
		return nil, err
	}
	return newCurrentAgentCancelRepository(projects)
}

func newCurrentAgentCancelRepository(projects projectStore) (*CurrentAgentCancelRepository, error) {
	if projects == nil {
		return nil, errors.New("current agent cancellation project database is required")
	}
	return &CurrentAgentCancelRepository{projects: projects}, nil
}

type currentAgentCancelQuerier interface {
	CancelCurrentAgentExecution(
		context.Context,
		sqlcgen.CancelCurrentAgentExecutionParams,
	) (sqlcgen.CancelCurrentAgentExecutionRow, error)
	ProjectCurrentAgentStop(
		context.Context,
		sqlcgen.ProjectCurrentAgentStopParams,
	) (sqlcgen.ProjectCurrentAgentStopRow, error)
	IsCurrentAgentCancellationReplay(
		context.Context,
		sqlcgen.IsCurrentAgentCancellationReplayParams,
	) (bool, error)
}

func (repository *CurrentAgentCancelRepository) CancelCurrentAgent(
	ctx context.Context,
	request agentexecutionapp.CurrentAgentCancelRequest,
) (agentexecutionapp.CurrentAgentCancelOutcome, error) {
	if repository == nil || repository.projects == nil || request.Validate() != nil {
		return agentexecutionapp.CurrentAgentCancelOutcome{}, agentexecutionapp.ErrInvalidCurrentAgentCancel
	}
	projectID, valid := currentAgentDatabaseID(request.ProjectID)
	if !valid {
		return agentexecutionapp.CurrentAgentCancelOutcome{}, agentexecutionapp.ErrInvalidCurrentAgentCancel
	}
	responseMessageID, err := currentPGUUID(request.ResponseMessageID)
	if err != nil {
		return agentexecutionapp.CurrentAgentCancelOutcome{}, agentexecutionapp.ErrInvalidCurrentAgentCancel
	}

	var outcome agentexecutionapp.CurrentAgentCancelOutcome
	err = repository.projects.WithinProjectTx(
		ctx,
		request.ProjectID,
		pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite},
		func(tx sqlExecutor) error {
			queries, ok := tx.(currentAgentCancelQuerier)
			if !ok {
				return errors.New("current agent cancellation query is unavailable")
			}
			isReplay := func() (bool, error) {
				replay, replayErr := queries.IsCurrentAgentCancellationReplay(
					ctx,
					sqlcgen.IsCurrentAgentCancellationReplayParams{
						ResponseMessageID: request.ResponseMessageID,
						ProjectID:         projectID,
						ActorUserID:       request.ActorUserID,
					},
				)
				if replayErr != nil {
					return false, fmt.Errorf("resolve current agent cancellation replay: %w", replayErr)
				}
				return replay, nil
			}
			row, queryErr := queries.CancelCurrentAgentExecution(
				ctx,
				sqlcgen.CancelCurrentAgentExecutionParams{
					ResponseMessageID: responseMessageID,
					ProjectID:         projectID,
					ActorUserID:       request.ActorUserID,
				},
			)
			if errors.Is(queryErr, pgx.ErrNoRows) {
				replay, replayErr := isReplay()
				if replayErr != nil {
					return replayErr
				}
				if !replay {
					return agentexecutionapp.ErrCurrentAgentCancelNotAllowed
				}
				outcome.Replay = true
				return nil
			}
			if queryErr != nil {
				return fmt.Errorf("cancel current agent execution: %w", queryErr)
			}
			if row.QuestionMessageGroupID == nil || *row.QuestionMessageGroupID <= 0 ||
				row.ResponseMessageGroupID <= 0 || row.ExecutionID == "" ||
				row.ClientExecutionGeneration == "" {
				return errors.New("current agent cancellation target is incomplete")
			}

			if err := cancelNodeRecovery(ctx, tx, row.ExecutionID, uint64(row.Generation)); err != nil {
				return err
			}
			if err := cancelCodeToolkitChildren(ctx, tx, row.ExecutionID, int64(row.Generation)); err != nil {
				return err
			}

			projection, projectionErr := queries.ProjectCurrentAgentStop(
				ctx,
				sqlcgen.ProjectCurrentAgentStopParams{
					ResponseMessageGroupID: row.ResponseMessageGroupID,
					QuestionMessageGroupID: *row.QuestionMessageGroupID,
					ExecutionID:            row.ExecutionID,
					ExecutionGeneration:    row.ClientExecutionGeneration,
				},
			)
			if projectionErr != nil {
				return fmt.Errorf("project current agent stop: %w", projectionErr)
			}
			if projection.Deleted {
				if !projection.QuestionDeleted || projection.Retained || projection.Salvaged {
					return errors.New("current agent cancellation deleted an incomplete pair")
				}
				outcome.Deleted = true
				return nil
			}
			if !projection.Deleted && !projection.Retained && !projection.Salvaged &&
				!projection.QuestionDeleted {
				// A concurrent stop of the same empty turn: this statement's
				// target CTE saw the answer before the other stop committed,
				// waited on the job row and matched its CANCELLED desired
				// state, but the other stop has since deleted the answer and
				// its question, so the projection finds nothing. For a caller
				// the replay check admits that is the repeated stop the
				// contract answers 204.
				replay, replayErr := isReplay()
				if replayErr != nil {
					return replayErr
				}
				if replay {
					outcome.Replay = true
					return nil
				}
			}
			if !projection.Retained || projection.QuestionDeleted {
				return errors.New("current agent cancellation projection did not settle")
			}
			outcome.Salvaged = projection.Salvaged
			return nil
		},
	)
	if err != nil {
		return agentexecutionapp.CurrentAgentCancelOutcome{}, err
	}
	return outcome, nil
}

var _ agentexecutionapp.CurrentAgentCancellationStore = (*CurrentAgentCancelRepository)(nil)
