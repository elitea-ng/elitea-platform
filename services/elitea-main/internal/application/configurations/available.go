package configurations

import (
	"bytes"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"

	configurationdomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/configurations"
)

const (
	currentAvailableSnapshotVersion  = "elitea.current-configuration-available-snapshot.v1"
	maxCurrentAvailableSnapshotBytes = 2 << 20
	maxCurrentAvailableEntries       = 1024

	currentMCPDynamicSource           = "indexer_mcp_configurations"
	currentProviderDynamicSource      = "provider_hub_configurations"
	currentDynamicRequiredMissing     = "required_not_included"
	currentDynamicSourceEmpty         = "current_source_returns_empty"
	currentMCPEmptySourceRevision     = "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"
	currentMCPConfigurationTypePrefix = "mcp_"
	currentSDKValidationFunction      = "applications_configuration_validator"
)

var (
	ErrInvalidCurrentAvailableSnapshot = errors.New("invalid current configuration available snapshot")
	ErrCurrentAvailableCatalogPartial  = errors.New("current configuration available catalog is partial")
)

//go:embed current_available_snapshot.json
var pinnedCurrentAvailableSnapshot string

// CurrentAvailableConfigurationType is one immutable entry from the current
// Configurations registry. ConfigSchema is the full ConfigurationCreateBase
// schema with the entry's Pydantic data schema substituted into its data
// property, matching the current /configurations/available response.
type CurrentAvailableConfigurationType struct {
	Type                 string
	Section              string
	ConfigSchema         json.RawMessage
	HasTestConnection    bool
	CheckConnectionLabel *string
	ValidationFunc       *string
	CheckConnectionFunc  *string
}

// UsesSDKValidation identifies the current generic configuration boundary.
// The function name is the registry contract used by the Configurations
// plugin; provider names are deliberately not duplicated in Go.
func (entry CurrentAvailableConfigurationType) UsesSDKValidation() bool {
	return entry.ValidationFunc != nil && *entry.ValidationFunc == currentSDKValidationFunction
}

// CurrentAvailableCatalog is the pinned, deterministic part of the current
// registry. Complete is false until the deployment-specific indexer MCP
// configuration snapshot is reconciled; callers must not expose a partial
// catalog as the production /configurations/available response.
type CurrentAvailableCatalog struct {
	entries                []CurrentAvailableConfigurationType
	entryIndexes           map[string]int
	sourceRevisions        map[string]string
	dynamicSourceRevisions map[string]string
	complete               bool
}

type currentAvailableSnapshotDocument struct {
	SchemaVersion  string                          `json:"schema_version"`
	Sources        map[string]string               `json:"sources"`
	DynamicSources map[string]string               `json:"dynamic_sources"`
	Entries        []currentAvailableSnapshotEntry `json:"entries"`
}

type currentAvailableSnapshotEntry struct {
	Type                 string          `json:"type"`
	Section              string          `json:"section"`
	ConfigSchema         json.RawMessage `json:"config_schema"`
	HasTestConnection    bool            `json:"has_test_connection"`
	CheckConnectionLabel *string         `json:"check_connection_label"`
	ValidationFunc       *string         `json:"validation_func"`
	CheckConnectionFunc  *string         `json:"check_connection_func"`
}

// LoadPinnedCurrentAvailableCatalog validates and loads the configuration
// schemas generated from the pinned current Configurations, LiteLLM,
// Artifacts, EliteA SDK, indexer worker, and elitea_core revisions.
func LoadPinnedCurrentAvailableCatalog() (*CurrentAvailableCatalog, error) {
	catalog, err := LoadCurrentAvailableCatalog([]byte(pinnedCurrentAvailableSnapshot))
	if err != nil {
		return nil, err
	}
	if err := catalog.addModelInputLimitSchema(); err != nil {
		return nil, err
	}
	return catalog, nil
}

// LoadCurrentAvailableCatalog is exported for deterministic fixture and
// version-upgrade verification. It accepts only the pinned snapshot contract;
// runtime registration remains a separate reconciliation boundary.
func LoadCurrentAvailableCatalog(raw []byte) (*CurrentAvailableCatalog, error) {
	if len(raw) == 0 || len(raw) > maxCurrentAvailableSnapshotBytes {
		return nil, ErrInvalidCurrentAvailableSnapshot
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var document currentAvailableSnapshotDocument
	if err := decoder.Decode(&document); err != nil {
		return nil, ErrInvalidCurrentAvailableSnapshot
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, ErrInvalidCurrentAvailableSnapshot
	}
	if document.SchemaVersion != currentAvailableSnapshotVersion ||
		len(document.Entries) == 0 || len(document.Entries) > maxCurrentAvailableEntries ||
		!validCurrentAvailableSources(document.Sources) ||
		!validCurrentAvailableDynamicSources(document.DynamicSources) ||
		!validCurrentAvailableMCPEntries(document.DynamicSources[currentMCPDynamicSource], document.Entries) {
		return nil, ErrInvalidCurrentAvailableSnapshot
	}

	definitions := make([]configurationdomain.RegistryEntryDefinition, len(document.Entries))
	entries := make([]CurrentAvailableConfigurationType, len(document.Entries))
	entryIndexes := make(map[string]int, len(document.Entries))
	for index, entry := range document.Entries {
		if !validCurrentAvailableOptionalString(entry.CheckConnectionLabel) ||
			!validCurrentAvailableOptionalString(entry.ValidationFunc) ||
			!validCurrentAvailableOptionalString(entry.CheckConnectionFunc) {
			return nil, ErrInvalidCurrentAvailableSnapshot
		}
		definitions[index] = configurationdomain.RegistryEntryDefinition{
			Type:                     entry.Type,
			Section:                  entry.Section,
			JSONSchema:               entry.ConfigSchema,
			ValidationSupported:      entry.ValidationFunc != nil,
			ConnectionCheckSupported: entry.HasTestConnection,
		}
		entries[index] = CurrentAvailableConfigurationType{
			Type:                 entry.Type,
			Section:              entry.Section,
			ConfigSchema:         bytes.Clone(entry.ConfigSchema),
			HasTestConnection:    entry.HasTestConnection,
			CheckConnectionLabel: cloneCurrentAvailableString(entry.CheckConnectionLabel),
			ValidationFunc:       cloneCurrentAvailableString(entry.ValidationFunc),
			CheckConnectionFunc:  cloneCurrentAvailableString(entry.CheckConnectionFunc),
		}
		entryIndexes[entry.Type] = index
	}
	if _, err := configurationdomain.NewRegistrySnapshot(definitions); err != nil {
		return nil, ErrInvalidCurrentAvailableSnapshot
	}

	return &CurrentAvailableCatalog{
		entries:                entries,
		entryIndexes:           entryIndexes,
		sourceRevisions:        cloneCurrentAvailableSources(document.Sources),
		dynamicSourceRevisions: cloneCurrentAvailableSources(document.DynamicSources),
		complete:               document.DynamicSources[currentMCPDynamicSource] == currentMCPEmptySourceRevision,
	}, nil
}

// EntryByType returns a defensive copy of one immutable registry entry. Type
// lookup is exact because the current registry is keyed by its lowercase type
// identifiers; callers must not silently normalize an unknown type into a
// different registered contract.
func (catalog *CurrentAvailableCatalog) EntryByType(typeName string) (CurrentAvailableConfigurationType, bool) {
	if catalog == nil {
		return CurrentAvailableConfigurationType{}, false
	}
	index, ok := catalog.entryIndexes[typeName]
	if !ok {
		return CurrentAvailableConfigurationType{}, false
	}
	return cloneCurrentAvailableEntry(catalog.entries[index]), true
}

// DataSchemaByType returns a defensive copy of the data schema embedded in the
// current ConfigurationCreateBase schema. The second result distinguishes an
// unknown type or a catalog entry without a usable object data schema.
func (catalog *CurrentAvailableCatalog) DataSchemaByType(typeName string) (map[string]any, bool) {
	entry, ok := catalog.EntryByType(typeName)
	if !ok {
		return nil, false
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(entry.ConfigSchema, &schema); err != nil {
		return nil, false
	}
	rawData, ok := schema.Properties["data"]
	if !ok {
		return nil, false
	}
	var dataSchema map[string]any
	decoder := json.NewDecoder(bytes.NewReader(rawData))
	decoder.UseNumber()
	if err := decoder.Decode(&dataSchema); err != nil || dataSchema == nil {
		return nil, false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return cloneCurrentJSONObject(dataSchema), true
}

// PinnedEntries returns a defensive copy of the pinned registry entries. An
// empty section list returns every pinned entry. A non-empty list follows
// Flask request.args.getlist semantics, including an empty section matching
// nothing and duplicate filters not duplicating results.
func (catalog *CurrentAvailableCatalog) PinnedEntries(sections ...string) []CurrentAvailableConfigurationType {
	if catalog == nil {
		return nil
	}
	if len(sections) == 0 {
		return cloneCurrentAvailableEntries(catalog.entries)
	}

	selected := make(map[string]struct{}, len(sections))
	for _, section := range sections {
		selected[section] = struct{}{}
	}
	entries := make([]CurrentAvailableConfigurationType, 0, len(catalog.entries))
	for _, entry := range catalog.entries {
		if _, ok := selected[entry.Section]; ok {
			entries = append(entries, cloneCurrentAvailableEntry(entry))
		}
	}
	return entries
}

// CompleteEntries refuses to turn the pinned subset into a production
// response while deployment-specific MCP configuration types are unavailable.
func (catalog *CurrentAvailableCatalog) CompleteEntries(sections ...string) ([]CurrentAvailableConfigurationType, error) {
	if catalog == nil || !catalog.complete {
		return nil, ErrCurrentAvailableCatalogPartial
	}
	return catalog.PinnedEntries(sections...), nil
}

func (catalog *CurrentAvailableCatalog) Complete() bool {
	return catalog != nil && catalog.complete
}

func (catalog *CurrentAvailableCatalog) SourceRevisions() map[string]string {
	if catalog == nil {
		return nil
	}
	return cloneCurrentAvailableSources(catalog.sourceRevisions)
}

// DynamicSourceRevisions returns the deployment-derived catalog source
// revisions separately from the pinned source commits. A present empty source
// is intentionally different from an unavailable source.
func (catalog *CurrentAvailableCatalog) DynamicSourceRevisions() map[string]string {
	if catalog == nil {
		return nil
	}
	return cloneCurrentAvailableSources(catalog.dynamicSourceRevisions)
}

func validCurrentAvailableSources(sources map[string]string) bool {
	required := [...]string{
		"configurations",
		"runtime_interface_litellm",
		"artifacts",
		"elitea_sdk",
		"elitea_core",
		"indexer_worker",
	}
	if len(sources) != len(required) {
		return false
	}
	for _, name := range required {
		revision, ok := sources[name]
		if !ok || len(revision) != 40 {
			return false
		}
		decoded, err := hex.DecodeString(revision)
		if err != nil || len(decoded) != 20 {
			return false
		}
	}
	return true
}

func validCurrentAvailableDynamicSources(sources map[string]string) bool {
	if len(sources) != 2 || sources[currentProviderDynamicSource] != currentDynamicSourceEmpty {
		return false
	}
	mcpRevision := sources[currentMCPDynamicSource]
	return mcpRevision == currentDynamicRequiredMissing || mcpRevision == currentMCPEmptySourceRevision
}

func validCurrentAvailableMCPEntries(
	mcpRevision string,
	entries []currentAvailableSnapshotEntry,
) bool {
	if mcpRevision != currentDynamicRequiredMissing && mcpRevision != currentMCPEmptySourceRevision {
		return false
	}
	for _, entry := range entries {
		if len(entry.Type) >= len(currentMCPConfigurationTypePrefix) &&
			entry.Type[:len(currentMCPConfigurationTypePrefix)] == currentMCPConfigurationTypePrefix {
			return false
		}
	}
	return true
}

func validCurrentAvailableOptionalString(value *string) bool {
	return value == nil || len(*value) <= configurationdomain.MaxRegistryIdentifierBytes
}

func cloneCurrentAvailableEntries(entries []CurrentAvailableConfigurationType) []CurrentAvailableConfigurationType {
	cloned := make([]CurrentAvailableConfigurationType, len(entries))
	for index, entry := range entries {
		cloned[index] = cloneCurrentAvailableEntry(entry)
	}
	return cloned
}

func cloneCurrentAvailableEntry(entry CurrentAvailableConfigurationType) CurrentAvailableConfigurationType {
	entry.ConfigSchema = bytes.Clone(entry.ConfigSchema)
	entry.CheckConnectionLabel = cloneCurrentAvailableString(entry.CheckConnectionLabel)
	entry.ValidationFunc = cloneCurrentAvailableString(entry.ValidationFunc)
	entry.CheckConnectionFunc = cloneCurrentAvailableString(entry.CheckConnectionFunc)
	return entry
}

func cloneCurrentAvailableString(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneCurrentAvailableSources(sources map[string]string) map[string]string {
	cloned := make(map[string]string, len(sources))
	for name, revision := range sources {
		cloned[name] = revision
	}
	return cloned
}
