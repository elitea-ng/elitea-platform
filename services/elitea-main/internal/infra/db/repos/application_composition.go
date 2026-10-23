package repos

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applicationcomposition"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/pipelinelimits"
)

// ApplicationToolReference is one agent or pipeline attached to a parent
// version as a tool: a row in `elitea_tools` (type='application', settings
// carrying the child pair) plus a row in `entity_tool_mapping` binding it to
// the parent version. eliteacore's application_relation.go is the one place
// that writes the pair; this is the one place that reads it, for that route,
// the publish/embed flows and the save-time composition check (FU-244).
type ApplicationToolReference struct {
	ToolID        int64
	Name          string
	ApplicationID string
	VersionID     string
}

// relationReader is satisfied by both *pgxpool.Pool and pgx.Tx.
type relationReader interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ApplicationToolSettingsVersionSQL reads the child version id out of a tool
// row's settings. Two spellings exist in the wild: Main writes
// `application_version_id` (the key the freeze admits), and rows written by
// the legacy runtime carry `version_id`. Readers accept both; writers emit
// only the new one.
const ApplicationToolSettingsVersionSQL = `COALESCE(tool.settings->>'application_version_id', tool.settings->>'version_id')`

// LegacyApplicationToolsExists probes for the legacy-owned `application_tools`
// table. No migration in this repository creates it — it exists only in
// tenants migrated from the legacy runtime — so every touch of it is
// probe-guarded, the same arrangement internal/infra/db/repos/applications.go
// uses for its delete. to_regclass accepts the quoted identifier this package
// already carries.
func LegacyApplicationToolsExists(ctx context.Context, q relationReader, schema string) (bool, error) {
	var name *string
	if err := q.QueryRow(ctx, `SELECT to_regclass($1)::text`,
		schema+".application_tools").Scan(&name); err != nil {
		return false, fmt.Errorf("probe application_tools: %w", err)
	}
	return name != nil, nil
}

// ListApplicationToolReferences returns the sub-agent references attached to
// one parent version: the canonical rows (`elitea_tools` +
// `entity_tool_mapping`, what this service writes) first, then — on a
// legacy-migrated tenant only — the rows the legacy runtime left in
// `application_tools`.
func ListApplicationToolReferences(ctx context.Context, q relationReader, schema, parentVersionID string) ([]ApplicationToolReference, error) {
	rows, err := q.Query(ctx, fmt.Sprintf(`
		SELECT tool.id, tool.name,
		       COALESCE(tool.settings->>'application_id', ''),
		       COALESCE(%s, '')
		FROM %s.entity_tool_mapping AS mapping
		JOIN %s.elitea_tools AS tool ON tool.id = mapping.tool_id
		WHERE mapping.entity_version_id = $1
		  AND mapping.entity_type = 'agent'
		  AND tool.type = 'application'
		ORDER BY tool.id`, ApplicationToolSettingsVersionSQL, schema, schema), parentVersionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []ApplicationToolReference
	for rows.Next() {
		var ref ApplicationToolReference
		if err := rows.Scan(&ref.ToolID, &ref.Name, &ref.ApplicationID, &ref.VersionID); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	hasLegacy, err := LegacyApplicationToolsExists(ctx, q, schema)
	if err != nil || !hasLegacy {
		return refs, err
	}
	legacyRows, err := q.Query(ctx, fmt.Sprintf(`
		SELECT tool.id, tool.name,
		       COALESCE(tool.settings->>'application_id', ''),
		       COALESCE(%s, '')
		FROM %s.application_tools AS tool
		WHERE tool.application_version_id = $1
		  AND tool.type = 'application'
		ORDER BY tool.id`, ApplicationToolSettingsVersionSQL, schema), parentVersionID)
	if err != nil {
		return nil, err
	}
	defer legacyRows.Close()
	for legacyRows.Next() {
		var ref ApplicationToolReference
		if err := legacyRows.Scan(&ref.ToolID, &ref.Name, &ref.ApplicationID, &ref.VersionID); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, legacyRows.Err()
}

// CompositionSource reads saved versions for applicationcomposition.Check,
// inside the caller's transaction or pool. schema is a quoted tenant schema
// (tenantschema.Quote). Each read is one version row plus its attachments;
// the walk itself is bounded by the composition's hop budget.
type CompositionSource struct {
	q      relationReader
	schema string
}

// NewCompositionSource reads through q in the quoted tenant schema.
func NewCompositionSource(q relationReader, schema string) CompositionSource {
	return CompositionSource{q: q, schema: schema}
}

// Version answers one saved version of the composition. A missing version, or
// one of another application, is ok=false: the walk does not follow it.
func (s CompositionSource) Version(ctx context.Context, id applicationcomposition.Identity) (applicationcomposition.Version, bool, error) {
	var name, agentType, instructions string
	err := s.q.QueryRow(ctx, fmt.Sprintf(`
		SELECT COALESCE(app.name, ''), COALESCE(version.agent_type, ''),
		       CASE WHEN version.agent_type = 'pipeline' AND octet_length(COALESCE(version.instructions, '')) <= $3
		            THEN COALESCE(version.instructions, '') ELSE '' END
		FROM %s.application_versions AS version
		JOIN %s.applications AS app ON app.id = version.application_id
		WHERE version.id = $1 AND version.application_id = $2`, s.schema, s.schema),
		id.VersionID, id.ApplicationID, pipelinelimits.MaxInstructionsBytes).Scan(&name, &agentType, &instructions)
	if errors.Is(err, pgx.ErrNoRows) {
		return applicationcomposition.Version{}, false, nil
	}
	if err != nil {
		return applicationcomposition.Version{}, false, fmt.Errorf("composition: read version: %w", err)
	}
	version := applicationcomposition.Version{Kind: applicationcomposition.KindAgent, Name: name}
	if agentType == "pipeline" {
		version.Kind = applicationcomposition.KindPipeline
		version.AgentNodes, version.StaticPause = applicationcomposition.ReadPipeline(instructions)
	}
	version.Attachments, err = s.Attachments(ctx, id.VersionID)
	if err != nil {
		return applicationcomposition.Version{}, false, err
	}
	return version, true, nil
}

// Attachments answers the agents and pipelines attached to one version.
// A reference whose ids are not numbers cannot be built and is left out.
func (s CompositionSource) Attachments(ctx context.Context, versionID int64) ([]applicationcomposition.Attachment, error) {
	refs, err := ListApplicationToolReferences(ctx, s.q, s.schema, strconv.FormatInt(versionID, 10))
	if err != nil {
		return nil, fmt.Errorf("composition: read attachments: %w", err)
	}
	attachments := make([]applicationcomposition.Attachment, 0, len(refs))
	for _, ref := range refs {
		applicationID, appErr := strconv.ParseInt(ref.ApplicationID, 10, 64)
		childVersionID, versionErr := strconv.ParseInt(ref.VersionID, 10, 64)
		if appErr != nil || versionErr != nil {
			continue
		}
		attachments = append(attachments, applicationcomposition.Attachment{
			Alias:    ref.Name,
			Identity: applicationcomposition.Identity{ApplicationID: applicationID, VersionID: childVersionID},
		})
	}
	return attachments, nil
}
