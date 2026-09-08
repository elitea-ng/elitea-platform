package runtimecomposition

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/toolkitnaming"
)

const (
	currentToolkitSchemaSnapshotVersion = "elitea.current-toolkit-schema-snapshot.v1"
	// The pinned snapshot carries per-tool argument schemas as well as the
	// settings annotations, so it is no longer a few kilobytes: it measured
	// 595,983 bytes at SDK revision b5113a1 (56.8% of this ceiling, up from
	// 0.8% before the argument schemas were retained). The limit is kept where
	// it is deliberately — it still admits the file with ~440 KiB of headroom,
	// and raising it to accommodate a future regeneration should be a decision
	// someone makes on purpose, after looking at why the file grew.
	maxCurrentToolkitSchemaSnapshotBytes = 1 << 20
	maxCurrentToolkitSchemaEntries       = 1024
	maxCurrentToolkitSchemaDepth         = 32
	maxCurrentToolkitSchemaNodes         = 65_536
	maxCurrentToolkitSchemaIdentifier    = 1024
	maxCurrentToolkitNameLength          = 4096
)

var (
	ErrCurrentToolkitSchemaSnapshotInvalid     = errors.New("current toolkit schema snapshot is invalid")
	ErrCurrentDynamicToolkitSchemasUnavailable = errors.New("current dynamic toolkit schemas are unavailable")
	ErrCurrentDynamicToolkitSchemaInvalid      = errors.New("current dynamic toolkit schema is invalid")
	ErrCurrentToolkitSchemaLookupInvalid       = errors.New("current toolkit schema lookup is invalid")
	ErrCurrentToolkitNameInputInvalid          = errors.New("current toolkit name input is invalid")
)

// This projection is generated from the pinned SDK registry through the same
// get_toolkits()[].schema() contract used by the current indexer_worker. It is
// intentionally data, not provider-specific Go code.
//
//go:embed current_toolkit_schema_snapshot.json
var pinnedCurrentToolkitSchemaSnapshotJSON []byte

type currentToolkitSchemaSnapshotDocument struct {
	SchemaVersion string                                  `json:"schema_version"`
	SDKRevision   string                                  `json:"sdk_revision"`
	Entries       []currentToolkitSchemaSnapshotJSONEntry `json:"entries"`
}

type currentToolkitSchemaSnapshotJSONEntry struct {
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties"`
	// ArgsSchemas is the per-tool argument JSON Schema, keyed by tool name,
	// exactly as model_json_schema() produced it in the pinned SDK. It is held
	// as decoded JSON (map[string]any trees) rather than a narrowed Go schema
	// type because it must survive verbatim to the HTTP response: a narrowed
	// representation drops sibling keywords such as $defs and leaves the $refs
	// that point at them dangling. The generic tree is still validated — every
	// node goes through cloneCurrentToolkitSchemaValue's depth/node/key budget,
	// the same walker the settings annotations use.
	ArgsSchemas map[string]map[string]any         `json:"args_schemas"`
	Naming      *currentToolkitSchemaSnapshotName `json:"naming"`
}

type currentToolkitSchemaSnapshotName struct {
	Field     *string `json:"field"`
	MaxLength *int    `json:"max_length"`
}

type currentToolkitSchemaSnapshotEntry struct {
	properties  map[string]any
	argsSchemas map[string]map[string]any
	nameField   string
	maxLength   int
}

// CurrentToolkitSchemaSnapshot is an immutable projection of the current
// process-wide toolkit_schemas registry. Returned schema maps are always
// detached copies.
type CurrentToolkitSchemaSnapshot struct {
	sdkRevision string
	entries     map[string]currentToolkitSchemaSnapshotEntry
}

// LoadPinnedCurrentToolkitSchemaSnapshot loads the SDK revision admitted by
// the current Python worker capability manifest. Startup composition must also
// compare this revision with the admitted worker manifest before dispatch.
func LoadPinnedCurrentToolkitSchemaSnapshot() (*CurrentToolkitSchemaSnapshot, error) {
	return LoadCurrentToolkitSchemaSnapshot(pinnedCurrentToolkitSchemaSnapshotJSON)
}

// LoadCurrentToolkitSchemaSnapshot parses one bounded, deterministic registry
// projection. The snapshot contains only annotations consumed by current
// settings expansion and toolkit naming; it is not a public JSON Schema API.
func LoadCurrentToolkitSchemaSnapshot(data []byte) (*CurrentToolkitSchemaSnapshot, error) {
	if len(data) == 0 || len(data) > maxCurrentToolkitSchemaSnapshotBytes {
		return nil, ErrCurrentToolkitSchemaSnapshotInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var document currentToolkitSchemaSnapshotDocument
	if err := decoder.Decode(&document); err != nil {
		return nil, ErrCurrentToolkitSchemaSnapshotInvalid
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, ErrCurrentToolkitSchemaSnapshotInvalid
	}
	if document.SchemaVersion != currentToolkitSchemaSnapshotVersion ||
		!validCurrentToolkitSchemaIdentifier(document.SDKRevision) ||
		len(document.Entries) == 0 || len(document.Entries) > maxCurrentToolkitSchemaEntries {
		return nil, ErrCurrentToolkitSchemaSnapshotInvalid
	}

	entries := make(map[string]currentToolkitSchemaSnapshotEntry, len(document.Entries))
	previousType := ""
	for index := range document.Entries {
		entry := document.Entries[index]
		if !validCurrentToolkitSchemaIdentifier(entry.Type) || entry.Properties == nil ||
			entry.ArgsSchemas == nil || entry.Naming == nil ||
			entry.Naming.MaxLength == nil || (index != 0 && entry.Type <= previousType) ||
			*entry.Naming.MaxLength < 0 || *entry.Naming.MaxLength > maxCurrentToolkitNameLength {
			return nil, ErrCurrentToolkitSchemaSnapshotInvalid
		}
		previousType = entry.Type
		budget := currentToolkitSchemaTreeBudget{}
		properties, err := cloneCurrentToolkitSchemaObject(entry.Properties, 0, &budget)
		if err != nil {
			return nil, ErrCurrentToolkitSchemaSnapshotInvalid
		}
		// One budget for the whole entry: the annotations and every argument
		// schema of a type share the node ceiling, so a type cannot buy extra
		// tree by splitting it across tools. The largest entry at revision
		// b5113a1 (github, 44 tools) measures 1,703 nodes.
		argsSchemas, err := cloneCurrentToolkitArgumentSchemas(entry.ArgsSchemas, &budget)
		if err != nil {
			return nil, ErrCurrentToolkitSchemaSnapshotInvalid
		}
		nameField := ""
		if entry.Naming.Field != nil {
			nameField = *entry.Naming.Field
			if !validCurrentToolkitSchemaIdentifier(nameField) ||
				!currentToolkitSchemaDeclaresNameField(properties, nameField) {
				return nil, ErrCurrentToolkitSchemaSnapshotInvalid
			}
		}
		entries[entry.Type] = currentToolkitSchemaSnapshotEntry{
			properties:  properties,
			argsSchemas: argsSchemas,
			nameField:   nameField,
			maxLength:   *entry.Naming.MaxLength,
		}
	}
	return &CurrentToolkitSchemaSnapshot{sdkRevision: document.SDKRevision, entries: entries}, nil
}

func (s *CurrentToolkitSchemaSnapshot) SDKRevision() string {
	if s == nil {
		return ""
	}
	return s.sdkRevision
}

func (s *CurrentToolkitSchemaSnapshot) EntryCount() int {
	if s == nil {
		return 0
	}
	return len(s.entries)
}

// ToolkitTypes returns every built-in toolkit type name the snapshot declares,
// sorted.
//
// This is the registry the admin Configuration page's guardrail fields need to
// offer as choices — the `enum_source: toolkit_names` the reference schema
// declares. pylon answered it from `elitea_core.toolkit_schemas`, an in-process
// dict populated at plugin load; this snapshot is the same registry, pinned to
// the exact SDK revision the workers are admitted to run, which makes it a
// BETTER source rather than a substitute: an operator cannot be offered a
// toolkit name the workers would not recognise.
func (s *CurrentToolkitSchemaSnapshot) ToolkitTypes() []string {
	if s == nil {
		return nil
	}
	types := make([]string, 0, len(s.entries))
	for toolkitType := range s.entries {
		types = append(types, toolkitType)
	}
	sort.Strings(types)
	return types
}

// ToolkitToolNames returns the tool names one built-in toolkit type declares,
// sorted. found=false means the type is not a built-in SDK toolkit.
//
// The names are the KEYS of the pinned argument schemas, which is where the tool
// list authoritatively lives — the same keys ListTypeSchemas serves as
// `selected_tools.args_schemas`. The reference read them out of a Literal enum
// nested in a Pydantic JSON Schema and had to handle two draft formats to do it
// (`enum`, and `const` for a single-value Literal); this snapshot has already
// resolved that, so there is one shape here and no format sniffing.
func (s *CurrentToolkitSchemaSnapshot) ToolkitToolNames(toolkitType string) ([]string, bool) {
	if s == nil || !validCurrentToolkitSchemaIdentifier(toolkitType) {
		return nil, false
	}
	entry, found := s.entries[toolkitType]
	if !found {
		return nil, false
	}
	names := make([]string, 0, len(entry.argsSchemas))
	for name := range entry.argsSchemas {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, true
}

// ToolkitArgumentSchemas returns a detached copy of one built-in toolkit
// type's per-tool argument schemas, keyed by tool name. found=false means the
// type is not a built-in SDK toolkit; an empty (non-nil) map means the type is
// built in and legitimately exposes no argument schemas — mcp, mcp_config and
// openapi are the three such types at revision b5113a1, because their tools are
// discovered at runtime from a remote server or specification rather than
// declared by an SDK model.
func (s *CurrentToolkitSchemaSnapshot) ToolkitArgumentSchemas(
	toolkitType string,
) (map[string]map[string]any, bool, error) {
	if s == nil || !validCurrentToolkitSchemaIdentifier(toolkitType) {
		return nil, false, ErrCurrentToolkitSchemaSnapshotInvalid
	}
	entry, found := s.entries[toolkitType]
	if !found {
		return nil, false, nil
	}
	budget := currentToolkitSchemaTreeBudget{}
	argsSchemas, err := cloneCurrentToolkitArgumentSchemas(entry.argsSchemas, &budget)
	if err != nil {
		return nil, false, ErrCurrentToolkitSchemaSnapshotInvalid
	}
	return argsSchemas, true, nil
}

func cloneCurrentToolkitArgumentSchemas(
	argsSchemas map[string]map[string]any,
	budget *currentToolkitSchemaTreeBudget,
) (map[string]map[string]any, error) {
	if argsSchemas == nil || budget == nil {
		return nil, ErrCurrentToolkitSchemaSnapshotInvalid
	}
	cloned := make(map[string]map[string]any, len(argsSchemas))
	for tool, schema := range argsSchemas {
		if !validCurrentToolkitSchemaIdentifier(tool) || schema == nil {
			return nil, ErrCurrentToolkitSchemaSnapshotInvalid
		}
		clonedSchema, err := cloneCurrentToolkitSchemaObject(schema, 0, budget)
		if err != nil {
			return nil, err
		}
		cloned[tool] = clonedSchema
	}
	return cloned, nil
}

func (s *CurrentToolkitSchemaSnapshot) findBuiltIn(
	toolkitType string,
) (configurationapp.CurrentToolkitSchema, currentToolkitSchemaSnapshotEntry, bool, error) {
	if s == nil {
		return configurationapp.CurrentToolkitSchema{}, currentToolkitSchemaSnapshotEntry{}, false,
			ErrCurrentToolkitSchemaSnapshotInvalid
	}
	entry, found := s.entries[toolkitType]
	if !found {
		return configurationapp.CurrentToolkitSchema{}, currentToolkitSchemaSnapshotEntry{}, false, nil
	}
	budget := currentToolkitSchemaTreeBudget{}
	properties, err := cloneCurrentToolkitSchemaObject(entry.properties, 0, &budget)
	if err != nil {
		return configurationapp.CurrentToolkitSchema{}, currentToolkitSchemaSnapshotEntry{}, false,
			ErrCurrentToolkitSchemaSnapshotInvalid
	}
	return configurationapp.CurrentToolkitSchema{Properties: properties}, entry, true, nil
}

// CurrentActorVisibleToolkitSchemaSource owns the current project, personal
// project, shared/public, toolkit-security and provider/MCP visibility rules.
// Parity requires personal > current > public Provider Hub precedence, private
// > current MCP precedence, MCP > Provider Hub title precedence, and live
// deployment blocklist filtering. A missing or invisible schema returns
// found=false. Implementations must not return a process-global unfiltered
// dynamic registry.
type CurrentActorVisibleToolkitSchemaSource interface {
	FindCurrentActorVisibleToolkitSchema(
		context.Context,
		int32,
		int32,
		string,
	) (configurationapp.CurrentToolkitSchema, bool, error)
}

// UnavailableCurrentActorVisibleToolkitSchemas is the explicit fail-closed
// adapter used until Provider Hub and MCP schema ownership is ported. It lets
// built-in SDK types run without pretending dynamic parity exists.
type UnavailableCurrentActorVisibleToolkitSchemas struct{}

func (UnavailableCurrentActorVisibleToolkitSchemas) FindCurrentActorVisibleToolkitSchema(
	ctx context.Context,
	_ int32,
	_ int32,
	_ string,
) (configurationapp.CurrentToolkitSchema, bool, error) {
	if ctx == nil {
		return configurationapp.CurrentToolkitSchema{}, false, ErrCurrentToolkitSchemaLookupInvalid
	}
	if err := ctx.Err(); err != nil {
		return configurationapp.CurrentToolkitSchema{}, false, err
	}
	return configurationapp.CurrentToolkitSchema{}, false, ErrCurrentDynamicToolkitSchemasUnavailable
}

// CurrentCompositeToolkitSchemaCatalog preserves the current expansion lookup
// order: the process-wide SDK/indexer snapshot wins; only a missing built-in
// type consults the actor-visible Provider Hub/MCP overlay.
type CurrentCompositeToolkitSchemaCatalog struct {
	builtIn *CurrentToolkitSchemaSnapshot
	dynamic CurrentActorVisibleToolkitSchemaSource
}

func NewCurrentCompositeToolkitSchemaCatalog(
	builtIn *CurrentToolkitSchemaSnapshot,
	dynamic CurrentActorVisibleToolkitSchemaSource,
) (*CurrentCompositeToolkitSchemaCatalog, error) {
	if builtIn == nil || dynamic == nil {
		return nil, errors.New("current built-in and dynamic toolkit schema catalogs are required")
	}
	return &CurrentCompositeToolkitSchemaCatalog{builtIn: builtIn, dynamic: dynamic}, nil
}

func (c *CurrentCompositeToolkitSchemaCatalog) FindEffectiveToolkitSchema(
	ctx context.Context,
	projectID int32,
	userID int32,
	toolkitType string,
) (configurationapp.CurrentToolkitSchema, bool, error) {
	if ctx == nil || projectID <= 0 || userID <= 0 || !validCurrentToolkitSchemaIdentifier(toolkitType) {
		return configurationapp.CurrentToolkitSchema{}, false, ErrCurrentToolkitSchemaLookupInvalid
	}
	if err := ctx.Err(); err != nil {
		return configurationapp.CurrentToolkitSchema{}, false, err
	}
	if c == nil || c.builtIn == nil || c.dynamic == nil {
		return configurationapp.CurrentToolkitSchema{}, false, ErrCurrentToolkitSchemaLookupInvalid
	}

	schema, _, found, err := c.builtIn.findBuiltIn(toolkitType)
	if err != nil || found {
		return schema, found, err
	}
	schema, found, err = c.dynamic.FindCurrentActorVisibleToolkitSchema(
		ctx,
		projectID,
		userID,
		toolkitType,
	)
	if err != nil || !found {
		return configurationapp.CurrentToolkitSchema{}, found, err
	}
	if err := ctx.Err(); err != nil {
		return configurationapp.CurrentToolkitSchema{}, false, err
	}
	if schema.Properties == nil {
		return configurationapp.CurrentToolkitSchema{}, false, ErrCurrentDynamicToolkitSchemaInvalid
	}
	budget := currentToolkitSchemaTreeBudget{}
	properties, cloneErr := cloneCurrentToolkitSchemaObject(schema.Properties, 0, &budget)
	if cloneErr != nil {
		return configurationapp.CurrentToolkitSchema{}, false, ErrCurrentDynamicToolkitSchemaInvalid
	}
	return configurationapp.CurrentToolkitSchema{Properties: properties}, true, nil
}

// CurrentBuiltInToolkitNameDeriver reproduces ToolDetails.set_toolkit_name
// using only the process-wide built-in snapshot. Dynamic Provider Hub/MCP
// schemas never influence naming in the current implementation.
type CurrentBuiltInToolkitNameDeriver struct {
	builtIn *CurrentToolkitSchemaSnapshot
}

func NewCurrentBuiltInToolkitNameDeriver(
	builtIn *CurrentToolkitSchemaSnapshot,
) (*CurrentBuiltInToolkitNameDeriver, error) {
	if builtIn == nil {
		return nil, errors.New("current built-in toolkit schema snapshot is required")
	}
	return &CurrentBuiltInToolkitNameDeriver{builtIn: builtIn}, nil
}

func (d *CurrentBuiltInToolkitNameDeriver) DeriveCurrentToolkitName(
	ctx context.Context,
	input CurrentToolkitNameInput,
) (string, error) {
	if ctx == nil || input.ProjectID <= 0 || input.UserID <= 0 ||
		!validCurrentToolkitSchemaIdentifier(input.ToolkitType) || input.Settings == nil ||
		d == nil || d.builtIn == nil {
		return "", ErrCurrentToolkitNameInputInvalid
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	_, entry, found, err := d.builtIn.findBuiltIn(input.ToolkitType)
	if err != nil {
		return "", err
	}

	var candidate any
	if found && entry.nameField != "" {
		candidate = input.Settings[entry.nameField]
	}
	if candidate == nil && input.StoredName != nil {
		candidate = *input.StoredName
	}
	if candidate == nil {
		return "", nil
	}
	text, ok := currentPythonScalarString(candidate)
	if !ok {
		// Current name-bearing SDK fields are strings. Failing closed here avoids
		// inventing a Go map/list rendering for a corrupted current row.
		return "", ErrCurrentToolkitNameInputInvalid
	}
	// The shared rule (internal/toolkitnaming). The per-type maximum length
	// below is this deriver's own, and stays here: it comes from the toolkit
	// schema snapshot, which the naming package must not depend on.
	cleaned := toolkitnaming.RuntimeName(text, "")
	if found && entry.maxLength > 0 && len(cleaned) > entry.maxLength {
		cleaned = cleaned[:entry.maxLength]
	}
	return cleaned, nil
}

func currentPythonScalarString(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case bool:
		if typed {
			return "True", true
		}
		return "False", true
	case json.Number:
		return typed.String(), true
	case int:
		return strconv.Itoa(typed), true
	case int8:
		return strconv.FormatInt(int64(typed), 10), true
	case int16:
		return strconv.FormatInt(int64(typed), 10), true
	case int32:
		return strconv.FormatInt(int64(typed), 10), true
	case int64:
		return strconv.FormatInt(typed, 10), true
	case uint:
		return strconv.FormatUint(uint64(typed), 10), true
	case uint8:
		return strconv.FormatUint(uint64(typed), 10), true
	case uint16:
		return strconv.FormatUint(uint64(typed), 10), true
	case uint32:
		return strconv.FormatUint(uint64(typed), 10), true
	case uint64:
		return strconv.FormatUint(typed, 10), true
	case float32:
		return strconv.FormatFloat(float64(typed), 'g', -1, 32), true
	case float64:
		return strconv.FormatFloat(typed, 'g', -1, 64), true
	default:
		return "", false
	}
}

func currentToolkitSchemaDeclaresNameField(properties map[string]any, field string) bool {
	property, ok := properties[field].(map[string]any)
	if !ok {
		return false
	}
	value, ok := property["toolkit_name"].(bool)
	return ok && value
}

func validCurrentToolkitSchemaIdentifier(value string) bool {
	return value != "" && len(value) <= maxCurrentToolkitSchemaIdentifier &&
		!strings.ContainsAny(value, "\x00\r\n")
}

type currentToolkitSchemaTreeBudget struct {
	nodes int
}

func cloneCurrentToolkitSchemaObject(
	value map[string]any,
	depth int,
	budget *currentToolkitSchemaTreeBudget,
) (map[string]any, error) {
	cloned, err := cloneCurrentToolkitSchemaValue(value, depth, budget)
	if err != nil {
		return nil, err
	}
	object, ok := cloned.(map[string]any)
	if !ok {
		return nil, ErrCurrentToolkitSchemaSnapshotInvalid
	}
	return object, nil
}

func cloneCurrentToolkitSchemaValue(
	value any,
	depth int,
	budget *currentToolkitSchemaTreeBudget,
) (any, error) {
	if budget == nil || depth > maxCurrentToolkitSchemaDepth {
		return nil, ErrCurrentToolkitSchemaSnapshotInvalid
	}
	budget.nodes++
	if budget.nodes > maxCurrentToolkitSchemaNodes {
		return nil, ErrCurrentToolkitSchemaSnapshotInvalid
	}
	switch typed := value.(type) {
	case nil, bool, json.Number:
		return typed, nil
	case string:
		if len(typed) > maxCurrentToolkitSchemaSnapshotBytes {
			return nil, ErrCurrentToolkitSchemaSnapshotInvalid
		}
		return typed, nil
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return typed, nil
	case float32:
		if math.IsNaN(float64(typed)) || math.IsInf(float64(typed), 0) {
			return nil, ErrCurrentToolkitSchemaSnapshotInvalid
		}
		return typed, nil
	case float64:
		// encoding/json produces float64 only when a dynamic source constructs
		// one directly. JSON Schema numbers are retained without coercion.
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return nil, ErrCurrentToolkitSchemaSnapshotInvalid
		}
		return typed, nil
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			if !validCurrentToolkitSchemaIdentifier(key) {
				return nil, ErrCurrentToolkitSchemaSnapshotInvalid
			}
			cloned, err := cloneCurrentToolkitSchemaValue(item, depth+1, budget)
			if err != nil {
				return nil, err
			}
			result[key] = cloned
		}
		return result, nil
	case []any:
		result := make([]any, len(typed))
		for index := range typed {
			cloned, err := cloneCurrentToolkitSchemaValue(typed[index], depth+1, budget)
			if err != nil {
				return nil, err
			}
			result[index] = cloned
		}
		return result, nil
	default:
		return nil, ErrCurrentToolkitSchemaSnapshotInvalid
	}
}

var _ configurationapp.CurrentToolkitSchemaCatalog = (*CurrentCompositeToolkitSchemaCatalog)(nil)
var _ CurrentToolkitNameDeriver = (*CurrentBuiltInToolkitNameDeriver)(nil)
