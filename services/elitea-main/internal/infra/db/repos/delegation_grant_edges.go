package repos

import (
	"context"
	"fmt"
	"strconv"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workerdelegation"
)

var _ workerdelegation.CompositionEdgeSource = (*DelegationGrants)(nil)

// ReachableApplicationCallees returns direct saved-application and pipeline
// composition edges (caller application id -> sorted unique callee application
// ids) strictly within projectID's tenant schema. Foreign-project references
// are excluded by tenant schema scoping, callee existence in p_<project>.applications,
// and exact project_id verification on tool settings.
func (s *DelegationGrants) ReachableApplicationCallees(ctx context.Context, projectID int64, callerIDs []int64) (map[int64][]int64, error) {
	if projectID <= 0 || len(callerIDs) == 0 {
		return nil, nil
	}
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return nil, err
	}
	projectText := strconv.FormatInt(projectID, 10)
	legacy, err := legacyApplicationToolsExists(ctx, pgxReader{s.pool}, schema)
	if err != nil {
		if undefinedTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("delegation grants: probe legacy application tools: %w", err)
	}
	query := fmt.Sprintf(`
SELECT DISTINCT version.application_id::bigint, callee.id::bigint
FROM %[1]s.entity_tool_mapping AS mapping
JOIN %[1]s.elitea_tools AS tool ON tool.id = mapping.tool_id
JOIN %[1]s.application_versions AS version ON version.id = mapping.entity_version_id
JOIN %[1]s.applications AS callee ON callee.id = (tool.settings->>'application_id')::bigint
WHERE mapping.entity_type = 'agent'
  AND tool.type = 'application'
  AND version.application_id = ANY($1::bigint[])
  AND tool.settings->>'application_id' ~ '^[0-9]{1,18}$'
  AND (tool.settings->>'project_id' IS NULL OR tool.settings->>'project_id' = '' OR tool.settings->>'project_id' = $2)
  AND version.application_id <> callee.id
  AND (version.status = 'published' OR version.id = (SELECT max(latest.id) FROM %[1]s.application_versions AS latest WHERE latest.application_id = version.application_id))`, schema)
	if legacy {
		query += fmt.Sprintf(`
UNION
SELECT DISTINCT version.application_id::bigint, callee.id::bigint
FROM %[1]s.application_tools AS tool
JOIN %[1]s.application_versions AS version ON version.id = tool.application_version_id
JOIN %[1]s.applications AS callee ON callee.id = (tool.settings->>'application_id')::bigint
WHERE tool.type = 'application'
  AND version.application_id = ANY($1::bigint[])
  AND tool.settings->>'application_id' ~ '^[0-9]{1,18}$'
  AND (tool.settings->>'project_id' IS NULL OR tool.settings->>'project_id' = '' OR tool.settings->>'project_id' = $2)
  AND version.application_id <> callee.id
  AND (version.status = 'published' OR version.id = (SELECT max(latest.id) FROM %[1]s.application_versions AS latest WHERE latest.application_id = version.application_id))`, schema)
	}
	query += `
ORDER BY 1, 2`
	rows, err := s.pool.Query(ctx, query, callerIDs, projectText)
	if err != nil {
		if undefinedTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("delegation grants: read reachable application callees: %w", err)
	}
	defer rows.Close()
	out := map[int64][]int64{}
	for rows.Next() {
		var callerID, calleeID int64
		if err := rows.Scan(&callerID, &calleeID); err != nil {
			return nil, fmt.Errorf("delegation grants: scan reachable application callee: %w", err)
		}
		if callerID > 0 && calleeID > 0 {
			out[callerID] = append(out[callerID], calleeID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("delegation grants: read reachable application callees: %w", err)
	}
	return out, nil
}
