package toolkits

import (
	"context"
	"fmt"
	"sort"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/rustonly"
)

// ToolkitCatalogueSource supplies the SETTINGS schema and the METADATA of every
// built-in toolkit type the admitted SDK declares.
//
// It is the seam that turned eight served types into fifty-two. Before it, the
// served catalogue was the hand-written toolkitTypeSchemas map, and the pinned
// SDK snapshot was consulted only to enrich the four of those eight the SDK also
// knows. The other forty-eight types were loaded into the process at startup,
// enumerable through the admin suggestions route, and offered to nobody.
//
// found=false means the type is not a built-in SDK toolkit — the four
// elitea_core-native types (database, custom, datasource, application) answer
// that way and keep their hand-written schemas.
//
// It is an interface here, and injected by the composition root, for the reason
// ToolkitArgumentSchemaSource states: the implementation lives in
// internal/runtimecomposition, which already imports this package.
type ToolkitCatalogueSource interface {
	ToolkitTypes() []string
	ToolkitCatalogueEntry(toolkitType string) (map[string]any, map[string]any, bool, error)
	ToolkitImportKey(toolkitType string) (string, bool)
}

// ToolkitCapabilitySource answers whether the worker this deployment runs can
// actually build a toolkit type, and why not when it cannot.
//
// Serving a type the worker cannot run is not a cosmetic defect. The Python
// worker image installs a measured subset of elitea-sdk[all], so nine SDK
// toolkits raise at import and fail at the first tool call. The Rust worker
// materializes twenty-two families and SKIPS the rest with a warning, so an
// unsupported toolkit attaches to an agent and then quietly does nothing. Both
// failures happen after the user has saved the toolkit, in the one place the
// user cannot act on them.
//
// A refused type is still SERVED, with metadata.hidden and a reason. Dropping
// it would make the absent type indistinguishable from a type the platform
// never had, which is precisely what the admin surface has to be able to tell
// apart.
type ToolkitCapabilitySource interface {
	SupportsToolkitType(toolkitType string, importKey string) (bool, string)
}

// WithCatalogue supplies the SDK settings schemas and metadata served by
// ListTypeSchemas. Without it the endpoint serves only the hand-written types,
// which is the pre-#? behaviour and eight tiles.
func WithCatalogue(source ToolkitCatalogueSource) Option {
	return func(h *Handler) { h.catalogue = source }
}

// WithWorkerCapability supplies the worker capability projection. Without it
// every catalogued type is offered. The composition root always supplies one
// (a nil *WorkerToolkitCapability is a valid projection that offers the SDK's
// types and withholds the ones only the Rust worker serves).
func WithWorkerCapability(source ToolkitCapabilitySource) Option {
	return func(h *Handler) { h.workerCapability = source }
}

// WithMemoryOnVectorStore states that the deployment keeps memory toolkit data
// in elitea-vector (the Rust indexing runtime, worker unit M9). The memory
// type is then served without its SDK `pgvector_configuration` field: the Rust
// worker reads nothing from it and such a deployment provisions no pgvector
// configuration, so a form that required one could not be saved. The SDK schema
// stays as pinned everywhere else (the Python worker uses the field).
func WithMemoryOnVectorStore(enabled bool) Option {
	return func(h *Handler) { h.memoryOnVectorStore = enabled }
}

// withoutPgvectorConfiguration copies a settings schema without the
// `pgvector_configuration` property and its `required` entry. Only the nodes on
// those paths are rebuilt, so the shared pinned schema is never written to.
func withoutPgvectorConfiguration(settingsSchema map[string]any) map[string]any {
	typeSchema := make(map[string]any, len(settingsSchema))
	for key, value := range settingsSchema {
		typeSchema[key] = value
	}
	if properties, ok := settingsSchema[toolkitSettingsPropertyKey].(map[string]any); ok {
		kept := make(map[string]any, len(properties))
		for name, property := range properties {
			if name != "pgvector_configuration" {
				kept[name] = property
			}
		}
		typeSchema[toolkitSettingsPropertyKey] = kept
	}
	switch required := settingsSchema["required"].(type) {
	case []any:
		kept := make([]any, 0, len(required))
		for _, name := range required {
			if name != "pgvector_configuration" {
				kept = append(kept, name)
			}
		}
		typeSchema["required"] = kept
	case []string:
		kept := make([]string, 0, len(required))
		for _, name := range required {
			if name != "pgvector_configuration" {
				kept = append(kept, name)
			}
		}
		typeSchema["required"] = kept
	}
	return typeSchema
}

// memoryScopeDescription is what the create form shows under "Memory scope".
const memoryScopeDescription = "Who can see what this toolkit remembers. " +
	"Personal: each person has their own private memory, and nobody else can read, search or delete it (the safe default). " +
	"Project: one memory shared by everyone in the project. " +
	"To give an agent both, attach two memory toolkits, one of each. " +
	"Once personal memory has been read in a conversation, that conversation can no longer write project memory or a shared index. " +
	"Other tools attached to the agent that can send data out (for example GitHub, Slack or email) are not limited this way, so attach personal memory only to agents whose other tools you trust with what you have told it. " +
	"A scheduled run acts as the person who owns the schedule and uses that person's personal memory. " +
	"A conversation with more than one person has no personal memory; project memory still works there."

// withMemoryScope adds the `memory_scope` setting the Rust worker's memory
// toolkit reads (worker unit M9). A copy: the shared schema is never written to.
func withMemoryScope(settingsSchema map[string]any) map[string]any {
	typeSchema := make(map[string]any, len(settingsSchema))
	for key, value := range settingsSchema {
		typeSchema[key] = value
	}
	properties, _ := settingsSchema[toolkitSettingsPropertyKey].(map[string]any)
	withScope := make(map[string]any, len(properties)+1)
	for name, property := range properties {
		withScope[name] = property
	}
	withScope["memory_scope"] = map[string]any{
		"type":        "string",
		"title":       "Memory scope",
		"description": memoryScopeDescription,
		// Two values only (owner decision 2026-10-14): the former `both` is
		// gone; a toolkit saved with it is read as personal by the worker.
		"enum":    []any{"personal", "project"},
		"default": "personal",
	}
	typeSchema[toolkitSettingsPropertyKey] = withScope
	return typeSchema
}

// Metadata keys this package writes. They are the reference deployment's own
// names: the web client reads metadata.hidden to drop a tile and
// metadata.categories[0] to group it.
const (
	metadataKey               = "metadata"
	metadataHiddenKey         = "hidden"
	metadataUnavailableKey    = "unavailable"
	metadataUnavailableReason = "unavailable_reason"
	// metadataUnavailableToolsKey lists the tools of a runnable type the
	// worker does not serve (ADR-0027); absent when it serves them all.
	metadataUnavailableToolsKey = "unavailable_tools"
	metadataLabelKey            = "label"
	toolkitNameAnnotation       = "toolkit_name"
	toolkitNameRequiredKey      = "name_required"
	toolkitSettingsPropertyKey  = "properties"
)

// nativeToolkitTypeMetadata labels the four types the SDK does not define.
//
// They carry a label and nothing else on purpose. The web client groups a type
// by metadata.categories[0] and falls back to "Other" when there is none, which
// is where these four render today; giving them a category would move existing
// tiles for no parity gain, because the reference deployment does not serve
// these types at all. The label is worth stating because the client's own
// override map is the only thing that names them now.
var nativeToolkitTypeMetadata = map[string]map[string]any{
	"custom":      {metadataLabelKey: "Custom"},
	"database":    {metadataLabelKey: "Database"},
	"datasource":  {metadataLabelKey: "Datasource"},
	"application": {metadataLabelKey: "Agent"},
	"imagegen": {
		metadataLabelKey: "ImageGen",
		"categories":     []any{"media"},
	},
}

// nativeWorkerGatedToolkitTypes names hand-written (non-catalogued) types that
// must still report the worker-capability verdict a catalogued type gets for
// free in toolkitTypeMetadata's other branch. imagegen is built by both
// workers (elitea_sdk/tools/imagegen on Python, the native imagegen family on
// Rust, #864); a deployment whose worker cannot build it would render it
// hidden the way a catalogued type would. The value is the import key passed
// to ToolkitCapabilitySource.SupportsToolkitType: Python's answer is a deny
// list keyed on import key (absence = supported), Rust's is an allow list
// keyed on the toolkit type itself (absence = unsupported, so the type must be
// in rustNativeToolTypes and the pinned Rust snapshot).
var nativeWorkerGatedToolkitTypes = map[string]string{
	"imagegen": "imagegen",
}

// toolkitTypeCatalogue builds the served type catalogue.
//
// Every node it returns is freshly built. toolkitTypeSchemas is package-level
// state shared by every request, and this function adds metadata to the schema
// it serves, so an in-place edit would leak one request's capability verdict
// into every later one.
func (h *Handler) toolkitTypeCatalogue(ctx context.Context) (map[string]map[string]any, error) {
	types := h.catalogueToolkitTypes()
	catalogue := make(map[string]map[string]any, len(types))
	for _, toolkitType := range types {
		typeSchema, err := h.toolkitTypeSchema(toolkitType)
		if err != nil {
			return nil, err
		}
		catalogue[toolkitType] = typeSchema
	}
	if h.dynamicTypeSchemas != nil {
		dynamic, err := h.dynamicTypeSchemas.ListToolkitTypeSchemas(ctx)
		if err != nil {
			return nil, fmt.Errorf("dynamic toolkit type schemas: %w", err)
		}
		for toolkitType, schema := range dynamic {
			if _, collision := catalogue[toolkitType]; collision {
				return nil, fmt.Errorf("dynamic toolkit type %q collides with a built-in type", toolkitType)
			}
			catalogue[toolkitType] = schema
		}
	}
	return catalogue, nil
}

// catalogueToolkitTypes is the union of the SDK catalogue and the four
// elitea_core-native types, sorted.
//
// The union, not a replacement. database, custom, datasource and application
// are not SDK toolkits and would disappear from the create page if the SDK
// catalogue simply took over; custom is created by an end-to-end journey.
func (h *Handler) catalogueToolkitTypes() []string {
	seen := make(map[string]struct{}, len(toolkitTypeSchemas))
	types := make([]string, 0, len(toolkitTypeSchemas))
	for toolkitType := range toolkitTypeSchemas {
		seen[toolkitType] = struct{}{}
		types = append(types, toolkitType)
	}
	if h != nil && h.catalogue != nil {
		for _, toolkitType := range h.catalogue.ToolkitTypes() {
			if _, found := seen[toolkitType]; found {
				continue
			}
			seen[toolkitType] = struct{}{}
			types = append(types, toolkitType)
		}
	}
	sort.Strings(types)
	return types
}

// toolkitTypeSchema assembles one served type schema from up to four sources.
//
// Preserve explicit UI contracts for native overrides. GitHub uses its SDK
// settings, including credential, embedding model, and branch fields.
func (h *Handler) toolkitTypeSchema(toolkitType string) (map[string]any, error) {
	settings, metadata, catalogued, err := h.toolkitCatalogueEntry(toolkitType)
	if err != nil {
		return nil, fmt.Errorf("toolkit type %q catalogue entry: %w", toolkitType, err)
	}
	handWritten, hasHandWritten := toolkitTypeSchemas[toolkitType]
	if !hasHandWritten && !catalogued {
		// catalogueToolkitTypes only produces names from these two sources.
		return nil, fmt.Errorf("toolkit type %q has no schema", toolkitType)
	}

	typeSchema := settings
	if hasHandWritten && (toolkitType != "github" || !catalogued) {
		typeSchema = handWritten
	} else {
		typeSchema = withNameRequired(typeSchema)
	}

	argsSchemas, found, err := h.toolkitArgumentSchemas(toolkitType)
	if err != nil {
		return nil, fmt.Errorf("toolkit type %q argument schemas: %w", toolkitType, err)
	}
	if found {
		typeSchema = withArgumentSchemas(typeSchema, argsSchemas)
	}

	definitions, configurationProperties, found, err := h.toolkitSettingsDefinitions(toolkitType)
	if err != nil {
		return nil, fmt.Errorf("toolkit type %q settings definitions: %w", toolkitType, err)
	}
	if found && len(definitions) > 0 {
		typeSchema = withSettingsDefinitions(typeSchema, definitions, configurationProperties)
	}

	// LAST of the schema-shaping steps, deliberately: it reads the tool list
	// off `properties.selected_tools` and both steps above can change that
	// node. A type whose tools are discovered at run time (Remote MCP) has no
	// list here and gets no groups — which is how the client knows to keep
	// rendering its flat list. See tool_groups.go.
	typeSchema = withToolGroups(typeSchema)
	if toolkitType == "memory" && h != nil && h.memoryOnVectorStore {
		typeSchema = withMemoryScope(withoutPgvectorConfiguration(typeSchema))
	}

	typeMetadata := h.toolkitTypeMetadata(toolkitType, metadata, catalogued)
	if unavailable := h.unavailableTools(toolkitType, typeSchema); len(unavailable) > 0 {
		typeMetadata[metadataUnavailableToolsKey] = unavailable
	}
	return withToolkitMetadata(typeSchema, typeMetadata), nil
}

// ToolCapabilitySource is the optional per-tool half of a
// ToolkitCapabilitySource: a type the worker runs may still lack some of its
// tools (a partial native family, ADR-0027).
type ToolCapabilitySource interface {
	SupportsTool(toolkitType, toolName string) (bool, string)
}

// unavailableTools lists, sorted, the tools of the type's
// `properties.selected_tools` the configured worker does not serve. They stay
// in the schema — a saved selection holding one must still validate — and
// metadata.unavailable_tools tells the client which of them to disable.
func (h *Handler) unavailableTools(toolkitType string, typeSchema map[string]any) []string {
	if h == nil {
		return nil
	}
	supports := rustonly.AbsentToolVerdict
	if h.workerCapability != nil {
		tools, ok := h.workerCapability.(ToolCapabilitySource)
		if !ok {
			return nil
		}
		supports = tools.SupportsTool
	}
	properties, _ := typeSchema["properties"].(map[string]any)
	selected, _ := properties["selected_tools"].(map[string]any)
	var unavailable []string
	for _, name := range toolNamesFromSelectedTools(selected) {
		if served, _ := supports(toolkitType, name); !served {
			unavailable = append(unavailable, name)
		}
	}
	sort.Strings(unavailable)
	return unavailable
}

func (h *Handler) toolkitCatalogueEntry(
	toolkitType string,
) (map[string]any, map[string]any, bool, error) {
	if h == nil || h.catalogue == nil {
		return nil, nil, false, nil
	}
	return h.catalogue.ToolkitCatalogueEntry(toolkitType)
}

// toolkitTypeMetadata merges the metadata of one type and records the worker
// capability verdict on it.
//
// Precedence is: the hand-written schema's own metadata, then the SDK's, then
// the native label table. The capability verdict is written LAST and is not
// negotiable — a type the worker cannot run is hidden whatever the SDK says
// about it.
func (h *Handler) toolkitTypeMetadata(
	toolkitType string,
	catalogued map[string]any,
	isCatalogued bool,
) map[string]any {
	metadata := make(map[string]any, len(catalogued)+3)
	for key, value := range catalogued {
		metadata[key] = value
	}
	if !isCatalogued {
		for key, value := range nativeToolkitTypeMetadata[toolkitType] {
			metadata[key] = value
		}
		// The capability projection answers about SDK toolkits. Most native
		// types are built by elitea-main and by the agent runtime, not by an
		// SDK toolkit class, so neither worker's answer describes them and a
		// verdict here would be an invention — except the ones named in
		// nativeWorkerGatedToolkitTypes, which really are worker-implemented
		// and do have a real verdict to report.
		if importKey, gated := nativeWorkerGatedToolkitTypes[toolkitType]; gated {
			supported, reason := h.supportsToolkitTypeByImportKey(toolkitType, importKey)
			if !supported {
				metadata[metadataHiddenKey] = true
				metadata[metadataUnavailableKey] = true
				metadata[metadataUnavailableReason] = reason
			}
		}
		return metadata
	}
	if label, _ := metadata[metadataLabelKey].(string); label == "" {
		// The reference chooser renders a tile per LABELLED type, so a type
		// the SDK gives no label is not a tile there. mcp_config is the one
		// such type: it is the container the pre-built MCP servers are
		// declared in, and it is never created directly. This client builds a
		// label from the key when the server sends none, so without this rule
		// it would invent a "Mcp Config" tile the reference deployment has
		// never shown.
		metadata[metadataHiddenKey] = true
		metadata[metadataUnavailableKey] = true
		metadata[metadataUnavailableReason] = "the toolkit declares no label and is not created directly"
		return metadata
	}
	supported, reason := h.supportsToolkitType(toolkitType)
	if supported {
		return metadata
	}
	metadata[metadataHiddenKey] = true
	metadata[metadataUnavailableKey] = true
	metadata[metadataUnavailableReason] = reason
	return metadata
}

func (h *Handler) supportsToolkitType(toolkitType string) (bool, string) {
	if h == nil || h.workerCapability == nil {
		// No statement of the worker: the SDK's types are offered, the ones only
		// the Rust worker serves are not (the same rule a nil projection applies).
		return rustonly.AbsentTypeVerdict(toolkitType)
	}
	importKey := ""
	found := false
	if h.catalogue != nil {
		importKey, found = h.catalogue.ToolkitImportKey(toolkitType)
	}
	if !found && !rustonly.IsType(toolkitType) {
		return true, ""
	}
	return h.workerCapability.SupportsToolkitType(toolkitType, importKey)
}

// supportsToolkitTypeByImportKey is supportsToolkitType for a hand-written
// type in nativeWorkerGatedToolkitTypes: the import key is declared here, not
// looked up in the SDK catalogue, because a native type has no catalogue
// entry to look it up from.
func (h *Handler) supportsToolkitTypeByImportKey(toolkitType, importKey string) (bool, string) {
	if h == nil || h.workerCapability == nil {
		return rustonly.AbsentTypeVerdict(toolkitType)
	}
	return h.workerCapability.SupportsToolkitType(toolkitType, importKey)
}

// withToolkitMetadata copies one type's schema with the metadata block merged
// in. A key the schema already declares wins: openapi hand-writes its label,
// icon and categories, and this must not overwrite them.
func withToolkitMetadata(
	typeSchema map[string]any,
	metadata map[string]any,
) map[string]any {
	if len(metadata) == 0 {
		return typeSchema
	}
	merged := make(map[string]any, len(metadata))
	for key, value := range metadata {
		merged[key] = value
	}
	if declared, ok := typeSchema[metadataKey].(map[string]any); ok {
		for key, value := range declared {
			merged[key] = value
		}
		// The verdict is written after the schema's own metadata, so a
		// hand-written `hidden: false` cannot resurrect a type the worker
		// cannot run.
		for _, key := range []string{
			metadataHiddenKey, metadataUnavailableKey, metadataUnavailableReason,
		} {
			if value, found := metadata[key]; found {
				merged[key] = value
			}
		}
	}
	withMetadata := make(map[string]any, len(typeSchema)+1)
	for key, value := range typeSchema {
		withMetadata[key] = value
	}
	withMetadata[metadataKey] = merged
	return withMetadata
}

// withNameRequired states whether the create form must ask for a toolkit name.
//
// The reference derives it the same way: a type is name-required unless one of
// its settings fields carries the SDK's `toolkit_name` annotation, in which case
// the toolkit takes its name from that field. Only types the SDK defines get
// this key; the four hand-written entries keep whatever they declare today,
// because changing it changes four working create forms.
func withNameRequired(settingsSchema map[string]any) map[string]any {
	typeSchema := make(map[string]any, len(settingsSchema)+1)
	for key, value := range settingsSchema {
		typeSchema[key] = value
	}
	typeSchema[toolkitNameRequiredKey] = !declaresToolkitNameField(settingsSchema)
	return typeSchema
}

func declaresToolkitNameField(settingsSchema map[string]any) bool {
	properties, ok := settingsSchema[toolkitSettingsPropertyKey].(map[string]any)
	if !ok {
		return false
	}
	for _, raw := range properties {
		property, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if declared, ok := property[toolkitNameAnnotation].(bool); ok && declared {
			return true
		}
	}
	return false
}
