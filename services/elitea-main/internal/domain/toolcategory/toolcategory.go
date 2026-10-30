// Package toolcategory is the ADR-0041 tool category (decision 5): what a
// tool does, for the approval policy. There is one definition of the
// category set (Category) and one declaration of the built-in tools'
// categories: the shared contract libs/proto/contracts/tool-categories-v1.json.
// The Rust worker compiles that file in (agent-runtime tool_category.rs);
// this package embeds a copy that TestContractCopyIsCurrent holds equal to
// it, because a Go module cannot embed a file outside its own directory.
//
// A tool that the contract does not list is `write`. That is safe (a write
// needs an approval wherever a write does) but noisy, so a toolkit port
// declares its tools in the contract.
package toolcategory

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

// Category is an ADR-0041 tool category.
type Category string

const (
	Read          Category = "read"
	Write         Category = "write"
	Delete        Category = "delete"
	ExternalSend  Category = "external_send"
	Payment       Category = "payment"
	CodeExecution Category = "code_execution"
)

// All lists the categories in the ADR's order.
var All = []Category{Read, Write, Delete, ExternalSend, Payment, CodeExecution}

// Default is the category of a tool that declares none.
const Default = Write

// Valid reports whether c is a known category.
func (c Category) Valid() bool {
	for _, known := range All {
		if c == known {
			return true
		}
	}
	return false
}

// Strings lists the category names in the ADR's order.
func Strings() []string {
	out := make([]string, 0, len(All))
	for _, category := range All {
		out = append(out, string(category))
	}
	return out
}

//go:embed tool-categories-v1.json
var contractJSON []byte

// ContractJSON is the embedded copy of the shared contract.
func ContractJSON() []byte { return append([]byte(nil), contractJSON...) }

type contract struct {
	Contract        string                         `json:"contract"`
	Categories      []Category                     `json:"categories"`
	DefaultCategory Category                       `json:"default_category"`
	Toolkits        map[string]map[string]Category `json:"toolkits"`
}

// declared maps a canonical toolkit type to its tools' categories, by
// canonical tool name.
var declared = mustLoad(contractJSON)

func mustLoad(raw []byte) map[string]map[string]Category {
	declarations, err := load(raw)
	if err != nil {
		panic(err)
	}
	return declarations
}

func load(raw []byte) (map[string]map[string]Category, error) {
	var document contract
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("toolcategory: the contract does not decode: %w", err)
	}
	if document.Contract != "elitea.tool-categories.v1" || document.DefaultCategory != Default {
		return nil, fmt.Errorf("toolcategory: the contract is %q with default %q", document.Contract, document.DefaultCategory)
	}
	out := make(map[string]map[string]Category, len(document.Toolkits))
	for toolkit, tools := range document.Toolkits {
		key := CanonicalKey(toolkit)
		if key == "" || out[key] != nil {
			return nil, fmt.Errorf("toolcategory: toolkit %q is empty or listed twice", toolkit)
		}
		entries := make(map[string]Category, len(tools))
		for tool, category := range tools {
			name := CanonicalKey(tool)
			if name == "" || !category.Valid() {
				return nil, fmt.Errorf("toolcategory: %s.%s has category %q", toolkit, tool, category)
			}
			if _, twice := entries[name]; twice {
				return nil, fmt.Errorf("toolcategory: %s.%s is listed twice", toolkit, tool)
			}
			entries[name] = category
		}
		out[key] = entries
	}
	return out, nil
}

// CanonicalKey is the guardrails match key: lower case, letters and digits
// only. Toolkit types and tool names are compared on it, as the sensitive
// tool rules are.
func CanonicalKey(value string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

// storedTypes maps a stored toolkit type to the type the contract lists.
var storedTypes = map[string]string{"k8s": "kubernetes"}

// Of answers the declared category of a built-in tool, or Default. A tool
// name with a routing prefix (`toolkit___tool`, `server:tool`) is matched
// as the worker matches sensitive tools: the whole name first, then the
// name with each prefix removed.
func Of(toolkitType, toolName string) Category {
	category, _ := lookup(toolkitType, toolName)
	return category
}

// Declared answers whether the contract lists the tool.
func Declared(toolkitType, toolName string) bool {
	_, ok := lookup(toolkitType, toolName)
	return ok
}

func lookup(toolkitType, toolName string) (Category, bool) {
	key := CanonicalKey(toolkitType)
	if alias, ok := storedTypes[key]; ok {
		key = alias
	}
	tools := declared[key]
	if tools == nil {
		return Default, false
	}
	current := strings.ToLower(strings.TrimSpace(toolName))
	for current != "" {
		if category, ok := tools[CanonicalKey(current)]; ok {
			return category, true
		}
		reduced := current
		if _, suffix, found := strings.Cut(reduced, "___"); found {
			reduced = strings.TrimSpace(suffix)
		}
		if _, suffix, found := strings.Cut(reduced, ":"); found {
			reduced = strings.TrimSpace(suffix)
		}
		if reduced == "" || reduced == current {
			break
		}
		current = reduced
	}
	return Default, false
}

// Parse answers the category a value names, and whether it names one.
func Parse(value string) (Category, bool) {
	category := Category(strings.TrimSpace(value))
	return category, category.Valid()
}
