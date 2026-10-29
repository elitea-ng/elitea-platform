// Package agentrelease is the domain of agent releases and draft revisions
// (ADR-0040 decision 2, issue #1492).
//
// A release is an immutable snapshot of a published agent version: its
// canonical definition (the shared application_version_details_json
// projection) with every toolkit's settings removed, and a pin per attached
// toolkit. A pin holds identifiers and SHA-256 digests only, never a value:
// toolkit settings and configuration data may hold a credential.
//
// A run of a release executes the release's definition. The toolkit
// settings it needs are read from the live toolkit rows and must hash to
// the pinned digests; a configuration reference must resolve to the pinned
// configuration with the pinned data digest. Anything else refuses the run
// with DriftCode, so a toolkit or configuration edited after the release
// never silently changes what the release runs.
package agentrelease

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Release is one stored release.
type Release struct {
	ID                   int64
	ProjectID            int64
	ApplicationID        int64
	ApplicationVersionID int64
	SourceVersionID      int64
	Name                 string
	DefinitionDigest     [32]byte
	State                string
	PublishedBy          int64
	CreatedAt            time.Time
	WithdrawnAt          *time.Time
	WithdrawnBy          int64
	// IsHead is true for the application's published release.
	IsHead bool
	// Definition and ToolkitPins are read for one release only.
	Definition  json.RawMessage
	ToolkitPins []ToolkitPin
}

// DigestHex is the release's definition digest in lower-case hex.
func (r Release) DigestHex() string { return hex.EncodeToString(r.DefinitionDigest[:]) }

// Revision is one draft save of a version.
type Revision struct {
	ID                   int64
	ProjectID            int64
	ApplicationID        int64
	ApplicationVersionID int64
	Revision             int
	Kind                 string
	ActorID              int64
	CreatedAt            time.Time
	Snapshot             json.RawMessage
	Diff                 json.RawMessage
}

// Release states.
const (
	StatePublished = "published"
	StateWithdrawn = "withdrawn"
)

// DriftCode is the typed refusal of a run whose pinned toolkit
// configuration no longer resolves to what the release pinned.
const DriftCode = "release_toolkit_drifted"

// Drift reasons. Each is safe to show a caller: none names a value.
const (
	DriftToolkitSetChanged      = "toolkit_set_changed"
	DriftToolkitMissing         = "toolkit_missing"
	DriftToolkitSettingsChanged = "toolkit_settings_changed"
	DriftConfigurationMissing   = "configuration_missing"
	DriftConfigurationChanged   = "configuration_changed"
	// DriftDefinitionMismatch is a reconstructed definition whose digest
	// is not the release's: the live toolkit rows no longer give back the
	// definition the release addresses.
	DriftDefinitionMismatch = "definition_mismatch"
)

var (
	// ErrNotFound is a release that is not in the project and application.
	ErrNotFound = errors.New("agent release not found")
	// ErrNotPublished refuses a rollback of an application with no
	// published release: there is no pointer to move.
	ErrNotPublished = errors.New("the application has no published release")
	// ErrAlreadyHead refuses a rollback to the release already published.
	ErrAlreadyHead = errors.New("the release is already the published release")
	// ErrWithdrawn refuses a rollback to a withdrawn release: its version
	// was unpublished. It still runs by id.
	ErrWithdrawn = errors.New("the release is withdrawn")
	// ErrDefinitionUnreadable is a stored definition that does not decode.
	ErrDefinitionUnreadable = errors.New("the release definition is not readable")
)

// DriftError refuses a release run whose pinned toolkit configuration no
// longer resolves (DriftCode).
type DriftError struct {
	ToolkitID int64
	Reason    string
}

func (e *DriftError) Error() string {
	if e.ToolkitID > 0 {
		return fmt.Sprintf("%s: toolkit %d: %s", DriftCode, e.ToolkitID, e.Reason)
	}
	return DriftCode + ": " + e.Reason
}

// ConfigurationPin is one configuration reference ({"elitea_title", ...})
// in a toolkit's settings. A private reference resolves in the personal
// project of the user who runs the agent, so it cannot be pinned to one
// row: only its path and title are recorded. A non-private reference
// records the row it resolved to at publish (empty UUID when it resolved
// to none) and the SHA-256 of that row's data.
type ConfigurationPin struct {
	Path                  string `json:"path"`
	Title                 string `json:"elitea_title"`
	Private               bool   `json:"private"`
	CredentialReferenceID string `json:"credential_reference_id,omitempty"`
	ConfigurationUUID     string `json:"configuration_uuid,omitempty"`
	ProjectID             int64  `json:"project_id,omitempty"`
	DataSHA256            string `json:"data_sha256,omitempty"`
}

// ToolkitPin pins one entry of the definition's `tools`, by position.
type ToolkitPin struct {
	Index     int    `json:"index"`
	ToolkitID int64  `json:"toolkit_id"`
	Type      string `json:"type"`
	// SettingsSHA256 is CanonicalSHA256 of the toolkit row's stored settings.
	SettingsSHA256 string `json:"settings_sha256"`
	// ProjectedSettingsSHA256 is CanonicalSHA256 of the settings the
	// definition carried: the stored settings with the agent's tool
	// selection applied (ProjectSettings).
	ProjectedSettingsSHA256 string `json:"projected_settings_sha256"`
	// SelectedTools is the agent's tool selection for this toolkit (tool
	// names, from entity_tool_mapping.selected_tools), JSON null for none.
	SelectedTools          json.RawMessage    `json:"selected_tools"`
	Configurations         []ConfigurationPin `json:"configurations"`
	CredentialReferenceIDs []string           `json:"credential_reference_ids"`
}

// CanonicalJSON decodes raw (numbers kept as written) and encodes it again
// with sorted object keys and no HTML escaping, so two encodings of one
// value give the same bytes.
func CanonicalJSON(raw []byte) ([]byte, error) {
	value, err := decodeNumbers(raw)
	if err != nil {
		return nil, err
	}
	return encodeCanonical(value)
}

// CanonicalSHA256 is the hex SHA-256 of CanonicalJSON(raw).
func CanonicalSHA256(raw []byte) (string, error) {
	canonical, err := CanonicalJSON(raw)
	if err != nil {
		return "", err
	}
	return sha256Hex(canonical), nil
}

// ValueSHA256 is CanonicalSHA256 of an already decoded value.
func ValueSHA256(value any) (string, error) {
	canonical, err := encodeCanonical(value)
	if err != nil {
		return "", err
	}
	return sha256Hex(canonical), nil
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func decodeNumbers(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if decoder.More() {
		return nil, errors.New("trailing JSON")
	}
	return value, nil
}

// DecodeObject decodes a JSON object with numbers kept as written.
func DecodeObject(raw []byte) (map[string]any, error) {
	value, err := decodeNumbers(raw)
	if err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("not a JSON object")
	}
	return object, nil
}

func encodeCanonical(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buffer.Bytes(), "\n"), nil
}

// ProjectSettings applies an agent's tool selection to a toolkit's stored
// settings exactly as the shared application_version_details_json
// projection does (internal/db/queries/agent_chat.sql):
//
//   - stored `selected_tools` non-empty: the stored settings, plus
//     `available_tools` = the stored selection, plus — when the agent
//     selected tools — `selected_tools` = the agent's selection narrowed to
//     the stored one, or the agent's selection when nothing is left;
//   - otherwise, an agent selection: the stored settings plus
//     `selected_tools` = the agent's selection;
//   - otherwise the stored settings.
//
// stored is not modified.
func ProjectSettings(stored map[string]any, selected json.RawMessage) (map[string]any, error) {
	var agentSelection []any
	if len(selected) > 0 && string(selected) != "null" {
		value, err := decodeNumbers(selected)
		if err != nil {
			return nil, err
		}
		agentSelection, _ = value.([]any)
	}
	out := make(map[string]any, len(stored)+2)
	for key, value := range stored {
		out[key] = value
	}
	storedSelection, _ := stored["selected_tools"].([]any)
	switch {
	case len(storedSelection) > 0:
		out["available_tools"] = stored["selected_tools"]
		if len(agentSelection) > 0 {
			narrowed := make([]any, 0, len(agentSelection))
			for _, element := range agentSelection {
				text, ok := elementText(element)
				if ok && containsString(storedSelection, text) {
					narrowed = append(narrowed, text)
				}
			}
			if len(narrowed) > 0 {
				out["selected_tools"] = narrowed
			} else {
				out["selected_tools"] = agentSelection
			}
		}
	case len(agentSelection) > 0:
		out["selected_tools"] = agentSelection
	}
	return out, nil
}

// elementText is jsonb_array_elements_text of one element: a string as
// itself, a JSON null as SQL NULL (false), anything else as its JSON text.
func elementText(element any) (string, bool) {
	switch value := element.(type) {
	case nil:
		return "", false
	case string:
		return value, true
	default:
		encoded, err := encodeCanonical(value)
		if err != nil {
			return "", false
		}
		return string(encoded), true
	}
}

// containsString is jsonb `?` on an array: a STRING element equal to text.
func containsString(values []any, text string) bool {
	for _, value := range values {
		if s, ok := value.(string); ok && s == text {
			return true
		}
	}
	return false
}

// ConfigurationReference is one {"elitea_title": ...} object in a
// toolkit's settings.
type ConfigurationReference struct {
	Path                  string
	Title                 string
	Private               bool
	CredentialReferenceID string
}

// ConfigurationReferences lists the configuration references in settings,
// in path order: every object with a non-empty string `elitea_title`, at
// any depth, not inside an array (the configuration expansion's walk).
func ConfigurationReferences(settings map[string]any) []ConfigurationReference {
	var references []ConfigurationReference
	var walk func(object map[string]any, path string, depth int)
	walk = func(object map[string]any, path string, depth int) {
		if depth > 32 {
			return
		}
		if title, ok := object["elitea_title"].(string); ok && strings.TrimSpace(title) != "" && path != "" {
			private, _ := object["private"].(bool)
			credential, _ := object["credential_reference_id"].(string)
			references = append(references, ConfigurationReference{
				Path: path, Title: title, Private: private, CredentialReferenceID: credential,
			})
		}
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if child, ok := object[key].(map[string]any); ok {
				walk(child, path+"/"+escapePointer(key), depth+1)
			}
		}
	}
	walk(settings, "", 0)
	return references
}

// SplitDefinition removes every tool's `settings` from a definition and
// returns the stripped definition (the release snapshot) with the projected
// settings of each tool, by position. A tool with no settings object maps
// to nil.
func SplitDefinition(definition []byte) (map[string]any, []map[string]any, error) {
	object, err := DecodeObject(definition)
	if err != nil {
		return nil, nil, ErrDefinitionUnreadable
	}
	tools, _ := object["tools"].([]any)
	settings := make([]map[string]any, len(tools))
	for index, entry := range tools {
		tool, ok := entry.(map[string]any)
		if !ok {
			return nil, nil, ErrDefinitionUnreadable
		}
		settings[index], _ = tool["settings"].(map[string]any)
		delete(tool, "settings")
	}
	return object, settings, nil
}

// ToolkitID reads tools[index].id of a stripped definition.
func ToolkitID(definition map[string]any, index int) (int64, bool) {
	tools, _ := definition["tools"].([]any)
	if index < 0 || index >= len(tools) {
		return 0, false
	}
	tool, _ := tools[index].(map[string]any)
	return positiveInteger(tool["id"])
}

// ToolCount is len(definition.tools).
func ToolCount(definition map[string]any) int {
	tools, _ := definition["tools"].([]any)
	return len(tools)
}

// WithToolSettings writes settings into tools[index].settings.
func WithToolSettings(definition map[string]any, index int, settings any) bool {
	tools, _ := definition["tools"].([]any)
	if index < 0 || index >= len(tools) {
		return false
	}
	tool, ok := tools[index].(map[string]any)
	if !ok {
		return false
	}
	tool["settings"] = settings
	return true
}

// Encode is the canonical encoding of a definition.
func Encode(definition map[string]any) ([]byte, error) {
	return encodeCanonical(definition)
}

func positiveInteger(value any) (int64, bool) {
	switch number := value.(type) {
	case json.Number:
		parsed, err := number.Int64()
		return parsed, err == nil && parsed > 0
	case float64:
		parsed := int64(number)
		return parsed, float64(parsed) == number && parsed > 0
	case int64:
		return number, number > 0
	case int:
		return int64(number), number > 0
	}
	return 0, false
}

func escapePointer(key string) string {
	return strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}
