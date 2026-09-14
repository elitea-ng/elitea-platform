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

func TestRejectAmbiguousToolkitNamesDropsEveryConflictingTarget(t *testing.T) {
	tools := []Tool{
		{Name: "Shared_get", toolkitID: 11, toolkitToolName: "get"},
		{Name: "Other_list", toolkitID: 12, toolkitToolName: "list"},
		{Name: "Shared_get", toolkitID: 13, toolkitToolName: "get"},
	}

	got := rejectAmbiguousToolkitNames(tools)
	if len(got) != 1 || got[0].Name != "Other_list" {
		t.Fatalf("tools = %+v, want only the unambiguous target", got)
	}
}

func TestRejectAmbiguousToolkitNamesCollapsesAnExactDuplicate(t *testing.T) {
	tools := []Tool{
		{Name: "Repo_get", toolkitID: 11, toolkitToolName: "get"},
		{Name: "Repo_get", toolkitID: 11, toolkitToolName: "get"},
	}

	got := rejectAmbiguousToolkitNames(tools)
	if len(got) != 1 || got[0].toolkitID != 11 || got[0].toolkitToolName != "get" {
		t.Fatalf("tools = %+v, want one exact target", got)
	}
}

func TestRejectAmbiguousToolkitNamesRejectsSanitisedOperationsInOneToolkit(t *testing.T) {
	tools := []Tool{
		{Name: "Repo_get_issue", toolkitID: 11, toolkitToolName: "get issue"},
		{Name: "Repo_get_issue", toolkitID: 11, toolkitToolName: "get/issue"},
	}

	if got := rejectAmbiguousToolkitNames(tools); len(got) != 0 {
		t.Fatalf("tools = %+v, want the ambiguous sanitised name omitted", got)
	}
}
