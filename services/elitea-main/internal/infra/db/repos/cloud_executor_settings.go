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
	_ workspacehandoff.ProjectSwitchStore   = (*CloudExecutorSettingsRepository)(nil)
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
	if err := r.projectExists(ctx, r.pool, schema); err != nil {
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

// projectExists answers workspacehandoff.ErrProjectNotFound for a project
// whose tenant configuration table does not exist.
func (r *CloudExecutorSettingsRepository) projectExists(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, schema string) error {
	var exists bool
	if err := q.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, schema+".configuration").Scan(&exists); err != nil {
		return fmt.Errorf("cloud executor: find project: %w", err)
	}
	if !exists {
		return workspacehandoff.ErrProjectNotFound
	}
	return nil
}

// SetCloudExecutorProjectEnabled writes the `enabled` field of the project's
// cloud_executor row and keeps every other field: an explicit value is
// merged in, nil removes the key so the project follows the platform's
// project default. Without a row, an explicit value creates one and nil
// changes nothing. The row is the one CloudExecutorSetting reads (the lowest
// id); a transaction-scoped advisory lock keeps two first writes from
// creating two rows.
func (r *CloudExecutorSettingsRepository) SetCloudExecutorProjectEnabled(ctx context.Context, projectID int64, enabled *bool) error {
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return workspacehandoff.ErrProjectNotFound
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("cloud executor: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := r.projectExists(ctx, tx, schema); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "cloud_executor_setting:"+schema); err != nil {
		return fmt.Errorf("cloud executor: lock project setting: %w", err)
	}
	var id int64
	err = tx.QueryRow(ctx, fmt.Sprintf(`
SELECT id FROM %s.configuration WHERE type = 'cloud_executor' ORDER BY id LIMIT 1 FOR UPDATE`, schema)).Scan(&id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if enabled == nil {
			return tx.Commit(ctx)
		}
		data, _ := json.Marshal(map[string]any{"enabled": *enabled})
		if _, err := tx.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s.configuration
    (uuid, project_id, label, elitea_title, type, section, data, meta, shared, status_ok, source, created_at, updated_at)
VALUES (gen_random_uuid(), $1, 'Cloud executor', 'cloud_executor', 'cloud_executor', 'project_settings', $2::jsonb,
        '{}'::jsonb, false, true, 'system', now() AT TIME ZONE 'UTC', now() AT TIME ZONE 'UTC')`, schema),
			projectID, data); err != nil {
			return fmt.Errorf("cloud executor: create project setting: %w", err)
		}
	case err != nil:
		return fmt.Errorf("cloud executor: read project setting: %w", err)
	case enabled == nil:
		if _, err := tx.Exec(ctx, fmt.Sprintf(`
UPDATE %s.configuration SET data = COALESCE(data, '{}'::jsonb) - 'enabled', updated_at = now() AT TIME ZONE 'UTC'
WHERE id = $1`, schema), id); err != nil {
			return fmt.Errorf("cloud executor: update project setting: %w", err)
		}
	default:
		if _, err := tx.Exec(ctx, fmt.Sprintf(`
UPDATE %s.configuration SET data = COALESCE(data, '{}'::jsonb) || jsonb_build_object('enabled', $2::boolean),
    updated_at = now() AT TIME ZONE 'UTC'
WHERE id = $1`, schema), id, *enabled); err != nil {
			return fmt.Errorf("cloud executor: update project setting: %w", err)
		}
	}
	return tx.Commit(ctx)
}
