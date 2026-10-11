package repos

import (
	"context"
	"errors"
	"fmt"
	"math"

	vectorstoreapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/vectorstore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type currentProjectPgvectorQueries interface {
	UpsertCurrentProjectPgvectorConfiguration(
		context.Context,
		sqlcgen.UpsertCurrentProjectPgvectorConfigurationParams,
	) (int32, error)
	DeleteCurrentProjectPgvectorConfiguration(
		context.Context,
		sqlcgen.DeleteCurrentProjectPgvectorConfigurationParams,
	) (int64, error)
}

type currentProjectPgvectorQueryFactory func(sqlExecutor) (currentProjectPgvectorQueries, error)

// CurrentProjectPgvectorConfigurationsRepository owns the tenant transaction
// for the exact current system PgVector row. The generated statement creates
// the full row or updates only data.connection_string when the title already
// exists, matching configurations_create_if_not_exists followed by
// configurations_update(data=...).
type CurrentProjectPgvectorConfigurationsRepository struct {
	projects projectStore
	queries  currentProjectPgvectorQueryFactory
}

func NewCurrentProjectPgvectorConfigurationsRepository(
	pool *pgxpool.Pool,
) (*CurrentProjectPgvectorConfigurationsRepository, error) {
	projects, err := newPostgresProjectStore(pool)
	if err != nil {
		return nil, err
	}
	return newCurrentProjectPgvectorConfigurationsRepository(projects, newCurrentProjectPgvectorQueries)
}

func newCurrentProjectPgvectorConfigurationsRepository(
	projects projectStore,
	queries currentProjectPgvectorQueryFactory,
) (*CurrentProjectPgvectorConfigurationsRepository, error) {
	if projects == nil || queries == nil {
		return nil, errors.New("current project pgvector configuration database is required")
	}
	return &CurrentProjectPgvectorConfigurationsRepository{projects: projects, queries: queries}, nil
}

func newCurrentProjectPgvectorQueries(executor sqlExecutor) (currentProjectPgvectorQueries, error) {
	queryer, ok := executor.(pgxExecutor)
	if !ok || queryer.queryer == nil {
		return nil, errors.New("current project pgvector transaction does not support generated queries")
	}
	return sqlcgen.New(queryer.queryer), nil
}

func (r *CurrentProjectPgvectorConfigurationsRepository) UpsertProjectPgvectorConfiguration(
	ctx context.Context,
	configuration vectorstoreapp.ProjectConfiguration,
) (int32, error) {
	if r == nil || r.projects == nil || r.queries == nil ||
		!validCurrentProjectPgvectorConfiguration(ctx, configuration) {
		return 0, vectorstoreapp.ErrInvalidProjectPgvectorRequest
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	var configurationID int32
	err := r.projects.WithinProjectTx(
		ctx,
		int64(configuration.ProjectID),
		pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite},
		func(tx sqlExecutor) error {
			queries, err := r.queries(tx)
			if err != nil {
				return err
			}
			configurationID, err = queries.UpsertCurrentProjectPgvectorConfiguration(
				ctx,
				sqlcgen.UpsertCurrentProjectPgvectorConfigurationParams{
					ConfigurationUuid: configuration.UUID,
					ProjectID:         configuration.ProjectID,
					Label:             cloneCurrentProjectPgvectorLabel(configuration.Label),
					EliteaTitle:       configuration.Title,
				},
			)
			return err
		},
	)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, ctxErr
		}
		if errors.Is(err, context.Canceled) {
			return 0, context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, context.DeadlineExceeded
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, vectorstoreapp.ErrProjectPgvectorConflict
		}
		return 0, vectorstoreapp.ErrProjectPgvectorConfiguration
	}
	if configurationID <= 0 {
		return 0, vectorstoreapp.ErrProjectPgvectorConfiguration
	}
	return configurationID, nil
}

// DeleteProjectPgvectorConfiguration removes the system PgVector row for one
// project, and reports whether a row was there to remove.
//
// It is the compensation half of the upsert (#371). An absent row is not an
// error: provisioning compensates every ATTEMPTED step, so this runs for a step
// that failed before it wrote anything. A missing tenant schema is not an error
// either — the schema is dropped by a later compensation in the same rollback,
// and either order must converge on "no row".
func (r *CurrentProjectPgvectorConfigurationsRepository) DeleteProjectPgvectorConfiguration(
	ctx context.Context,
	projectID int64,
	title string,
) (bool, error) {
	if r == nil || r.projects == nil || r.queries == nil ||
		ctx == nil || projectID <= 0 || projectID > math.MaxInt32 || title == "" {
		return false, vectorstoreapp.ErrInvalidProjectPgvectorRequest
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}

	var removed int64
	err := r.projects.WithinProjectTx(
		ctx,
		projectID,
		pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite},
		func(tx sqlExecutor) error {
			queries, err := r.queries(tx)
			if err != nil {
				return err
			}
			removed, err = queries.DeleteCurrentProjectPgvectorConfiguration(
				ctx,
				sqlcgen.DeleteCurrentProjectPgvectorConfigurationParams{
					ProjectID:   int32(projectID),
					EliteaTitle: title,
				},
			)
			return err
		},
	)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		if errors.Is(err, context.Canceled) {
			return false, context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return false, context.DeadlineExceeded
		}
		return false, vectorstoreapp.ErrProjectPgvectorConfiguration
	}
	return removed > 0, nil
}

func validCurrentProjectPgvectorConfiguration(
	ctx context.Context,
	configuration vectorstoreapp.ProjectConfiguration,
) bool {
	return ctx != nil &&
		configuration.ProjectID > 0 &&
		len(configuration.UUID) == 36 &&
		configuration.Title != "" &&
		configuration.Type == vectorstoreapp.ProjectPgvectorType &&
		configuration.Section == vectorstoreapp.ProjectPgvectorSection &&
		configuration.Source == vectorstoreapp.ProjectPgvectorSource &&
		configuration.ConnectionStringReference == vectorstoreapp.ProjectPgvectorReference
}

func cloneCurrentProjectPgvectorLabel(label *string) *string {
	if label == nil {
		return nil
	}
	cloned := *label
	return &cloned
}

var _ vectorstoreapp.ProjectConfigurationRepository = (*CurrentProjectPgvectorConfigurationsRepository)(nil)

// ExistsCurrentProjectPgvectorConfiguration reports whether the project's
// system PgVector row exists in its tenant schema, reading through the
// transaction the caller supplies.
//
// The project delete asks this inside its deciding transaction (#1211), so the
// answer comes from the snapshot that removes the project and no second pool
// connection is taken while the project row is locked. The generated statement
// names the table without a schema, so the tenant search_path is installed in a
// SAVEPOINT and the savepoint is rolled back: SET LOCAL ends with it, and the
// caller's own search_path is untouched for the statements that follow.
//
// The caller has checked that the tenant table exists; an absent table is an
// error here, and (in PostgreSQL) an error would abort the supplied
// transaction, which is why that check comes first.
func ExistsCurrentProjectPgvectorConfiguration(
	ctx context.Context,
	tx interface {
		Begin(context.Context) (pgx.Tx, error)
	},
	projectID int64,
	title string,
) (bool, error) {
	if tx == nil || ctx == nil || projectID <= 0 || projectID > math.MaxInt32 || title == "" {
		return false, vectorstoreapp.ErrInvalidProjectPgvectorRequest
	}
	savepoint, err := tx.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = savepoint.Rollback(context.WithoutCancel(ctx)) }()
	// The schema name is built from an int64 id, never from caller input.
	if _, err := savepoint.Exec(ctx,
		"SELECT set_config('search_path', $1, true)",
		fmt.Sprintf("p_%d, public", projectID),
	); err != nil {
		return false, err
	}
	return sqlcgen.New(savepoint).ExistsCurrentProjectPgvectorConfiguration(
		ctx,
		sqlcgen.ExistsCurrentProjectPgvectorConfigurationParams{
			ProjectID:   int32(projectID),
			EliteaTitle: title,
		},
	)
}
