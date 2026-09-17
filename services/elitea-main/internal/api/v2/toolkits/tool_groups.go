package toolkits

import "strings"

// Tool GROUPS: the served, per-type answer to "what does this tool do to the
// target system?"
//
// ─────────────────────────────────────────────────────────────────────────────
// WHY THE SERVER ANSWERS THIS AT ALL
// ─────────────────────────────────────────────────────────────────────────────
// The toolkit editor renders one flat chip list of every tool a type offers.
// For GitHub that is 26+ chips in one row, with `delete_file` sitting between
// `create_file` and `get_file_metadata` and nothing distinguishing them. The
// person ticking those boxes is deciding what an autonomous agent will be
// allowed to do, and the screen gives them no way to see that one of the boxes
// is destructive.
//
// The grouping could have been invented in the web client. It is here instead
// because the tool list is served from here — `properties.selected_tools`
// (`args_schemas` keys, or the older `items.enum`) is this handler's contract
// with the client — and a classification that lived only in the browser would
// have to be re-derived by every other consumer of the same catalogue, and
// would drift the moment the SDK adds a tool.
//
// THE STORED SHAPE IS UNCHANGED. `selected_tools` remains the same array of
// tool NAMES it has always been. Nothing here is written to a toolkit row; an
// agent, a pipeline or an MCP client reading a saved toolkit sees exactly what
// it saw before.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHY THE CLASSIFICATION IS DERIVED FROM THE NAME
// ─────────────────────────────────────────────────────────────────────────────
// The pinned SDK snapshot carries no category, capability or side-effect
// annotation of any kind — its per-tool content is the argument schema and
// nothing else (verified across all 53 entries of
// runtimecomposition/current_toolkit_schema_snapshot.json). So there is no
// upstream field to serve, and the choice is between serving nothing and
// deriving something from the one signal that exists: the tool's name.
//
// The SDK names tools verb-first and consistently — 161 `get_*`, 75 `create_*`,
// 66 `list_*`, 46 `update_*`, 36 `search_*`, 26 `delete_*` across 444 distinct
// tools — so the derivation is reliable in practice, and it is deliberately
// CONSERVATIVE where it is not: the FIRST recognised verb token wins, which is
// what keeps `search_index` a read (it searches an index) while `index_data` is
// a write (it indexes data). A last-token or "any token" rule gets that pair
// backwards.
//
// A tool whose name carries no recognised verb (`places`, `fields_metadata`,
// `job_stats`) falls to Read. That is the correct answer for every such name in
// the pinned snapshot, and the fallback is stated here rather than left
// implicit because it is the one place this derivation could be wrong about a
// future tool: a new SDK tool with an unrecognised WRITE verb would be grouped
// as a read. The group is a display aid over a list the user still ticks
// individually — it never grants or withholds anything — so the failure mode is
// a mislabelled chip, not an unintended permission.

// ToolGroup is the served group identifier. The web client maps these to the
// group's label, badge and tooltip; keeping the identifiers opaque and stable
// is what lets the wording change without a server release.
type ToolGroup string

const (
	// ToolGroupRead returns data. Nothing is created, changed or destroyed.
	ToolGroupRead ToolGroup = "read"
	// ToolGroupCreateUpdate creates or modifies data in the target system.
	ToolGroupCreateUpdate ToolGroup = "create_update"
	// ToolGroupDelete destroys data. Not reversible from this platform.
	ToolGroupDelete ToolGroup = "delete"
	// ToolGroupExecute runs a caller-supplied query, script, pipeline or raw
	// API call. The effect is not bounded by the tool.
	ToolGroupExecute ToolGroup = "execute"
)

// toolGroupOrder is the FIXED display order, least to most dangerous. It is
// served rather than left to the client so every surface that renders these
// groups orders them identically.
var toolGroupOrder = []any{
	string(ToolGroupRead),
	string(ToolGroupCreateUpdate),
	string(ToolGroupDelete),
	string(ToolGroupExecute),
}

// toolGroupVerbs maps a name token to the group it decides.
//
// Read verbs ARE listed even though Read is also the fallback, and that is
// load-bearing rather than redundant: the first RECOGNISED token wins, so
// `search` has to be recognised for `search_index` to come out a read instead
// of being decided by the `index` token behind it.
var toolGroupVerbs = map[string]ToolGroup{
	// Returns data. Nothing is created, changed or destroyed.
	"get": ToolGroupRead, "list": ToolGroupRead, "read": ToolGroupRead,
	"search": ToolGroupRead, "find": ToolGroupRead, "grep": ToolGroupRead,
	"stepback": ToolGroupRead, "similarity": ToolGroupRead, "describe": ToolGroupRead,
	"show": ToolGroupRead, "download": ToolGroupRead, "browse": ToolGroupRead,
	"retrieve": ToolGroupRead, "fetch": ToolGroupRead, "view": ToolGroupRead,
	"extract": ToolGroupRead, "analyze": ToolGroupRead, "metadata": ToolGroupRead,
	"stats": ToolGroupRead, "healthcheck": ToolGroupRead, "count": ToolGroupRead,
	"preview": ToolGroupRead, "export": ToolGroupRead, "diff": ToolGroupRead,

	// Destroys data.
	"delete": ToolGroupDelete, "remove": ToolGroupDelete, "drop": ToolGroupDelete,
	"purge": ToolGroupDelete, "destroy": ToolGroupDelete, "erase": ToolGroupDelete,
	"truncate": ToolGroupDelete, "clear": ToolGroupDelete, "unlink": ToolGroupDelete,
	"uninstall": ToolGroupDelete, "revoke": ToolGroupDelete,

	// Runs something the caller supplied. Not bounded by the tool.
	"execute": ToolGroupExecute, "exec": ToolGroupExecute, "run": ToolGroupExecute,
	"invoke": ToolGroupExecute, "eval": ToolGroupExecute, "generic": ToolGroupExecute,
	"sandbox": ToolGroupExecute, "trigger": ToolGroupExecute, "raw": ToolGroupExecute,

	// Creates or modifies.
	"create": ToolGroupCreateUpdate, "add": ToolGroupCreateUpdate, "update": ToolGroupCreateUpdate,
	"set": ToolGroupCreateUpdate, "write": ToolGroupCreateUpdate, "upload": ToolGroupCreateUpdate,
	"append": ToolGroupCreateUpdate, "patch": ToolGroupCreateUpdate, "edit": ToolGroupCreateUpdate,
	"modify": ToolGroupCreateUpdate, "post": ToolGroupCreateUpdate, "put": ToolGroupCreateUpdate,
	"insert": ToolGroupCreateUpdate, "upsert": ToolGroupCreateUpdate, "send": ToolGroupCreateUpdate,
	"attach": ToolGroupCreateUpdate, "link": ToolGroupCreateUpdate, "move": ToolGroupCreateUpdate,
	"copy": ToolGroupCreateUpdate, "rename": ToolGroupCreateUpdate, "assign": ToolGroupCreateUpdate,
	"apply": ToolGroupCreateUpdate, "index": ToolGroupCreateUpdate, "import": ToolGroupCreateUpdate,
	"publish": ToolGroupCreateUpdate, "comment": ToolGroupCreateUpdate, "submit": ToolGroupCreateUpdate,
	"merge": ToolGroupCreateUpdate, "push": ToolGroupCreateUpdate, "save": ToolGroupCreateUpdate,
	"replace": ToolGroupCreateUpdate, "mark": ToolGroupCreateUpdate, "enable": ToolGroupCreateUpdate,
	"disable": ToolGroupCreateUpdate, "fork": ToolGroupCreateUpdate, "transition": ToolGroupCreateUpdate,
	"register": ToolGroupCreateUpdate, "restore": ToolGroupCreateUpdate, "duplicate": ToolGroupCreateUpdate,
	"checkout": ToolGroupCreateUpdate, "manage": ToolGroupCreateUpdate, "confirm": ToolGroupCreateUpdate,
	"invite": ToolGroupCreateUpdate, "fill": ToolGroupCreateUpdate, "commit": ToolGroupCreateUpdate,
	"close": ToolGroupCreateUpdate, "cancel": ToolGroupCreateUpdate, "generate": ToolGroupCreateUpdate,
	"sync": ToolGroupCreateUpdate,
}

// ClassifyToolGroup answers which group one tool name belongs to.
//
// Exported for the tests that pin the whole pinned-snapshot catalogue against
// it; nothing outside this package calls it in production.
func ClassifyToolGroup(name string) ToolGroup {
	for _, token := range toolNameTokens(name) {
		if group, found := toolGroupVerbs[token]; found {
			return group
		}
	}
	return ToolGroupRead
}

// toolNameTokens splits a tool name into lower-case words, in order.
//
// Both spellings the SDK uses are handled: `snake_case` (the overwhelming
// majority) and the handful of `camelCase` names (`indexDocuments`,
// `stepbackSearch`). A camel boundary that splits `indexDocuments` into
// `index`+`documents` is what keeps that one classified with its snake_case
// twin `index_data` instead of falling to the read fallback.
func toolNameTokens(name string) []string {
	tokens := make([]string, 0, 4)
	current := strings.Builder{}
	flush := func() {
		if current.Len() > 0 {
			tokens = append(tokens, current.String())
			current.Reset()
		}
	}
	for _, character := range name {
		switch {
		case character == '_' || character == '-' || character == '.' || character == ' ':
			flush()
		case character >= 'A' && character <= 'Z':
			flush()
			current.WriteRune(character - 'A' + 'a')
		default:
			current.WriteRune(character)
		}
	}
	flush()
	return tokens
}

// withToolGroups copies one type's settings schema with
// `properties.selected_tools.tool_groups` and `.tool_group_order` added.
//
// It is a no-op for a type whose tool list is not known at catalogue time —
// a Remote MCP toolkit's tools are discovered from the live server and
// arrive in the instance's own `settings.available_mcp_tools`, never here. A
// type with no groups is exactly how the client decides to keep rendering the
// flat list it always did, so "no answer" and "everything is a read" must not
// look the same.
func withToolGroups(settingsSchema map[string]any) map[string]any {
	settingsProperties, _ := settingsSchema["properties"].(map[string]any)
	selectedTools, _ := settingsProperties["selected_tools"].(map[string]any)
	names := toolNamesFromSelectedTools(selectedTools)
	if len(names) == 0 {
		return settingsSchema
	}

	groups := make(map[string]any, len(names))
	for _, name := range names {
		groups[name] = string(ClassifyToolGroup(name))
	}

	// Every node on the written path is rebuilt: toolkitTypeSchemas is
	// package-level state shared by every request, and an in-place edit would
	// leak into the next one — the same rule toolkitTypeCatalogue states.
	rebuiltSelectedTools := make(map[string]any, len(selectedTools)+2)
	for key, value := range selectedTools {
		rebuiltSelectedTools[key] = value
	}
	rebuiltSelectedTools["tool_groups"] = groups
	rebuiltSelectedTools["tool_group_order"] = append([]any(nil), toolGroupOrder...)

	rebuiltProperties := make(map[string]any, len(settingsProperties))
	for key, value := range settingsProperties {
		rebuiltProperties[key] = value
	}
	rebuiltProperties["selected_tools"] = rebuiltSelectedTools

	rebuilt := make(map[string]any, len(settingsSchema))
	for key, value := range settingsSchema {
		rebuilt[key] = value
	}
	rebuilt["properties"] = rebuiltProperties
	return rebuilt
}

// toolNamesFromSelectedTools reads the tool names off a served
// `selected_tools` node: the `args_schemas` keys, or the older `items.enum`.
//
// Exactly the lookup the web client performs
// (`ToolBase.render.tsx`'s resolveAvailableTools and
// `lib/helpers/indexesTabVisibility.ts`), so the group map can never be keyed
// on names the chip list does not render.
func toolNamesFromSelectedTools(selectedTools map[string]any) []string {
	if argsSchemas, ok := selectedTools["args_schemas"].(map[string]map[string]any); ok && len(argsSchemas) > 0 {
		names := make([]string, 0, len(argsSchemas))
		for name := range argsSchemas {
			names = append(names, name)
		}
		return names
	}
	if argsSchemas, ok := selectedTools["args_schemas"].(map[string]any); ok && len(argsSchemas) > 0 {
		names := make([]string, 0, len(argsSchemas))
		for name := range argsSchemas {
			names = append(names, name)
		}
		return names
	}
	items, _ := selectedTools["items"].(map[string]any)
	enum, _ := items["enum"].([]any)
	names := make([]string, 0, len(enum))
	for _, value := range enum {
		if name, ok := value.(string); ok && name != "" {
			names = append(names, name)
		}
	}
	return names
}
