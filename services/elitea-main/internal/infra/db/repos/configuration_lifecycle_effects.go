package repos

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type currentConfigurationLifecycleEffectsProjectQueries interface {
	SetCurrentConfigurationLifecycleStatus(context.Context, sqlcgen.SetCurrentConfigurationLifecycleStatusParams) (int64, error)
	ListCurrentConfigurationRenameToolkits(context.Context, sqlcgen.ListCurrentConfigurationRenameToolkitsParams) ([]sqlcgen.ListCurrentConfigurationRenameToolkitsRow, error)
	GetCurrentConfigurationRenameToolkit(context.Context, sqlcgen.GetCurrentConfigurationRenameToolkitParams) (sqlcgen.GetCurrentConfigurationRenameToolkitRow, error)
	CompareAndSwapCurrentConfigurationRenameToolkit(context.Context, sqlcgen.CompareAndSwapCurrentConfigurationRenameToolkitParams) (int64, error)
	ReplaceCurrentDeletedLLMApplicationReferences(context.Context, sqlcgen.ReplaceCurrentDeletedLLMApplicationReferencesParams) (sqlcgen.ReplaceCurrentDeletedLLMApplicationReferencesRow, error)
}

type currentConfigurationLifecycleEffectsSharedQueries interface {
	ListActiveCurrentProjectIDs(context.Context, int32) ([]int32, error)
}

type currentConfigurationLifecycleEffectsProjectQueryFactory func(sqlExecutor) (currentConfigurationLifecycleEffectsProjectQueries, error)

// CurrentConfigurationLifecycleEffectsRepository owns the bounded PostgreSQL
// projections used by configuration lifecycle internal effects. Tenant rows
// are always reached through a project-scoped transaction; only the active
// project directory is read from the shared schema.
type CurrentConfigurationLifecycleEffectsRepository struct {
	projects       projectStore
	projectQueries currentConfigurationLifecycleEffectsProjectQueryFactory
	sharedQueries  currentConfigurationLifecycleEffectsSharedQueries
}

func NewCurrentConfigurationLifecycleEffectsRepository(
	pool *pgxpool.Pool,
) (*CurrentConfigurationLifecycleEffectsRepository, error) {
	projects, err := newPostgresProjectStore(pool)
	if err != nil {
		return nil, err
	}
	return newCurrentConfigurationLifecycleEffectsRepository(
		projects,
		newCurrentConfigurationLifecycleEffectsProjectQueries,
		sqlcgen.New(pool),
	)
}

func newCurrentConfigurationLifecycleEffectsRepository(
	projects projectStore,
	projectQueries currentConfigurationLifecycleEffectsProjectQueryFactory,
	sharedQueries currentConfigurationLifecycleEffectsSharedQueries,
) (*CurrentConfigurationLifecycleEffectsRepository, error) {
	if projects == nil || projectQueries == nil || sharedQueries == nil {
		return nil, errors.New("current configuration lifecycle effects database is required")
	}
	return &CurrentConfigurationLifecycleEffectsRepository{
		projects: projects, projectQueries: projectQueries, sharedQueries: sharedQueries,
	}, nil
}

func newCurrentConfigurationLifecycleEffectsProjectQueries(
	tx sqlExecutor,
) (currentConfigurationLifecycleEffectsProjectQueries, error) {
	executor, ok := tx.(pgxExecutor)
	if !ok || executor.queryer == nil {
		return nil, errors.New("current configuration lifecycle effects transaction does not support generated queries")
	}
	return sqlcgen.New(executor.queryer), nil
}

func (r *CurrentConfigurationLifecycleEffectsRepository) SetCurrentConfigurationLifecycleStatus(
	ctx context.Context,
	target configurationapp.CurrentConfigurationLifecycleStatusTarget,
) (bool, error) {
	if !validCurrentConfigurationLifecycleEffectsContext(r, ctx) ||
		target.ProjectID <= 0 || target.ConfigurationID <= 0 ||
		!validCurrentPersistenceUUID(target.ConfigurationUUID, false) {
		return false, configurationapp.ErrInvalidCurrentConfigurationLifecycleInternalEffect
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}

	var affected int64
	err := r.projects.WithinProjectTx(ctx, int64(target.ProjectID), pgx.TxOptions{
		IsoLevel:   pgx.ReadCommitted,
		AccessMode: pgx.ReadWrite,
	}, func(tx sqlExecutor) error {
		queries, err := r.projectQueries(tx)
		if err != nil {
			return err
		}
		affected, err = queries.SetCurrentConfigurationLifecycleStatus(
			ctx,
			sqlcgen.SetCurrentConfigurationLifecycleStatusParams{
				StatusOk:          target.StatusOK,
				ProjectID:         target.ProjectID,
				ConfigurationID:   target.ConfigurationID,
				ConfigurationUuid: target.ConfigurationUUID,
			},
		)
		return err
	})
	if err != nil {
		return false, currentConfigurationLifecycleEffectsStorageError(ctx, err)
	}
	if affected < 0 || affected > 1 {
		return false, configurationapp.ErrCurrentConfigurationLifecycleInternalUnavailable
	}
	return affected == 1, nil
}

func (r *CurrentConfigurationLifecycleEffectsRepository) ListCurrentConfigurationRenameToolkits(
	ctx context.Context,
	projectID int32,
	limits configurationapp.CurrentConfigurationRenameScanLimits,
) ([]configurationapp.CurrentConfigurationRenameToolkit, error) {
	if !validCurrentConfigurationLifecycleEffectsContext(r, ctx) || projectID <= 0 ||
		limits.MaxRows <= 0 || limits.MaxRows > configurationapp.MaxCurrentConfigurationRenameToolkits+1 ||
		limits.MaxSettingsBytes <= 0 || limits.MaxSettingsBytes > configurationapp.MaxCurrentConfigurationRenameSettingsBytes ||
		limits.MaxTotalBytes <= 0 || limits.MaxTotalBytes > configurationapp.MaxCurrentConfigurationRenameTotalBytes {
		return nil, configurationapp.ErrInvalidCurrentConfigurationLifecycleInternalEffect
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	rows := []sqlcgen.ListCurrentConfigurationRenameToolkitsRow{}
	err := r.projects.WithinProjectTx(ctx, int64(projectID), pgx.TxOptions{
		IsoLevel:   pgx.ReadCommitted,
		AccessMode: pgx.ReadOnly,
	}, func(tx sqlExecutor) error {
		queries, err := r.projectQueries(tx)
		if err != nil {
			return err
		}
		rows, err = queries.ListCurrentConfigurationRenameToolkits(
			ctx,
			sqlcgen.ListCurrentConfigurationRenameToolkitsParams{
				MaxSettingsBytes: int64(limits.MaxSettingsBytes),
				MaxTotalBytes:    int64(limits.MaxTotalBytes),
				LimitRows:        int32(limits.MaxRows),
			},
		)
		return err
	})
	if err != nil {
		return nil, currentConfigurationLifecycleEffectsStorageError(ctx, err)
	}
	if len(rows) > limits.MaxRows {
		return nil, configurationapp.ErrCurrentConfigurationLifecycleInternalUnavailable
	}

	toolkits := make([]configurationapp.CurrentConfigurationRenameToolkit, 0, len(rows))
	var previousID int32
	var materializedBytes int64
	for _, row := range rows {
		if row.ID <= previousID || !validCurrentConfigurationSettingsVersion(row.SettingsVersion) ||
			row.SettingsBytes <= 0 || row.TotalBytes < row.SettingsBytes ||
			row.SettingsBytes > int64(limits.MaxSettingsBytes) ||
			row.TotalBytes > int64(limits.MaxTotalBytes) || len(row.Settings) == 0 {
			if row.SettingsBytes > int64(limits.MaxSettingsBytes) ||
				row.TotalBytes > int64(limits.MaxTotalBytes) || len(row.Settings) == 0 {
				return nil, configurationapp.ErrCurrentConfigurationLifecycleInternalLimit
			}
			return nil, configurationapp.ErrCurrentConfigurationLifecycleInternalUnavailable
		}
		materializedBytes += int64(len(row.Settings))
		if materializedBytes > int64(limits.MaxTotalBytes) {
			return nil, configurationapp.ErrCurrentConfigurationLifecycleInternalLimit
		}
		toolkits = append(toolkits, configurationapp.CurrentConfigurationRenameToolkit{
			ToolkitID: row.ID,
			Version:   row.SettingsVersion,
			Settings:  append(json.RawMessage(nil), row.Settings...),
		})
		previousID = row.ID
	}
	return toolkits, nil
}

func (r *CurrentConfigurationLifecycleEffectsRepository) GetCurrentConfigurationRenameToolkit(
	ctx context.Context,
	projectID int32,
	toolkitID int32,
) (configurationapp.CurrentConfigurationRenameToolkit, bool, error) {
	if !validCurrentConfigurationLifecycleEffectsContext(r, ctx) || projectID <= 0 || toolkitID <= 0 {
		return configurationapp.CurrentConfigurationRenameToolkit{}, false,
			configurationapp.ErrInvalidCurrentConfigurationLifecycleInternalEffect
	}
	if err := ctx.Err(); err != nil {
		return configurationapp.CurrentConfigurationRenameToolkit{}, false, err
	}

	var row sqlcgen.GetCurrentConfigurationRenameToolkitRow
	found := true
	err := r.projects.WithinProjectTx(ctx, int64(projectID), pgx.TxOptions{
		IsoLevel:   pgx.ReadCommitted,
		AccessMode: pgx.ReadOnly,
	}, func(tx sqlExecutor) error {
		queries, err := r.projectQueries(tx)
		if err != nil {
			return err
		}
		row, err = queries.GetCurrentConfigurationRenameToolkit(
			ctx,
			sqlcgen.GetCurrentConfigurationRenameToolkitParams{
				MaxSettingsBytes: configurationapp.MaxCurrentConfigurationRenameSettingsBytes,
				ToolkitID:        toolkitID,
			},
		)
		if errors.Is(err, pgx.ErrNoRows) {
			found = false
			return nil
		}
		return err
	})
	if err != nil {
		return configurationapp.CurrentConfigurationRenameToolkit{}, false,
			currentConfigurationLifecycleEffectsStorageError(ctx, err)
	}
	if !found {
		return configurationapp.CurrentConfigurationRenameToolkit{}, false, nil
	}
	if row.ID != toolkitID || !validCurrentConfigurationSettingsVersion(row.SettingsVersion) ||
		row.SettingsBytes <= 0 {
		return configurationapp.CurrentConfigurationRenameToolkit{}, false,
			configurationapp.ErrCurrentConfigurationLifecycleInternalUnavailable
	}
	if row.SettingsBytes > configurationapp.MaxCurrentConfigurationRenameSettingsBytes || len(row.Settings) == 0 ||
		len(row.Settings) > configurationapp.MaxCurrentConfigurationRenameSettingsBytes {
		return configurationapp.CurrentConfigurationRenameToolkit{}, false,
			configurationapp.ErrCurrentConfigurationLifecycleInternalLimit
	}
	return configurationapp.CurrentConfigurationRenameToolkit{
		ToolkitID: row.ID,
		Version:   row.SettingsVersion,
		Settings:  append(json.RawMessage(nil), row.Settings...),
	}, true, nil
}

func (r *CurrentConfigurationLifecycleEffectsRepository) CompareAndSwapCurrentConfigurationRenameToolkit(
	ctx context.Context,
	update configurationapp.CurrentConfigurationRenameToolkitUpdate,
) (bool, error) {
	if !validCurrentConfigurationLifecycleEffectsContext(r, ctx) || update.ProjectID <= 0 ||
		update.ToolkitID <= 0 || !validCurrentConfigurationSettingsVersion(update.ExpectedVersion) ||
		len(update.Settings) == 0 || len(update.Settings) > configurationapp.MaxCurrentConfigurationRenameSettingsBytes ||
		!json.Valid(update.Settings) {
		return false, configurationapp.ErrInvalidCurrentConfigurationLifecycleInternalEffect
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}

	var affected int64
	err := r.projects.WithinProjectTx(ctx, int64(update.ProjectID), pgx.TxOptions{
		IsoLevel:   pgx.ReadCommitted,
		AccessMode: pgx.ReadWrite,
	}, func(tx sqlExecutor) error {
		queries, err := r.projectQueries(tx)
		if err != nil {
			return err
		}
		affected, err = queries.CompareAndSwapCurrentConfigurationRenameToolkit(
			ctx,
			sqlcgen.CompareAndSwapCurrentConfigurationRenameToolkitParams{
				Settings:        append([]byte(nil), update.Settings...),
				ToolkitID:       update.ToolkitID,
				ExpectedVersion: update.ExpectedVersion,
			},
		)
		return err
	})
	if err != nil {
		return false, currentConfigurationLifecycleEffectsStorageError(ctx, err)
	}
	if affected < 0 || affected > 1 {
		return false, configurationapp.ErrCurrentConfigurationLifecycleInternalUnavailable
	}
	return affected == 1, nil
}

func (r *CurrentConfigurationLifecycleEffectsRepository) ListActiveCurrentProjectIDs(
	ctx context.Context,
	maxRows int,
) ([]int32, error) {
	if !validCurrentConfigurationLifecycleEffectsContext(r, ctx) ||
		maxRows <= 0 || maxRows > configurationapp.MaxCurrentDeletedLLMProjects+1 {
		return nil, configurationapp.ErrInvalidCurrentConfigurationLifecycleInternalEffect
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	projectIDs, err := r.sharedQueries.ListActiveCurrentProjectIDs(ctx, int32(maxRows))
	if err != nil {
		return nil, currentConfigurationLifecycleEffectsStorageError(ctx, err)
	}
	if len(projectIDs) > maxRows {
		return nil, configurationapp.ErrCurrentConfigurationLifecycleInternalUnavailable
	}
	for index, projectID := range projectIDs {
		if projectID <= 0 || (index > 0 && projectID <= projectIDs[index-1]) {
			return nil, configurationapp.ErrCurrentConfigurationLifecycleInternalUnavailable
		}
	}
	return append([]int32(nil), projectIDs...), nil
}

func (r *CurrentConfigurationLifecycleEffectsRepository) ReplaceCurrentDeletedLLMApplicationReferences(
	ctx context.Context,
	replacement configurationapp.CurrentDeletedLLMReferenceReplacement,
) (int, error) {
	if !validCurrentConfigurationLifecycleEffectsContext(r, ctx) ||
		!validCurrentConfigurationLifecycleModelName(replacement.DeletedModelName) ||
		!validCurrentConfigurationLifecycleModelName(replacement.DefaultModelName) ||
		replacement.ProjectID <= 0 || replacement.DefaultModelProjectID <= 0 ||
		replacement.MaxRows <= 0 || replacement.MaxRows > configurationapp.MaxCurrentDeletedLLMApplicationVersions {
		return 0, configurationapp.ErrInvalidCurrentConfigurationLifecycleInternalEffect
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	var result sqlcgen.ReplaceCurrentDeletedLLMApplicationReferencesRow
	err := r.projects.WithinProjectTx(ctx, int64(replacement.ProjectID), pgx.TxOptions{
		IsoLevel:   pgx.ReadCommitted,
		AccessMode: pgx.ReadWrite,
	}, func(tx sqlExecutor) error {
		queries, err := r.projectQueries(tx)
		if err != nil {
			return err
		}
		result, err = queries.ReplaceCurrentDeletedLLMApplicationReferences(
			ctx,
			sqlcgen.ReplaceCurrentDeletedLLMApplicationReferencesParams{
				DeletedModelName:      replacement.DeletedModelName,
				ScanLimit:             int32(replacement.MaxRows + 1),
				MaxRows:               int32(replacement.MaxRows),
				DefaultModelName:      replacement.DefaultModelName,
				DefaultModelProjectID: replacement.DefaultModelProjectID,
			},
		)
		return err
	})
	if err != nil {
		return 0, currentConfigurationLifecycleEffectsStorageError(ctx, err)
	}
	if result.MatchedCount < 0 || result.UpdatedCount < 0 ||
		result.MatchedCount > int64(replacement.MaxRows+1) ||
		result.UpdatedCount > result.MatchedCount {
		return 0, configurationapp.ErrCurrentConfigurationLifecycleInternalUnavailable
	}
	if result.MatchedCount > int64(replacement.MaxRows) {
		if result.UpdatedCount != 0 {
			return 0, configurationapp.ErrCurrentConfigurationLifecycleInternalUnavailable
		}
		return 0, configurationapp.ErrCurrentConfigurationLifecycleInternalLimit
	}
	if result.UpdatedCount != result.MatchedCount {
		return 0, configurationapp.ErrCurrentConfigurationLifecycleInternalUnavailable
	}
	return int(result.UpdatedCount), nil
}

func validCurrentConfigurationLifecycleEffectsContext(
	repository *CurrentConfigurationLifecycleEffectsRepository,
	ctx context.Context,
) bool {
	return repository != nil && repository.projects != nil && repository.projectQueries != nil &&
		repository.sharedQueries != nil && ctx != nil
}

func validCurrentConfigurationSettingsVersion(value string) bool {
	if len(value) != 32 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}

func validCurrentConfigurationLifecycleModelName(value string) bool {
	return value != "" && len(value) <= configurationapp.MaxCurrentDeletedLLMModelNameBytes &&
		value == strings.TrimSpace(value) && !strings.ContainsRune(value, '\x00')
}

func currentConfigurationLifecycleEffectsStorageError(ctx context.Context, err error) error {
	if ctx != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return configurationapp.ErrCurrentConfigurationLifecycleInternalUnavailable
}

var (
	_ configurationapp.CurrentConfigurationLifecycleStatusRepository = (*CurrentConfigurationLifecycleEffectsRepository)(nil)
	_ configurationapp.CurrentConfigurationRenameRepository          = (*CurrentConfigurationLifecycleEffectsRepository)(nil)
	_ configurationapp.CurrentDeletedLLMProjectRepository            = (*CurrentConfigurationLifecycleEffectsRepository)(nil)
	_ configurationapp.CurrentDeletedLLMApplicationRepository        = (*CurrentConfigurationLifecycleEffectsRepository)(nil)
)
