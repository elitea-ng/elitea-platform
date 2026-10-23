package repos

import (
	"context"
	"errors"
	"fmt"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrCurrentPersonalProjectNotFound = errors.New("current personal project not found")

type currentExpansionScopeQueries interface {
	ResolveCurrentPersonalProjectID(context.Context, int32) (int32, error)
}

// CurrentExpansionScopeRepository supplies the current configured public
// project identity and resolves personal projects from the current project and
// auth membership tables. The public identity is injected from configuration;
// this adapter does not assume project 1.
type CurrentExpansionScopeRepository struct {
	queries         currentExpansionScopeQueries
	publicProjectID int32
}

func NewCurrentExpansionScopeRepository(
	pool *pgxpool.Pool,
	publicProjectID int32,
) (*CurrentExpansionScopeRepository, error) {
	if pool == nil {
		return nil, errors.New("current expansion scope database is required")
	}
	return newCurrentExpansionScopeRepository(sqlcgen.New(pool), publicProjectID)
}

func newCurrentExpansionScopeRepository(
	queries currentExpansionScopeQueries,
	publicProjectID int32,
) (*CurrentExpansionScopeRepository, error) {
	if queries == nil || publicProjectID <= 0 {
		return nil, errors.New("current expansion scope dependencies are required")
	}
	return &CurrentExpansionScopeRepository{
		queries:         queries,
		publicProjectID: publicProjectID,
	}, nil
}

func (r *CurrentExpansionScopeRepository) PublicProjectID(ctx context.Context) (int32, error) {
	if ctx == nil {
		return 0, configurationapp.ErrInvalidCurrentExpansion
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return r.publicProjectID, nil
}

func (r *CurrentExpansionScopeRepository) PersonalProjectID(ctx context.Context, userID int32) (int32, error) {
	if ctx == nil || userID <= 0 {
		return 0, configurationapp.ErrInvalidCurrentExpansion
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	projectID, err := r.queries.ResolveCurrentPersonalProjectID(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrCurrentPersonalProjectNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("resolve current personal project: %w", err)
	}
	if projectID <= 0 {
		return 0, errors.New("resolved current personal project is invalid")
	}
	return projectID, nil
}

var _ configurationapp.CurrentExpansionScope = (*CurrentExpansionScopeRepository)(nil)
