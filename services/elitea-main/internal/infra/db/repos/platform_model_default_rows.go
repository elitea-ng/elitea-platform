package repos

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
)

// PlatformModelDefaultRowsRepository holds the two reads the platform default
// model release needs (#6826): a page of active project ids, and whether a
// model row still serves a name.
type PlatformModelDefaultRowsRepository struct {
	pool     *pgxpool.Pool
	projects projectStore
}

// NewPlatformModelDefaultRowsRepository composes the repository.
func NewPlatformModelDefaultRowsRepository(pool *pgxpool.Pool) (*PlatformModelDefaultRowsRepository, error) {
	projects, err := newPostgresProjectStore(pool)
	if err != nil {
		return nil, err
	}
	return &PlatformModelDefaultRowsRepository{pool: pool, projects: projects}, nil
}

const listActiveProjectIDsAfter = `
SELECT id
  FROM centry.project
 WHERE create_success IS TRUE
   AND suspended IS FALSE
   AND id > $1
 ORDER BY id
 LIMIT $2`

// ListActiveProjectIDsAfter returns at most limit active project ids greater
// than after, in ascending order. It is the keyset page of the same set
// ListActiveCurrentProjectIDs reads.
func (r *PlatformModelDefaultRowsRepository) ListActiveProjectIDsAfter(
	ctx context.Context, after int32, limit int,
) ([]int32, error) {
	if r == nil || r.pool == nil {
		return nil, errors.New("the project directory is not available")
	}
	if ctx == nil || after < 0 || limit <= 0 || limit > configurationapp.PlatformModelDefaultProjectPage {
		return nil, configurationapp.ErrInvalidPlatformModelDefault
	}
	rows, err := r.pool.Query(ctx, listActiveProjectIDsAfter, after, limit)
	if err != nil {
		return nil, err
	}
	projectIDs, err := pgx.CollectRows(rows, pgx.RowTo[int32])
	if err != nil {
		return nil, err
	}
	return projectIDs, nil
}

// modelRowExists reads data.name, or elitea_title for a vector storage row:
// the value a stored default names (models.go reads the same). A row that is
// only disabled still counts: a disabled model keeps its defaults.
const modelRowExists = `
SELECT EXISTS (
    SELECT 1
      FROM configuration
     WHERE project_id = $1
       AND section = $2
       AND CASE WHEN section = 'vectorstorage' THEN elitea_title ELSE data->>'name' END = $3
       AND (NOT $4::boolean OR shared = true)
       AND id <> $5
)`

// ModelRowExists reports whether the project has a row in the section that
// serves the name.
func (r *PlatformModelDefaultRowsRepository) ModelRowExists(
	ctx context.Context, query configurationapp.PlatformModelDefaultRowQuery,
) (bool, error) {
	if r == nil || r.projects == nil {
		return false, errors.New("the configuration store is not available")
	}
	if ctx == nil || query.ProjectID <= 0 || query.Name == "" ||
		!configurationapp.IsSupportedCurrentModelSection(query.Section) {
		return false, configurationapp.ErrInvalidPlatformModelDefault
	}
	var exists bool
	err := r.projects.WithinProjectTx(ctx, int64(query.ProjectID), pgx.TxOptions{
		IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly,
	}, func(tx sqlExecutor) error {
		return tx.QueryRow(ctx, modelRowExists,
			query.ProjectID, string(query.Section), query.Name, query.SharedOnly, query.ExcludeID,
		).Scan(&exists)
	})
	if err != nil {
		return false, err
	}
	return exists, nil
}

var (
	_ configurationapp.PlatformModelDefaultProjects = (*PlatformModelDefaultRowsRepository)(nil)
	_ configurationapp.PlatformModelDefaultRows     = (*PlatformModelDefaultRowsRepository)(nil)
)
