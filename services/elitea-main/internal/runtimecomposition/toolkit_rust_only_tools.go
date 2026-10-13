package runtimecomposition

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"sort"
	"sync"
)

// What the Rust worker serves that no SDK toolkit declares (ADR-0032):
//
//   - "types": tools of an SDK type that the SDK does not have (P3: the 19
//     authoring tools of the `pptx` family beside the SDK's fill_template and
//     translate_presentation);
//   - "new_types": whole toolkit types the SDK does not have (P4: `docx` and
//     `xlsx`), each with its catalogue metadata, its settings form and the
//     argument schema of every tool.
//
// The SDK snapshots are generated from the pinned SDK registry
// (scripts/contract/sync_toolkit_schema_snapshot.py) and a regeneration
// replaces them, so none of this can live in them. It is a second pinned
// document, merged into the argument-schema snapshot AND the catalogue snapshot
// when they are loaded; the toolkit editor's tool list is the keys of a type's
// argument schemas, so a merged tool appears in the picker and the test pane,
// and a new type appears in the type chooser. A Python deployment serves none
// of it: a tool of "types" is marked unavailable, and a type of "new_types" is
// withheld whole (SupportsToolkitType).
//
// The document is checked against the Rust families, which own the schemas:
// agent-runtime's xlsx_tests.rs fails if this file differs from what they
// serve (it writes the file when ELITEA_WRITE_RUST_ONLY_TOOLS=1), and
// TestRustOnlyToolsDoNotShadowSDKTools fails if the SDK ever declares one of
// these names or types.
//
//go:embed rust_only_toolkit_tools.json
var pinnedRustOnlyToolkitToolsJSON []byte

const rustOnlyToolkitToolsVersion = "elitea.rust-only-toolkit-tools.v1"

type rustOnlyToolkitToolsDocument struct {
	SchemaVersion string                               `json:"schema_version"`
	Types         map[string]map[string]map[string]any `json:"types"`
	NewTypes      map[string]rustOnlyNewType           `json:"new_types"`
}

// rustOnlyNewType is a toolkit type the SDK does not have: the same three
// things the SDK snapshots carry for one.
type rustOnlyNewType struct {
	Metadata    map[string]any            `json:"metadata"`
	Settings    map[string]any            `json:"settings"`
	ArgsSchemas map[string]map[string]any `json:"args_schemas"`
}

var errRustOnlyToolkitTools = errors.New("rust-only toolkit tools document is invalid")

func decodeRustOnlyToolkitTools(data []byte) (rustOnlyToolkitToolsDocument, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var document rustOnlyToolkitToolsDocument
	if err := decoder.Decode(&document); err != nil ||
		document.SchemaVersion != rustOnlyToolkitToolsVersion {
		return document, errRustOnlyToolkitTools
	}
	return document, nil
}

// applyRustOnlyToolkitTools merges the document into the argument-schema
// entries: its tools into the SDK types they extend, and its new types as
// entries of their own, kept sorted by type.
func applyRustOnlyToolkitTools(
	entries []currentToolkitSchemaSnapshotJSONEntry,
	data []byte,
) ([]currentToolkitSchemaSnapshotJSONEntry, error) {
	document, err := decodeRustOnlyToolkitTools(data)
	if err != nil {
		return nil, err
	}
	for toolkitType, tools := range document.Types {
		applied := false
		for index := range entries {
			if entries[index].Type != toolkitType {
				continue
			}
			if entries[index].ArgsSchemas == nil {
				return nil, errRustOnlyToolkitTools
			}
			for name, schema := range tools {
				if _, shadows := entries[index].ArgsSchemas[name]; shadows || schema == nil {
					return nil, errRustOnlyToolkitTools
				}
				entries[index].ArgsSchemas[name] = schema
			}
			applied = true
		}
		if !applied {
			return nil, errRustOnlyToolkitTools
		}
	}
	for toolkitType, entry := range document.NewTypes {
		if err := validateRustOnlyNewType(toolkitType, entry); err != nil {
			return nil, err
		}
		for index := range entries {
			if entries[index].Type == toolkitType {
				// The SDK declares it now: the SDK's names and schemas are the
				// contract, and this overlay must be reconciled with them.
				return nil, errRustOnlyToolkitTools
			}
		}
		zero := 0
		entries = append(entries, currentToolkitSchemaSnapshotJSONEntry{
			Type:        toolkitType,
			Properties:  map[string]any{},
			ArgsSchemas: entry.ArgsSchemas,
			Naming:      &currentToolkitSchemaSnapshotName{MaxLength: &zero},
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Type < entries[j].Type })
	return entries, nil
}

// applyRustOnlyToolkitCatalogue adds the new types to the catalogue entries,
// kept sorted by type.
func applyRustOnlyToolkitCatalogue(
	entries []currentToolkitCatalogueSnapshotJSONEntry,
	data []byte,
) ([]currentToolkitCatalogueSnapshotJSONEntry, error) {
	document, err := decodeRustOnlyToolkitTools(data)
	if err != nil {
		return nil, err
	}
	for toolkitType, entry := range document.NewTypes {
		if err := validateRustOnlyNewType(toolkitType, entry); err != nil {
			return nil, err
		}
		for index := range entries {
			if entries[index].Type == toolkitType {
				return nil, errRustOnlyToolkitTools
			}
		}
		entries = append(entries, currentToolkitCatalogueSnapshotJSONEntry{
			Type:     toolkitType,
			Metadata: entry.Metadata,
			Settings: entry.Settings,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Type < entries[j].Type })
	return entries, nil
}

func validateRustOnlyNewType(toolkitType string, entry rustOnlyNewType) error {
	if !validCurrentToolkitSchemaIdentifier(toolkitType) ||
		entry.Metadata == nil || entry.Settings == nil || len(entry.ArgsSchemas) == 0 {
		return errRustOnlyToolkitTools
	}
	for _, schema := range entry.ArgsSchemas {
		if schema == nil {
			return errRustOnlyToolkitTools
		}
	}
	return nil
}

var (
	rustOnlyNamesOnce sync.Once
	rustOnlyNames     map[string]map[string]struct{}
	rustOnlyTypes     map[string]struct{}
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
		rustOnlyTypes = map[string]struct{}{}
		for toolkitType := range document.NewTypes {
			rustOnlyTypes[toolkitType] = struct{}{}
		}
	})
	return rustOnlyNames
}

// rustOnlyToolkitTypeNames is the set of toolkit types only the Rust worker
// serves: what a Python deployment must withhold whole.
func rustOnlyToolkitTypeNames() map[string]struct{} {
	rustOnlyToolkitToolNames()
	return rustOnlyTypes
}
