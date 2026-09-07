package toolkits

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

type Tool struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Type        string         `json:"type"`
	Description string         `json:"description,omitempty"`
	OwnerID     string         `json:"owner_id,omitempty"`
	Settings    map[string]any `json:"settings,omitempty"`
}

type Repository interface {
	ListTypes(ctx context.Context, projectID string) ([]string, error)
	AvailableTools(ctx context.Context, projectID, toolkitID string) ([]Tool, error)
	DiscoverTools(ctx context.Context, projectID, toolkitType string) ([]Tool, error)
	ValidateToolkit(ctx context.Context, projectID, toolkitID string) (bool, error)
	ForkToolkit(ctx context.Context, projectID string, body map[string]any) (Tool, error)

	// CRUD for toolkit instances (/tools/ and /tool/ paths)
	ListToolkits(ctx context.Context, projectID string, page, pageSize int) ([]map[string]any, int, error)
	CreateToolkit(ctx context.Context, projectID string, body map[string]any) (map[string]any, error)
	GetToolkit(ctx context.Context, projectID, toolkitID string) (map[string]any, error)
	UpdateToolkit(ctx context.Context, projectID, toolkitID string, body map[string]any) (map[string]any, error)
	DeleteToolkit(ctx context.Context, projectID, toolkitID string) error
}

// NOTE(#126): a ToolTester interface and a `tester` field stood here, plus
// TestToolRequest/TestToolResponse aliases onto internal/infra/indexersvc. The
// only implementation was that prototype Redis RPC client, which no composition
// root ever assigned, so both tool-testing endpoints have always answered 503.
// The transport was retired (see the IndexerDeps note in
// internal/api/router.go) and the seam went with it. #194 records the gap.

// ToolkitArgumentSchemaSource supplies one built-in toolkit type's per-tool
// argument JSON Schemas, keyed by tool name. found=false means the type is not
// a built-in SDK toolkit; an empty map means it is, and legitimately declares no
// argument schemas (mcp, mcp_config and openapi discover their tools at runtime).
//
// It is an interface here, and injected by the composition root, because the
// only implementation lives in internal/runtimecomposition — the package that
// owns the digest-pinned SDK snapshot. That package already imports ten sibling
// internal/api/v2 packages, so importing it from an api package would invert the
// dependency and put this package one edge away from an import cycle.
type ToolkitArgumentSchemaSource interface {
	ToolkitArgumentSchemas(toolkitType string) (map[string]map[string]any, bool, error)
}

// ToolkitSettingsDefinitionSource supplies the JSON Schema definitions one
// built-in toolkit type's SETTINGS properties reference: the "$defs" block, and
// the properties that "$ref" into it, keyed by property name. found=false means
// the type is not a built-in SDK toolkit.
//
// It is a second, separate seam from ToolkitArgumentSchemaSource because the
// two answer different questions from different pinned files — argument schemas
// describe a TOOL's inputs, definitions describe a SETTINGS field's referenced
// configuration — and because the implementation joins two snapshots that only
// internal/runtimecomposition owns. The same dependency rule applies: that
// package imports this one, so the interface lives here and the composition
// root injects the implementation.
//
// Unassigned, the endpoint serves settings schemas with no "$defs" at all. The
// web client then produces no property of kind `configuration`, so the toolkit
// credential picker and the index schedule credential select are both
// unreachable — the defect #330 records.
type ToolkitSettingsDefinitionSource interface {
	ToolkitSettingsDefinitions(toolkitType string) (map[string]any, map[string]any, bool, error)
}

type Handler struct {
	repo                Repository
	pool                *pgxpool.Pool
	argumentSchemas     ToolkitArgumentSchemaSource
	settingsDefinitions ToolkitSettingsDefinitionSource
	guardrails          GuardrailPolicySource
	// catalogue serves the settings schema and metadata of every built-in SDK
	// toolkit type. Nil restores the eight hand-written types below.
	catalogue ToolkitCatalogueSource
	// workerCapability decides whether a catalogued type is offered as
	// creatable. Nil offers every catalogued type.
	workerCapability ToolkitCapabilitySource
	// settingsValidator resolves a credential reference before it is persisted.
	// Nil restores the pre-#613 behaviour: every save accepted unresolved. See
	// settings_validation.go.
	settingsValidator ToolkitSettingsValidator
}

// Option configures a Handler at construction, matching the pattern
// internal/api/v2/secrets uses for its permission resolver.
type Option func(*Handler)

// WithArgumentSchemas supplies the toolkit argument schemas served by
// ListTypeSchemas. Without it the endpoint falls back to the built-in tool name
// lists below, which carry no argument schemas at all — see ListTypeSchemas.
func WithArgumentSchemas(source ToolkitArgumentSchemaSource) Option {
	return func(h *Handler) { h.argumentSchemas = source }
}

// WithSettingsDefinitions supplies the settings "$defs" served by
// ListTypeSchemas. Without it the endpoint serves settings schemas that
// reference nothing — see ToolkitSettingsDefinitionSource.
func WithSettingsDefinitions(source ToolkitSettingsDefinitionSource) Option {
	return func(h *Handler) { h.settingsDefinitions = source }
}

func NewHandler(pool *pgxpool.Pool, opts ...Option) *Handler {
	h := &Handler{repo: &pgRepo{pool: pool}, pool: pool}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

func NewHandlerWithRepo(repo Repository, opts ...Option) *Handler {
	h := &Handler{repo: repo}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// knownToolkitTypes is the baseline list of toolkit types pylon_indexer supports.
// DB types are merged in at runtime so newly-registered types appear automatically.
var knownToolkitTypes = []string{
	"datasource",
	"file_loader",
	"web_loader",
	"confluence_loader",
	"github_loader",
	"jira_loader",
	"s3_loader",
	"openapi",
	"database",
	"custom",
}

// ListTypes serves the toolkit type list.
//
// This route keeps its fallback on purpose. The static knownToolkitTypes list
// is a correct answer on its own, and the create-toolkit form must stay usable
// when the tenant read fails. The route therefore stays at 200. But the
// degradation is now recorded (#381). Before, the repository turned the failure
// into an empty list and a nil error, and the handler dropped the error with
// `_`, so a dead pool served the static list and left no trace anywhere.
func (h *Handler) ListTypes(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	dbTypes, err := h.repo.ListTypes(r.Context(), projectID)
	if err != nil {
		slog.ErrorContext(r.Context(), "toolkit_types: tenant type read failed; serving the static type list only",
			"project_id", projectID, "err", err)
		dbTypes = nil
	}

	// Merge DB types with known list, deduplicating.
	seen := make(map[string]struct{})
	merged := make([]string, 0, len(knownToolkitTypes)+len(dbTypes))
	for _, t := range knownToolkitTypes {
		if _, ok := seen[t]; !ok {
			seen[t] = struct{}{}
			merged = append(merged, t)
		}
	}
	for _, t := range dbTypes {
		if _, ok := seen[t]; !ok {
			seen[t] = struct{}{}
			merged = append(merged, t)
		}
	}

	// Guardrails last, over the merged list, so a blocked type cannot re-enter
	// through the tenant read after being filtered out of the static one.
	merged = filterBlockedToolkitTypes(h.guardrailPolicy(r.Context(), "toolkit_types"), merged)

	writeJSON(w, http.StatusOK, map[string]any{"rows": merged, "total": len(merged)})
}

// toolkitTypeSchemas defines the JSON Schema for each toolkit type's SETTINGS —
// the fields a user fills in when creating a toolkit of that type (bucket,
// repository, connection_string, …).
//
// Its selected_tools.args_schemas entries are NOT authoritative and are
// replaced, per type, by ListTypeSchemas: the tool ARGUMENT schemas come from
// the digest-pinned SDK snapshot embedded in internal/runtimecomposition
// (current_toolkit_schema_snapshot.json, generated by
// scripts/contract/sync_toolkit_schema_snapshot.py from the exact elitea-sdk
// revision the Python workers are admitted to run). That snapshot is the only
// source in this repository that reflects the tools and arguments the workers
// actually accept; the names below were hand-written and several of them — every
// artifact tool except index_data, for instance — name no SDK tool at all.
//
// The map is now an OVERRIDE, not the catalogue. Four of its keys — database,
// custom, datasource and application — are the elitea_core-native types the SDK
// does not define at all, and they exist nowhere else. The other four —
// artifact, github, jira and openapi — also exist in the SDK catalogue, and the
// entries here still win, because they carry client contract the SDK model does
// not: openapi's ui_component and its "a URL is not fetched" description,
// github's inline access_token. Replacing those four is a change to four
// working create forms and belongs in its own change.
//
// Every OTHER type is served from the pinned SDK catalogue snapshot. See
// type_catalogue.go.
//
// Note that the snapshot's own "properties" are NOT a replacement for the
// settings schemas here: they are an annotation projection (configuration_model,
// configuration_types, toolkit_name) covering only annotated fields, so e.g.
// artifact's bucket and github's repository do not appear in them at all.
//
// One of those annotations IS authoritative, however. configuration_types marks
// a settings field as a reference to a saved configuration, and ListTypeSchemas
// expands it into a "$defs" entry plus a "$ref" property that replaces the
// hand-written stub of the same name (see withSettingsDefinitions). The entries
// below therefore do not declare the *_configuration fields themselves; the
// pinned SDK snapshot names them. github's access_token and jira's
// url/username/password are the pre-configuration inline shape and are left as
// they are — removing them changes the create-toolkit form, which #330 does not
// own.
var toolkitTypeSchemas = map[string]map[string]any{
	"artifact": {
		"type": "object",
		"properties": map[string]any{
			"bucket":                 map[string]any{"type": "string"},
			"pgvector_configuration": map[string]any{"type": "object"},
			"embedding_model":        map[string]any{"type": "string"},
			"selected_tools": map[string]any{
				"type": "object",
				"args_schemas": map[string]any{
					"index_data":      map[string]any{"type": "object"},
					"search_data":     map[string]any{"type": "object"},
					"list_buckets":    map[string]any{"type": "object"},
					"list_artifacts":  map[string]any{"type": "object"},
					"read_artifact":   map[string]any{"type": "object"},
					"upload_artifact": map[string]any{"type": "object"},
					"delete_artifact": map[string]any{"type": "object"},
				},
			},
		},
	},
	"github": {
		"type": "object",
		"properties": map[string]any{
			"repository":   map[string]any{"type": "string"},
			"access_token": map[string]any{"type": "string"},
			"selected_tools": map[string]any{
				"type": "object",
				"args_schemas": map[string]any{
					"get_issue":           map[string]any{"type": "object"},
					"list_issues":         map[string]any{"type": "object"},
					"create_issue":        map[string]any{"type": "object"},
					"update_issue":        map[string]any{"type": "object"},
					"comment_on_issue":    map[string]any{"type": "object"},
					"get_pull_request":    map[string]any{"type": "object"},
					"list_pull_requests":  map[string]any{"type": "object"},
					"create_pull_request": map[string]any{"type": "object"},
					"get_file_content":    map[string]any{"type": "object"},
					"list_files":          map[string]any{"type": "object"},
					"search_code":         map[string]any{"type": "object"},
					"create_file":         map[string]any{"type": "object"},
					"update_file":         map[string]any{"type": "object"},
					"delete_file":         map[string]any{"type": "object"},
					"list_branches":       map[string]any{"type": "object"},
					"create_branch":       map[string]any{"type": "object"},
				},
			},
		},
	},
	"jira": {
		"type": "object",
		"properties": map[string]any{
			"url":      map[string]any{"type": "string"},
			"username": map[string]any{"type": "string"},
			"password": map[string]any{"type": "string"},
			"selected_tools": map[string]any{
				"type": "object",
				"args_schemas": map[string]any{
					"get_issue":    map[string]any{"type": "object"},
					"search_jql":   map[string]any{"type": "object"},
					"create_issue": map[string]any{"type": "object"},
					"update_issue": map[string]any{"type": "object"},
					"add_comment":  map[string]any{"type": "object"},
				},
			},
		},
	},
	"openapi": {
		"type":          "object",
		"title":         "openapi",
		"name_required": true,
		"required":      []any{"openapi_configuration", "spec"},
		"metadata": map[string]any{
			"label":            "OpenAPI",
			"icon_url":         "openapi.svg",
			"categories":       []any{"integrations"},
			"extra_categories": []any{"api", "openapi", "swagger"},
		},
		"properties": map[string]any{
			"base_url": map[string]any{
				"anyOf": []any{
					map[string]any{"type": "string"},
					map[string]any{"type": "null"},
				},
				"default":     nil,
				"title":       "Base Url",
				"description": "Optional base URL override. Use it when the specification has no absolute server URL.",
			},
			// INLINE ONLY, and the description says so.
			//
			// It used to read "OpenAPI specification as a URL or raw JSON or
			// YAML text", paraphrasing the SDK field's own
			// "OpenAPI specification (URL or raw JSON/YAML text)". No component
			// in this system has ever fetched that URL:
			//
			//   - the native worker refuses one outright — parse_source returns
			//     UnsupportedSource for a string starting http:// or https://
			//     (services/elitea-worker-rust/src/toolkits/families/openapi/spec.rs);
			//   - the SDK worker does not fetch either: _parse_openapi_spec
			//     (elitea_sdk/tools/openapi/api_wrapper.py:513) only runs
			//     json.loads then yaml.safe_load, and a bare URL parses to a
			//     string, so it ends at "OpenAPI spec must parse to an object";
			//   - elitea-main never reads it over the network at all —
			//     DiscoverTools answers from the stored row;
			//   - the create form's editor parses the pasted text with the same
			//     JSON-then-YAML pair (OpenAPISchemaInput.tsx) and shows no
			//     operations for a URL. Its placeholder already says
			//     "Enter or drag&drop your OpenAPI schema here, or choose file"
			//     and needs no correction.
			//
			// So the promise had no implementation anywhere, and a user who
			// believed it saved a toolkit that fails at its first tool call
			// rather than at save time. Fetching it here instead would put an
			// outbound request on a tenant-authored URL inside elitea-main,
			// which has no egress allowlist and deliberately does not build one
			// — the standing rule, stated at internal/api/v2/configurations/
			// check_connection.go:10-14, is that the gateway owns the SSRF-safe
			// egress guard (#13) and "elitea-main must not reimplement that
			// guard to reach the same provider a second, unguarded way".
			// Server-side spec fetching is therefore a gateway-side feature, not
			// a line in this map; the description now states the shape that is
			// actually implemented.
			"spec": map[string]any{
				"type":         "string",
				"title":        "Spec",
				"description":  "OpenAPI specification as raw JSON or YAML text. A URL is not fetched.",
				"ui_component": "openapi_spec",
			},
			"selected_tools": map[string]any{
				"type":         "array",
				"title":        "Selected Tools",
				"description":  "Optional operation IDs to enable. An empty list enables all operations.",
				"default":      []any{},
				"items":        map[string]any{"type": "string"},
				"args_schemas": map[string]any{},
			},
		},
	},
	"database": {
		"type": "object",
		"properties": map[string]any{
			"connection_string": map[string]any{"type": "string"},
			"selected_tools": map[string]any{
				"type": "object",
				"args_schemas": map[string]any{
					"query":          map[string]any{"type": "object"},
					"list_tables":    map[string]any{"type": "object"},
					"describe_table": map[string]any{"type": "object"},
				},
			},
		},
	},
	"custom": {
		"type": "object",
		"properties": map[string]any{
			"selected_tools": map[string]any{
				"type":         "object",
				"args_schemas": map[string]any{},
			},
		},
	},
	"datasource": {
		"type": "object",
		"properties": map[string]any{
			"selected_tools": map[string]any{
				"type": "object",
				"args_schemas": map[string]any{
					"search_data": map[string]any{"type": "object"},
					"index_data":  map[string]any{"type": "object"},
				},
			},
		},
	},
	"application": {
		"type": "object",
		"properties": map[string]any{
			"application_id":         map[string]any{"type": "integer"},
			"application_version_id": map[string]any{"type": "integer"},
			"selected_tools": map[string]any{
				"type": "object",
				"args_schemas": map[string]any{
					"ask_agent": map[string]any{"type": "object"},
				},
			},
		},
	},
}

// writeToolkitInternalError logs the cause and answers a fixed 500 body.
//
// A raw `err.Error()` crosses a trust boundary. A pgx error names the database
// user, database, host and port when the server is unreachable, and table or
// constraint names otherwise. Give the caller a stable message. Keep the cause
// in the log, where the operator reads it.
//
// This is the `{"error": …}` twin of writeIndexInternalError
// (index_write.go:105), which serves the `{"ok": false, "error": …}` index
// routes. Use the shape the route already answers with.
func writeToolkitInternalError(w http.ResponseWriter, r *http.Request, operation, message string, err error) {
	slog.ErrorContext(r.Context(), operation+": "+message, "error", err)
	writeJSON(w, http.StatusInternalServerError, map[string]any{"error": message})
}

// ListTypeSchemas serves the toolkit TYPE catalogue: a map of toolkit type name
// to its settings JSON Schema, with each type's per-tool argument schemas at
// properties.selected_tools.args_schemas — the exact path the web client indexes
// into (apps/elitea-web/src/features/toolkits/ui/test-tools/
// useGetSelectedToolSchema.ts and ui/form/ToolBase/).
//
// The catalogue is assembled by toolkitTypeCatalogue in type_catalogue.go from
// four pinned sources: the SDK settings schemas and metadata, the per-tool
// argument schemas, the configuration definitions, and the worker capability
// projection. toolkitTypeSchemas below is the fifth, and it is an OVERRIDE for
// the eight keys it names rather than the catalogue itself.
//
// Each type also carries a "$defs" block beside its properties, holding the
// configuration definitions its settings reference. The web client keys its
// whole configuration-property kind off that block, so a type schema without it
// renders no credential picker at all.
func (h *Handler) ListTypeSchemas(w http.ResponseWriter, r *http.Request) {
	catalogue, err := h.toolkitTypeCatalogue()
	if err != nil {
		// A built-in snapshot that will not yield its schemas is a broken
		// binary, not a client error, and serving the placeholder tool lists
		// instead would reproduce the empty create-index form silently.
		writeToolkitInternalError(w, r, "list_type_schemas",
			"failed to build the toolkit type catalogue", err)
		return
	}
	catalogue = applyGuardrailsToCatalogue(
		h.guardrailPolicy(r.Context(), "list_type_schemas"), catalogue,
	)
	writeJSON(w, http.StatusOK, catalogue)
}

func (h *Handler) toolkitArgumentSchemas(toolkitType string) (map[string]map[string]any, bool, error) {
	if h == nil || h.argumentSchemas == nil {
		return nil, false, nil
	}
	return h.argumentSchemas.ToolkitArgumentSchemas(toolkitType)
}

func (h *Handler) toolkitSettingsDefinitions(
	toolkitType string,
) (map[string]any, map[string]any, bool, error) {
	if h == nil || h.settingsDefinitions == nil {
		return nil, nil, false, nil
	}
	return h.settingsDefinitions.ToolkitSettingsDefinitions(toolkitType)
}

// withSettingsDefinitions copies one type's settings schema with the "$defs"
// block added at the schema root and the configuration properties merged into
// properties. The root is where the web client reads them: it hands one type's
// schema to convertToolkitSchema, which takes its $defs key set from
// Object.keys(schema.$defs).
//
// A configuration property REPLACES the hand-written entry of the same name.
// The pinned SDK snapshot is authoritative about which settings field is a
// configuration reference, and the hand-written stubs predate that: artifact's
// pgvector_configuration was a bare {"type":"object"}, which the client sorts
// into the ordinary-property bucket and renders as a plain object field.
// Settings that no configuration property names are carried over untouched.
func withSettingsDefinitions(
	settingsSchema map[string]any,
	definitions map[string]any,
	configurationProperties map[string]any,
) map[string]any {
	typeSchema := make(map[string]any, len(settingsSchema)+1)
	for key, value := range settingsSchema {
		typeSchema[key] = value
	}
	settingsProperties, _ := settingsSchema["properties"].(map[string]any)
	properties := make(map[string]any, len(settingsProperties)+len(configurationProperties))
	for key, value := range settingsProperties {
		properties[key] = value
	}
	for key, value := range configurationProperties {
		properties[key] = value
	}
	typeSchema["properties"] = properties
	typeSchema["$defs"] = definitions
	return typeSchema
}

// withArgumentSchemas copies one type's settings schema with
// properties.selected_tools.args_schemas replaced. Only the three nodes on that
// path are rebuilt; the settings properties beside selected_tools are carried
// over as they are, and are never written to.
func withArgumentSchemas(
	settingsSchema map[string]any,
	argsSchemas map[string]map[string]any,
) map[string]any {
	typeSchema := make(map[string]any, len(settingsSchema))
	for key, value := range settingsSchema {
		typeSchema[key] = value
	}
	settingsProperties, _ := settingsSchema["properties"].(map[string]any)
	properties := make(map[string]any, len(settingsProperties)+1)
	for key, value := range settingsProperties {
		properties[key] = value
	}
	selectedTools := map[string]any{"type": "object"}
	if existing, ok := settingsProperties["selected_tools"].(map[string]any); ok {
		for key, value := range existing {
			selectedTools[key] = value
		}
	}
	selectedTools["args_schemas"] = argsSchemas
	properties["selected_tools"] = selectedTools
	typeSchema["properties"] = properties
	return typeSchema
}

// availableToolsReadFailed and discoverToolsReadFailed are the named reasons
// that the two tool-list reads report. The client gets the reason and no
// database detail. The log line carries the wrapped driver error (#381 AC6).
const (
	availableToolsReadFailed = "available tools read failed"
	discoverToolsReadFailed  = "discover tools read failed"
)

// AvailableTools lists the tools that one toolkit instance has.
//
// A read fault must not answer 200 with an empty list (#381). An empty list is a
// correct state for a toolkit with no attached tools. A failed read that copies
// that same body removes the only signal that keeps the two apart. The screen
// then shows "no tools" for a dead pool, for a missing tenant schema and for a
// bad row. No record shows that a read was tried and lost. This handler keeps
// the two outcomes apart. No tools gives 200 and an empty list. A lost read
// gives 500 and a named reason.
func (h *Handler) AvailableTools(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	toolkitID := chi.URLParam(r, "toolkitID")
	tools, err := h.repo.AvailableTools(r.Context(), projectID, toolkitID)
	if err != nil {
		slog.ErrorContext(r.Context(), "toolkit_available_tools: "+availableToolsReadFailed,
			"project_id", projectID, "toolkit_id", toolkitID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": availableToolsReadFailed})
		return
	}
	tools = filterBlockedTools(h.guardrailPolicy(r.Context(), "toolkit_available_tools"), tools)
	writeJSON(w, http.StatusOK, map[string]any{"tools": tools, "total": len(tools)})
}

// DiscoverTools lists the tools that one toolkit type offers. It keeps the two
// outcomes apart for the same reason that AvailableTools does (#381).
func (h *Handler) DiscoverTools(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	toolkitType := chi.URLParam(r, "toolkitType")
	// The type is in the URL, so a blocked one is refused outright rather than
	// answered with an empty list: "this type is blocked" and "this project has
	// no toolkits of this type" are different facts.
	if h.refuseBlockedToolkitType(w, r, "toolkit_discover_tools", toolkitType) {
		return
	}
	tools, err := h.repo.DiscoverTools(r.Context(), projectID, toolkitType)
	if err != nil {
		slog.ErrorContext(r.Context(), "toolkit_discover_tools: "+discoverToolsReadFailed,
			"project_id", projectID, "toolkit_type", toolkitType, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": discoverToolsReadFailed})
		return
	}
	tools = filterBlockedTools(h.guardrailPolicy(r.Context(), "toolkit_discover_tools"), tools)
	writeJSON(w, http.StatusOK, map[string]any{"tools": tools, "total": len(tools)})
}

func (h *Handler) ValidateToolkit(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	toolkitID := chi.URLParam(r, "toolkitID")
	valid, err := h.repo.ValidateToolkit(r.Context(), projectID, toolkitID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"valid": false,
			"settings_errors": []map[string]any{
				{"loc": []string{"embedding_model"}, "msg": err.Error(), "type": "value_error"},
			},
		})
		return
	}
	if !valid {
		writeJSON(w, http.StatusBadRequest, map[string]any{"valid": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"valid": true})
}

func (h *Handler) ForkToolkit(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	if forked, _ := body["type"].(string); h.refuseBlockedToolkitType(w, r, "fork_toolkit", forked) {
		return
	}
	tool, err := h.repo.ForkToolkit(r.Context(), projectID, body)
	if err != nil {
		// This route answers `{"ok": false, …}`, so it uses the index helper
		// rather than writeToolkitInternalError. Both hide the cause.
		writeIndexInternalError(w, r, "fork_toolkit", "failed to fork the toolkit", err)
		return
	}
	writeJSON(w, http.StatusOK, tool)
}

// TestTool reports that running a single tool has no backend in this stack.
// See the NOTE(#126) above the Handler declaration; the 503 body is unchanged
// from what every deployment already returned.
func (h *Handler) TestTool(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}

	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "indexer service not available"})
}

// TestToolkitTool reports that running a toolkit's tool has no backend in this
// stack. See the NOTE(#126) above the Handler declaration; the 503 body is
// unchanged from what every deployment already returned.
func (h *Handler) TestToolkitTool(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}

	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "indexer service not available"})
}

func (h *Handler) ExportToolkit(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	toolkitID := chi.URLParam(r, "toolkitID")
	ctx := r.Context()

	s, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	var name, toolType, desc string
	var settings, envVars, meta []byte
	err := h.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT name, type, COALESCE(description, ''), COALESCE(settings::text, '{}'),
			COALESCE(env_vars::text, '[]'), COALESCE(meta::text, '{}')
		FROM %s.elitea_tools WHERE id = $1`, s), toolkitID).Scan(
		&name, &toolType, &desc, &settings, &envVars, &meta)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "toolkit not found"})
		return
	}

	var settingsObj, envVarsObj, metaObj any
	// DB columns are stored as valid JSON; Unmarshal into any cannot fail for well-formed rows.
	_ = json.Unmarshal(settings, &settingsObj)
	_ = json.Unmarshal(envVars, &envVarsObj)
	_ = json.Unmarshal(meta, &metaObj)

	fork := r.URL.Query().Get("fork") == "true"
	result := map[string]any{
		"id":          toolkitID,
		"name":        name,
		"type":        toolType,
		"description": desc,
		"settings":    redactSettings(settingsObj),
		"env_vars":    envVarsObj,
		"meta":        metaObj,
		"forked":      fork,
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) IndexMeta(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	toolkitID := chi.URLParam(r, "toolkitID")
	ctx := r.Context()

	s, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	q := fmt.Sprintf(`SELECT id, name, status, progress, created_at FROM %s.index_meta WHERE toolkit_id = $1 ORDER BY created_at DESC`, s)
	rows, err := h.pool.Query(ctx, q, toolkitID)
	if err != nil {
		// Was: `writeJSON(w, http.StatusOK, map[string]any{"items": []any{},
		// "total": 0})`. Two defects in one line, both found while mounting the
		// web client's Indexes tab (#149):
		//
		//  1. It answers a DIFFERENT SHAPE than the success path below, which
		//     writes a bare JSON array. A caller that (correctly) reads this
		//     endpoint as an array got an object instead and crashed —
		//     measured as an `e.map is not a function` error boundary on
		//     /app/toolkits/all/{id}.
		//  2. It reports 200 for a failed query, so a genuinely broken or
		//     missing `index_meta` table looks exactly like "this toolkit has
		//     no indexes yet". That is precisely how the missing table went
		//     unnoticed: `p_1.index_meta` was absent from
		//     `internal/infra/db/migrations/001_initial.sql` (added in the
		//     same change as this one) and every request quietly took this
		//     branch.
		//
		// A read that could not run is an error, not an empty list.
		//
		// The body carries a fixed message, never `err.Error()`. The raw pgx
		// error names the database user, database, host and port when the
		// server is unreachable, and constraint or table names otherwise.
		slog.ErrorContext(ctx, "index_meta_list: index_meta read failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to list indexes"})
		return
	}
	defer rows.Close()

	var items []map[string]any
	for rows.Next() {
		var id, name, status string
		var progress float64
		var createdAt any
		if err := rows.Scan(&id, &name, &status, &progress, &createdAt); err != nil {
			continue
		}
		items = append(items, map[string]any{"id": id, "name": name, "status": status, "progress": progress, "created_at": createdAt})
	}
	if items == nil {
		items = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, items)
}

func (h *Handler) IndexMetaGet(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	indexMetaID := chi.URLParam(r, "indexMetaID")
	ctx := r.Context()

	s, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	q := fmt.Sprintf(`SELECT id, name, status, progress, created_at FROM %s.index_meta WHERE id = $1`, s)
	var id, name, status string
	var progress float64
	var createdAt any
	err := h.pool.QueryRow(ctx, q, indexMetaID).Scan(&id, &name, &status, &progress, &createdAt)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "name": name, "status": status, "progress": progress, "created_at": createdAt})
}

// IndexMetaUpdate, IndexMetaDelete and IndexCancel live in index_write.go.
// They used to be three one-line stubs here that answered `{"ok":true}`/204
// without touching the database (#180).

// NOTE(#394): `func (h *Handler) IndexTypes` stood here, mounted at
// GET /index_types/prompt_lib/{projectID} in internal/api/router.go.
//
// It was the PROTOTYPE catalogue. It began as a six-element slice written by
// hand — file_loader, web_loader, confluence_loader, github_loader,
// jira_loader, s3_loader — each with an invented display name, description and
// `supported_extensions` list. Nothing produced those values: no SDK, no
// snapshot, no database, no configuration. #367 replaced the wrong 200 with a
// 501 refusal, and #394 deletes the refusal with the route.
//
// internal/api/v2/indextypes answers this path now, from the snapshot pinned
// out of the SDK (elitea_sdk/runtime/langchain/document_loaders/constants.py,
// materialized as
// internal/runtimecomposition/current_index_types_snapshot.json). It is a
// strict superset in content and a different shape — document/image/code
// extension maps rather than a loader list — so the guess was not even a
// subset a client could safely narrow to.

// List returns all toolkit instances for a project.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")

	// UI sends limit/offset
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit < 1 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	page := (offset / limit) + 1

	items, total, err := h.repo.ListToolkits(r.Context(), projectID, page, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to list toolkits"})
		return
	}
	if items == nil {
		items = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": items, "total": total})
}

// Create creates a new toolkit instance.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	user, _ := auth.UserFromContext(r.Context())
	userID := user.ID
	if userID == "" {
		userID = "1"
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}

	if err := validateToolkitCreate(body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	createdType, _ := body["type"].(string)
	if h.refuseBlockedToolkitType(w, r, "create_toolkit", createdType) {
		return
	}
	// The credential gate, between the type refusal and the write, so a refused
	// save persists nothing. validateToolkitCreate above only checks that a
	// github body NAMES a `github_configuration` key; this is the only thing on
	// the write path that resolves what that key points at.
	if settings, ok := body["settings"].(map[string]any); ok &&
		h.refuseUnresolvableToolkitSettings(w, r, "create_toolkit", projectID, createdType, settings) {
		return
	}
	body["_author_id"] = userID
	item, err := h.repo.CreateToolkit(r.Context(), projectID, body)
	if err != nil {
		writeToolkitInternalError(w, r, "create_toolkit", "failed to create the toolkit", err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func validateToolkitCreate(body map[string]any) error {
	toolType, _ := body["type"].(string)
	settings, _ := body["settings"].(map[string]any)

	switch toolType {
	case "github":
		if settings == nil {
			return fmt.Errorf("settings is required for github toolkit")
		}
		repo, _ := settings["repository"].(string)
		if repo == "" {
			return fmt.Errorf("settings.repository is required for github toolkit")
		}
		if settings["github_configuration"] == nil {
			return fmt.Errorf("settings.github_configuration is required for github toolkit")
		}
	}
	return nil
}

// Get returns a single toolkit instance.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	toolkitID := chi.URLParam(r, "toolkitID")
	item, err := h.repo.GetToolkit(r.Context(), projectID, toolkitID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	writeJSON(w, http.StatusOK, item)
}

// Update updates a toolkit instance (handles PUT and PATCH).
// Also handles tool-entity relation changes when body contains has_relation.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	toolkitID := chi.URLParam(r, "toolkitID")
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}

	// Detect tool-entity relation operation (add_tool_to / remove_tool_from)
	if _, hasRelation := body["has_relation"]; hasRelation {
		h.updateToolRelation(w, r, projectID, toolkitID, body)
		return
	}

	// A body that names a blocked type is refused. A body that omits `type` is
	// not: an update that does not restate the type cannot be used to introduce
	// a blocked one, and refusing it would make an existing toolkit of a
	// newly-blocked type impossible to edit — including impossible to point at
	// something harmless before deleting it.
	updatedType, _ := body["type"].(string)
	if h.refuseBlockedToolkitType(w, r, "update_toolkit", updatedType) {
		return
	}
	// Only a body that actually REPLACES settings is validated. UpdateToolkit
	// builds a partial UPDATE (each of `type` and `settings` is independently
	// optional), so a PATCH that merely renames a toolkit carries no settings —
	// and validating a nil map as `{}` would refuse renames of a toolkit that is
	// already broken, which is the one edit most likely to be an attempt to fix
	// it. The type comes from the body when it restates one and from the stored
	// row otherwise; a failed read degrades to the unvalidated save rather than
	// making the toolkit uneditable.
	if settings, ok := body["settings"].(map[string]any); ok {
		if updatedType == "" {
			updatedType = h.storedToolkitType(r.Context(), projectID, toolkitID)
		}
		if h.refuseUnresolvableToolkitSettings(w, r, "update_toolkit", projectID, updatedType, settings) {
			return
		}
	}
	item, err := h.repo.UpdateToolkit(r.Context(), projectID, toolkitID, body)
	if err != nil {
		writeToolkitInternalError(w, r, "update_toolkit", "failed to update the toolkit", err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (h *Handler) updateToolRelation(w http.ResponseWriter, r *http.Request, projectID, toolID string, body map[string]any) {
	ctx := r.Context()
	s, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	entityVersionID, _ := body["entity_version_id"].(string)
	if entityVersionID == "" {
		if f, ok := body["entity_version_id"].(float64); ok {
			entityVersionID = strconv.Itoa(int(f))
		}
	}
	entityID, _ := body["entity_id"].(string)
	if entityID == "" {
		if f, ok := body["entity_id"].(float64); ok {
			entityID = strconv.Itoa(int(f))
		}
	}
	entityType, _ := body["entity_type"].(string)
	if entityType == "" {
		entityType = "agent"
	}
	hasRelation, _ := body["has_relation"].(bool)

	// Guard: block changes to published/embedded versions
	var verStatus string
	err := h.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT status FROM %s.application_versions WHERE id = $1`, s), entityVersionID).Scan(&verStatus)
	if err == nil && (verStatus == "published" || verStatus == "embedded") {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "Cannot change tools on a published version. Unpublish first.",
		})
		return
	}

	if hasRelation {
		selectedTools, hasSelectedTools, err := selectedToolsPayload(body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		q := selectedToolsRelationInsertSQL(s, hasSelectedTools)
		args := []any{entityVersionID, entityID, entityType, toolID}
		if hasSelectedTools {
			args = append(args, selectedTools)
		}
		if _, err := h.pool.Exec(ctx, q, args...); err != nil {
			writeToolkitInternalError(w, r, "update_tool_relation", "failed to add the tool relation", err)
			return
		}
	} else {
		q := fmt.Sprintf(`DELETE FROM %s.entity_tool_mapping WHERE entity_version_id = $1 AND tool_id = $2`, s)
		_, err := h.pool.Exec(ctx, q, entityVersionID, toolID)
		if err != nil {
			writeToolkitInternalError(w, r, "update_tool_relation", "failed to remove the tool relation", err)
			return
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{"message": "ok"})
}

// selectedToolsPayload reads the per-mapping `selected_tools` list off the
// relation request body.
//
// ABSENT vs EMPTY, deliberately distinguished (#248). The same body serves two
// different intents on this one route:
//
//   - An ATTACH (`ToolMenu`, and the import/clone paths) sends no
//     `selected_tools` key at all — it has nothing to say about the selection.
//     Treating that as "select nothing" would wipe a selection the user saved
//     earlier, every time the toolkit was re-attached or the same mapping was
//     touched for any other reason.
//   - A SELECTION EDIT sends the full resulting list, and `[]` ("I unchecked
//     the last tool") is a meaningful value that must be stored.
//
// So presence of the key — not its length — decides whether the column is
// written, exactly the distinction `applications.replaceVersionVariables`'
// callers already draw (`hasVariables` on the update path vs `len(...) > 0` on
// the create path). A JSON `null` is treated as absent for the same reason: it
// carries no list.
//
// The value must be a JSON array of strings. Anything else is rejected rather
// than coerced: the column is read back verbatim by
// `applications.fetchVersionDetails` into `version_details.tools[].selected_tools`,
// and the UI indexes it as a string list.
func selectedToolsPayload(body map[string]any) ([]byte, bool, error) {
	raw, present := body["selected_tools"]
	if !present || raw == nil {
		return nil, false, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, false, fmt.Errorf("selected_tools must be an array of tool names")
	}
	names := make([]string, 0, len(list))
	for _, entry := range list {
		name, ok := entry.(string)
		if !ok {
			return nil, false, fmt.Errorf("selected_tools must be an array of tool names")
		}
		names = append(names, name)
	}
	encoded, err := json.Marshal(names)
	if err != nil {
		return nil, false, fmt.Errorf("selected_tools must be an array of tool names")
	}
	return encoded, true, nil
}

// selectedToolsRelationInsertSQL builds the attach statement. `_entity_tool_unique
// (entity_version_id, tool_id, entity_type)` is the upsert target, so a selection
// edit on an ALREADY-attached toolkit (the only way the UI ever sends the key)
// updates the existing mapping row instead of conflicting into a no-op — the
// bug this whole branch removes: the previous statement was an unconditional
// `DO NOTHING` and never named `selected_tools` at all.
func selectedToolsRelationInsertSQL(schema string, withSelectedTools bool) string {
	if !withSelectedTools {
		return fmt.Sprintf(`INSERT INTO %s.entity_tool_mapping (entity_version_id, entity_id, entity_type, tool_id) VALUES ($1, $2, $3, $4) ON CONFLICT (entity_version_id, tool_id, entity_type) DO NOTHING`, schema)
	}
	return fmt.Sprintf(`INSERT INTO %s.entity_tool_mapping (entity_version_id, entity_id, entity_type, tool_id, selected_tools) VALUES ($1, $2, $3, $4, $5::jsonb) ON CONFLICT (entity_version_id, tool_id, entity_type) DO UPDATE SET selected_tools = EXCLUDED.selected_tools, updated_at = now()`, schema)
}

// Delete removes a toolkit instance.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	toolkitID := chi.URLParam(r, "toolkitID")
	if err := h.repo.DeleteToolkit(r.Context(), projectID, toolkitID); err != nil {
		writeToolkitInternalError(w, r, "delete_toolkit", "failed to delete the toolkit", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type pgRepo struct {
	pool *pgxpool.Pool
}

// ListTypes reports the toolkit types that the tenant schema holds.
//
// The error is now returned to the caller. Before #381 this function turned a
// failed query into an empty list and a nil error, so the handler could not
// know that the read was lost. The handler continues to serve the static type
// list when the read fails, because the create-toolkit form needs a type list
// to stay usable. The difference is that the handler now records the
// degradation. See the ListTypes handler above.
func (r *pgRepo) ListTypes(ctx context.Context, projectID string) ([]string, error) {
	s, err := tenantschema.Quote(projectID)
	if err != nil {
		return nil, err
	}
	q := fmt.Sprintf(`SELECT DISTINCT type FROM %s.elitea_tools WHERE type != '' ORDER BY type`, s)
	rows, err := r.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query toolkit types in %q: %w", s, err)
	}
	defer rows.Close()
	types := []string{}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("scan toolkit type row in %q: %w", s, err)
		}
		types = append(types, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read toolkit type rows in %q: %w", s, err)
	}
	return types, nil
}

// AvailableTools reads the tools that are attached to one toolkit instance.
//
// Every read fault is returned to the caller (#381). Three separate faults were
// discarded here before:
//
//  1. The query itself failed. A dead pool or a missing tenant schema became
//     `[]Tool{}, nil`.
//  2. A row failed to scan. `continue` dropped that row and kept the rest, so a
//     type change in the table could empty the list with no signal at all.
//  3. The row set failed part way through. `rows.Err()` was never read, so a
//     connection that died mid-read looked like the end of the data.
//
// All three now produce an error. Only a real empty result produces an empty
// list, and the empty list is non-nil so that the response encodes as `[]`.
func (r *pgRepo) AvailableTools(ctx context.Context, projectID, toolkitID string) ([]Tool, error) {
	s, err := tenantschema.Quote(projectID)
	if err != nil {
		return nil, err
	}
	q := fmt.Sprintf(`
		SELECT t.id, t.name, t.type, COALESCE(t.description, '')
		FROM %s.entity_tool_mapping etm
		JOIN %s.elitea_tools t ON t.id = etm.tool_id
		WHERE etm.entity_version_id = $1`, s, s)
	rows, err := r.pool.Query(ctx, q, toolkitID)
	if err != nil {
		return nil, fmt.Errorf("query available tools for toolkit %q in %q: %w", toolkitID, s, err)
	}
	return scanTools(rows, fmt.Sprintf("available tools for toolkit %q in %q", toolkitID, s))
}

// DiscoverTools reads the tools that one toolkit type offers. It returns every
// read fault for the same reason that AvailableTools does (#381).
func (r *pgRepo) DiscoverTools(ctx context.Context, projectID, toolkitType string) ([]Tool, error) {
	s, err := tenantschema.Quote(projectID)
	if err != nil {
		return nil, err
	}
	q := fmt.Sprintf(`SELECT id, name, type, COALESCE(description, '') FROM %s.elitea_tools WHERE type = $1 ORDER BY name`, s)
	rows, err := r.pool.Query(ctx, q, toolkitType)
	if err != nil {
		return nil, fmt.Errorf("query discoverable tools of type %q in %q: %w", toolkitType, s, err)
	}
	return scanTools(rows, fmt.Sprintf("discoverable tools of type %q in %q", toolkitType, s))
}

// scanTools reads a four-column tool row set into a slice. A scan fault and a
// row-set fault both stop the read and give an error. The subject names the
// read in the error text.
func scanTools(rows pgx.Rows, subject string) ([]Tool, error) {
	defer rows.Close()
	tools := []Tool{}
	for rows.Next() {
		var t Tool
		if err := rows.Scan(&t.ID, &t.Name, &t.Type, &t.Description); err != nil {
			return nil, fmt.Errorf("scan row %d of %s: %w", len(tools)+1, subject, err)
		}
		tools = append(tools, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", subject, err)
	}
	return tools, nil
}

func (r *pgRepo) ValidateToolkit(ctx context.Context, projectID, toolkitID string) (bool, error) {
	s, err := tenantschema.Quote(projectID)
	if err != nil {
		return false, err
	}

	var settingsStr *string
	q := fmt.Sprintf(`SELECT settings::text FROM %s.elitea_tools WHERE id = $1`, s)
	err = r.pool.QueryRow(ctx, q, toolkitID).Scan(&settingsStr)
	if err != nil {
		return false, fmt.Errorf("toolkit not found")
	}
	if settingsStr == nil {
		return true, nil
	}

	var settings map[string]any
	// settingsStr is a DB JSON column; Unmarshal into map cannot fail for well-formed rows.
	_ = json.Unmarshal([]byte(*settingsStr), &settings)

	// Check if referenced embedding_model config still exists
	if embModel, ok := settings["embedding_model"]; ok && embModel != nil {
		embName := fmt.Sprintf("%v", embModel)
		if embName != "" {
			var configExists bool
			cq := fmt.Sprintf(`SELECT EXISTS(
				SELECT 1 FROM %s.configuration
				WHERE type = 'embedding_model' AND data->>'name' = $1
			)`, s)
			if err := r.pool.QueryRow(ctx, cq, embName).Scan(&configExists); err != nil {
				return false, fmt.Errorf("check embedding model: %w", err)
			}
			if !configExists {
				return false, fmt.Errorf("embedding model '%s' not found", embName)
			}
		}
	}

	return true, nil
}

func (r *pgRepo) ForkToolkit(ctx context.Context, projectID string, body map[string]any) (Tool, error) {
	s, err := tenantschema.Quote(projectID)
	if err != nil {
		return Tool{}, err
	}
	sourceID, _ := body["source_id"].(string)
	// author_id is copied from the source row, and it has to be.
	//
	// The column is INTEGER NOT NULL in the tenant table this stack creates
	// (internal/infra/db/migrations/001_initial.sql), and this INSERT did not
	// name it. Every fork therefore died with
	//
	//	null value in column "author_id" of relation "elitea_tools"
	//	violates not-null constraint (SQLSTATE 23502)
	//
	// which the handler turned into a 500 with a fixed message, so the route
	// had never worked and said nothing about why. Found by the repository
	// lifecycle test in repository_integration_test.go.
	//
	// The SOURCE's author is copied rather than the caller's, because this
	// method is given no principal: Handler.ForkToolkit passes the request
	// body through unchanged, and the body carries no `_author_id` the way
	// CreateToolkit's does. Copying the source author is the value that is
	// certainly correct for the row; attributing a fork to the person who
	// forked it needs the principal to reach this seam, which is a change to
	// the handler's contract rather than to this statement.
	q := fmt.Sprintf(`
		INSERT INTO %s.elitea_tools (name, type, description, owner_id, author_id, settings, env_vars, meta)
		SELECT name || ' (copy)', type, description, owner_id, author_id, settings, env_vars, meta
		FROM %s.elitea_tools WHERE id = $1
		RETURNING id, name, type, COALESCE(description, '')`, s, s)
	var t Tool
	err = r.pool.QueryRow(ctx, q, sourceID).Scan(&t.ID, &t.Name, &t.Type, &t.Description)
	if err != nil {
		return Tool{}, fmt.Errorf("fork toolkit: %w", err)
	}
	return t, nil
}

func (r *pgRepo) ListToolkits(ctx context.Context, projectID string, page, pageSize int) ([]map[string]any, int, error) {
	s, err := tenantschema.Quote(projectID)
	if err != nil {
		return nil, 0, err
	}
	offset := (page - 1) * pageSize

	var total int
	countQ := fmt.Sprintf(`SELECT COUNT(*) FROM %s.elitea_tools`, s)
	if err := r.pool.QueryRow(ctx, countQ).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := fmt.Sprintf(`
		SELECT id, type, name, COALESCE(description,''),
		       COALESCE(settings::text,'{}'), COALESCE(meta::text,'{}'),
		       created_at, author_id
		FROM %s.elitea_tools
		ORDER BY name LIMIT $1 OFFSET $2`, s)
	rows, err := r.pool.Query(ctx, q, pageSize, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var items []map[string]any
	for rows.Next() {
		var id, typ, name, desc string
		var settingsRaw, metaRaw []byte
		var createdAt, authorID any
		if err := rows.Scan(&id, &typ, &name, &desc, &settingsRaw, &metaRaw, &createdAt, &authorID); err != nil {
			continue
		}
		var settings, meta any
		// DB columns are valid JSON; Unmarshal into any cannot fail for well-formed rows.
		_ = json.Unmarshal(settingsRaw, &settings)
		_ = json.Unmarshal(metaRaw, &meta)
		items = append(items, map[string]any{
			"id":          id,
			"type":        typ,
			"name":        name,
			"description": desc,
			"settings":    redactSettings(settings),
			"meta":        meta,
			"created_at":  createdAt,
			"author_id":   authorID,
		})
	}
	return items, total, nil
}

// redactSettings returns a deep copy of settings with credential-like values
// removed. Toolkit configuration is not a secret transport; execution code
// reads the stored JSON directly and must never depend on this API response.
func redactSettings(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, nested := range v {
			if isSensitiveSettingKey(key) {
				continue
			}
			out[key] = redactSettings(nested)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, nested := range v {
			out[i] = redactSettings(nested)
		}
		return out
	default:
		return value
	}
}

// isSensitiveSettingKey delegates to the word-based classifier in
// secret_settings_key.go. It used to match by SUBSTRING, which removed
// `max_tokens` and `toolkit_configuration_max_tokens` from every toolkit read
// (#705). Read that file before changing this rule.
func isSensitiveSettingKey(key string) bool {
	return IsSecretSettingKey(key)
}

// tenantOwnerID converts a tenant project id into the integer written to the
// tenant tables' owner_id column. The p_<id> schema name is the project id, so
// a schema we can address is by definition a project id we can store.
func tenantOwnerID(projectID string) (int, error) {
	ownerID, err := strconv.Atoi(projectID)
	if err != nil || ownerID <= 0 {
		return 0, fmt.Errorf("create toolkit: %q is not a project id", projectID)
	}
	return ownerID, nil
}

// createToolkitInsertSQL builds the elitea_tools INSERT for one tenant schema.
//
// owner_id is the OWNING PROJECT id, not the creating user. author_id is the
// creating user. The two are different columns because they are different
// things, and copying author_id into owner_id would write a user id into a
// project-id column. Evidence, in order of weight:
//
//   - The legacy runtime's tenant tables that DO have owner_id store the
//     project id there: every row of p_1.applications in the legacy database
//     has owner_id = 1, and legacy elitea_core queries filter with
//     `Skill.owner_id == project_id` and construct rows with
//     `{'owner_id': project_id}` (utils/skill_utils.py:1066,1114;
//     utils/application_utils.py:273).
//   - Publishing writes `{'owner_id': public_project_id, 'shared_owner_id':
//     src['project_id']}` (utils/publish_utils.py:1010-1011) — both halves of
//     the owner_id/shared_owner_id pair are project ids.
//   - elitea_tools' own sibling column shared_owner_id is unambiguously a
//     project id (utils/fork.py:143-146 uses it as parent_project_id), and
//     owner_id is the same name without the "shared" qualifier.
//   - ForkToolkit already carries owner_id across a same-schema copy
//     (handler.go, `SELECT ... owner_id ... FROM %s.elitea_tools`), which is
//     only coherent if the value is a property of the project, not of the
//     user who happens to be forking.
//
// The current Pylon schema has no owner_id column. The standalone migration
// adds it as NOT NULL. The write must therefore select the statement that
// matches the tenant table that is actually deployed.
func createToolkitInsertSQL(schema string, includeOwnerID bool) string {
	if !includeOwnerID {
		// %s, not %q: `schema` arrives ALREADY quoted from tenantSchema
		// (tenant_schema.go), which uses tenantschema.Quote rather than Go's
		// own quoting — %q here would wrap the quotes in quotes and every
		// create against a current pylon table would name no such schema.
		return fmt.Sprintf(`
		INSERT INTO %s.elitea_tools (name, type, description, settings, meta, author_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, type, name, COALESCE(description,''),
		          COALESCE(settings::text,'{}'), COALESCE(meta::text,'{}'),
		       created_at, author_id`, schema)
	}
	return fmt.Sprintf(`
		INSERT INTO %s.elitea_tools (name, type, description, settings, meta, owner_id, author_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, type, name, COALESCE(description,''),
		          COALESCE(settings::text,'{}'), COALESCE(meta::text,'{}'),
		       created_at, author_id`, schema)
}

func (r *pgRepo) toolkitOwnerIDExists(ctx context.Context, schema string) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM information_schema.columns
    WHERE table_schema = $1
      AND table_name = 'elitea_tools'
      AND column_name = 'owner_id'
)`, schema).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("read toolkit table shape: %w", err)
	}
	return exists, nil
}

func (r *pgRepo) CreateToolkit(ctx context.Context, projectID string, body map[string]any) (map[string]any, error) {
	s, err := tenantschema.Quote(projectID)
	if err != nil {
		return nil, err
	}
	name, _ := body["name"].(string)
	typ, _ := body["type"].(string)
	desc, _ := body["description"].(string)
	authorIDStr, _ := body["_author_id"].(string)
	if authorIDStr == "" {
		authorIDStr = "1"
	}

	settingsJSON, _ := json.Marshal(body["settings"])
	if settingsJSON == nil || string(settingsJSON) == "null" {
		settingsJSON = []byte("{}")
	}
	metaJSON, _ := json.Marshal(body["meta"])
	if metaJSON == nil || string(metaJSON) == "null" {
		metaJSON = []byte("{}")
	}
	// owner_id is the OWNING PROJECT, not the creating user — see
	// createToolkitInsertSQL for the evidence. author_id already carries the
	// principal; the project is exactly the tenant schema we are writing into.
	ownerID, err := tenantOwnerID(projectID)
	if err != nil {
		return nil, err
	}

	// toolkitOwnerIDExists binds the schema as a PARAMETER against
	// information_schema, which stores the UNQUOTED name — `s` carries the
	// identifier quotes and would match nothing, silently locking every write
	// onto the owner_id-less fallback.
	//
	// Both branches fixed this independently and this keeps the canonical
	// helper rather than a second `p_%d` spelling: a hand-built name is how
	// the bug came back the first time, through a merge that fed the quoted
	// `s` here (#129 D1).
	schemaName, err := tenantschema.Name(projectID)
	if err != nil {
		return nil, err
	}
	includeOwnerID, err := r.toolkitOwnerIDExists(ctx, schemaName)
	if err != nil {
		return nil, err
	}
	q := createToolkitInsertSQL(s, includeOwnerID)
	var id, retType, retName, retDesc string
	var settingsRaw, metaRaw []byte
	var createdAt, authorID any
	var row pgx.Row
	if includeOwnerID {
		row = r.pool.QueryRow(ctx, q, name, typ, desc, string(settingsJSON), string(metaJSON), ownerID, authorIDStr)
	} else {
		row = r.pool.QueryRow(ctx, q, name, typ, desc, string(settingsJSON), string(metaJSON), authorIDStr)
	}
	err = row.Scan(
		&id, &retType, &retName, &retDesc, &settingsRaw, &metaRaw,
		&createdAt, &authorID)
	if err != nil {
		return nil, fmt.Errorf("create toolkit: %w", err)
	}
	var settings, meta any
	// DB columns are valid JSON; Unmarshal into any cannot fail for well-formed rows.
	_ = json.Unmarshal(settingsRaw, &settings)
	_ = json.Unmarshal(metaRaw, &meta)
	return map[string]any{
		"id":          id,
		"type":        retType,
		"name":        retName,
		"description": retDesc,
		"settings":    redactSettings(settings),
		"meta":        meta,
		"created_at":  createdAt,
		"author_id":   authorID,
	}, nil
}

func (r *pgRepo) GetToolkit(ctx context.Context, projectID, toolkitID string) (map[string]any, error) {
	s, err := tenantschema.Quote(projectID)
	if err != nil {
		return nil, err
	}
	q := fmt.Sprintf(`
		SELECT t.id, t.type, t.name, COALESCE(t.description,''),
		       COALESCE(t.settings::text,'{}'), COALESCE(t.meta::text,'{}'),
		       t.created_at, t.author_id,
		       COALESCE(u.id, 0), COALESCE(u.email, ''), COALESCE(u.name, '')
		FROM %s.elitea_tools t
		LEFT JOIN public.auth_core__user u ON u.id = t.author_id
		WHERE t.id = $1`, s)
	var id, typ, name, desc string
	var settingsRaw, metaRaw []byte
	var createdAt, authorID any
	var uID int
	var uEmail, uName string
	if err := r.pool.QueryRow(ctx, q, toolkitID).Scan(
		&id, &typ, &name, &desc, &settingsRaw, &metaRaw,
		&createdAt, &authorID,
		&uID, &uEmail, &uName); err != nil {
		return nil, fmt.Errorf("get toolkit: %w", err)
	}
	var settings, meta any
	// DB columns are valid JSON; Unmarshal into any cannot fail for well-formed rows.
	_ = json.Unmarshal(settingsRaw, &settings)
	_ = json.Unmarshal(metaRaw, &meta)

	author := map[string]any{"id": strconv.Itoa(uID), "email": uEmail, "name": uName}

	// toolkit_name is a sanitized version: only alphanumeric chars kept
	sanitizedName := sanitizeToolkitName(name)

	result := map[string]any{
		"id":           id,
		"type":         typ,
		"name":         name,
		"toolkit_name": sanitizedName,
		"description":  desc,
		"settings":     redactSettings(settings),
		"meta":         meta,
		"created_at":   createdAt,
		"author_id":    authorID,
		"author":       author,
		"agent_type":   nil,
		"online":       nil,
	}
	return result, nil
}

func (r *pgRepo) UpdateToolkit(ctx context.Context, projectID, toolkitID string, body map[string]any) (map[string]any, error) {
	s, err := tenantschema.Quote(projectID)
	if err != nil {
		return nil, err
	}
	// Build partial update from provided fields.
	var setClauses []string
	var args []any
	argIdx := 1
	if v, ok := body["name"]; ok {
		setClauses = append(setClauses, fmt.Sprintf("name = $%d", argIdx))
		args = append(args, v)
		argIdx++
	}
	if v, ok := body["type"]; ok {
		setClauses = append(setClauses, fmt.Sprintf("type = $%d", argIdx))
		args = append(args, v)
		argIdx++
	}
	if v, ok := body["description"]; ok {
		setClauses = append(setClauses, fmt.Sprintf("description = $%d", argIdx))
		args = append(args, v)
		argIdx++
	}
	if v, ok := body["settings"]; ok {
		b, _ := json.Marshal(v)
		setClauses = append(setClauses, fmt.Sprintf("settings = $%d", argIdx))
		args = append(args, string(b))
		argIdx++
	}
	if v, ok := body["meta"]; ok {
		b, _ := json.Marshal(v)
		setClauses = append(setClauses, fmt.Sprintf("meta = $%d", argIdx))
		args = append(args, string(b))
		argIdx++
	}
	if len(setClauses) == 0 {
		return r.GetToolkit(ctx, projectID, toolkitID)
	}
	args = append(args, toolkitID)
	q := fmt.Sprintf(`
		UPDATE %s.elitea_tools SET %s WHERE id = $%d
		RETURNING id, type, name, COALESCE(description,''),
		          COALESCE(settings::text,'{}'), COALESCE(meta::text,'{}'),
		          created_at, author_id`,
		s, strings.Join(setClauses, ", "), argIdx)
	var id, typ, name, desc string
	var settingsRaw, metaRaw []byte
	var createdAt, authorID any
	if err := r.pool.QueryRow(ctx, q, args...).Scan(
		&id, &typ, &name, &desc, &settingsRaw, &metaRaw,
		&createdAt, &authorID); err != nil {
		return nil, fmt.Errorf("update toolkit: %w", err)
	}
	var settings, meta any
	// DB columns are valid JSON; Unmarshal into any cannot fail for well-formed rows.
	_ = json.Unmarshal(settingsRaw, &settings)
	_ = json.Unmarshal(metaRaw, &meta)
	return map[string]any{
		"id":          id,
		"type":        typ,
		"name":        name,
		"description": desc,
		"settings":    redactSettings(settings),
		"meta":        meta,
		"created_at":  createdAt,
		"author_id":   authorID,
	}, nil
}

func (r *pgRepo) DeleteToolkit(ctx context.Context, projectID, toolkitID string) error {
	s, err := tenantschema.Quote(projectID)
	if err != nil {
		return err
	}
	q := fmt.Sprintf(`DELETE FROM %s.elitea_tools WHERE id = $1`, s)
	_, err = r.pool.Exec(ctx, q, toolkitID)
	return err
}

func sanitizeToolkitName(name string) string {
	var b strings.Builder
	for _, c := range name {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		}
	}
	return b.String()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	// Response is already committed; encoding errors cannot be surfaced to the client.
	_ = json.NewEncoder(w).Encode(v)
}
