package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

// CloudExecutorSettingsRepository reads the two layers of the cloud executor
// policy (ADR-0034 decision 11): the platform ceiling (centry.platform_config,
// section `cloud_executor`) and the project's `cloud_executor` configuration
// row in its tenant `configuration` table.
type CloudExecutorSettingsRepository struct {
	pool *pgxpool.Pool
}

var (
	_ workspacehandoff.PlatformReader       = (*CloudExecutorSettingsRepository)(nil)
	_ workspacehandoff.ProjectSettingReader = (*CloudExecutorSettingsRepository)(nil)
)

func NewCloudExecutorSettingsRepository(pool *pgxpool.Pool) (*CloudExecutorSettingsRepository, error) {
	if pool == nil {
		return nil, errors.New("cloud executor settings database is required")
	}
	return &CloudExecutorSettingsRepository{pool: pool}, nil
}

func (r *CloudExecutorSettingsRepository) CloudExecutorSettings(ctx context.Context) (platformconfig.CloudExecutorSettings, error) {
	return platformconfig.LoadCloudExecutorSettings(ctx, r.pool)
}

// CloudExecutorSetting reads the project row. A project with none, or with a
// row whose data does not decode, narrows nothing — but an undecodable row is
// an error, never a silent "unset": a project that switched the network off
// must not get it back through a malformed save.
func (r *CloudExecutorSettingsRepository) CloudExecutorSetting(ctx context.Context, projectID int64) (workspacehandoff.ProjectSetting, bool, error) {
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return workspacehandoff.ProjectSetting{}, false, err
	}
	var raw []byte
	err = r.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT data FROM %s.configuration WHERE type = 'cloud_executor' ORDER BY id LIMIT 1`, schema)).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return workspacehandoff.ProjectSetting{}, false, nil
	}
	if err != nil {
		return workspacehandoff.ProjectSetting{}, false, fmt.Errorf("cloud executor: read project setting: %w", err)
	}
	var setting workspacehandoff.ProjectSetting
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &setting); err != nil {
			return workspacehandoff.ProjectSetting{}, false, fmt.Errorf("cloud executor: decode project setting: %w", err)
		}
	}
	return setting, true, nil
}
