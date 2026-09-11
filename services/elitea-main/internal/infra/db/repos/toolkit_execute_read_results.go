package repos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"

	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const replayEventToolkitExecuteReadResult = "toolkit.execute.read.result"

type ToolkitExecuteReadResultsRepository struct {
	projects projectStore
}

func NewToolkitExecuteReadResultsRepository(
	pool *pgxpool.Pool,
) (*ToolkitExecuteReadResultsRepository, error) {
	projects, err := newPostgresProjectStore(pool)
	if err != nil {
		return nil, err
	}
	return newToolkitExecuteReadResultsRepository(projects)
}

func newToolkitExecuteReadResultsRepository(
	projects projectStore,
) (*ToolkitExecuteReadResultsRepository, error) {
	if projects == nil {
		return nil, errors.New("direct toolkit result project database is required")
	}
	return &ToolkitExecuteReadResultsRepository{projects: projects}, nil
}

func (r *ToolkitExecuteReadResultsRepository) ProjectToolkitExecuteRead(
	ctx context.Context,
	projection outputapp.ToolkitExecuteReadProjection,
) (outputapp.ProjectionOutcome, error) {
	if err := projection.Frame.Validate(); err != nil || projection.Frame.Sequence > math.MaxInt64 {
		return outputapp.ProjectionOutcome{}, err
	}
	record, projectID, err := toolkitExecuteReadOutputRecord(projection.Frame)
	if err != nil {
		return outputapp.ProjectionOutcome{}, err
	}
	var outcome outputapp.ProjectionOutcome
	cancellationWon := false
	err = r.projects.WithinProjectTx(
		ctx,
		projectID,
		pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite},
		func(tx sqlExecutor) error {
			existing, err := loadExistingOutput(ctx, tx, record)
			if err == nil {
				if sameDurableOutput(existing, record) {
					if err := verifyStoredToolkitExecuteReadResult(ctx, tx, record, projection.Frame.Result); err != nil {
						return err
					}
					cursor, err := replayCursor(ctx, tx, record.EventID)
					if err != nil {
						return fmt.Errorf("load replayed direct toolkit cursor: %w", err)
					}
					outcome = outputapp.ProjectionOutcome{Inserted: false, Cursor: cursor, CommittedSequence: record.Sequence}
					return nil
				}
				if sameCanonicalCancellation(existing, record) {
					cancellationWon = true
					return nil
				}
				return outputapp.ErrToolkitExecuteReadOutputConflict
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}

			inserted, err := insertOutputInbox(ctx, tx, record)
			if err != nil {
				return err
			}
			if !inserted.Inserted {
				existing, loadErr := loadExistingOutput(ctx, tx, record)
				if loadErr == nil {
					if sameDurableOutput(existing, record) {
						if err := verifyStoredToolkitExecuteReadResult(ctx, tx, record, projection.Frame.Result); err != nil {
							return err
						}
						cursor, err := replayCursor(ctx, tx, record.EventID)
						if err != nil {
							return err
						}
						outcome = outputapp.ProjectionOutcome{Inserted: false, Cursor: cursor, CommittedSequence: record.Sequence}
						return nil
					}
					if sameCanonicalCancellation(existing, record) {
						cancellationWon = true
						return nil
					}
					return outputapp.ErrToolkitExecuteReadOutputConflict
				}
				if !errors.Is(loadErr, pgx.ErrNoRows) {
					return loadErr
				}
				if inserted.CancellationRejected {
					if err := materializeCanonicalCancellation(ctx, tx, record, false); err != nil {
						return err
					}
					cancellationWon = true
					return nil
				}
				if inserted.DeadlineRejected {
					return outputapp.ErrOutputDeadlineExceeded
				}
				return runtimedomain.ErrStaleFence
			}

			queries, err := runtimeSQLCQueries(tx)
			if err != nil {
				return err
			}
			result := projection.Frame.Result
			if err := queries.InsertToolkitExecuteReadResult(
				ctx,
				sqlcgen.InsertToolkitExecuteReadResultParams{
					ExecutionID: record.ExecutionID, Generation: int64(record.Generation),
					EventID: record.EventID, ResultJson: append([]byte(nil), result.ResultJSON...),
					ToolkitType: result.ToolkitType, ToolkitName: result.ToolkitName, ToolName: result.ToolName,
				},
			); err != nil {
				return fmt.Errorf("insert direct toolkit result: %w", err)
			}
			cursor, err := appendReplayEvent(
				ctx, tx, record, replayEventToolkitExecuteReadResult,
				append([]byte(nil), result.ResultJSON...),
			)
			if err != nil {
				return err
			}
			if err := markOutputProjected(ctx, tx, record.EventID); err != nil {
				return err
			}
			outcome = outputapp.ProjectionOutcome{Inserted: true, Cursor: cursor, CommittedSequence: record.Sequence}
			return nil
		},
	)
	if err != nil {
		return outputapp.ProjectionOutcome{}, err
	}
	if cancellationWon {
		return outputapp.ProjectionOutcome{}, outputapp.ErrOutputCancelled
	}
	return outcome, nil
}

func toolkitExecuteReadOutputRecord(
	frame outputapp.ToolkitExecuteReadFrame,
) (outputRecord, int64, error) {
	resourceProjectID, err := parseProjectID(frame.ResourceProjectID)
	if err != nil {
		return outputRecord{}, 0, outputapp.ErrInvalidToolkitExecuteReadOutput
	}
	projectionProjectID, err := parseProjectID(frame.ProjectionProjectID)
	if err != nil {
		return outputRecord{}, 0, outputapp.ErrInvalidToolkitExecuteReadOutput
	}
	return frameOutputRecord(
		frame.EventID, frame.LogicalOutputID, frame.StreamID, frame.TenantID,
		resourceProjectID, projectionProjectID, frame.Sequence,
		frame.ClaimHandoffWatermark, frame.OccurredAt, frame.Fence,
		payloadTypeToolkitExecuteReadResult, frame.PayloadDigest, frame.EncodedResult,
		frame.Settlement, frame.EncodedSettlement,
	), projectionProjectID, nil
}

func verifyStoredToolkitExecuteReadResult(
	ctx context.Context,
	tx sqlExecutor,
	record outputRecord,
	wanted outputapp.ToolkitExecuteReadResult,
) error {
	queries, err := runtimeSQLCQueries(tx)
	if err != nil {
		return err
	}
	row, err := queries.GetToolkitExecuteReadCompletion(
		ctx,
		sqlcgen.GetToolkitExecuteReadCompletionParams{
			ExecutionID: record.ExecutionID, Generation: int64(record.Generation),
		},
	)
	if err != nil {
		return fmt.Errorf("load replayed direct toolkit result: %w", err)
	}
	if row.ToolkitType == nil || row.ToolkitName == nil || row.ToolName == nil ||
		!bytes.Equal(row.ResultJson, wanted.ResultJSON) || *row.ToolkitType != wanted.ToolkitType ||
		*row.ToolkitName != wanted.ToolkitName || *row.ToolName != wanted.ToolName {
		return outputapp.ErrToolkitExecuteReadOutputConflict
	}
	return nil
}

var _ outputapp.ToolkitExecuteReadProjector = (*ToolkitExecuteReadResultsRepository)(nil)
