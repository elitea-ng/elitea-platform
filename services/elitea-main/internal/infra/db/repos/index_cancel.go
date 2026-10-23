package repos

import (
	"context"
	"errors"
	"fmt"
	"math"

	indexingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexing"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5/pgxpool"
)

type currentIndexCancellationQueries interface {
	RequestCurrentIndexIngestCancellation(
		context.Context,
		sqlcgen.RequestCurrentIndexIngestCancellationParams,
	) (bool, error)
}

// CurrentIndexCancellationRepository changes only the durable desired state of
// the exact active index execution. Worker settlement owns terminal state; a
// stopped run's vectors stay in elitea-vector (indexing.RegistryManualStopCleaner
// deletes none).
type CurrentIndexCancellationRepository struct {
	queries currentIndexCancellationQueries
}

func NewCurrentIndexCancellationRepository(
	pool *pgxpool.Pool,
) (*CurrentIndexCancellationRepository, error) {
	if pool == nil {
		return nil, errors.New("current index cancellation database is required")
	}
	return newCurrentIndexCancellationRepository(sqlcgen.New(pool))
}

func newCurrentIndexCancellationRepository(
	queries currentIndexCancellationQueries,
) (*CurrentIndexCancellationRepository, error) {
	if queries == nil {
		return nil, errors.New("current index cancellation database is required")
	}
	return &CurrentIndexCancellationRepository{queries: queries}, nil
}

func (r *CurrentIndexCancellationRepository) RequestCurrentIndexCancellation(
	ctx context.Context,
	request indexingapp.CurrentIndexCancelRequest,
) (bool, error) {
	if r == nil || r.queries == nil || ctx == nil {
		return false, indexingapp.ErrInvalidCurrentIndexCancel
	}
	if err := request.Validate(); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	projectID, ok := currentIndexCancellationDatabaseID(request.ProjectID)
	if !ok {
		return false, indexingapp.ErrInvalidCurrentIndexCancel
	}
	toolkitID, ok := currentIndexCancellationDatabaseID(request.ToolkitID)
	if !ok {
		return false, indexingapp.ErrInvalidCurrentIndexCancel
	}

	transitioned, err := r.queries.RequestCurrentIndexIngestCancellation(
		ctx,
		sqlcgen.RequestCurrentIndexIngestCancellationParams{
			ExecutionID:       request.ExecutionID,
			ResourceProjectID: projectID,
			ToolkitID:         toolkitID,
			IndexName:         request.IndexName,
		},
	)
	if err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return false, contextError
		}
		return false, fmt.Errorf("request current index cancellation: %w", err)
	}
	return transitioned, nil
}

func currentIndexCancellationDatabaseID(value int64) (int32, bool) {
	if value <= 0 || value > math.MaxInt32 {
		return 0, false
	}
	return int32(value), true
}

var _ indexingapp.CurrentIndexCancellationStore = (*CurrentIndexCancellationRepository)(nil)
