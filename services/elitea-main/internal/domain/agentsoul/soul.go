// Package agentsoul validates an agent version's soul document (ADR-0050
// decision 1a, issue 1604).
//
// THE CONTRACT IS THE SOURCE. The shape is the AgentSoulV1 component of
// api/openapi/v2.yaml, read out of the embedded spec and compiled once, the
// way internal/api/v2/drafts/schema.go serves AgentDefinition. Two rules sit
// in front of the schema because a schema error cannot name them:
//
//   - A soul grants nothing. A key that would read as a grant (tools,
//     credentials, permissions and their kin, at any depth) is refused with
//     CodeCapabilityKey before anything else, so the refusal says why.
//   - At most MaxBytes of text: the UTF-8 bytes of every string value,
//     summed. Keys are not counted, so the measure is the same for the JSON
//     column, the editor and a SOUL.md file (CodeTooLarge).
//
// Everything else the schema refuses is CodeInvalid.
package agentsoul

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	specfiles "github.com/elitea-ng/elitea-platform/services/elitea-main/api/openapi"
)

const (
	// Component is the schema's name in v2.yaml.
	Component = "AgentSoulV1"
	// SchemaID is the only `schema` value of this version.
	SchemaID = "elitea.soul/v1"
	// MaxBytes bounds the text of one soul.
	MaxBytes = 16 * 1024
	// maxEncodedBytes bounds the JSON a soul may arrive as before it is
	// decoded: the text bound plus room for keys and escaping.
	maxEncodedBytes = 4 * MaxBytes

	CodeCapabilityKey = "soul_capability_key"
	CodeTooLarge      = "soul_too_large"
	CodeInvalid       = "soul_invalid"
)

// capabilityKeys are the keys that would read as a grant. The schema refuses
// every unknown key too; these are named so the refusal says why.
var capabilityKeys = map[string]struct{}{
	"tool": {}, "tools": {}, "toolkit": {}, "toolkits": {}, "toolset": {}, "toolsets": {},
	"mcp": {}, "mcp_server": {}, "mcp_servers": {}, "skill": {}, "skills": {},
	"credential": {}, "credentials": {}, "secret": {}, "secrets": {}, "api_key": {}, "api_keys": {},
	"token": {}, "tokens": {}, "password": {}, "passwords": {},
	"permission": {}, "permissions": {}, "scope": {}, "scopes": {}, "grant": {}, "grants": {},
	"capability": {}, "capabilities": {}, "role_grants": {}, "allow": {}, "allowed_tools": {},
	"internal_tools": {}, "data_classes": {}, "risk_tier": {},
}

// Error is a refused soul. Code is one of the Code constants; Message names
// the rule and the key or field, never a value.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

var (
	compileOnce sync.Once
	compiled    *jsonschema.Schema
	compileErr  error
)

func validator() (*jsonschema.Schema, error) {
	compileOnce.Do(func() { compiled, compileErr = compile(specfiles.SpecYAML) })
	return compiled, compileErr
}

func compile(spec []byte) (*jsonschema.Schema, error) {
	var document struct {
		Components struct {
			Schemas map[string]any `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(spec, &document); err != nil {
		return nil, fmt.Errorf("agentsoul: parse the embedded spec: %w", err)
	}
	component, ok := document.Components.Schemas[Component]
	if !ok {
		return nil, fmt.Errorf("agentsoul: v2.yaml has no %s component", Component)
	}
	encoded, err := json.Marshal(component)
	if err != nil {
		return nil, fmt.Errorf("agentsoul: encode %s: %w", Component, err)
	}
	if bytes.Contains(encoded, []byte(`"$ref"`)) {
		return nil, fmt.Errorf("agentsoul: %s must be self-contained (no $ref)", Component)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("agentsoul: decode %s: %w", Component, err)
	}
	compiler := jsonschema.NewCompiler()
	const url = "elitea:agent-soul-v1.json"
	if err := compiler.AddResource(url, instance); err != nil {
		return nil, fmt.Errorf("agentsoul: add %s: %w", Component, err)
	}
	return compiler.Compile(url)
}

// Normalize checks a soul decoded from a request body (a map from
// encoding/json) and answers its canonical JSON, the bytes to store. A nil
// value is "no soul" and answers nil, nil.
func Normalize(value any) (json.RawMessage, error) {
	if value == nil {
		return nil, nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, &Error{Code: CodeInvalid, Message: "the soul must be an object"}
	}
	if key, found := firstCapabilityKey(object, ""); found {
		return nil, &Error{Code: CodeCapabilityKey,
			Message: "a soul grants nothing: the key " + key + " names a capability; tools, credentials and permissions are set on the agent, not in its soul"}
	}
	if size := textBytes(object); size > MaxBytes {
		return nil, &Error{Code: CodeTooLarge,
			Message: fmt.Sprintf("the soul holds %d bytes of text; at most %d are allowed", size, MaxBytes)}
	}
	schema, err := validator()
	if err != nil {
		return nil, err
	}
	if err := schema.Validate(object); err != nil {
		var validation *jsonschema.ValidationError
		if errors.As(err, &validation) {
			return nil, &Error{Code: CodeInvalid, Message: "the soul does not match " + Component + ": " + failingFields(validation)}
		}
		return nil, err
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return nil, &Error{Code: CodeInvalid, Message: "the soul cannot be encoded"}
	}
	return encoded, nil
}

// normalizeJSON is Normalize over encoded JSON, as the tests feed it. Empty
// input and JSON null are "no soul".
func normalizeJSON(raw []byte) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	if len(trimmed) > maxEncodedBytes {
		return nil, &Error{Code: CodeTooLarge, Message: fmt.Sprintf("the soul is larger than %d bytes", maxEncodedBytes)}
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, &Error{Code: CodeInvalid, Message: "the soul is not JSON"}
	}
	return Normalize(value)
}

func firstCapabilityKey(value any, path string) (string, bool) {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			name := key
			if path != "" {
				name = path + "." + key
			}
			if _, banned := capabilityKeys[strings.ToLower(strings.TrimSpace(key))]; banned {
				return name, true
			}
			if found, ok := firstCapabilityKey(typed[key], name); ok {
				return found, true
			}
		}
	case []any:
		for index, item := range typed {
			if found, ok := firstCapabilityKey(item, fmt.Sprintf("%s[%d]", path, index)); ok {
				return found, true
			}
		}
	}
	return "", false
}

func textBytes(value any) int {
	switch typed := value.(type) {
	case string:
		return len(typed)
	case map[string]any:
		total := 0
		for _, item := range typed {
			total += textBytes(item)
		}
		return total
	case []any:
		total := 0
		for _, item := range typed {
			total += textBytes(item)
		}
		return total
	}
	return 0
}

func failingFields(err *jsonschema.ValidationError) string {
	fields := map[string]struct{}{}
	var walk func(*jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) > 0 {
			for _, cause := range e.Causes {
				walk(cause)
			}
			return
		}
		location := strings.Join(e.InstanceLocation, ".")
		if location == "" {
			location = "(root)"
		}
		fields[location] = struct{}{}
	}
	walk(err)
	names := make([]string, 0, len(fields))
	for field := range fields {
		names = append(names, field)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
