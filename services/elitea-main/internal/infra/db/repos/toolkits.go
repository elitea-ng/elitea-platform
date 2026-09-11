package repos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalidCurrentToolkitRequest = errors.New("current toolkit request is invalid")
	ErrCurrentToolkitNotFound       = errors.New("current toolkit was not found")
	ErrInvalidCurrentToolkitRow     = errors.New("current toolkit row is invalid")
	ErrCurrentFolderAccessPartial   = errors.New("current folder access projection is incomplete")
)

// CurrentToolkit is the provider-neutral row stored in p_<project_id>.elitea_tools.
// Settings and Meta retain their JSON shapes; JSON numbers remain json.Number
// values so integral identifiers are not rounded through float64.
type CurrentToolkit struct {
	ID            int32
	CreatedAt     time.Time
	UpdatedAt     *time.Time
	Type          string
	Name          *string
	Description   *string
	Settings      any
	AuthorID      int32
	SharedOwnerID *int32
	SharedID      *int32
	Meta          any
}

type currentToolkitQueries interface {
	CurrentFolderAccessState(context.Context) (int32, error)
	GetCurrentMCPToolkitVisibleToActor(
		context.Context,
		sqlcgen.GetCurrentMCPToolkitVisibleToActorParams,
	) (sqlcgen.EliteaTool, error)
	GetCurrentToolkit(context.Context, int32) (sqlcgen.EliteaTool, error)
}

type currentToolkitQueryFactory func(sqlExecutor) (currentToolkitQueries, error)

// CurrentToolkitsRepository reads one current toolkit through an authorized
// project transaction. Name derivation, schema lookup, configuration expansion,
// secrets, and response enrichment belong to later application boundaries.
type CurrentToolkitsRepository struct {
	projects projectStore
	queries  currentToolkitQueryFactory
}

func NewCurrentToolkitsRepository(pool *pgxpool.Pool) (*CurrentToolkitsRepository, error) {
	projects, err := newPostgresProjectStore(pool)
	if err != nil {
		return nil, err
	}
	return newCurrentToolkitsRepository(projects, newCurrentToolkitQueries)
}

func newCurrentToolkitsRepository(
	projects projectStore,
	queries currentToolkitQueryFactory,
) (*CurrentToolkitsRepository, error) {
	if projects == nil || queries == nil {
		return nil, errors.New("current toolkit database is required")
	}
	return &CurrentToolkitsRepository{projects: projects, queries: queries}, nil
}

func newCurrentToolkitQueries(tx sqlExecutor) (currentToolkitQueries, error) {
	executor, ok := tx.(pgxExecutor)
	if !ok || executor.queryer == nil {
		return nil, errors.New("current toolkit transaction does not support generated queries")
	}
	return sqlcgen.New(executor.queryer), nil
}

func (r *CurrentToolkitsRepository) Get(
	ctx context.Context,
	projectID int32,
	toolkitID int32,
) (CurrentToolkit, error) {
	return r.get(ctx, projectID, toolkitID, nil)
}

// GetMCPVisible reads one toolkit with the current folder restriction for the
// authenticated actor. The social plugin is optional. An absent projection
// preserves project RBAC, while a partial projection fails closed.
func (r *CurrentToolkitsRepository) GetMCPVisible(
	ctx context.Context,
	projectID int32,
	actorID int32,
	toolkitID int32,
) (CurrentToolkit, error) {
	if actorID <= 0 {
		return CurrentToolkit{}, ErrInvalidCurrentToolkitRequest
	}
	return r.get(ctx, projectID, toolkitID, &actorID)
}

func (r *CurrentToolkitsRepository) get(
	ctx context.Context,
	projectID int32,
	toolkitID int32,
	actorID *int32,
) (CurrentToolkit, error) {
	if ctx == nil || projectID <= 0 || toolkitID <= 0 {
		return CurrentToolkit{}, ErrInvalidCurrentToolkitRequest
	}
	if err := ctx.Err(); err != nil {
		return CurrentToolkit{}, err
	}

	var toolkit CurrentToolkit
	err := r.projects.WithinProjectTx(ctx, int64(projectID), pgx.TxOptions{
		IsoLevel:   pgx.ReadCommitted,
		AccessMode: pgx.ReadOnly,
	}, func(tx sqlExecutor) error {
		queries, err := r.queries(tx)
		if err != nil {
			return err
		}
		var row sqlcgen.EliteaTool
		if actorID == nil {
			row, err = queries.GetCurrentToolkit(ctx, toolkitID)
		} else {
			var state int32
			state, err = queries.CurrentFolderAccessState(ctx)
			if err == nil {
				switch state {
				case 0:
					row, err = queries.GetCurrentToolkit(ctx, toolkitID)
				case 1:
					row, err = queries.GetCurrentMCPToolkitVisibleToActor(
						ctx,
						sqlcgen.GetCurrentMCPToolkitVisibleToActorParams{
							ToolkitID: toolkitID,
							ActorID:   *actorID,
						},
					)
				default:
					return ErrCurrentFolderAccessPartial
				}
			}
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCurrentToolkitNotFound
		}
		if err != nil {
			return fmt.Errorf("get current toolkit row: %w", err)
		}
		toolkit, err = mapCurrentToolkit(row, toolkitID)
		return err
	})
	if err != nil {
		return CurrentToolkit{}, err
	}
	return toolkit, nil
}

func mapCurrentToolkit(row sqlcgen.EliteaTool, toolkitID int32) (CurrentToolkit, error) {
	if row.ID != toolkitID || !row.CreatedAt.Valid {
		return CurrentToolkit{}, ErrInvalidCurrentToolkitRow
	}
	settings, err := decodeCurrentToolkitJSON(row.Settings, "settings")
	if err != nil {
		return CurrentToolkit{}, err
	}
	meta, err := decodeCurrentToolkitJSON(row.Meta, "metadata")
	if err != nil {
		return CurrentToolkit{}, err
	}

	var updatedAt *time.Time
	if row.UpdatedAt.Valid {
		value := row.UpdatedAt.Time
		updatedAt = &value
	}
	return CurrentToolkit{
		ID:            row.ID,
		CreatedAt:     row.CreatedAt.Time,
		UpdatedAt:     updatedAt,
		Type:          row.Type,
		Name:          row.Name,
		Description:   row.Description,
		Settings:      settings,
		AuthorID:      row.AuthorID,
		SharedOwnerID: row.SharedOwnerID,
		SharedID:      row.SharedID,
		Meta:          meta,
	}, nil
}

func decodeCurrentToolkitJSON(raw []byte, field string) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("%w: decode %s", ErrInvalidCurrentToolkitRow, field)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: decode %s", ErrInvalidCurrentToolkitRow, field)
	}
	return value, nil
}
