package repos

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"

	indexscheduleapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexschedule"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type currentIndexScheduleDeleteQueries interface {
	LockCurrentIndexScheduleToolkitMeta(context.Context, int32) ([]byte, error)
	UpdateCurrentIndexScheduleToolkitMeta(
		context.Context,
		sqlcgen.UpdateCurrentIndexScheduleToolkitMetaParams,
	) (int64, error)
}

type currentIndexScheduleDeleteQueryFactory func(
	sqlExecutor,
) (currentIndexScheduleDeleteQueries, error)

type CurrentIndexScheduleDeleteRepository struct {
	projects projectStore
	queries  currentIndexScheduleDeleteQueryFactory
}

func NewCurrentIndexScheduleDeleteRepository(
	pool *pgxpool.Pool,
) (*CurrentIndexScheduleDeleteRepository, error) {
	projects, err := newPostgresProjectStore(pool)
	if err != nil {
		return nil, err
	}
	return newCurrentIndexScheduleDeleteRepository(
		projects,
		newCurrentIndexScheduleDeleteQueries,
	)
}

func newCurrentIndexScheduleDeleteRepository(
	projects projectStore,
	queries currentIndexScheduleDeleteQueryFactory,
) (*CurrentIndexScheduleDeleteRepository, error) {
	if projects == nil || queries == nil {
		return nil, errors.New("current index schedule delete database is required")
	}
	return &CurrentIndexScheduleDeleteRepository{
		projects: projects,
		queries:  queries,
	}, nil
}

func newCurrentIndexScheduleDeleteQueries(
	tx sqlExecutor,
) (currentIndexScheduleDeleteQueries, error) {
	executor, ok := tx.(pgxExecutor)
	if !ok || executor.queryer == nil {
		return nil, errors.New(
			"current index schedule delete transaction does not support generated queries",
		)
	}
	return sqlcgen.New(executor.queryer), nil
}

func (repository *CurrentIndexScheduleDeleteRepository) Delete(
	ctx context.Context,
	mutation indexscheduleapp.DeleteMutation,
) (indexscheduleapp.DeleteResult, error) {
	if repository == nil || repository.projects == nil || ctx == nil ||
		mutation.ProjectID <= 0 || mutation.ProjectID > math.MaxInt32 ||
		mutation.ToolkitID <= 0 || mutation.ToolkitID > math.MaxInt32 ||
		mutation.IndexMetaID == "" || mutation.TargetKey == "" ||
		len(mutation.TargetKey) > 64 ||
		strings.ContainsAny(mutation.TargetKey, "\x00\r\n") {
		return indexscheduleapp.DeleteResult{}, indexscheduleapp.ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return indexscheduleapp.DeleteResult{}, err
	}

	var result indexscheduleapp.DeleteResult
	err := repository.projects.WithinProjectTx(
		ctx,
		mutation.ProjectID,
		pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite},
		func(tx sqlExecutor) error {
			queries, err := repository.queries(tx)
			if err != nil {
				return indexscheduleapp.ErrScheduleUnavailable
			}
			metaRaw, err := queries.LockCurrentIndexScheduleToolkitMeta(
				ctx,
				int32(mutation.ToolkitID),
			)
			if errors.Is(err, pgx.ErrNoRows) {
				return indexscheduleapp.ErrToolkitNotFound
			}
			if err != nil {
				return indexscheduleapp.ErrScheduleUnavailable
			}
			if len(metaRaw) == 0 ||
				len(metaRaw) > maxCurrentIndexScheduleMetadataBytes {
				return indexscheduleapp.ErrScheduleResultTooLarge
			}
			meta, err := decodeCurrentScheduleObject(metaRaw)
			if err != nil {
				return err
			}
			indexes, err := currentScheduleObject(meta, "indexes_meta")
			if err != nil {
				return err
			}
			rawIndex, exists := indexes[mutation.IndexMetaID]
			index, ok := rawIndex.(map[string]any)
			if !exists || !ok || len(index) == 0 {
				return indexscheduleapp.ErrScheduleIndexNotFound
			}
			schedules, err := currentScheduleObject(index, "schedules")
			if err != nil {
				return err
			}
			if _, exists := schedules[mutation.TargetKey]; !exists {
				return indexscheduleapp.ErrScheduleUserNotFound
			}
			delete(schedules, mutation.TargetKey)
			index["schedules"] = schedules
			indexes[mutation.IndexMetaID] = index
			meta["indexes_meta"] = indexes
			encoded, err := json.Marshal(meta)
			if err != nil {
				return indexscheduleapp.ErrInvalidToolkit
			}
			if len(encoded) > maxCurrentIndexScheduleMetadataBytes {
				return indexscheduleapp.ErrScheduleResultTooLarge
			}
			updated, err := queries.UpdateCurrentIndexScheduleToolkitMeta(
				ctx,
				sqlcgen.UpdateCurrentIndexScheduleToolkitMetaParams{
					Meta:      encoded,
					ToolkitID: int32(mutation.ToolkitID),
				},
			)
			if err != nil || updated != 1 {
				return indexscheduleapp.ErrScheduleUnavailable
			}
			result = indexscheduleapp.DeleteResult{IndexesMeta: indexes}
			return nil
		},
	)
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled),
			errors.Is(err, context.DeadlineExceeded),
			errors.Is(err, indexscheduleapp.ErrInvalidRequest),
			errors.Is(err, indexscheduleapp.ErrToolkitNotFound),
			errors.Is(err, indexscheduleapp.ErrScheduleIndexNotFound),
			errors.Is(err, indexscheduleapp.ErrScheduleUserNotFound),
			errors.Is(err, indexscheduleapp.ErrInvalidToolkit),
			errors.Is(err, indexscheduleapp.ErrScheduleResultTooLarge):
			return indexscheduleapp.DeleteResult{}, err
		default:
			return indexscheduleapp.DeleteResult{}, indexscheduleapp.ErrScheduleUnavailable
		}
	}
	return result, nil
}

var _ indexscheduleapp.DeleteStore = (*CurrentIndexScheduleDeleteRepository)(nil)
