package configurations

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

var ErrInvalidCurrentConfigurationSecrets = errors.New("invalid current configuration secrets")

// HiddenSecretMutation is one plaintext value that must be persisted in the
// authorized project's hidden-secret store. Callers own the returned values
// and must not log or place them in ordinary configuration snapshots.
type HiddenSecretMutation struct {
	Field string
	Path  []string
	Name  string
	Value string
}

// CurrentSecretFieldError identifies the configuration field that could not be
// sanitized without including its value in the error.
type CurrentSecretFieldError struct {
	Field  string
	reason string
}

func (e *CurrentSecretFieldError) Error() string {
	return fmt.Sprintf("configuration field %q: %s", e.Field, e.reason)
}

func (e *CurrentSecretFieldError) Unwrap() error {
	return ErrInvalidCurrentConfigurationSecrets
}

// CurrentSecretIDGenerator returns the name for a new hidden secret. The name
// must be exactly 32 lowercase hexadecimal characters, matching the current
// UUID-without-hyphens representation.
type CurrentSecretIDGenerator func() (string, error)

// ExtractCurrentConfigurationSecrets returns a deep-copied configuration data
// object in which raw password values are replaced by hidden-secret references.
// Properties is the registry schema's data.properties object. Unknown
// top-level fields are rejected, matching the current Configurations behavior;
// nested objects are traversed only where their schema declares properties.
//
// Existing exact {{secret.NAME}} references, empty strings, and null values are
// preserved. Mutations are returned in deterministic field-path order. The
// caller's data is never modified, including when extraction fails.
func ExtractCurrentConfigurationSecrets(
	ctx context.Context,
	data map[string]any,
	properties map[string]any,
	configurationType string,
	newID CurrentSecretIDGenerator,
) (map[string]any, []HiddenSecretMutation, error) {
	return sealCurrentConfigurationSecrets(ctx, data, properties, configurationType, newID, true)
}

// SealCurrentConfigurationSecrets does what ExtractCurrentConfigurationSecrets
// does, but it keeps an unknown top-level field instead of refusing it.
//
// The compatibility write route accepts free-form data today. A refusal there
// turns a request that works into a 400. This variant removes the plaintext
// and changes nothing else.
func SealCurrentConfigurationSecrets(
	ctx context.Context,
	data map[string]any,
	properties map[string]any,
	configurationType string,
	newID CurrentSecretIDGenerator,
) (map[string]any, []HiddenSecretMutation, error) {
	return sealCurrentConfigurationSecrets(ctx, data, properties, configurationType, newID, false)
}

func sealCurrentConfigurationSecrets(
	ctx context.Context,
	data map[string]any,
	properties map[string]any,
	configurationType string,
	newID CurrentSecretIDGenerator,
	rejectUnknown bool,
) (map[string]any, []HiddenSecretMutation, error) {
	if ctx == nil {
		return nil, nil, ErrInvalidCurrentConfigurationSecrets
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if data == nil {
		return nil, nil, nil
	}

	extractor := currentSecretExtractor{
		configurationType: configurationType,
		newID:             newID,
		existingIDs:       make(map[string]struct{}),
		generatedIDs:      make(map[string]struct{}),
	}
	sanitized, err := extractor.object(ctx, data, properties, "", nil, rejectUnknown)
	if err != nil {
		return nil, nil, err
	}
	return sanitized, extractor.mutations, nil
}

type currentSecretExtractor struct {
	configurationType string
	newID             CurrentSecretIDGenerator
	existingIDs       map[string]struct{}
	generatedIDs      map[string]struct{}
	mutations         []HiddenSecretMutation
}

func (e *currentSecretExtractor) object(
	ctx context.Context,
	data map[string]any,
	properties map[string]any,
	parentPath string,
	parentSegments []string,
	rejectUnknown bool,
) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if rejectUnknown {
		for _, key := range keys {
			if _, known := properties[key]; !known {
				path := currentSecretFieldPath(parentPath, key)
				return nil, e.fieldError(path, fmt.Sprintf("is not valid for configuration type %q", e.configurationType))
			}
		}
	}

	sanitized := make(map[string]any, len(data))
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		path := currentSecretFieldPath(parentPath, key)
		pathSegments := appendCurrentSecretFieldSegment(parentSegments, key)
		rawSchema, known := properties[key]
		if !known {
			sanitized[key] = cloneCurrentJSONValue(data[key])
			continue
		}

		fieldSchema, ok := rawSchema.(map[string]any)
		if !ok {
			sanitized[key] = cloneCurrentJSONValue(data[key])
			continue
		}
		value, err := e.value(ctx, data[key], fieldSchema, path, pathSegments)
		if err != nil {
			return nil, err
		}
		sanitized[key] = value
	}
	return sanitized, nil
}

func (e *currentSecretExtractor) value(
	ctx context.Context,
	value any,
	schema map[string]any,
	path string,
	pathSegments []string,
) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if currentSchemaIsPassword(schema) {
		return e.password(ctx, value, path, pathSegments)
	}

	object, isObject := value.(map[string]any)
	if !isObject {
		return cloneCurrentJSONValue(value), nil
	}
	properties, hasProperties := currentSchemaObjectProperties(schema)
	if !hasProperties {
		return cloneCurrentJSONValue(value), nil
	}
	return e.object(ctx, object, properties, path, pathSegments, false)
}

func (e *currentSecretExtractor) password(
	ctx context.Context,
	value any,
	path string,
	pathSegments []string,
) (any, error) {
	if value == nil {
		return nil, nil
	}
	plaintext, ok := value.(string)
	if !ok {
		return nil, e.fieldError(path, "password value must be a string or null")
	}
	if plaintext == "" {
		return "", nil
	}
	if name, ok := currentSecretReferenceName(plaintext); ok {
		if _, generated := e.generatedIDs[name]; generated {
			return nil, e.fieldError(path, "hidden-secret identifier is duplicated")
		}
		e.existingIDs[name] = struct{}{}
		return plaintext, nil
	}
	if e.newID == nil {
		return nil, e.fieldError(path, "hidden-secret identifier generator is required")
	}

	name, err := e.newID()
	if err != nil {
		return nil, e.fieldError(path, "could not generate a hidden-secret identifier")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !currentSecretIDValid(name) {
		return nil, e.fieldError(path, "generated hidden-secret identifier is invalid")
	}
	if _, exists := e.existingIDs[name]; exists {
		return nil, e.fieldError(path, "hidden-secret identifier is duplicated")
	}
	if _, exists := e.generatedIDs[name]; exists {
		return nil, e.fieldError(path, "hidden-secret identifier is duplicated")
	}

	e.generatedIDs[name] = struct{}{}
	e.mutations = append(e.mutations, HiddenSecretMutation{
		Field: path,
		Path:  append([]string(nil), pathSegments...),
		Name:  name,
		Value: plaintext,
	})
	return "{{secret." + name + "}}", nil
}

func (e *currentSecretExtractor) fieldError(field, reason string) error {
	return &CurrentSecretFieldError{Field: field, reason: reason}
}

func currentSchemaIsPassword(schema map[string]any) bool {
	if format, _ := schema["format"].(string); format == "password" {
		return true
	}
	for _, option := range currentSchemaAnyOf(schema) {
		if currentSchemaIsPassword(option) {
			return true
		}
	}
	return false
}

func currentSchemaObjectProperties(schema map[string]any) (map[string]any, bool) {
	if properties, ok := schema["properties"].(map[string]any); ok {
		return properties, true
	}
	for _, option := range currentSchemaAnyOf(schema) {
		if properties, ok := currentSchemaObjectProperties(option); ok {
			return properties, true
		}
	}
	return nil, false
}

func currentSchemaAnyOf(schema map[string]any) []map[string]any {
	switch rawOptions := schema["anyOf"].(type) {
	case []any:
		options := make([]map[string]any, 0, len(rawOptions))
		for _, rawOption := range rawOptions {
			if option, ok := rawOption.(map[string]any); ok {
				options = append(options, option)
			}
		}
		return options
	case []map[string]any:
		return rawOptions
	default:
		return nil
	}
}

func currentSecretReferenceName(value string) (string, bool) {
	const (
		prefix = "{{secret."
		suffix = "}}"
	)
	if !strings.HasPrefix(value, prefix) || !strings.HasSuffix(value, suffix) {
		return "", false
	}
	name := value[len(prefix) : len(value)-len(suffix)]
	if name == "" {
		return "", false
	}
	for index := 0; index < len(name); index++ {
		character := name[index]
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '_' {
			continue
		}
		return "", false
	}
	return name, true
}

func currentSecretIDValid(value string) bool {
	if len(value) != 32 {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func currentSecretFieldPath(parent, field string) string {
	if parent == "" {
		return field
	}
	return parent + "." + field
}

func appendCurrentSecretFieldSegment(parent []string, field string) []string {
	path := make([]string, len(parent)+1)
	copy(path, parent)
	path[len(parent)] = field
	return path
}

// IsCurrentSecretReference reports whether a stored value is a
// `{{secret.NAME}}` reference rather than a literal.
//
// Exported for the admin global provider surface, which lists rows in a schema
// EVERY project can read. Sealing makes the reference the redaction — a write
// through this platform never leaves an api_key in the row — but a row imported
// from a legacy deployment predates that guarantee, and an admin listing is
// exactly where such a value would be handed to a browser. The listing asks this
// so it can redact what is not already a reference, rather than trusting that
// every row it reads was written by a path that seals.
func IsCurrentSecretReference(value string) bool {
	_, ok := currentSecretReferenceName(value)
	return ok
}

// CurrentHiddenSecretReferences returns the hidden-secret names a stored
// configuration or toolkit value references, sorted and without duplicates.
//
// Only a WHOLE string of the exact `{{secret.NAME}}` shape whose NAME has the
// 32-hex shape a seal mints is counted. That is the only form the seal path
// writes, so it is the only form whose hidden secret a row can own. A
// user-named secret (any other NAME) belongs to the project's secret list, not
// to the row, and is never reported here.
func CurrentHiddenSecretReferences(value any) []string {
	seen := map[string]struct{}{}
	collectCurrentHiddenSecretReferences(value, seen)
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func collectCurrentHiddenSecretReferences(value any, seen map[string]struct{}) {
	switch typed := value.(type) {
	case string:
		if name, ok := currentSecretReferenceName(typed); ok && currentSecretIDValid(name) {
			seen[name] = struct{}{}
		}
	case map[string]any:
		for _, nested := range typed {
			collectCurrentHiddenSecretReferences(nested, seen)
		}
	case []any:
		for _, nested := range typed {
			collectCurrentHiddenSecretReferences(nested, seen)
		}
	}
}
