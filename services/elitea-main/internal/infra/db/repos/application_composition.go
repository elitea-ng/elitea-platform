package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applicationcomposition"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/pipelinelimits"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
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

// compositionReader is what the attachment and version reads need: a pgx pool
// or transaction (through pgxReader) or a project transaction's sqlExecutor.
type compositionReader interface {
	QueryRow(ctx context.Context, sql string, args ...any) sqlRow
	Query(ctx context.Context, sql string, args ...any) (sqlRows, error)
}

type pgxReader struct{ q relationReader }

func (r pgxReader) QueryRow(ctx context.Context, sql string, args ...any) sqlRow {
	return r.q.QueryRow(ctx, sql, args...)
}

func (r pgxReader) Query(ctx context.Context, sql string, args ...any) (sqlRows, error) {
	return r.q.Query(ctx, sql, args...)
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
	return legacyApplicationToolsExists(ctx, pgxReader{q}, schema)
}

func legacyApplicationToolsExists(ctx context.Context, q compositionReader, schema string) (bool, error) {
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
	return listApplicationToolReferences(ctx, pgxReader{q}, schema, parentVersionID)
}

func listApplicationToolReferences(ctx context.Context, q compositionReader, schema, parentVersionID string) ([]ApplicationToolReference, error) {
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

	hasLegacy, err := legacyApplicationToolsExists(ctx, q, schema)
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
	q      compositionReader
	schema string
}

// NewCompositionSource reads through q in the quoted tenant schema.
func NewCompositionSource(q relationReader, schema string) CompositionSource {
	return CompositionSource{q: pgxReader{q}, schema: schema}
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
	refs, err := listApplicationToolReferences(ctx, s.q, s.schema, strconv.FormatInt(versionID, 10))
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

// checkParticipantComposition refuses an agent or pipeline participant whose
// saved composition breaks a bound (FU-244, PLAN §8.8): a conversation runs it
// as a root, so it is walked as one. A participant that is not an application,
// or names no version yet, is not checked here (its turn's start check runs).
// conversationSchema is the quoted schema of the conversation's project; an
// entity of another project (entity_meta.project_id) is read in its own.
func checkParticipantComposition(ctx context.Context, q relationReader, conversationSchema, entityName string, meta, settings map[string]any) error {
	if entityName != "application" && entityName != "pipeline" {
		return nil
	}
	applicationID, okApplication := positiveInteger(meta["id"])
	versionID, okVersion := positiveInteger(settings["version_id"])
	if !okApplication || !okVersion {
		return nil
	}
	schema := conversationSchema
	if owner, ok := positiveInteger(meta["project_id"]); ok {
		quoted, err := tenantschema.Quote(strconv.FormatInt(owner, 10))
		if err != nil {
			return nil
		}
		schema = quoted
	}
	root := applicationcomposition.Identity{ApplicationID: applicationID, VersionID: versionID}
	err := applicationcomposition.Check(ctx, NewCompositionSource(q, schema), root, nil)
	if refusal := applicationcomposition.Refusal(err); refusal != nil {
		return refusal
	}
	if err != nil {
		return fmt.Errorf("conversations: participant composition: %w", err)
	}
	return nil
}

// positiveInteger reads a JSON id: a number, or a decimal string.
func positiveInteger(value any) (int64, bool) {
	var parsed int64
	switch typed := value.(type) {
	case float64:
		if typed != float64(int64(typed)) {
			return 0, false
		}
		parsed = int64(typed)
	case json.Number:
		number, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		parsed = number
	case string:
		number, err := strconv.ParseInt(typed, 10, 64)
		if err != nil {
			return 0, false
		}
		parsed = number
	default:
		return 0, false
	}
	return parsed, parsed > 0
}

// checkStartComposition refuses a run whose stored composition breaks a bound
// (FU-244, PLAN §8.8) with the typed composition refusal, read in the start's
// own project transaction.
func checkStartComposition(ctx context.Context, q sqlExecutor, schema string, applicationID, versionID int32) error {
	root := applicationcomposition.Identity{ApplicationID: int64(applicationID), VersionID: int64(versionID)}
	err := applicationcomposition.Check(ctx, CompositionSource{q: q, schema: schema}, root, nil)
	if applicationcomposition.Refusal(err) != nil {
		return applicationcomposition.ErrAtStart
	}
	if err != nil {
		return fmt.Errorf("current application composition: %w", err)
	}
	return nil
}
