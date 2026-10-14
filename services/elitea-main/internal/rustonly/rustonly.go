// Package rustonly holds what only the Rust worker serves and no SDK toolkit
// declares (ADR-0032 P3, P4), and the rule for a deployment that has not said
// which worker it runs: those types and tools are withheld, never advertised.
//
// It is a leaf package so the catalogue handler and the capability projection
// apply the SAME rule: the handler cannot import runtimecomposition (which
// imports the handler), so the data and the verdicts live here.
//
// The document is checked against the Rust families, which own the schemas:
// agent-runtime's xlsx_tests.rs fails if this file differs from what they serve
// (ELITEA_WRITE_RUST_ONLY_TOOLS=1 rewrites it).
package rustonly

import (
	_ "embed"
	"encoding/json"
	"sync"
)

// JSON is the pinned document: `types` (tools of an SDK type that the SDK does
// not have) and `new_types` (whole types the SDK does not have).
//
//go:embed rust_only_toolkit_tools.json
var JSON []byte

type document struct {
	Types    map[string]map[string]json.RawMessage `json:"types"`
	NewTypes map[string]json.RawMessage            `json:"new_types"`
}

var (
	once  sync.Once
	tools map[string]map[string]struct{}
	types map[string]struct{}
)

func load() {
	once.Do(func() {
		tools = map[string]map[string]struct{}{}
		types = map[string]struct{}{}
		var d document
		if json.Unmarshal(JSON, &d) != nil {
			return
		}
		for toolkitType, names := range d.Types {
			set := make(map[string]struct{}, len(names))
			for name := range names {
				set[name] = struct{}{}
			}
			tools[toolkitType] = set
		}
		for toolkitType := range d.NewTypes {
			types[toolkitType] = struct{}{}
		}
	})
}

// ToolNames is, per toolkit type, the set of tool names only the Rust worker
// serves.
func ToolNames() map[string]map[string]struct{} {
	load()
	return tools
}

// TypeNames is the set of toolkit types only the Rust worker serves.
func TypeNames() map[string]struct{} {
	load()
	return types
}

// IsType reports whether only the Rust worker serves the type.
func IsType(toolkitType string) bool {
	_, ok := TypeNames()[toolkitType]
	return ok
}

// AbsentTypeVerdict is the answer for a deployment that has not stated its
// worker: the SDK's types are offered as before, the types only the Rust worker
// serves are not (absence of a statement is not a statement that a Rust worker
// is there).
func AbsentTypeVerdict(toolkitType string) (bool, string) {
	if IsType(toolkitType) {
		return false, "This deployment has not stated which agent worker it runs, so the " +
			toolkitType + " toolkit, which only the Rust worker serves, is not offered."
	}
	return true, ""
}

// AbsentToolVerdict is AbsentTypeVerdict for one tool.
func AbsentToolVerdict(toolkitType, toolName string) (bool, string) {
	if _, ok := ToolNames()[toolkitType][toolName]; ok {
		return false, "This deployment has not stated which agent worker it runs, so the " +
			toolName + " tool of the " + toolkitType + " toolkit, which only the Rust worker serves, is not offered."
	}
	return true, ""
}
