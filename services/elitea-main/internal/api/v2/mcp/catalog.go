package mcp

// Per-project tool assembly — the half of issue 252 that serves REAL data.
//
// The external project listing has two row-backed sources, matching what pylon
// `utils/mcp_service.py:__get_all_tools` reads:
//
//	AGENTS   — `applications` whose version carries the tag named `mcp`. One
//	           tool per agent, taking a single `task` string. pylon resolves the
//	           `mcp` tag id first and lists NOTHING when the project has no such
//	           tag; that is preserved, because the tag is the opt-in.
//	TOOLKITS — `elitea_tools` rows flagged `meta.mcp_options.available_by_mcp`.
//	           One tool per entry of `settings.selected_tools`, named
//	           `<toolkit>_<tool>`.
//
// A separate fixed source serves internal builder categories. It is selected
// only by an exact category path and never enters the external project listing.
// A project with no tagged agents and no flagged toolkits therefore still gets
// an empty external list, which is a true statement about the project's rows.
// When the optional social folder projection exists, both sources also exclude
// entities in a no_access folder for the authenticated actor.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// Tool is one MCP tool descriptor. The JSON tags are the protocol's, including
// the camelCase `inputSchema`.
//
// # The unexported target discriminator
//
// The three exported fields are the whole of the wire shape, and they are not
// enough to RUN anything. `toolIdentifier` is lossy (every character outside
// `[A-Za-z0-9_\-]` becomes `_`) and `dedupeByName` drops collisions, so a name
// cannot be mapped back to the row it came from: two differently-named agents
// can collapse to one identifier, and the listing keeps whichever the SELECT
// emitted first. Re-deriving a call target from the name would therefore be
// able to run an agent the caller was never shown.
//
// So the descriptor carries its own target, populated by the SAME SELECTs that
// build the listing (`agentTools` already selected `application.id` and threw
// it away). The fields are UNEXPORTED, which is what keeps this internal:
// encoding/json never marshals an unexported field, so the `tools/list`
// response is byte-identical to what it was before this existed. That is
// pinned by TestToolWireShapeExcludesTargetDiscriminator — a serialised
// discriminator would change every existing client's tool list.
//
// Zero means "not an agent": a toolkit tool has no application behind it, and
// callTool uses exactly that to tell the two halves apart.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`

	// applicationID is the `applications` row this tool runs.
	applicationID int64
	// applicationVersionID is the `application_versions` row the run is pinned
	// to. It is REQUIRED alongside applicationID rather than optional: the turn
	// resolver joins `application_versions` through the participant mapping's
	// `entity_settings ->> 'version_id'` (internal/db/queries/agent_chat.sql,
	// ResolveCurrentApplicationTurn), so a target with no version cannot be
	// admitted at all.
	applicationVersionID int64
	// toolkitID and toolkitToolName pin an external toolkit call to the exact
	// opted-in row and selected SDK operation that produced this descriptor.
	// The durable executor consumes both fields and re-reads the row before
	// admission, so it never reverses a lossy MCP name into an arbitrary row.
	toolkitID       int64
	toolkitToolName string
	// internalApplicationOperation is populated only for the fixed
	// elitea_core/applications category. It is never serialized.
	internalApplicationOperation internalApplicationOperation
	// internalSkillOperation is populated only for the fixed skills category.
	// It is never serialized.
	internalSkillOperation internalSkillOperation
	// internalToolkitOperation is populated only for the fixed toolkit-builder
	// category. It is never serialized.
	internalToolkitOperation internalToolkitOperation
	// internalConfigurationOperation is populated only for the fixed
	// configurations category. It is never serialized.
	internalConfigurationOperation internalConfigurationOperation
	// internalNotificationOperation is populated only for the fixed
	// notifications category. It is never serialized.
	internalNotificationOperation internalNotificationOperation
	// internalProjectContextOperation is populated only for the fixed
	// project-context builder category. It is never serialized.
	internalProjectContextOperation internalProjectContextOperation
	// internalSecretOperation is populated only for the fixed secrets category.
	// It is never serialized.
	internalSecretOperation internalSecretOperation
	// permission is re-checked for each internal API invocation.
	permission string
}

// runnableAgent reports whether this descriptor names an agent this service can
// execute. Toolkit descriptors have their own exact-target predicate below.
func (t Tool) runnableAgent() bool {
	return t.applicationID > 0 && t.applicationVersionID > 0
}

// runnableToolkitTool reports whether this descriptor names a toolkit tool this
// service can run. The two predicates are mutually exclusive by construction:
// each listing query populates one pair of fields and never the other.
func (t Tool) runnableToolkitTool() bool {
	return t.toolkitID > 0 && t.toolkitToolName != ""
}

// toolSource is the seam the HTTP layer depends on, so the protocol handling in
// server.go can be exercised without a database. The production implementation
// is postgresToolSource, below.
type toolSource interface {
	tools(ctx context.Context, schema string, s scope) ([]Tool, error)
}

// agentTaskSchema is the input schema every agent tool carries — pylon's, field
// for field. An agent takes free-form instruction text, so the schema is one
// required string; there is no per-agent schema to read anywhere.
func agentTaskSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task": map[string]any{
				"type":        "string",
				"description": "Task or message for agent",
			},
		},
		"required": []string{"task"},
	}
}

// invalidToolNameChars is pylon's `_build_agent_identifier`:
// `re.sub(r'[^A-Za-z0-9_\-]', "_", name)`. MCP tool names are matched literally
// by clients, so the sanitisation has to agree with pylon's exactly or the same
// agent gets a different tool name on the two stacks.
var invalidToolNameChars = regexp.MustCompile(`[^A-Za-z0-9_\-]`)

func toolIdentifier(name string) string {
	return invalidToolNameChars.ReplaceAllString(name, "_")
}

type postgresToolSource struct {
	handler *Handler
}

func (p postgresToolSource) tools(ctx context.Context, schema string, s scope) ([]Tool, error) {
	if s.kind == scopeCategory {
		if s.category == internalApplicationsCategory {
			return internalApplicationTools(), nil
		}
		if s.category == internalSkillsCategory {
			return internalSkillTools(), nil
		}
		if s.category == internalToolkitsCategory {
			return internalToolkitTools(), nil
		}
		if s.category == internalConfigurationsCategory {
			return internalConfigurationTools(), nil
		}
		if s.category == internalNotificationsCategory {
			return internalNotificationTools(), nil
		}
		if s.category == internalProjectContextCategory {
			return internalProjectContextTools(), nil
		}
		if s.category == internalSecretsCategory {
			return internalSecretTools(), nil
		}
	}

	access, err := p.externalAccess(ctx, schema)
	if err != nil {
		return nil, err
	}
	switch s.kind {
	case scopeResource:
		if s.resourceType == "toolkit" {
			return p.toolkitTools(ctx, schema, access, &s.resourceID)
		}
		return p.agentToolForVersion(ctx, schema, access, s.resourceID)
	case scopeCategory:
		if s.category == "applications" {
			return p.agentTools(ctx, schema, access)
		}
		return p.toolkitTools(ctx, schema, access, nil)
	default:
		// pylon lists toolkits first, then agents. Order is not protocol-
		// significant, but a stable one makes the listing diffable between the
		// two stacks during parity checks.
		toolkitTools, err := p.toolkitTools(ctx, schema, access, nil)
		if err != nil {
			return nil, err
		}
		agentTools, err := p.agentTools(ctx, schema, access)
		if err != nil {
			return nil, err
		}
		return dedupeByName(append(toolkitTools, agentTools...)), nil
	}
}

type externalCatalogAccess struct {
	actorID            int64
	folderRestrictions bool
}

var errExternalCatalogIdentity = errors.New("MCP external catalog requires an owning user")
var errPartialFolderAccessProjection = errors.New("MCP folder access projection is incomplete")

// externalAccess resolves the actor and detects the optional social folder
// projection once per tools/list or tools/call lookup. A partial projection
// fails closed because it cannot produce a reliable authorization answer.
func (p postgresToolSource) externalAccess(
	ctx context.Context,
	schema string,
) (externalCatalogAccess, error) {
	if p.handler == nil || p.handler.pool == nil {
		return externalCatalogAccess{}, errNoPool
	}
	user, ok := auth.UserFromContext(ctx)
	if !ok {
		return externalCatalogAccess{}, errExternalCatalogIdentity
	}
	actorID, ok := user.OwningUserID()
	if !ok {
		return externalCatalogAccess{}, errExternalCatalogIdentity
	}

	var state int32
	err := p.handler.pool.QueryRow(ctx, `
		SELECT CASE
			WHEN to_regclass($1) IS NOT NULL
			 AND to_regclass($2) IS NOT NULL
			 AND to_regclass($3) IS NOT NULL THEN 1
			WHEN to_regclass($3) IS NULL THEN 0
			ELSE -1
		END::integer`,
		schema+".entity_folders",
		schema+".social_folder_items",
		schema+".folder_access_overrides",
	).Scan(&state)
	if err != nil {
		return externalCatalogAccess{}, err
	}
	switch state {
	case 0:
		return externalCatalogAccess{actorID: actorID}, nil
	case 1:
		return externalCatalogAccess{actorID: actorID, folderRestrictions: true}, nil
	default:
		return externalCatalogAccess{}, errPartialFolderAccessProjection
	}
}

// agentTools lists the agents this project exposes over MCP.
//
// The join through `tags` on the literal name `mcp` is the opt-in. pylon
// resolves the tag id separately and passes an empty filter list when the
// project has no `mcp` tag, in which case `__get_all_tools` skips the agent
// block entirely — the same outcome this join produces, without the round trip.
//
// DISTINCT ON the application: an agent with several tagged versions is one
// tool, not one per version. pylon reaches the same place by listing
// applications rather than versions.
func (p postgresToolSource) agentTools(
	ctx context.Context,
	schema string,
	access externalCatalogAccess,
) ([]Tool, error) {
	if p.handler.pool == nil {
		return nil, errNoPool
	}
	// `version.id` is selected for the target discriminator, and the ORDER BY
	// gained `version.id DESC` so DISTINCT ON picks a DEFINED row — the newest
	// tagged version — rather than whichever one the plan happened to emit
	// first. Neither changes the listing: `Name` and `Description` come from
	// `application`, which DISTINCT ON already collapsed to one row per id.
	query := fmt.Sprintf(`
		SELECT DISTINCT ON (application.id)
		       application.id, version.id, application.name, COALESCE(application.description, '')
		FROM %[1]s.applications AS application
		JOIN %[1]s.application_versions AS version ON version.application_id = application.id
		JOIN %[1]s.application_version_tag_association AS association ON association.version_id = version.id
		JOIN %[1]s.tags AS tag ON tag.id = association.tag_id
		WHERE tag.name = 'mcp'`, schema)
	args := []any{}
	if access.folderRestrictions {
		args = append(args, access.actorID)
		query += fmt.Sprintf(`
		  AND NOT EXISTS (
		      SELECT 1
		      FROM %[1]s.social_folder_items AS item
		      JOIN %[1]s.folder_access_overrides AS access
		        ON access.folder_id = item.folder_id
		      WHERE item.entity IN ('agent', 'pipeline')
		        AND item.entity_id = application.id
		        AND access.user_id = $1
		        AND access.access_level = 'no_access'
		  )`, schema)
	}
	query += " ORDER BY application.id, version.id DESC"
	rows, err := p.handler.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tools []Tool
	for rows.Next() {
		var id, versionID int64
		var name, description string
		if err := rows.Scan(&id, &versionID, &name, &description); err != nil {
			return nil, err
		}
		tools = append(tools, Tool{
			Name:                 toolIdentifier(name),
			Description:          description,
			InputSchema:          agentTaskSchema(),
			applicationID:        id,
			applicationVersionID: versionID,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return dedupeByName(tools), nil
}

// agentToolForVersion is the resource-scoped variant: one application version
// id in, that application's single tool out.
//
// Unlike the listing above this does NOT require the `mcp` tag. That is pylon's
// behaviour (`__get_application_tools` looks the version up and emits the tool
// unconditionally) and it is coherent: the caller reached this endpoint by
// naming one specific version, so the project-wide opt-in tag is not what is
// selecting it. The tenant boundary still holds — the version is looked up in
// this project's schema only, so a version id from another project is not
// found.
func (p postgresToolSource) agentToolForVersion(
	ctx context.Context,
	schema string,
	access externalCatalogAccess,
	versionID int64,
) ([]Tool, error) {
	if p.handler.pool == nil {
		return nil, errNoPool
	}
	var applicationID int64
	var name, description string
	query := fmt.Sprintf(`
		SELECT application.id, application.name, COALESCE(application.description, '')
		FROM %[1]s.application_versions AS version
		JOIN %[1]s.applications AS application ON application.id = version.application_id
		WHERE version.id = $1`, schema)
	args := []any{versionID}
	if access.folderRestrictions {
		args = append(args, access.actorID)
		query += fmt.Sprintf(`
		  AND NOT EXISTS (
		      SELECT 1
		      FROM %[1]s.social_folder_items AS item
		      JOIN %[1]s.folder_access_overrides AS access
		        ON access.folder_id = item.folder_id
		      WHERE item.entity IN ('agent', 'pipeline')
		        AND item.entity_id = application.id
		        AND access.user_id = $2
		        AND access.access_level = 'no_access'
		  )`, schema)
	}
	err := p.handler.pool.QueryRow(ctx, query, args...).Scan(&applicationID, &name, &description)
	if err != nil {
		if isNoRows(err) {
			// An id that names nothing in this project is an empty listing, not
			// an error: it is exactly what pylon does (it logs and returns []),
			// and it does not tell a caller probing ids whether the row exists
			// in some other tenant.
			return nil, nil
		}
		return nil, err
	}
	return []Tool{{
		Name:        toolIdentifier(name),
		Description: description,
		InputSchema: agentTaskSchema(),
		// The version is the one the CALLER named in the URL, not a
		// project-wide pick — which is the whole point of this scope, and the
		// reason a resource-scoped call runs exactly the version addressed.
		applicationID:        applicationID,
		applicationVersionID: versionID,
	}}, nil
}

// toolkitTools lists the tools of the project's MCP-exposed toolkits, or of one
// named toolkit when toolkitID is non-nil.
//
// `meta.mcp_options.available_by_mcp` is the opt-in, and it is applied in the
// single-toolkit case too: pylon refuses to serve a toolkit that was not
// flagged even when it is addressed directly, and dropping that check here
// would make the resource-scoped URL a way around the flag.
//
// The `#>>` text extraction rather than a boolean cast is deliberate: the key
// is absent on most rows, and `(meta -> 'x' ->> 'y')::boolean` on a row where
// the intermediate object is missing is NULL rather than false — fine — but on
// a row where someone stored the string "yes" it raises, which would fail the
// whole listing because of one malformed toolkit.
func (p postgresToolSource) toolkitTools(
	ctx context.Context,
	schema string,
	access externalCatalogAccess,
	toolkitID *int64,
) ([]Tool, error) {
	if p.handler.pool == nil {
		return nil, errNoPool
	}
	query := fmt.Sprintf(`
		SELECT id, name, type, COALESCE(description, ''), COALESCE(settings -> 'selected_tools', '[]'::jsonb)
		FROM %s.elitea_tools
		WHERE meta #>> '{mcp_options,available_by_mcp}' = 'true'`, schema)
	args := []any{}
	if toolkitID != nil {
		query += " AND id = $1"
		args = append(args, *toolkitID)
	}
	if access.folderRestrictions {
		args = append(args, access.actorID)
		query += fmt.Sprintf(`
		  AND NOT EXISTS (
		      SELECT 1
		      FROM %[1]s.social_folder_items AS item
		      JOIN %[1]s.folder_access_overrides AS access
		        ON access.folder_id = item.folder_id
		      WHERE item.entity IN ('toolkit', 'mcp')
		        AND item.entity_id = elitea_tools.id
		        AND access.user_id = $%[2]d
		        AND access.access_level = 'no_access'
		  )`, schema, len(args))
	}
	query += " ORDER BY id"

	rows, err := p.handler.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tools []Tool
	for rows.Next() {
		var id int64
		var name, toolkitType, description string
		var selected []byte
		if err := rows.Scan(&id, &name, &toolkitType, &description, &selected); err != nil {
			return nil, err
		}
		argumentSchemas, schemasKnown, err := p.toolkitSchemas(toolkitType)
		if err != nil {
			return nil, err
		}
		for _, tool := range selectedToolNames(selected) {
			inputSchema := unknownToolkitToolSchema()
			if schemasKnown {
				if schema, found := argumentSchemas[tool]; found {
					inputSchema = schema
				}
			}
			tools = append(tools, Tool{
				Name: toolIdentifier(name + "_" + tool),
				// pylon's exact sentence. Per-argument descriptions remain in
				// InputSchema, from the same SDK snapshot used by the editor.
				Description: fmt.Sprintf(
					"Tool '%s' from toolkit type '%s'. Toolkit description: %s", tool, toolkitType, description),
				InputSchema:     inputSchema,
				toolkitID:       id,
				toolkitToolName: tool,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return rejectAmbiguousToolkitNames(tools), nil
}

// toolkitSchemas obtains one detached per-type schema map. It is deliberately
// called once per toolkit row rather than once per selected operation: the
// digest-pinned source performs a bounded deep clone, and repeating that work
// for every selected tool would turn a 44-tool GitHub toolkit into 44 clones of
// the same catalogue entry.
func (p postgresToolSource) toolkitSchemas(toolkitType string) (map[string]map[string]any, bool, error) {
	if p.handler == nil || p.handler.toolkitArgumentSchemas == nil {
		return nil, false, nil
	}
	return p.handler.toolkitArgumentSchemas.ToolkitArgumentSchemas(toolkitType)
}

// unknownToolkitToolSchema is the fallback for an argument schema the pinned
// catalogue genuinely cannot know.
//
// pylon fills this from `get_toolkit_schemas(...)[type].properties.selected_tools
// .args_schemas[tool]` — a registry the Python worker builds by importing the
// SDK and calling `schema()` on every toolkit class. Main's composition root
// injects the equivalent digest-pinned projection from
// `internal/runtimecomposition/current_toolkit_schema_snapshot.json`.
//
// Dynamic MCP, MCP-config and OpenAPI tools are discovered from a remote server
// or specification and therefore legitimately have no built-in argument
// schema. An open object is honest for those and for stale selected-tool names:
// it says "object, contents unconstrained", not "this tool takes no input".
func unknownToolkitToolSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"additionalProperties": true,
		"description": "Argument schema unavailable in the pinned built-in toolkit catalogue; " +
			"the operation may be dynamically discovered or no longer present. Arguments are passed through unchanged.",
	}
}

// selectedToolNames reads `settings.selected_tools`.
//
// Both encodings that occur in the wild are accepted: the list of plain names
// pylon writes, and the list of objects the toolkit editor sends for types
// whose tools carry per-tool settings. Anything else in the array is skipped
// rather than failing the listing — one malformed entry must not remove a whole
// toolkit from a client's tool list.
func selectedToolNames(raw []byte) []string {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		var name string
		if err := json.Unmarshal(entry, &name); err == nil {
			if name = strings.TrimSpace(name); name != "" {
				names = append(names, name)
			}
			continue
		}
		var object struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(entry, &object); err == nil {
			if name = strings.TrimSpace(object.Name); name != "" {
				names = append(names, name)
			}
		}
	}
	return names
}

// dedupeByName drops later tools that collide on the sanitised name, keeping
// the first.
//
// The collision is real, not theoretical: `toolIdentifier` maps every character
// outside `[A-Za-z0-9_-]` to `_`, so agents named "Release Notes" and
// "Release/Notes" both become `Release_Notes`. pylon logs and skips the second
// one for agents and API tools. Serving both would be worse than dropping one:
// `tools/call` resolves by name, so a duplicate makes which tool actually runs
// depend on iteration order.
func dedupeByName(tools []Tool) []Tool {
	if len(tools) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(tools))
	unique := make([]Tool, 0, len(tools))
	for _, tool := range tools {
		if _, duplicate := seen[tool.Name]; duplicate {
			continue
		}
		seen[tool.Name] = struct{}{}
		unique = append(unique, tool)
	}
	return unique
}

// rejectAmbiguousToolkitNames makes the project-wide toolkit catalogue agree
// with the current platform's dispatch guard: a sanitised name produced by two
// different toolkit targets resolves to neither target. Advertising whichever
// row happened to be read first would be misleading because a later
// tools/call cannot safely choose between them.
//
// Repeated copies of the same selected operation are harmless legacy data and
// collapse to one descriptor. A resource-scoped catalogue still exposes its
// exact toolkit row because there is no second target in that scope.
func rejectAmbiguousToolkitNames(tools []Tool) []Tool {
	if len(tools) == 0 {
		return nil
	}
	type target struct {
		toolkitID int64
		toolName  string
	}
	firstTargets := make(map[string]target, len(tools))
	ambiguous := make(map[string]struct{})
	for _, tool := range tools {
		current := target{toolkitID: tool.toolkitID, toolName: tool.toolkitToolName}
		first, found := firstTargets[tool.Name]
		if !found {
			firstTargets[tool.Name] = current
			continue
		}
		if first != current {
			ambiguous[tool.Name] = struct{}{}
		}
	}

	unique := make([]Tool, 0, len(tools))
	seen := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if _, rejected := ambiguous[tool.Name]; rejected {
			continue
		}
		if _, duplicate := seen[tool.Name]; duplicate {
			continue
		}
		seen[tool.Name] = struct{}{}
		unique = append(unique, tool)
	}
	return unique
}

// sortToolsByName gives the listing a deterministic order regardless of which
// source produced an entry, so a client diffing two responses sees content
// changes only.
func sortToolsByName(tools []Tool) {
	sort.SliceStable(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
}
