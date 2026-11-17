package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	dg "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/delegationgrant"
)

// DelegationCallees reads the grant set of an agent or pipeline version from
// the tenant schema: the toolkit rows bound to the version
// (entity_tool_mapping, entity_type 'agent' for both kinds), less the nested
// applications, which are edges of their own.
type DelegationCallees struct {
	pool    *pgxpool.Pool
	classes interface {
		AgentDataClasses(ctx context.Context, projectID, applicationID int64) ([]string, error)
	}
}

// NewDelegationCallees builds the reader. classes reads the data classes an
// agent declares on its registry row (WorkerEgressSources satisfies it).
func NewDelegationCallees(pool *pgxpool.Pool, classes interface {
	AgentDataClasses(ctx context.Context, projectID, applicationID int64) ([]string, error)
}) (*DelegationCallees, error) {
	if pool == nil || classes == nil {
		return nil, errors.New("delegation callees: a pool and a data class reader are required")
	}
	return &DelegationCallees{pool: pool, classes: classes}, nil
}

// ApplicationName is the name of an agent or pipeline of the project, or
// dg.ErrCalleeNotFound.
func (r *DelegationCallees) ApplicationName(ctx context.Context, projectID, applicationID int64) (string, error) {
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return "", err
	}
	var name string
	err = r.pool.QueryRow(ctx, fmt.Sprintf(`SELECT name FROM %s.applications WHERE id = $1`, schema), applicationID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", dg.ErrCalleeNotFound
	}
	if err != nil {
		return "", fmt.Errorf("delegation callee: read the application: %w", err)
	}
	return name, nil
}

// Snapshot reads the grant set the version holds now.
func (r *DelegationCallees) Snapshot(ctx context.Context, projectID, applicationID, versionID int64) (dg.Callee, error) {
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return dg.Callee{}, err
	}
	snapshot := dg.Callee{ApplicationID: applicationID, VersionID: versionID}
	err = r.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT application.name
FROM %s.applications AS application
JOIN %s.application_versions AS version ON version.application_id = application.id
WHERE application.id = $1 AND version.id = $2`, schema, schema), applicationID, versionID).Scan(&snapshot.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return dg.Callee{}, dg.ErrCalleeNotFound
	}
	if err != nil {
		return dg.Callee{}, fmt.Errorf("delegation callee: read the version: %w", err)
	}
	rows, err := r.pool.Query(ctx, fmt.Sprintf(`
SELECT tool.id, tool.type, tool.settings
FROM %s.entity_tool_mapping AS mapping
JOIN %s.elitea_tools AS tool ON tool.id = mapping.tool_id
WHERE mapping.entity_version_id = $1 AND mapping.entity_type = 'agent'
ORDER BY tool.id`, schema, schema), versionID)
	if err != nil {
		return dg.Callee{}, fmt.Errorf("delegation callee: read the toolkits: %w", err)
	}
	defer rows.Close()
	var tools []dg.Tool
	for rows.Next() {
		var (
			id       int64
			toolType string
			settings []byte
		)
		if err := rows.Scan(&id, &toolType, &settings); err != nil {
			return dg.Callee{}, fmt.Errorf("delegation callee: scan a toolkit: %w", err)
		}
		tool := dg.Tool{ID: id, Type: toolType}
		if len(settings) > 0 {
			if err := json.Unmarshal(settings, &tool.Settings); err != nil {
				tool.Settings = nil // a malformed settings document holds no tool, no credential and no host
			}
		}
		tools = append(tools, tool)
	}
	if err := rows.Err(); err != nil {
		return dg.Callee{}, fmt.Errorf("delegation callee: read the toolkits: %w", err)
	}
	classes, err := r.classes.AgentDataClasses(ctx, projectID, applicationID)
	if err != nil {
		return dg.Callee{}, err
	}
	snapshot.Set = dg.FromTools(tools, classes)
	return snapshot, nil
}
