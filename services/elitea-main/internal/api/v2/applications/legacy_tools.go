package applications

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// legacyToolsReader is what legacyApplicationTools needs: a pgx pool or
// transaction.
type legacyToolsReader interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// legacyApplicationTools reads the sub-agent rows the legacy runtime left in
// `application_tools` for one version, each stamped with extra (the caller's
// author or project keys).
//
// No migration in this repository creates that table: it exists only in
// tenants migrated from the legacy runtime, and this service writes the same
// references to elitea_tools + entity_tool_mapping. So the read is
// probe-guarded (repos.LegacyApplicationToolsExists): a tenant without the
// table has no legacy rows and is not queried. On a tenant that has it, a
// failed read is an error, never a silently shorter tools list. The two
// version reads used to query it unguarded and drop the error, so every
// application GET on a fresh tenant logged `relation "p_N.application_tools"
// does not exist`.
func legacyApplicationTools(ctx context.Context, q legacyToolsReader, schema, versionID string, extra map[string]any) ([]map[string]any, error) {
	exists, err := repos.LegacyApplicationToolsExists(ctx, q, schema)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	rows, err := q.Query(ctx, fmt.Sprintf(`
		SELECT id, COALESCE(name, ''), COALESCE(type, ''), COALESCE(settings::text, 'null')
		FROM %s.application_tools
		WHERE application_version_id = $1`, schema), versionID)
	if err != nil {
		return nil, fmt.Errorf("read legacy application_tools: %w", err)
	}
	defer rows.Close()
	var tools []map[string]any
	for rows.Next() {
		var id int
		var name, toolType, settingsText string
		if err := rows.Scan(&id, &name, &toolType, &settingsText); err != nil {
			return nil, fmt.Errorf("scan legacy application_tools: %w", err)
		}
		var settings any
		if err := json.Unmarshal([]byte(settingsText), &settings); err != nil {
			return nil, fmt.Errorf("legacy application_tools %d: settings: %w", id, err)
		}
		tool := map[string]any{"id": id, "name": name, "type": toolType, "settings": settings}
		for key, value := range extra {
			tool[key] = value
		}
		tools = append(tools, tool)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read legacy application_tools: %w", err)
	}
	return tools, nil
}
