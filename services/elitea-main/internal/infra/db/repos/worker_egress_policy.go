package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workeregress"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// WorkerEgressSources reads what a worker command's public egress policy is
// assembled from (ADR-0044 decision 2, application/workeregress): the
// platform section (centry.platform_config `worker_egress`), the project's
// `worker_egress` configuration row in its tenant `configuration` table, and
// the command's input bundle entries (elitea_runtime.input_bundle_entries),
// whose toolkit settings name the hosts the run's toolkits reach.
type WorkerEgressSources struct{ pool *pgxpool.Pool }

var (
	_ workeregress.PlatformReader = (*WorkerEgressSources)(nil)
	_ workeregress.ProjectReader  = (*WorkerEgressSources)(nil)
	_ workeregress.InputReader    = (*WorkerEgressSources)(nil)
)

// NewWorkerEgressSources reads through pool.
func NewWorkerEgressSources(pool *pgxpool.Pool) (*WorkerEgressSources, error) {
	if pool == nil {
		return nil, errors.New("worker egress sources database is required")
	}
	return &WorkerEgressSources{pool: pool}, nil
}

// WorkerEgressSettings reads the platform section.
func (s *WorkerEgressSources) WorkerEgressSettings(ctx context.Context) (platformconfig.WorkerEgressSettings, error) {
	return platformconfig.LoadWorkerEgressSettings(ctx, s.pool)
}

// WorkerEgressProjectEntries reads the project's own entries. A project with
// no row has none. An undecodable row is an error, never "none": the assembly
// then refuses the command instead of silently dropping the project's hosts.
// The entries are returned as stored; the assembly validates them.
func (s *WorkerEgressSources) WorkerEgressProjectEntries(ctx context.Context, projectID int64) ([]string, error) {
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return nil, err
	}
	var raw []byte
	err = s.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT data FROM %s.configuration WHERE type = $1 ORDER BY id LIMIT 1`, schema),
		configurationapp.WorkerEgressConfigurationType).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) || undefinedTable(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("worker egress: read the project setting: %w", err)
	}
	var setting struct {
		Allowlist []string `json:"allowlist"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &setting); err != nil {
			return nil, fmt.Errorf("worker egress: decode the project setting: %w", err)
		}
	}
	return setting.Allowlist, nil
}

// InputEntries reads the named entries of one input bundle.
func (s *WorkerEgressSources) InputEntries(ctx context.Context, inputBundleID string, entryIDs []string) (map[string][]byte, error) {
	rows, err := s.pool.Query(ctx, `
SELECT entry_id, content_bytes
FROM elitea_runtime.input_bundle_entries
WHERE input_bundle_id = $1 AND entry_id = ANY($2::text[])`, inputBundleID, entryIDs)
	if err != nil {
		return nil, fmt.Errorf("worker egress: read the input entries: %w", err)
	}
	defer rows.Close()
	entries := make(map[string][]byte, len(entryIDs))
	for rows.Next() {
		var id string
		var content []byte
		if err := rows.Scan(&id, &content); err != nil {
			return nil, fmt.Errorf("worker egress: read an input entry: %w", err)
		}
		entries[id] = content
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("worker egress: read the input entries: %w", err)
	}
	return entries, nil
}
