package configurations

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"unicode/utf8"

	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

const MaxRegistryIdentifierBytes = 128

var (
	ErrEmptyRegistrySnapshot     = errors.New("empty configuration registry snapshot")
	ErrInvalidRegistryIdentifier = errors.New("invalid configuration registry identifier")
	ErrDuplicateRegistryType     = errors.New("duplicate configuration registry type")
	ErrInvalidRegistrySchema     = errors.New("invalid configuration registry schema")
)

// RegistryEntryDefinition is the declarative input used to construct a
// registry snapshot. JSONSchema is copied and canonicalized by
// NewRegistrySnapshot.
type RegistryEntryDefinition struct {
	Type                     string
	Section                  string
	JSONSchema               []byte
	ValidationSupported      bool
	ConnectionCheckSupported bool
}

// RegistryEntry is an immutable entry admitted into a registry snapshot.
// CanonicalSchema returns a defensive copy of the schema bytes.
type RegistryEntry struct {
	typeName                 string
	section                  string
	canonicalSchema          []byte
	schemaDigest             runtimedomain.Digest
	validationSupported      bool
	connectionCheckSupported bool
}

func (e RegistryEntry) Type() string {
	return e.typeName
}

func (e RegistryEntry) Section() string {
	return e.section
}

func (e RegistryEntry) CanonicalSchema() []byte {
	return bytes.Clone(e.canonicalSchema)
}

func (e RegistryEntry) SchemaDigest() runtimedomain.Digest {
	return e.schemaDigest
}

func (e RegistryEntry) ValidationSupported() bool {
	return e.validationSupported
}

func (e RegistryEntry) ConnectionCheckSupported() bool {
	return e.connectionCheckSupported
}

// RegistrySnapshot is an immutable, deterministically ordered view of the
// installed configuration registry.
type RegistrySnapshot struct {
	entries       []RegistryEntry
	catalogDigest runtimedomain.Digest
}

// NewRegistrySnapshot validates and canonicalizes all definitions. A
// configuration type is unique across the registry, matching the current
// registry's type-keyed ownership model.
func NewRegistrySnapshot(definitions []RegistryEntryDefinition) (RegistrySnapshot, error) {
	if len(definitions) == 0 {
		return RegistrySnapshot{}, ErrEmptyRegistrySnapshot
	}

	entries := make([]RegistryEntry, len(definitions))
	seenTypes := make(map[string]struct{}, len(definitions))
	for i, definition := range definitions {
		if !validRegistryIdentifier(definition.Type) || !validRegistryIdentifier(definition.Section) {
			return RegistrySnapshot{}, ErrInvalidRegistryIdentifier
		}
		if _, exists := seenTypes[definition.Type]; exists {
			return RegistrySnapshot{}, ErrDuplicateRegistryType
		}
		seenTypes[definition.Type] = struct{}{}

		canonicalSchema, err := canonicalizeRegistrySchema(definition.JSONSchema)
		if err != nil {
			return RegistrySnapshot{}, ErrInvalidRegistrySchema
		}
		entries[i] = RegistryEntry{
			typeName:                 definition.Type,
			section:                  definition.Section,
			canonicalSchema:          canonicalSchema,
			schemaDigest:             runtimedomain.SHA256(canonicalSchema),
			validationSupported:      definition.ValidationSupported,
			connectionCheckSupported: definition.ConnectionCheckSupported,
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].typeName != entries[j].typeName {
			return entries[i].typeName < entries[j].typeName
		}
		return entries[i].section < entries[j].section
	})

	catalogBytes, err := canonicalRegistryCatalog(entries)
	if err != nil {
		return RegistrySnapshot{}, ErrInvalidRegistrySchema
	}
	return RegistrySnapshot{
		entries:       entries,
		catalogDigest: runtimedomain.SHA256(catalogBytes),
	}, nil
}

func (s RegistrySnapshot) Entries() []RegistryEntry {
	return append([]RegistryEntry(nil), s.entries...)
}

func (s RegistrySnapshot) CatalogDigest() runtimedomain.Digest {
	return s.catalogDigest
}

func validRegistryIdentifier(value string) bool {
	if value == "" || len(value) > MaxRegistryIdentifierBytes || !utf8.ValidString(value) {
		return false
	}
	if value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for i := 1; i < len(value); i++ {
		character := value[i]
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			continue
		}
		if character == '_' || character == '-' || character == '.' {
			continue
		}
		return false
	}
	return true
}

func canonicalizeRegistrySchema(raw []byte) ([]byte, error) {
	if len(raw) == 0 || !utf8.Valid(raw) {
		return nil, ErrInvalidRegistrySchema
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeUniqueJSONValue(decoder)
	if err != nil {
		return nil, ErrInvalidRegistrySchema
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, ErrInvalidRegistrySchema
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, ErrInvalidRegistrySchema
	}

	return marshalDeterministicJSON(value)
}

func decodeUniqueJSONValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}

	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return token, nil
	}

	switch delimiter {
	case '{':
		object := make(map[string]any)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, ErrInvalidRegistrySchema
			}
			if _, exists := object[key]; exists {
				return nil, ErrInvalidRegistrySchema
			}
			value, err := decodeUniqueJSONValue(decoder)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
			return nil, ErrInvalidRegistrySchema
		}
		return object, nil
	case '[':
		array := make([]any, 0)
		for decoder.More() {
			value, err := decodeUniqueJSONValue(decoder)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		if token, err = decoder.Token(); err != nil || token != json.Delim(']') {
			return nil, ErrInvalidRegistrySchema
		}
		return array, nil
	default:
		return nil, ErrInvalidRegistrySchema
	}
}

type registryCatalogDocument struct {
	Entries []registryCatalogEntry `json:"entries"`
}

type registryCatalogEntry struct {
	ConnectionCheckSupported bool            `json:"connection_check_supported"`
	Schema                   json.RawMessage `json:"schema"`
	Section                  string          `json:"section"`
	Type                     string          `json:"type"`
	ValidationSupported      bool            `json:"validation_supported"`
}

func canonicalRegistryCatalog(entries []RegistryEntry) ([]byte, error) {
	document := registryCatalogDocument{Entries: make([]registryCatalogEntry, len(entries))}
	for i, entry := range entries {
		document.Entries[i] = registryCatalogEntry{
			ConnectionCheckSupported: entry.connectionCheckSupported,
			Schema:                   entry.canonicalSchema,
			Section:                  entry.section,
			Type:                     entry.typeName,
			ValidationSupported:      entry.validationSupported,
		}
	}
	return marshalDeterministicJSON(document)
}

func marshalDeterministicJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}), nil
}
