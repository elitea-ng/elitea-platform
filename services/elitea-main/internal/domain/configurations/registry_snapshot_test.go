package configurations

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

func TestRegistrySnapshotCanonicalizesAndOrdersEntries(t *testing.T) {
	sourceSchema := []byte(`{
		"type": "object",
		"properties": {
			"token": {"type": "string"},
			"url": {"type": "string"}
		}
	}`)
	snapshot, err := NewRegistrySnapshot([]RegistryEntryDefinition{
		{
			Type:                     "vector_store",
			Section:                  "vectorstorage",
			JSONSchema:               []byte(`{"type":"object"}`),
			ValidationSupported:      true,
			ConnectionCheckSupported: true,
		},
		{
			Type:                "source_control",
			Section:             "credentials",
			JSONSchema:          sourceSchema,
			ValidationSupported: true,
		},
	})
	if err != nil {
		t.Fatalf("NewRegistrySnapshot() error = %v", err)
	}

	entries := snapshot.Entries()
	if len(entries) != 2 {
		t.Fatalf("len(Entries()) = %d, want 2", len(entries))
	}
	if entries[0].Type() != "source_control" || entries[1].Type() != "vector_store" {
		t.Fatalf("entry order = [%q, %q], want [source_control, vector_store]", entries[0].Type(), entries[1].Type())
	}

	wantSchema := []byte(`{"properties":{"token":{"type":"string"},"url":{"type":"string"}},"type":"object"}`)
	if got := entries[0].CanonicalSchema(); !bytes.Equal(got, wantSchema) {
		t.Fatalf("CanonicalSchema() = %s, want %s", got, wantSchema)
	}
	if got, want := entries[0].SchemaDigest(), runtimedomain.SHA256(wantSchema); got != want {
		t.Fatalf("SchemaDigest() = %s, want %s", got, want)
	}
	if !entries[0].ValidationSupported() || entries[0].ConnectionCheckSupported() {
		t.Fatalf("source capabilities = validation:%t check:%t, want true/false", entries[0].ValidationSupported(), entries[0].ConnectionCheckSupported())
	}
	if !entries[1].ValidationSupported() || !entries[1].ConnectionCheckSupported() {
		t.Fatalf("vector capabilities = validation:%t check:%t, want true/true", entries[1].ValidationSupported(), entries[1].ConnectionCheckSupported())
	}
}

func TestRegistrySnapshotDigestIsDeterministic(t *testing.T) {
	first, err := NewRegistrySnapshot([]RegistryEntryDefinition{
		{Type: "vector_store", Section: "vectorstorage", JSONSchema: []byte(`{"type":"object"}`), ValidationSupported: true},
		{Type: "source_control", Section: "credentials", JSONSchema: []byte(`{"required":["token"],"type":"object"}`), ValidationSupported: true},
	})
	if err != nil {
		t.Fatalf("first NewRegistrySnapshot() error = %v", err)
	}
	second, err := NewRegistrySnapshot([]RegistryEntryDefinition{
		{Type: "source_control", Section: "credentials", JSONSchema: []byte(`{ "type": "object", "required": ["token"] }`), ValidationSupported: true},
		{Type: "vector_store", Section: "vectorstorage", JSONSchema: []byte("{\n\t\"type\": \"object\"\n}"), ValidationSupported: true},
	})
	if err != nil {
		t.Fatalf("second NewRegistrySnapshot() error = %v", err)
	}
	if first.CatalogDigest() != second.CatalogDigest() {
		t.Fatalf("equivalent snapshots have different digests: %s != %s", first.CatalogDigest(), second.CatalogDigest())
	}

	changed, err := NewRegistrySnapshot([]RegistryEntryDefinition{
		{Type: "source_control", Section: "credentials", JSONSchema: []byte(`{"type":"object","required":["token"]}`), ValidationSupported: true, ConnectionCheckSupported: true},
		{Type: "vector_store", Section: "vectorstorage", JSONSchema: []byte(`{"type":"object"}`), ValidationSupported: true},
	})
	if err != nil {
		t.Fatalf("changed NewRegistrySnapshot() error = %v", err)
	}
	if first.CatalogDigest() == changed.CatalogDigest() {
		t.Fatal("capability change did not change catalog digest")
	}
}

func TestRegistrySnapshotRejectsInvalidDefinitions(t *testing.T) {
	valid := RegistryEntryDefinition{
		Type:                "source_control",
		Section:             "credentials",
		JSONSchema:          []byte(`{"type":"object"}`),
		ValidationSupported: true,
	}

	tests := []struct {
		name        string
		definitions []RegistryEntryDefinition
		wantErr     error
	}{
		{name: "empty snapshot", wantErr: ErrEmptyRegistrySnapshot},
		{name: "empty type", definitions: []RegistryEntryDefinition{{Section: valid.Section, JSONSchema: valid.JSONSchema}}, wantErr: ErrInvalidRegistryIdentifier},
		{name: "empty section", definitions: []RegistryEntryDefinition{{Type: valid.Type, JSONSchema: valid.JSONSchema}}, wantErr: ErrInvalidRegistryIdentifier},
		{name: "uppercase type", definitions: []RegistryEntryDefinition{{Type: "Source", Section: valid.Section, JSONSchema: valid.JSONSchema}}, wantErr: ErrInvalidRegistryIdentifier},
		{name: "leading digit", definitions: []RegistryEntryDefinition{{Type: "1source", Section: valid.Section, JSONSchema: valid.JSONSchema}}, wantErr: ErrInvalidRegistryIdentifier},
		{name: "path separator", definitions: []RegistryEntryDefinition{{Type: "source/control", Section: valid.Section, JSONSchema: valid.JSONSchema}}, wantErr: ErrInvalidRegistryIdentifier},
		{name: "identifier too long", definitions: []RegistryEntryDefinition{{Type: strings.Repeat("a", MaxRegistryIdentifierBytes+1), Section: valid.Section, JSONSchema: valid.JSONSchema}}, wantErr: ErrInvalidRegistryIdentifier},
		{name: "duplicate type", definitions: []RegistryEntryDefinition{valid, {Type: valid.Type, Section: "other", JSONSchema: valid.JSONSchema}}, wantErr: ErrDuplicateRegistryType},
		{name: "empty schema", definitions: []RegistryEntryDefinition{{Type: valid.Type, Section: valid.Section}}, wantErr: ErrInvalidRegistrySchema},
		{name: "malformed schema", definitions: []RegistryEntryDefinition{{Type: valid.Type, Section: valid.Section, JSONSchema: []byte(`{"type":`)}}, wantErr: ErrInvalidRegistrySchema},
		{name: "multiple values", definitions: []RegistryEntryDefinition{{Type: valid.Type, Section: valid.Section, JSONSchema: []byte(`{} {}`)}}, wantErr: ErrInvalidRegistrySchema},
		{name: "array schema", definitions: []RegistryEntryDefinition{{Type: valid.Type, Section: valid.Section, JSONSchema: []byte(`[]`)}}, wantErr: ErrInvalidRegistrySchema},
		{name: "null schema", definitions: []RegistryEntryDefinition{{Type: valid.Type, Section: valid.Section, JSONSchema: []byte(`null`)}}, wantErr: ErrInvalidRegistrySchema},
		{name: "duplicate schema key", definitions: []RegistryEntryDefinition{{Type: valid.Type, Section: valid.Section, JSONSchema: []byte(`{"type":"object","type":"array"}`)}}, wantErr: ErrInvalidRegistrySchema},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewRegistrySnapshot(test.definitions)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("NewRegistrySnapshot() error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestRegistrySnapshotDoesNotAliasMutableInputOrOutput(t *testing.T) {
	schema := []byte(`{"type":"object"}`)
	snapshot, err := NewRegistrySnapshot([]RegistryEntryDefinition{{
		Type:                "source_control",
		Section:             "credentials",
		JSONSchema:          schema,
		ValidationSupported: true,
	}})
	if err != nil {
		t.Fatalf("NewRegistrySnapshot() error = %v", err)
	}
	wantDigest := snapshot.CatalogDigest()

	schema[0] = '['
	entries := snapshot.Entries()
	returnedSchema := entries[0].CanonicalSchema()
	returnedSchema[0] = '['
	entries[0] = RegistryEntry{}

	gotEntries := snapshot.Entries()
	if got := string(gotEntries[0].CanonicalSchema()); got != `{"type":"object"}` {
		t.Fatalf("stored schema changed through an alias: %s", got)
	}
	if snapshot.CatalogDigest() != wantDigest {
		t.Fatal("catalog digest changed through an alias")
	}
}
