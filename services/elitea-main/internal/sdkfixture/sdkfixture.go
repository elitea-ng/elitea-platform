// Package sdkfixture is the contract-test side of adding a type that the
// pinned SDK snapshots do not have (D1 of epic #2254, #2255).
//
// A native definition (a Go credential type, or a Rust-owned toolkit entry) is
// only as good as its fidelity to the upstream SDK, which is the contract the
// existing saved agents and the SDK-written documentation rely on. So each
// native type is checked against a FIXTURE: the SDK's own pydantic
// `model_json_schema()` output, captured ONCE with provenance (repository,
// commit, path, dates) and checked in under testdata/sdk-fixtures/<type>.json.
// Nothing here runs Python: capturing is a one-off human step documented in
// docs/contributing/adding-types.md, and the Go tests only read the JSON.
//
// A fixture comparison is deliberately narrower than "equal": it fails on what
// a client or a saved agent would notice (field names, required, secret flags,
// defaults, enums, scope lists, tool names and argument schemas) and ignores
// prose that a native definition may word better (titles and descriptions of a
// credential's fields).
package sdkfixture

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
)

const (
	// Version is the fixture document's schema version.
	Version = "elitea.sdk-fixture.v1"
	// KindConfiguration is a credential type: ModelSchema is the pydantic data
	// schema of the SDK configuration class.
	KindConfiguration = "configuration"
	// KindToolkit is a toolkit type: ModelSchema is the toolkit's settings
	// schema (`toolkit_config_schema()`), Tools the argument schema of each tool.
	KindToolkit = "toolkit"
)

var (
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	datePattern   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// Upstream says where a fixture came from, so a reviewer can re-capture it.
type Upstream struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	// Commit is the upstream commit the files were read at (40 hex).
	Commit string `json:"commit"`
	// Paths are the upstream files that define the schema.
	Paths []string `json:"paths"`
	// CommittedAt is the commit's date, CapturedAt the day the fixture was made.
	CommittedAt string `json:"committed_at"`
	CapturedAt  string `json:"captured_at"`
	// CapturedWith names the tool and version that produced the schema.
	CapturedWith string `json:"captured_with"`
}

// Fixture is one captured upstream schema.
type Fixture struct {
	Version     string                    `json:"fixture_version"`
	Kind        string                    `json:"kind"`
	Type        string                    `json:"type"`
	Upstream    Upstream                  `json:"upstream"`
	ModelSchema map[string]any            `json:"model_schema"`
	Tools       map[string]map[string]any `json:"tools,omitempty"`
}

// Load reads and validates one fixture file.
func Load(path string) (Fixture, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Fixture{}, err
	}
	return Parse(raw)
}

// Parse validates a fixture document: strict fields, known kind, provenance.
func Parse(raw []byte) (Fixture, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var fixture Fixture
	if err := decoder.Decode(&fixture); err != nil {
		return Fixture{}, fmt.Errorf("sdk fixture: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return Fixture{}, errors.New("sdk fixture: trailing data")
	}
	switch {
	case fixture.Version != Version:
		return Fixture{}, fmt.Errorf("sdk fixture: version %q, want %q", fixture.Version, Version)
	case fixture.Kind != KindConfiguration && fixture.Kind != KindToolkit:
		return Fixture{}, fmt.Errorf("sdk fixture: unknown kind %q", fixture.Kind)
	case fixture.Type == "":
		return Fixture{}, errors.New("sdk fixture: no type")
	case fixture.Upstream.Repo == "" || fixture.Upstream.Branch == "" || len(fixture.Upstream.Paths) == 0 ||
		fixture.Upstream.CapturedWith == "":
		return Fixture{}, errors.New("sdk fixture: provenance is incomplete (repo, branch, paths, captured_with)")
	case !commitPattern.MatchString(fixture.Upstream.Commit):
		return Fixture{}, errors.New("sdk fixture: upstream.commit must be a 40-hex commit")
	case !datePattern.MatchString(fixture.Upstream.CommittedAt) || !datePattern.MatchString(fixture.Upstream.CapturedAt):
		return Fixture{}, errors.New("sdk fixture: upstream dates must be YYYY-MM-DD")
	case fixture.ModelSchema == nil:
		return Fixture{}, errors.New("sdk fixture: no model_schema")
	case fixture.Kind == KindToolkit && len(fixture.Tools) == 0:
		return Fixture{}, errors.New("sdk fixture: a toolkit fixture needs its tools")
	case fixture.Kind == KindConfiguration && len(fixture.Tools) != 0:
		return Fixture{}, errors.New("sdk fixture: a configuration fixture has no tools")
	}
	return fixture, nil
}

// credentialKeys are the keys of a credential field that a client or a saved
// credential depends on. Titles and descriptions are prose and are not compared.
var credentialKeys = []string{
	"type", "anyOf", "format", "default", "enum", "items", "secret", "writeOnly",
	"minimum", "maximum", "minLength", "maxLength",
}

// DiffConfiguration compares a native credential type's data schema (its
// `properties` and `required`) with the fixture. It returns one line per
// difference; empty means the definition honours the fixture.
func (f Fixture) DiffConfiguration(properties map[string]any, required []string) []string {
	if f.Kind != KindConfiguration {
		return []string{"fixture is not a configuration fixture"}
	}
	var diffs []string
	want, _ := f.ModelSchema["properties"].(map[string]any)
	diffs = append(diffs, diffNames("field", keysOf(want), keysOf(properties))...)
	for _, name := range sortedKeys(want) {
		have, ok := properties[name].(map[string]any)
		if !ok {
			continue
		}
		wantField, _ := want[name].(map[string]any)
		for _, key := range credentialKeys {
			if !equalJSON(wantField[key], have[key]) {
				diffs = append(diffs, fmt.Sprintf("field %q: %s is %s, upstream has %s",
					name, key, render(have[key]), render(wantField[key])))
			}
		}
	}
	diffs = append(diffs, diffNames("required field", stringsOf(f.ModelSchema["required"]), required)...)
	return diffs
}

// DiffToolkit compares a toolkit type's settings schema and tool argument
// schemas with the fixture.
//
// Settings are compared field by field in full, except selected_tools, whose
// per-tool schemas are served separately (only its tool-name enum is compared),
// and the top-level `$defs` (a credential field's `$ref` target is resolved at
// serve time). Every tool's argument schema must equal the fixture's.
func (f Fixture) DiffToolkit(settings map[string]any, args map[string]map[string]any) []string {
	if f.Kind != KindToolkit {
		return []string{"fixture is not a toolkit fixture"}
	}
	var diffs []string
	want, _ := f.ModelSchema["properties"].(map[string]any)
	have, _ := settings["properties"].(map[string]any)
	diffs = append(diffs, diffNames("settings field", keysOf(want), keysOf(have))...)
	for _, name := range sortedKeys(want) {
		haveField, ok := have[name]
		if !ok {
			continue
		}
		wantField := want[name]
		if name == "selected_tools" {
			wantField, haveField = selectedToolsShape(wantField), selectedToolsShape(haveField)
		}
		if !equalJSON(wantField, haveField) {
			diffs = append(diffs, fmt.Sprintf("settings field %q is %s, upstream has %s",
				name, render(haveField), render(wantField)))
		}
	}
	diffs = append(diffs, diffNames("required settings field",
		stringsOf(f.ModelSchema["required"]), stringsOf(settings["required"]))...)
	if !equalJSON(f.ModelSchema["title"], settings["title"]) {
		diffs = append(diffs, fmt.Sprintf("settings title is %s, upstream has %s",
			render(settings["title"]), render(f.ModelSchema["title"])))
	}
	wantTools := make([]string, 0, len(f.Tools))
	for name := range f.Tools {
		wantTools = append(wantTools, name)
	}
	haveTools := make([]string, 0, len(args))
	for name := range args {
		haveTools = append(haveTools, name)
	}
	diffs = append(diffs, diffNames("tool", wantTools, haveTools)...)
	for _, name := range sortedKeys(f.Tools) {
		haveSchema, ok := args[name]
		if ok && !equalJSON(f.Tools[name], haveSchema) {
			diffs = append(diffs, fmt.Sprintf("tool %q: argument schema is %s, upstream has %s",
				name, render(haveSchema), render(f.Tools[name])))
		}
	}
	return diffs
}

// selectedToolsShape keeps what identifies the tool list: its tool-name enum,
// default and type. The per-tool schemas and groups are compared elsewhere.
func selectedToolsShape(value any) any {
	field, ok := value.(map[string]any)
	if !ok {
		return value
	}
	shape := map[string]any{}
	for _, key := range []string{"type", "default"} {
		if item, present := field[key]; present {
			shape[key] = item
		}
	}
	if items, ok := field["items"].(map[string]any); ok {
		shape["enum"] = items["enum"]
	}
	return shape
}

func diffNames(kind string, want, have []string) []string {
	var diffs []string
	wantSet, haveSet := toSet(want), toSet(have)
	for _, name := range sortedSet(wantSet) {
		if !haveSet[name] {
			diffs = append(diffs, fmt.Sprintf("%s %q is in upstream and missing here", kind, name))
		}
	}
	for _, name := range sortedSet(haveSet) {
		if !wantSet[name] {
			diffs = append(diffs, fmt.Sprintf("%s %q is here and not in upstream", kind, name))
		}
	}
	return diffs
}

func equalJSON(a, b any) bool {
	left, errLeft := json.Marshal(a)
	right, errRight := json.Marshal(b)
	return errLeft == nil && errRight == nil && bytes.Equal(left, right)
}

func render(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "?"
	}
	return string(encoded)
}

func stringsOf(value any) []string {
	items, _ := value.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	if typed, ok := value.([]string); ok {
		return typed
	}
	return out
}

func keysOf(value map[string]any) []string { return sortedKeys(value) }

func sortedKeys[V any](value map[string]V) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func toSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

func sortedSet(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
