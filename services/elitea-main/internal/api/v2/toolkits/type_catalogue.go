package toolkits

import (
	"fmt"
	"sort"
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
// worker image installs a measured subset of elitea-sdk[all], so thirteen SDK
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
// every catalogued type is offered, which is the honest answer when the
// deployment has not said which worker it runs.
func WithWorkerCapability(source ToolkitCapabilitySource) Option {
	return func(h *Handler) { h.workerCapability = source }
}

// Metadata keys this package writes. They are the reference deployment's own
// names: the web client reads metadata.hidden to drop a tile and
// metadata.categories[0] to group it.
const (
	metadataKey                = "metadata"
	metadataHiddenKey          = "hidden"
	metadataUnavailableKey     = "unavailable"
	metadataUnavailableReason  = "unavailable_reason"
	metadataLabelKey           = "label"
	toolkitNameAnnotation      = "toolkit_name"
	toolkitNameRequiredKey     = "name_required"
	toolkitSettingsPropertyKey = "properties"
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
}

// toolkitTypeCatalogue builds the served type catalogue.
//
// Every node it returns is freshly built. toolkitTypeSchemas is package-level
// state shared by every request, and this function adds metadata to the schema
// it serves, so an in-place edit would leak one request's capability verdict
// into every later one.
func (h *Handler) toolkitTypeCatalogue() (map[string]map[string]any, error) {
	types := h.catalogueToolkitTypes()
	catalogue := make(map[string]map[string]any, len(types))
	for _, toolkitType := range types {
		typeSchema, err := h.toolkitTypeSchema(toolkitType)
		if err != nil {
			return nil, err
		}
		catalogue[toolkitType] = typeSchema
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
// The hand-written entry WINS over the SDK settings schema where both exist.
// The four overlapping keys (artifact, github, jira, openapi) carry client
// contract that the SDK model does not: openapi's ui_component and its
// "a URL is not fetched" description, github's inline access_token. Replacing
// them would change four working create forms in a change whose subject is the
// forty-four that do not exist yet.
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
	if hasHandWritten {
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

	return withToolkitMetadata(
		typeSchema,
		h.toolkitTypeMetadata(toolkitType, metadata, catalogued),
	), nil
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
		// The capability projection answers about SDK toolkits. The four
		// native types are built by elitea-main and by the agent runtime, not
		// by an SDK toolkit class, so neither worker's answer describes them
		// and a verdict here would be an invention.
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
	if h == nil || h.workerCapability == nil || h.catalogue == nil {
		return true, ""
	}
	importKey, found := h.catalogue.ToolkitImportKey(toolkitType)
	if !found {
		return true, ""
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
