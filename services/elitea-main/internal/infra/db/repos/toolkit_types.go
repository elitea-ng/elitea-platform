package repos

import (
	"context"
	"errors"
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrInvalidCurrentToolkitTypesRequest = errors.New("current toolkit types request is invalid")

type currentToolkitTypesQueries interface {
	ListCurrentToolkitTypes(
		context.Context,
		sqlcgen.ListCurrentToolkitTypesParams,
	) ([]string, error)
}

type currentToolkitTypesQueryFactory func(sqlExecutor) (currentToolkitTypesQueries, error)

// CurrentToolkitTypesRepository lists the distinct toolkit types stored in one
// authorized current tenant. The project store, rather than a caller-provided
// schema name, owns selection of p_<project_id>.
type CurrentToolkitTypesRepository struct {
	projects projectStore
	queries  currentToolkitTypesQueryFactory
}

func NewCurrentToolkitTypesRepository(pool *pgxpool.Pool) (*CurrentToolkitTypesRepository, error) {
	projects, err := newPostgresProjectStore(pool)
	if err != nil {
		return nil, err
	}
	return newCurrentToolkitTypesRepository(projects, newCurrentToolkitTypesQueries)
}

func newCurrentToolkitTypesRepository(
	projects projectStore,
	queries currentToolkitTypesQueryFactory,
) (*CurrentToolkitTypesRepository, error) {
	if projects == nil || queries == nil {
		return nil, errors.New("current toolkit types database is required")
	}
	return &CurrentToolkitTypesRepository{projects: projects, queries: queries}, nil
}

func newCurrentToolkitTypesQueries(tx sqlExecutor) (currentToolkitTypesQueries, error) {
	executor, ok := tx.(pgxExecutor)
	if !ok || executor.queryer == nil {
		return nil, errors.New("current toolkit types transaction does not support generated queries")
	}
	return sqlcgen.New(executor.queryer), nil
}

func (repository *CurrentToolkitTypesRepository) ListCurrentToolkitTypes(
	ctx context.Context,
	projectID int32,
	filterMCP bool,
	filterApplication bool,
) ([]string, error) {
	if ctx == nil || projectID <= 0 {
		return nil, ErrInvalidCurrentToolkitTypesRequest
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	types := []string{}
	err := repository.projects.WithinProjectTx(ctx, int64(projectID), pgx.TxOptions{
		IsoLevel:   pgx.ReadCommitted,
		AccessMode: pgx.ReadOnly,
	}, func(tx sqlExecutor) error {
		queries, err := repository.queries(tx)
		if err != nil {
			return err
		}
		types, err = queries.ListCurrentToolkitTypes(ctx, sqlcgen.ListCurrentToolkitTypesParams{
			FilterMcp:         filterMCP,
			FilterApplication: filterApplication,
		})
		if err != nil {
			return fmt.Errorf("list current toolkit types: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return types, nil
}
