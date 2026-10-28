package repos

import (
	"context"
	"errors"
	"fmt"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	interruptdomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/executioninterrupt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type CurrentAgentCancelRepository struct {
	projects projectStore
	signals  *ExecutionInterruptSignals
}

// SetInterruptSignals installs the after-commit live wake for the
// agent_hitl_resolved frames a stop emits. Call it during composition.
func (repository *CurrentAgentCancelRepository) SetInterruptSignals(signals *ExecutionInterruptSignals) {
	repository.signals = signals
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
	var notice resolvedNotice
	err = repository.projects.WithinProjectTx(
		ctx,
		request.ProjectID,
		pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite},
		func(tx sqlExecutor) error {
			notice = resolvedNotice{}
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

			// The conversation of the turn, read while its rows exist: a stop
			// that deletes the turn deletes its steering in that conversation.
			// A concurrent stop may have deleted them already: then this stop
			// deletes nothing, and the id is not used.
			var conversationID int64
			if err := tx.QueryRow(ctx, `SELECT conversation_id FROM chat_message_group WHERE id = $1`,
				row.ResponseMessageGroupID).Scan(&conversationID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("read the stopped turn's conversation: %w", err)
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
			// Only a fresh send stopped before output is deleted, always with
			// its question. A regeneration's question is the original,
			// already answered one: the projection keeps the pair and settles
			// the empty answer, so it reaches the Retained check below
			// (#1093).
			if projection.Deleted {
				if !projection.QuestionDeleted || projection.Retained || projection.Salvaged {
					return errors.New("current agent cancellation deleted an incomplete pair")
				}
				outcome.Deleted = true
				var closeErr error
				notice, closeErr = cancelOpenInterruptCards(ctx, tx, request)
				if closeErr != nil {
					return closeErr
				}
				// The steering of the deleted turn goes with it, now.
				return DiscardTurnSteering(ctx, tx, int64(projectID), conversationID, request.ResponseMessageID)
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
			var closeErr error
			notice, closeErr = cancelOpenInterruptCards(ctx, tx, request)
			return closeErr
		},
	)
	if err != nil {
		return agentexecutionapp.CurrentAgentCancelOutcome{}, err
	}
	repository.signals.resolved(ctx, notice)
	return outcome, nil
}

var _ agentexecutionapp.CurrentAgentCancellationStore = (*CurrentAgentCancelRepository)(nil)

// cancelOpenInterruptCards closes the response's open per-interrupt cards in
// the stop transaction (fanout-interrupt-decisions §5: atomic with the stop),
// after the response row lock is held, and emits agent_hitl_resolved for each.
// The cancelled fence then refuses any late ACK.
func cancelOpenInterruptCards(ctx context.Context, tx sqlExecutor, request agentexecutionapp.CurrentAgentCancelRequest) (resolvedNotice, error) {
	_, notice, err := closeOpenExecutionInterrupts(ctx, tx, request.ResponseMessageID, interruptdomain.StateCancelled,
		interruptdomain.Closer{ActorUserID: request.ActorUserID})
	if err != nil {
		return resolvedNotice{}, fmt.Errorf("cancel open interrupt cards: %w", err)
	}
	return notice, nil
}

type governedRefusalQuerier interface {
	CancelCurrentAgentExecution(
		context.Context,
		sqlcgen.CancelCurrentAgentExecutionParams,
	) (sqlcgen.CancelCurrentAgentExecutionRow, error)
	ProjectGovernedContinuationRefusal(
		context.Context,
		sqlcgen.ProjectGovernedContinuationRefusalParams,
	) (int32, error)
}

// SettleGovernedRefusal settles the paused turn of a governed continuation
// that admission refused (#1485). It cancels the paused execution with the
// same ownership-checked statement a stop uses, then projects the answer as
// an error with the pause keys cleared. Unlike a stop it never deletes the
// turn. settled=false when the stop statement found nothing to cancel: the
// turn was settled or stopped already, or the actor may not stop it.
func (repository *CurrentAgentCancelRepository) SettleGovernedRefusal(
	ctx context.Context,
	request agentexecutionapp.GovernedRefusalSettlement,
) (bool, error) {
	cancel := agentexecutionapp.CurrentAgentCancelRequest{
		ProjectID: request.ProjectID, ActorUserID: request.ActorUserID,
		ResponseMessageID: request.ResponseMessageID,
	}
	if repository == nil || repository.projects == nil || cancel.Validate() != nil ||
		request.Reason == "" || len(request.Reason) > 256 || request.Code == "" {
		return false, agentexecutionapp.ErrInvalidCurrentAgentCancel
	}
	projectID, valid := currentAgentDatabaseID(request.ProjectID)
	if !valid {
		return false, agentexecutionapp.ErrInvalidCurrentAgentCancel
	}
	responseMessageID, err := currentPGUUID(request.ResponseMessageID)
	if err != nil {
		return false, agentexecutionapp.ErrInvalidCurrentAgentCancel
	}
	settled := false
	err = repository.projects.WithinProjectTx(
		ctx,
		request.ProjectID,
		pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite},
		func(tx sqlExecutor) error {
			queries, ok := tx.(governedRefusalQuerier)
			if !ok {
				return errors.New("governed refusal settlement query is unavailable")
			}
			row, queryErr := queries.CancelCurrentAgentExecution(ctx, sqlcgen.CancelCurrentAgentExecutionParams{
				ResponseMessageID: responseMessageID,
				ProjectID:         projectID,
				ActorUserID:       request.ActorUserID,
			})
			if errors.Is(queryErr, pgx.ErrNoRows) {
				return nil
			}
			if queryErr != nil {
				return fmt.Errorf("cancel the refused governed continuation: %w", queryErr)
			}
			if row.ResponseMessageGroupID <= 0 || row.ExecutionID == "" || row.ClientExecutionGeneration == "" {
				return errors.New("governed refusal settlement target is incomplete")
			}
			if err := cancelNodeRecovery(ctx, tx, row.ExecutionID, uint64(row.Generation)); err != nil {
				return err
			}
			if err := cancelCodeToolkitChildren(ctx, tx, row.ExecutionID, int64(row.Generation)); err != nil {
				return err
			}
			_, projectionErr := queries.ProjectGovernedContinuationRefusal(ctx, sqlcgen.ProjectGovernedContinuationRefusalParams{
				ErrorMessage:           request.Reason,
				ErrorCode:              agentexecutionapp.GovernedRefusalErrorCode,
				RefusalCode:            request.Code,
				ResponseMessageGroupID: row.ResponseMessageGroupID,
				ExecutionID:            row.ExecutionID,
				ExecutionGeneration:    row.ClientExecutionGeneration,
			})
			if errors.Is(projectionErr, pgx.ErrNoRows) {
				// The answer is gone (a concurrent stop deleted the turn):
				// the job is cancelled and there is nothing left to project.
				return nil
			}
			if projectionErr != nil {
				return fmt.Errorf("project the refused governed continuation: %w", projectionErr)
			}
			settled = true
			return nil
		},
	)
	if err != nil {
		return false, err
	}
	return settled, nil
}

var _ agentexecutionapp.GovernedRefusalSettler = (*CurrentAgentCancelRepository)(nil)
