package mcp

// Per-project tool assembly — the half of issue 252 that serves REAL data.
//
// Two sources, both rows in the project's own schema, both matching what pylon
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
// Nothing here is hardcoded and nothing is invented: a project with no tagged
// agents and no flagged toolkits gets an empty list, which for THIS endpoint is
// a true statement about the project's rows (unlike the empty list registry.go
// refuses to fabricate, which would be a statement about sockets attached to a
// different process).

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
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
	// toolkitID and toolkitToolName are the toolkit half's discriminator, and
	// they are BOTH required to run one (#616): the producer needs the saved
	// `elitea_tools` row to redeem settings from, and the SDK tool name, which
	// is NOT the published MCP name — that one is `<toolkit>_<tool>` with
	// pylon's sanitiser applied, and no toolkit has a tool called that.
	toolkitID       int64
	toolkitToolName string
}

// runnableAgent reports whether this descriptor names an agent this service can
// execute. A toolkit tool answers false — see runnableToolkitTool.
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
	switch s.kind {
	case scopeResource:
		if s.resourceType == "toolkit" {
			return p.toolkitTools(ctx, schema, &s.resourceID)
		}
		return p.agentToolForVersion(ctx, schema, s.resourceID)
	case scopeCategory:
		if s.category == "applications" {
			return p.agentTools(ctx, schema)
		}
		return p.toolkitTools(ctx, schema, nil)
	default:
		// pylon lists toolkits first, then agents. Order is not protocol-
		// significant, but a stable one makes the listing diffable between the
		// two stacks during parity checks.
		toolkitTools, err := p.toolkitTools(ctx, schema, nil)
		if err != nil {
			return nil, err
		}
		agentTools, err := p.agentTools(ctx, schema)
		if err != nil {
			return nil, err
		}
		return dedupeByName(append(toolkitTools, agentTools...)), nil
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
func (p postgresToolSource) agentTools(ctx context.Context, schema string) ([]Tool, error) {
	if p.handler.pool == nil {
		return nil, errNoPool
	}
	// `version.id` is selected for the target discriminator, and the ORDER BY
	// gained `version.id DESC` so DISTINCT ON picks a DEFINED row — the newest
	// tagged version — rather than whichever one the plan happened to emit
	// first. Neither changes the listing: `Name` and `Description` come from
	// `application`, which DISTINCT ON already collapsed to one row per id.
	rows, err := p.handler.pool.Query(ctx, fmt.Sprintf(`
		SELECT DISTINCT ON (application.id)
		       application.id, version.id, application.name, COALESCE(application.description, '')
		FROM %[1]s.applications AS application
		JOIN %[1]s.application_versions AS version ON version.application_id = application.id
		JOIN %[1]s.application_version_tag_association AS association ON association.version_id = version.id
		JOIN %[1]s.tags AS tag ON tag.id = association.tag_id
		WHERE tag.name = 'mcp'
		ORDER BY application.id, version.id DESC`, schema))
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
func (p postgresToolSource) agentToolForVersion(ctx context.Context, schema string, versionID int64) ([]Tool, error) {
	if p.handler.pool == nil {
		return nil, errNoPool
	}
	var applicationID int64
	var name, description string
	err := p.handler.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT application.id, application.name, COALESCE(application.description, '')
		FROM %[1]s.application_versions AS version
		JOIN %[1]s.applications AS application ON application.id = version.application_id
		WHERE version.id = $1`, schema), versionID).Scan(&applicationID, &name, &description)
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
func (p postgresToolSource) toolkitTools(ctx context.Context, schema string, toolkitID *int64) ([]Tool, error) {
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
		for _, tool := range selectedToolNames(selected) {
			tools = append(tools, Tool{
				toolkitID:       id,
				toolkitToolName: tool,
				Name:            toolIdentifier(name + "_" + tool),
				// pylon's exact sentence. It is the only description a toolkit
				// tool has: the per-tool text lives in the SDK's argument
				// schemas, which this service does not hold (see below).
				Description: fmt.Sprintf(
					"Tool '%s' from toolkit type '%s'. Toolkit description: %s", tool, toolkitType, description),
				InputSchema: toolkitToolSchema(),
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return dedupeByName(tools), nil
}

// toolkitToolSchema is the input schema a toolkit tool advertises.
//
// pylon fills this from `get_toolkit_schemas(...)[type].properties.selected_tools
// .args_schemas[tool]` — a registry the Python worker builds by importing the
// SDK and calling `schema()` on every toolkit class. This service holds a
// projection of that registry
// (`internal/runtimecomposition/current_toolkit_schema_snapshot.json`), but the
// projection carries only the settings-expansion and naming annotations, not
// per-tool argument schemas, so there is nothing here to read.
//
// An open object is what pylon itself emits whenever the lookup misses
// (`.get(tool, {})`), and it is the honest schema for a tool whose arguments
// this service genuinely does not know: it says "an object, contents
// unconstrained" rather than "no arguments", which `{"type":"object",
// "properties":{}}` with no additionalProperties would imply to a strict client.
func toolkitToolSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"additionalProperties": true,
		"description": "Argument schema unavailable: toolkit tool argument schemas live in the Python SDK " +
			"registry, which this service does not hold. Arguments are passed through unchanged.",
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

// sortToolsByName gives the listing a deterministic order regardless of which
// source produced an entry, so a client diffing two responses sees content
// changes only.
func sortToolsByName(tools []Tool) {
	sort.SliceStable(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
}
