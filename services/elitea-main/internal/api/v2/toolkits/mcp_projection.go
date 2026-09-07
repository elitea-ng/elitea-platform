package toolkits

// The MCP half of the catalogue: one generic Remote MCP type, plus one type per
// pre-built catalogue row.
//
// # Why the generic type is here and not in the snapshot
//
// The SDK DOES declare `mcp` — `elitea_sdk.runtime.toolkits.mcp.McpToolkit.
// toolkit_config_schema` — but the pinned snapshot keeps only ANNOTATED fields,
// and none of the Remote MCP settings carry an annotation. The snapshot's entry
// is therefore `{"args_schemas":{},"properties":{},"type":"mcp"}`: the type is
// known, the settings are not. `remoteMCPType` below restates that schema, field
// for field, from the SDK source at the pinned revision.
//
// The consequence of it being absent is the whole of validation gap 1: the web
// client's MCP page filters the served catalogue to its mcp-flavoured entries
// (`useGetCurrentMCPSchemas.hooks.ts`), finds none, and renders "Still no local
// MCP available" with no Remote section at all. No MCP can be created from the
// browser. Serving this one entry is what opens that page, and it is the reason
// the E2E journey J18 asserts the catalogue and the page agree in BOTH
// directions rather than asserting a fixed list.
//
// # Why the settings schema carries no `transport`
//
// Production offers none, and neither runtime reads one: the Go discoverer
// speaks streamable HTTP only (`internal/mcpregistry/discover.go`), and the
// native worker builds the same transport unconditionally
// (`services/elitea-worker-rust/src/toolkits/mcp.rs`). A transport selector
// would be a control that nothing honours, which is the exact fault
// `mcpregistry.Resolve` records for `ssl_verify`.
//
// # Headers are a supported field with one honest limit
//
// `headers` is how a Remote MCP toolkit authenticates, and both the discovery
// path here and the Python worker send them. The NATIVE Rust worker refuses a
// toolkit that carries any header at all (`reject_unowned_auth`, mcp.rs:576) —
// it accepts only a continuation token fetched with the claim. That is a
// deliberate authority decision in that runtime, not an oversight here, so the
// field is served with a description that says where it applies.

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpregistry"
)

// remoteMCPToolkitType is the generic type a user picks to connect one remote
// MCP server. It is the SDK's own registry key.
// Unexported: no package outside this one names it, and an exported constant
// would advertise a seam nothing uses.
const remoteMCPToolkitType = "mcp"

// remoteMCPSettingsProperties restates the SDK's `mcp` settings schema.
//
// Ported field for field from `elitea_sdk/runtime/toolkits/mcp.py`'s
// `toolkit_config_schema` at the pinned revision. The `tooltip` and `example`
// keys are the SDK's own `json_schema_extra`, which the web form renders.
func remoteMCPSettingsProperties() map[string]any {
	return map[string]any{
		"url": map[string]any{
			"type":        "string",
			"title":       "URL",
			"description": "MCP server HTTP URL",
			"tooltip":     "HTTP URL for the MCP server (http:// or https://)",
			"example":     "https://your-mcp-server.com/mcp",
		},
		"headers": map[string]any{
			"type":  "object",
			"title": "Headers",
			"description": "HTTP headers for authentication and configuration. " +
				"The native agent runtime accepts no static header and refuses a " +
				"toolkit that carries one.",
			"tooltip": "HTTP headers to send with requests (e.g. Authorization)",
			"default": map[string]any{},
		},
		"client_id": map[string]any{
			"type":        "string",
			"title":       "Client ID",
			"description": "OAuth Client ID (if applicable)",
		},
		"client_secret": map[string]any{
			"type":        "string",
			"title":       "Client Secret",
			"description": "OAuth Client Secret (if applicable)",
			"format":      "password",
			"secret":      true,
			"writeOnly":   true,
		},
		"scopes": map[string]any{
			"type":        "array",
			"title":       "Scopes",
			"items":       map[string]any{"type": "string"},
			"description": "OAuth Scopes (if applicable)",
			"default":     []any{},
		},
		"timeout": map[string]any{
			"type":        "integer",
			"title":       "Timeout",
			"description": "Request timeout in seconds (1-3600)",
			"default":     300,
		},
		"enable_caching": map[string]any{
			"type":        "boolean",
			"title":       "Enable Caching",
			"description": "Enable caching of tool schemas and responses",
			"default":     true,
		},
		"cache_ttl": map[string]any{
			"type":        "integer",
			"title":       "Cache TTL",
			"description": "Cache TTL in seconds (60-3600)",
			"default":     300,
		},
		// NO `items.enum`, and that absence is load-bearing.
		//
		// `ToolBase.render.tsx`'s `resolveAvailableTools` picks the tool list
		// in this order: the keys of `selected_tools.args_schemas` when that
		// map is non-empty, ELSE `selected_tools.items.enum`, ELSE
		// `settings.available_mcp_tools`. The last of those is where a
		// discovery writes what the server just published.
		//
		// Declaring `enum: []` therefore does not mean "no tools known yet" —
		// it means "the tool list is this empty list", and the `??` chain stops
		// there. Measured in a browser against a live MCP server: the discovery
		// succeeded, the server answered with its `echo` tool, and the chip
		// picker stayed empty because the empty enum won. The SDK declares
		// `List[str]` with no enum for exactly this reason, and this restates
		// that.
		//
		// `args_schemas` is present and empty on purpose: the web form indexes
		// that exact path, and an absent key and an empty one render
		// differently.
		"selected_tools": map[string]any{
			"type":         "array",
			"title":        "Selected Tools",
			"items":        map[string]any{"type": "string"},
			"default":      []any{},
			"description":  "Specific tools to enable (empty = all tools)",
			"tooltip":      "Leave empty to enable all tools from the MCP server",
			"args_schemas": map[string]any{},
		},
	}
}

// remoteMCPType is the served `mcp` entry.
//
// `title` is load-bearing and must stay `mcp`: the web form keys its entire
// Remote-MCP behaviour — the "Load Tools" action and the MCP authorisation
// status pane — off `schema.title === 'mcp'`
// (`apps/elitea-web/src/features/toolkits/ui/form/ToolBase/
// ToolBase.render.tsx`). A schema without it renders as an ordinary toolkit and
// the server can never be contacted.
//
// `metadata` is the SDK's own, verbatim, including `categories: ["other"]`.
// That looks wrong and is not: the MCP page selects its entries by KEY, not by
// category (`isMcpFlavouredKey`), and the toolkit page excludes the same key.
// Changing the category here would move nothing and would diverge from
// production.
func remoteMCPType() ProjectedToolkitType {
	return ProjectedToolkitType{
		Type: remoteMCPToolkitType,
		Schema: map[string]any{
			"type":          "object",
			"title":         remoteMCPToolkitType,
			"name_required": true,
			"required":      []any{"url"},
			"metadata": map[string]any{
				"label":            "Remote MCP",
				"icon_url":         nil,
				"categories":       []any{"other"},
				"extra_categories": []any{"remote tools", "sse", "http"},
				"description": "Connect to a remote Model Context Protocol (MCP) " +
					"server via HTTP to access tools",
				// The web form's tool section shows a "Load Tools" action for a
				// type whose tools are discovered rather than declared. The
				// label matches production's.
				"check_connection_supported": true,
				"check_connection_label":     "Load Tools",
			},
			"properties": remoteMCPSettingsProperties(),
		},
	}
}

// PrebuiltMCPCatalogue reads the pre-built MCP server catalogue.
//
// An interface so the projection can be driven from a table of rows in a unit
// test. `mcpregistry.PrebuiltStore` is a concrete struct with no seam of its
// own, and giving it one would mean editing the two packages that field it.
type PrebuiltMCPCatalogue interface {
	List(ctx context.Context) ([]mcpregistry.PrebuiltServer, error)
}

// remoteMCPProjection serves the one generic Remote MCP type.
//
// IT IS ITS OWN SOURCE, deliberately, and not the first entry of the catalogue
// source below. A projection source that fails contributes NOTHING — that is
// the merge contract, and it is the right rule for a source backed by a table.
// Folding the generic type into the catalogue source would therefore make an
// unreadable `elitea_mcp.prebuilt_servers` withdraw the Remote MCP type as
// well, and closing the browser's only MCP create surface because a
// platform-wide table could not be read is a much worse answer than offering
// one fewer pre-built server. This type needs no database, so it is not exposed
// to one.
type remoteMCPProjection struct{}

func (remoteMCPProjection) Name() string { return "remote_mcp" }

func (remoteMCPProjection) ProjectToolkitTypes(context.Context) ([]ProjectedToolkitType, error) {
	return []ProjectedToolkitType{remoteMCPType()}, nil
}

// prebuiltMCPProjection serves one type per ENABLED pre-built catalogue row.
type prebuiltMCPProjection struct {
	catalogue PrebuiltMCPCatalogue
}

// newPrebuiltMCPProjection builds the projection over a pool. A nil pool yields
// a source with nothing to read, which contributes an empty answer rather than
// an error: no pool is not a failed read.
func newPrebuiltMCPProjection(pool *pgxpool.Pool) *prebuiltMCPProjection {
	if pool == nil {
		return &prebuiltMCPProjection{}
	}
	return &prebuiltMCPProjection{catalogue: mcpregistry.NewPrebuiltStore(pool)}
}

func (p *prebuiltMCPProjection) Name() string { return "mcp_prebuilt_catalogue" }

// ProjectToolkitTypes answers one type per enabled row.
//
// A DISABLED row is not offered. `mcpregistry.Resolve` refuses to fill a
// disabled entry's settings, so a tile for one would create a toolkit with an
// empty URL and no record of why.
func (p *prebuiltMCPProjection) ProjectToolkitTypes(ctx context.Context) ([]ProjectedToolkitType, error) {
	if p == nil || p.catalogue == nil {
		return nil, nil
	}
	entries, err := p.catalogue.List(ctx)
	if err != nil {
		return nil, err
	}
	projected := make([]ProjectedToolkitType, 0, len(entries))
	for _, entry := range entries {
		if !entry.Enabled || entry.Key == "" {
			continue
		}
		projected = append(projected, prebuiltMCPType(entry))
	}
	return projected, nil
}

// prebuiltMCPType projects one catalogue row.
//
// The type name is pylon's: the normalised key with the `mcp_` prefix restored,
// which is the exact string `mcpregistry.Resolve` matches when the create path
// fills the row's URL, headers, timeout and credentials into the toolkit's own
// settings. The two must agree or a pre-built toolkit is created with an empty
// URL and no record of why.
//
// `url` is NOT required here, and that is the difference from the generic type:
// the catalogue supplies it server-side. The field stays in the schema so an
// operator can point one toolkit at a different path on the same server, which
// is the case `Resolve`'s caller-wins rule exists for.
func prebuiltMCPType(entry mcpregistry.PrebuiltServer) ProjectedToolkitType {
	label := entry.DisplayName
	if label == "" {
		label = entry.Key
	}
	properties := remoteMCPSettingsProperties()
	// A pre-built entry names its own server. Showing the operator's URL as the
	// field's default tells the user which server they are about to attach to,
	// and an empty submitted value still resolves to the same string server-side.
	if entry.ServerURL != "" {
		if urlProperty, ok := properties["url"].(map[string]any); ok {
			urlProperty["default"] = entry.ServerURL
			urlProperty["description"] = "MCP server HTTP URL. " +
				"This pre-built server supplies its own URL; leave the field empty to use it."
		}
	}
	return ProjectedToolkitType{
		Type: mcpregistry.PrebuiltToolkitTypePrefix + entry.Key,
		Schema: map[string]any{
			"type":          "object",
			"title":         mcpregistry.PrebuiltToolkitTypePrefix + entry.Key,
			"name_required": true,
			// No required field: everything a pre-built server needs comes from
			// the catalogue row.
			"required": []any{},
			"metadata": map[string]any{
				"label":                      label,
				"icon_url":                   nil,
				"categories":                 []any{"mcp"},
				"extra_categories":           []any{"remote tools", "http"},
				"description":                "Pre-built MCP server offered by this deployment.",
				"mcp_server_name":            entry.Key,
				"mcp_server_type":            "prebuilt",
				"check_connection_supported": true,
				"check_connection_label":     "Load Tools",
			},
			"properties": properties,
		},
	}
}
