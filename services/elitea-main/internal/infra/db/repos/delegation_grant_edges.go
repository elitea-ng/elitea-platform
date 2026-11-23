package repos

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workerdelegation"
)

const (
	// maxReachableEdgeBatchRows caps total composition edge rows scanned in
	// one level query alongside the per-caller ROW_NUMBER bound.
	maxReachableEdgeBatchRows = 4096
)

var _ workerdelegation.CompositionEdgeSource = (*DelegationGrants)(nil)

// ReachableApplicationCallees returns direct saved-application and pipeline
// composition edges (caller application id -> sorted unique callee application
// ids) strictly within projectID's tenant schema. Foreign-project references
// are excluded by tenant schema scoping, callee existence in p_<project>.applications,
// and exact project_id verification on tool settings. Both caller input count
// and returned edges per caller / per batch are strictly bounded.
func (s *DelegationGrants) ReachableApplicationCallees(ctx context.Context, projectID int64, callerIDs []int64) (map[int64][]int64, error) {
	if projectID <= 0 || len(callerIDs) == 0 {
		return nil, nil
	}
	boundedCallers := normalizeBoundedCallerIDs(callerIDs)
	if len(boundedCallers) == 0 {
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
	// Guard the ::bigint cast inside JOIN ON via CASE so PostgreSQL never
	// raises 22P02 when planning JOIN predicates before WHERE filters.
	// Pattern ^[1-9][0-9]{0,17}$ matches strictPositiveJSONInt (positive
	// canonical base-10 integer without zero or leading zeros).
	unionQuery := fmt.Sprintf(`
SELECT DISTINCT version.application_id::bigint AS caller_id, callee.id::bigint AS callee_id
FROM %[1]s.entity_tool_mapping AS mapping
JOIN %[1]s.elitea_tools AS tool ON tool.id = mapping.tool_id
JOIN %[1]s.application_versions AS version ON version.id = mapping.entity_version_id
JOIN %[1]s.applications AS callee
  ON callee.id = CASE
    WHEN tool.settings->>'application_id' ~ '^[1-9][0-9]{0,17}$'
    THEN (tool.settings->>'application_id')::bigint
  END
WHERE mapping.entity_type = 'agent'
  AND tool.type = 'application'
  AND version.application_id = ANY($1::bigint[])
  AND tool.settings->>'application_id' ~ '^[1-9][0-9]{0,17}$'
  AND (tool.settings->>'project_id' IS NULL OR tool.settings->>'project_id' = '' OR tool.settings->>'project_id' = $2)
  AND version.application_id <> callee.id
  AND (version.status = 'published' OR version.id = (SELECT max(latest.id) FROM %[1]s.application_versions AS latest WHERE latest.application_id = version.application_id))`, schema)
	if legacy {
		unionQuery += fmt.Sprintf(`
UNION
SELECT DISTINCT version.application_id::bigint AS caller_id, callee.id::bigint AS callee_id
FROM %[1]s.application_tools AS tool
JOIN %[1]s.application_versions AS version ON version.id = tool.application_version_id
JOIN %[1]s.applications AS callee
  ON callee.id = CASE
    WHEN tool.settings->>'application_id' ~ '^[1-9][0-9]{0,17}$'
    THEN (tool.settings->>'application_id')::bigint
  END
WHERE tool.type = 'application'
  AND version.application_id = ANY($1::bigint[])
  AND tool.settings->>'application_id' ~ '^[1-9][0-9]{0,17}$'
  AND (tool.settings->>'project_id' IS NULL OR tool.settings->>'project_id' = '' OR tool.settings->>'project_id' = $2)
  AND version.application_id <> callee.id
  AND (version.status = 'published' OR version.id = (SELECT max(latest.id) FROM %[1]s.application_versions AS latest WHERE latest.application_id = version.application_id))`, schema)
	}
	query := fmt.Sprintf(`
WITH edges AS (
%s
), ranked AS (
  SELECT caller_id, callee_id,
         ROW_NUMBER() OVER (PARTITION BY caller_id ORDER BY callee_id) AS rn
  FROM edges
)
SELECT caller_id, callee_id
FROM ranked
WHERE rn <= $3
ORDER BY caller_id, callee_id
LIMIT $4`, unionQuery)
	rows, err := s.pool.Query(ctx, query, boundedCallers, projectText, workerdelegation.MaxGrantClosureNodes, maxReachableEdgeBatchRows)
	if err != nil {
		if undefinedTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("delegation grants: read reachable application callees: %w", err)
	}
	defer rows.Close()
	out := map[int64][]int64{}
	scanned := 0
	for rows.Next() {
		if scanned >= maxReachableEdgeBatchRows {
			break
		}
		var callerID, calleeID int64
		if err := rows.Scan(&callerID, &calleeID); err != nil {
			return nil, fmt.Errorf("delegation grants: scan reachable application callee: %w", err)
		}
		scanned++
		if callerID > 0 && calleeID > 0 && len(out[callerID]) < workerdelegation.MaxGrantClosureNodes {
			out[callerID] = append(out[callerID], calleeID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("delegation grants: read reachable application callees: %w", err)
	}
	return out, nil
}

func normalizeBoundedCallerIDs(callerIDs []int64) []int64 {
	positive := make([]int64, 0, min(len(callerIDs), workerdelegation.MaxGrantClosureNodes))
	for _, id := range callerIDs {
		if id > 0 {
			positive = append(positive, id)
		}
	}
	slices.Sort(positive)
	positive = slices.Compact(positive)
	if len(positive) > workerdelegation.MaxGrantClosureNodes {
		positive = positive[:workerdelegation.MaxGrantClosureNodes]
	}
	return positive
}
