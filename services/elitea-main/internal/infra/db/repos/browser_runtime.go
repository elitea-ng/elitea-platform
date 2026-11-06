package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/browserruntime"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// BrowserRuntimeSources reads what the runtime-context `browser` field is
// built from (ADR-0049, #1596, application/browserruntime): the platform
// `browser` and `worker_egress` sections, the project's `browser` and
// `worker_egress` configuration rows, the project's settled browser seconds
// in gateway.llm_request_logs, and the execution's trigger origin.
type BrowserRuntimeSources struct {
	pool   *pgxpool.Pool
	egress *WorkerEgressSources
}

var (
	_ browserruntime.PlatformReader = (*BrowserRuntimeSources)(nil)
	_ browserruntime.ProjectReader  = (*BrowserRuntimeSources)(nil)
	_ browserruntime.UsageReader    = (*BrowserRuntimeSources)(nil)
	_ browserruntime.TriggerReader  = (*BrowserRuntimeSources)(nil)
)

// NewBrowserRuntimeSources reads through pool.
func NewBrowserRuntimeSources(pool *pgxpool.Pool) (*BrowserRuntimeSources, error) {
	egress, err := NewWorkerEgressSources(pool)
	if err != nil {
		return nil, errors.New("browser runtime sources database is required")
	}
	return &BrowserRuntimeSources{pool: pool, egress: egress}, nil
}

// BrowserSettings reads the platform `browser` section.
func (s *BrowserRuntimeSources) BrowserSettings(ctx context.Context) (platformconfig.BrowserSettings, error) {
	return platformconfig.LoadBrowserSettings(ctx, s.pool)
}

// WorkerEgressSettings reads the platform `worker_egress` section.
func (s *BrowserRuntimeSources) WorkerEgressSettings(ctx context.Context) (platformconfig.WorkerEgressSettings, error) {
	return s.egress.WorkerEgressSettings(ctx)
}

// WorkerEgressProjectEntries reads the project's `worker_egress` entries.
func (s *BrowserRuntimeSources) WorkerEgressProjectEntries(ctx context.Context, projectID int64) ([]string, error) {
	return s.egress.WorkerEgressProjectEntries(ctx, projectID)
}

// BrowserProjectSetting reads the project's `browser` row. A project with
// no row (or no tenant table) has the cloud browser off. An undecodable row
// is an error, never "off" or "on".
func (s *BrowserRuntimeSources) BrowserProjectSetting(ctx context.Context, projectID int64) (browserruntime.ProjectSetting, error) {
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return browserruntime.ProjectSetting{}, err
	}
	var raw []byte
	err = s.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT data FROM %s.configuration WHERE type = $1 ORDER BY id LIMIT 1`, schema),
		configurationapp.BrowserConfigurationType).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) || undefinedTable(err) {
		return browserruntime.ProjectSetting{}, nil
	}
	if err != nil {
		return browserruntime.ProjectSetting{}, fmt.Errorf("browser: read the project setting: %w", err)
	}
	var setting browserruntime.ProjectSetting
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &setting); err != nil {
			return browserruntime.ProjectSetting{}, fmt.Errorf("browser: the project setting is corrupt: %w", err)
		}
	}
	return setting, nil
}

// BrowserSecondsSince sums the settled browser engine seconds of one
// project since a time: the metered tool rows of provider `browser` and
// operation `engine_seconds` (shared 0226). The read uses the index
// idx_llm_request_logs_project_time (project_id, occurred_at DESC) and
// reads only the rows of the project since that time. A database without
// the gateway table has no usage.
func (s *BrowserRuntimeSources) BrowserSecondsSince(ctx context.Context, projectID int64, since time.Time) (int64, error) {
	if projectID <= 0 || projectID > math.MaxInt32 {
		return 0, fmt.Errorf("browser: project %d is out of range", projectID)
	}
	var used int64
	err := s.pool.QueryRow(ctx, `
SELECT coalesce(sum(units), 0)::bigint
FROM gateway.llm_request_logs
WHERE project_id = $1
  AND occurred_at >= $2
  AND operation = 'tool'
  AND provider = 'browser'
  AND model = 'engine_seconds'
  AND units > 0`, int32(projectID), since).Scan(&used)
	if undefinedColumnOrTable(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("browser: read the month's browser seconds: %w", err)
	}
	return used, nil
}

// TriggerOrigin reads the execution's trigger origin. An execution with no
// job row is manual.
func (s *BrowserRuntimeSources) TriggerOrigin(ctx context.Context, executionID string) (string, error) {
	var origin string
	err := s.pool.QueryRow(ctx, `
SELECT trigger_origin FROM elitea_runtime.execution_jobs
WHERE execution_id = $1 ORDER BY generation DESC LIMIT 1`, executionID).Scan(&origin)
	if errors.Is(err, pgx.ErrNoRows) {
		return "manual", nil
	}
	if err != nil {
		return "", fmt.Errorf("browser: read the trigger origin: %w", err)
	}
	return origin, nil
}
