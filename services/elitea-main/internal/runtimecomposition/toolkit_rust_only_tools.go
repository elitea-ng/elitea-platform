package runtimecomposition

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"sync"
)

// The tools the Rust worker serves that no SDK toolkit declares (ADR-0032 P3:
// the 19 authoring tools of the `pptx` family beside the SDK's fill_template
// and translate_presentation).
//
// The SDK snapshot is generated from the pinned SDK registry
// (scripts/contract/sync_toolkit_schema_snapshot.py) and a regeneration
// replaces it, so these tools cannot live in it. They are a second pinned
// document, merged into the matching type's argument schemas when the pinned
// snapshot is loaded; the toolkit editor's tool list is the keys of those
// argument schemas, so a merged tool appears in the picker and the test pane.
//
// The document is checked against the Rust family, which owns the schemas:
// agent-runtime's pptx_tests.rs fails if this file differs from what the
// family serves (it writes the file when ELITEA_WRITE_RUST_ONLY_TOOLS=1), and
// TestRustOnlyToolsDoNotShadowSDKTools fails if the SDK ever declares one of
// these names.
//
//go:embed rust_only_toolkit_tools.json
var pinnedRustOnlyToolkitToolsJSON []byte

const rustOnlyToolkitToolsVersion = "elitea.rust-only-toolkit-tools.v1"

type rustOnlyToolkitToolsDocument struct {
	SchemaVersion string                               `json:"schema_version"`
	Types         map[string]map[string]map[string]any `json:"types"`
}

var errRustOnlyToolkitTools = errors.New("rust-only toolkit tools document is invalid")

func applyRustOnlyToolkitTools(entries []currentToolkitSchemaSnapshotJSONEntry, data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var document rustOnlyToolkitToolsDocument
	if err := decoder.Decode(&document); err != nil ||
		document.SchemaVersion != rustOnlyToolkitToolsVersion {
		return errRustOnlyToolkitTools
	}
	for toolkitType, tools := range document.Types {
		applied := false
		for index := range entries {
			if entries[index].Type != toolkitType {
				continue
			}
			if entries[index].ArgsSchemas == nil {
				return errRustOnlyToolkitTools
			}
			for name, schema := range tools {
				if _, shadows := entries[index].ArgsSchemas[name]; shadows || schema == nil {
					return errRustOnlyToolkitTools
				}
				entries[index].ArgsSchemas[name] = schema
			}
			applied = true
		}
		if !applied {
			return errRustOnlyToolkitTools
		}
	}
	return nil
}

var (
	rustOnlyNamesOnce sync.Once
	rustOnlyNames     map[string]map[string]struct{}
)

// rustOnlyToolkitToolNames is, per toolkit type, the set of tool names only the
// Rust worker serves: what a Python deployment must mark unavailable in the
// catalogue (WorkerToolkitCapability.SupportsTool), through the same
// metadata.unavailable_tools mechanism a partial Rust family uses.
func rustOnlyToolkitToolNames() map[string]map[string]struct{} {
	rustOnlyNamesOnce.Do(func() {
		rustOnlyNames = map[string]map[string]struct{}{}
		var document rustOnlyToolkitToolsDocument
		if json.Unmarshal(pinnedRustOnlyToolkitToolsJSON, &document) != nil {
			return
		}
		for toolkitType, tools := range document.Types {
			names := make(map[string]struct{}, len(tools))
			for name := range tools {
				names[name] = struct{}{}
			}
			rustOnlyNames[toolkitType] = names
		}
	})
	return rustOnlyNames
}
