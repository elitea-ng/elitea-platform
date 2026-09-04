package mcp

import (
	"errors"
	"testing"
)

type errorToolkitArgumentSchemas struct {
	err error
}

func (s errorToolkitArgumentSchemas) ToolkitArgumentSchemas(
	string,
) (map[string]map[string]any, bool, error) {
	return nil, false, s.err
}

func TestToolkitSchemasAreOptionalForProtocolOnlyComposition(t *testing.T) {
	source := postgresToolSource{handler: &Handler{}}
	schemas, found, err := source.toolkitSchemas("github")
	if err != nil || found || schemas != nil {
		t.Fatalf("schemas = %v, found = %t, err = %v; want nil, false, nil", schemas, found, err)
	}
}

func TestToolkitSchemasPropagateSnapshotFailure(t *testing.T) {
	want := errors.New("snapshot invalid")
	source := postgresToolSource{handler: &Handler{
		toolkitArgumentSchemas: errorToolkitArgumentSchemas{err: want},
	}}
	if _, _, err := source.toolkitSchemas("github"); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestUnknownToolkitSchemaDoesNotClaimNoArguments(t *testing.T) {
	schema := unknownToolkitToolSchema()
	if schema["type"] != "object" || schema["additionalProperties"] != true {
		t.Fatalf("schema = %v, want an open object", schema)
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok || len(properties) != 0 {
		t.Fatalf("properties = %v, want an empty map", schema["properties"])
	}
}
