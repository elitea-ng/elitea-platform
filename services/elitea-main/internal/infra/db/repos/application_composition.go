package repos

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applicationcomposition"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/pipelinelimits"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/chatauthority"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/projectaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/publicproject"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
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

// CompositionSource reads saved versions for applicationcomposition, inside the
// caller's transaction or pool. schema is a quoted tenant schema
// (tenantschema.Quote). One source serves one request: it probes the legacy
// attachment table once, caches every version it read (a missing one too), and
// reads a level of the walk in one batch (Prefetch): two statements for the
// version rows and their attachments, plus one for legacy attachments on a
// legacy-migrated tenant. It is not safe for concurrent use.
type CompositionSource struct {
	q      compositionReader
	schema string
	legacy *bool
	cache  map[applicationcomposition.Identity]cachedVersion
}

type cachedVersion struct {
	version applicationcomposition.Version
	ok      bool
}

// NewCompositionSource reads through q in the quoted tenant schema.
func NewCompositionSource(q relationReader, schema string) *CompositionSource {
	return newCompositionSource(pgxReader{q}, schema)
}

func newCompositionSource(q compositionReader, schema string) *CompositionSource {
	return &CompositionSource{q: q, schema: schema, cache: map[applicationcomposition.Identity]cachedVersion{}}
}

// Version answers one saved version of the composition. A missing version, or
// one of another application, is ok=false: the walk does not follow it.
func (s *CompositionSource) Version(ctx context.Context, id applicationcomposition.Identity) (applicationcomposition.Version, bool, error) {
	if err := s.Prefetch(ctx, []applicationcomposition.Identity{id}); err != nil {
		return applicationcomposition.Version{}, false, err
	}
	read := s.cache[id]
	version := read.version
	// The caller may append to what it gets (an attach walks the parent with
	// the new attachment added); the cached slices stay its own.
	version.Attachments = slices.Clone(version.Attachments)
	version.AgentNodes = slices.Clone(version.AgentNodes)
	return version, read.ok, nil
}

// Prefetch reads every id not read yet in one batch.
func (s *CompositionSource) Prefetch(ctx context.Context, ids []applicationcomposition.Identity) error {
	var applicationIDs, versionIDs []int64
	for _, id := range ids {
		if _, read := s.cache[id]; read {
			continue
		}
		s.cache[id] = cachedVersion{}
		applicationIDs = append(applicationIDs, id.ApplicationID)
		versionIDs = append(versionIDs, id.VersionID)
	}
	if len(versionIDs) == 0 {
		return nil
	}
	rows, err := s.q.Query(ctx, fmt.Sprintf(`
		SELECT version.application_id, version.id, COALESCE(app.name, ''), COALESCE(version.agent_type, ''),
		       CASE WHEN version.agent_type = 'pipeline' AND octet_length(COALESCE(version.instructions, '')) <= $3
		            THEN COALESCE(version.instructions, '') ELSE '' END
		FROM unnest($1::bigint[], $2::bigint[]) AS wanted(application_id, version_id)
		JOIN %s.application_versions AS version
		  ON version.id = wanted.version_id AND version.application_id = wanted.application_id
		JOIN %s.applications AS app ON app.id = version.application_id`, s.schema, s.schema),
		applicationIDs, versionIDs, pipelinelimits.MaxInstructionsBytes)
	if err != nil {
		return fmt.Errorf("composition: read versions: %w", err)
	}
	found := map[int64]applicationcomposition.Identity{}
	for rows.Next() {
		var id applicationcomposition.Identity
		var name, agentType, instructions string
		if err := rows.Scan(&id.ApplicationID, &id.VersionID, &name, &agentType, &instructions); err != nil {
			rows.Close()
			return fmt.Errorf("composition: read versions: %w", err)
		}
		version := applicationcomposition.Version{Kind: applicationcomposition.KindAgent, Name: name}
		if agentType == "pipeline" {
			version.Kind = applicationcomposition.KindPipeline
			version.AgentNodes, version.StaticPause, version.ParallelNode, version.LLMBranches = applicationcomposition.ReadPipeline(instructions)
		}
		s.cache[id] = cachedVersion{version: version, ok: true}
		found[id.VersionID] = id
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("composition: read versions: %w", err)
	}
	if len(found) == 0 {
		return nil
	}
	parents := make([]int64, 0, len(found))
	for versionID := range found {
		parents = append(parents, versionID)
	}
	attachments, err := s.attachments(ctx, parents)
	if err != nil {
		return fmt.Errorf("composition: read attachments: %w", err)
	}
	for versionID, id := range found {
		read := s.cache[id]
		read.version.Attachments = attachments[versionID]
		s.cache[id] = read
	}
	return nil
}

// Parents answers, in one batch, the saved versions that attach any of ids
// as a tool (the reverse of attachments), on a legacy-migrated tenant also
// through the legacy table.
func (s *CompositionSource) Parents(ctx context.Context, ids []applicationcomposition.Identity) ([]applicationcomposition.Identity, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if s.legacy == nil {
		legacy, err := legacyApplicationToolsExists(ctx, s.q, s.schema)
		if err != nil {
			return nil, fmt.Errorf("composition: read parents: %w", err)
		}
		s.legacy = &legacy
	}
	children := make([]string, 0, len(ids))
	for _, id := range ids {
		children = append(children, strconv.FormatInt(id.VersionID, 10))
	}
	sql := fmt.Sprintf(`
		SELECT DISTINCT version.application_id::bigint, version.id::bigint
		FROM %[1]s.entity_tool_mapping AS mapping
		JOIN %[1]s.elitea_tools AS tool ON tool.id = mapping.tool_id
		JOIN %[1]s.application_versions AS version ON version.id = mapping.entity_version_id
		WHERE mapping.entity_type = 'agent' AND tool.type = 'application'
		  AND %[2]s = ANY($1::text[])`, s.schema, ApplicationToolSettingsVersionSQL)
	if *s.legacy {
		sql += fmt.Sprintf(`
		UNION
		SELECT version.application_id::bigint, version.id::bigint
		FROM %[1]s.application_tools AS tool
		JOIN %[1]s.application_versions AS version ON version.id = tool.application_version_id
		WHERE tool.type = 'application' AND %[2]s = ANY($1::text[])`, s.schema, ApplicationToolSettingsVersionSQL)
	}
	rows, err := s.q.Query(ctx, sql, children)
	if err != nil {
		return nil, fmt.Errorf("composition: read parents: %w", err)
	}
	defer rows.Close()
	var parents []applicationcomposition.Identity
	for rows.Next() {
		var parent applicationcomposition.Identity
		if err := rows.Scan(&parent.ApplicationID, &parent.VersionID); err != nil {
			return nil, fmt.Errorf("composition: read parents: %w", err)
		}
		parents = append(parents, parent)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("composition: read parents: %w", err)
	}
	slices.SortFunc(parents, func(a, b applicationcomposition.Identity) int {
		return cmp.Or(cmp.Compare(a.VersionID, b.VersionID), cmp.Compare(a.ApplicationID, b.ApplicationID))
	})
	return parents, nil
}

// attachments answers the agents and pipelines attached to each parent
// version, in the order ListApplicationToolReferences gives them. A reference
// whose ids are not numbers cannot be built and is left out.
func (s *CompositionSource) attachments(ctx context.Context, parents []int64) (map[int64][]applicationcomposition.Attachment, error) {
	if s.legacy == nil {
		legacy, err := legacyApplicationToolsExists(ctx, s.q, s.schema)
		if err != nil {
			return nil, err
		}
		s.legacy = &legacy
	}
	byParent := map[int64][]applicationcomposition.Attachment{}
	read := func(sql string) error {
		rows, err := s.q.Query(ctx, sql, parents)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var parent, toolID int64
			var name, applicationID, versionID string
			if err := rows.Scan(&parent, &toolID, &name, &applicationID, &versionID); err != nil {
				return err
			}
			childApplication, appErr := strconv.ParseInt(applicationID, 10, 64)
			childVersion, versionErr := strconv.ParseInt(versionID, 10, 64)
			if appErr != nil || versionErr != nil {
				continue
			}
			byParent[parent] = append(byParent[parent], applicationcomposition.Attachment{
				Alias:    name,
				Identity: applicationcomposition.Identity{ApplicationID: childApplication, VersionID: childVersion},
			})
		}
		return rows.Err()
	}
	if err := read(fmt.Sprintf(`
		SELECT mapping.entity_version_id::bigint, tool.id, tool.name,
		       COALESCE(tool.settings->>'application_id', ''), COALESCE(%s, '')
		FROM %s.entity_tool_mapping AS mapping
		JOIN %s.elitea_tools AS tool ON tool.id = mapping.tool_id
		WHERE mapping.entity_version_id = ANY($1::bigint[])
		  AND mapping.entity_type = 'agent'
		  AND tool.type = 'application'
		ORDER BY mapping.entity_version_id, tool.id`, ApplicationToolSettingsVersionSQL, s.schema, s.schema)); err != nil {
		return nil, err
	}
	if !*s.legacy {
		return byParent, nil
	}
	err := read(fmt.Sprintf(`
		SELECT tool.application_version_id::bigint, tool.id, tool.name,
		       COALESCE(tool.settings->>'application_id', ''), COALESCE(%s, '')
		FROM %s.application_tools AS tool
		WHERE tool.application_version_id = ANY($1::bigint[])
		  AND tool.type = 'application'
		ORDER BY tool.application_version_id, tool.id`, ApplicationToolSettingsVersionSQL, s.schema))
	return byParent, err
}

// errParticipantNotFound refuses a participant whose agent or pipeline the
// conversation cannot use, without saying whether it exists.
var errParticipantNotFound = apierr.NotFound("The agent or pipeline was not found.")

// checkParticipantComposition refuses an agent or pipeline participant whose
// saved composition breaks a bound (FU-244, PLAN §8.8): a conversation runs it
// as a root, so it is walked as one. A participant that is not an application,
// or names no version yet, is not checked here (its turn's start check runs).
// conversationSchema is the quoted schema of the conversation's project; the
// project the participant names is authorized before anything is read in it
// (participantSchema).
func checkParticipantComposition(ctx context.Context, q relationReader, conversationSchema, entityName string, meta, settings map[string]any) error {
	if entityName != "application" && entityName != "pipeline" {
		return nil
	}
	applicationID, okApplication := positiveInteger(meta["id"])
	versionID, okVersion := positiveInteger(settings["version_id"])
	if !okApplication || !okVersion {
		return nil
	}
	root := applicationcomposition.Identity{ApplicationID: applicationID, VersionID: versionID}
	schema, err := participantSchema(ctx, q, conversationSchema, meta["project_id"], root)
	if err != nil {
		return err
	}
	err = applicationcomposition.Check(ctx, NewCompositionSource(q, schema), root, nil)
	if refusal := applicationcomposition.Refusal(err); refusal != nil {
		return refusal
	}
	if err != nil {
		return fmt.Errorf("conversations: participant composition: %w", err)
	}
	return nil
}

// participantSchema authorizes the project a participant names before its
// composition is read there. The conversation's own project is read as is (the
// conversation route authorized the caller for it). Another project is read
// only when the caller may act inside it (projectaccess.Membership, the rule of
// the HTTP project gate), or when it is the catalogue project and the version
// is published there, which every caller may use. Anything else is refused as
// not found before any read in that project; every failure refuses. An absent
// project is the conversation's own.
func participantSchema(ctx context.Context, q relationReader, conversationSchema string, project any, root applicationcomposition.Identity) (string, error) {
	if project == nil {
		return conversationSchema, nil
	}
	owner, ok := positiveInteger(project)
	if !ok {
		return "", errParticipantNotFound
	}
	ownerSchema, err := tenantschema.Quote(strconv.FormatInt(owner, 10))
	if err != nil {
		return "", errParticipantNotFound
	}
	if ownerSchema == conversationSchema {
		return conversationSchema, nil
	}
	actor, err := chatauthority.Actor(ctx)
	if err != nil {
		return "", errParticipantNotFound
	}
	// Membership admits a platform administrator to any project id, so the
	// project must also exist (the HTTP gate pairs the two the same way).
	var member bool
	if err := q.QueryRow(ctx, "SELECT "+projectaccess.Membership(1, 2)+" AND "+projectaccess.ProjectExists(1), owner, actor).Scan(&member); err != nil {
		return "", fmt.Errorf("conversations: participant project access: %w", err)
	}
	if member {
		return ownerSchema, nil
	}
	if owner != int64(publicproject.ID()) {
		return "", errParticipantNotFound
	}
	var published bool
	if err := q.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS (
		SELECT 1 FROM %s.application_versions
		WHERE id = $1 AND application_id = $2 AND status = 'published')`, ownerSchema),
		root.VersionID, root.ApplicationID).Scan(&published); err != nil {
		return "", fmt.Errorf("conversations: participant catalogue version: %w", err)
	}
	if !published {
		return "", errParticipantNotFound
	}
	return ownerSchema, nil
}

// positiveInteger reads a JSON id: a number, or a decimal string.
func positiveInteger(value any) (int64, bool) {
	var parsed int64
	switch typed := value.(type) {
	case int:
		parsed = int64(typed)
	case int64:
		parsed = typed
	case int32:
		parsed = int64(typed)
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
	err := applicationcomposition.Check(ctx, newCompositionSource(q, schema), root, nil)
	if applicationcomposition.Refusal(err) != nil {
		return applicationcomposition.AtStart(err)
	}
	if err != nil {
		return fmt.Errorf("current application composition: %w", err)
	}
	return nil
}
