// Package outputcontract checks the output contract of an agent version or
// of a pipeline LLM node when it is saved (ADR-0043 decision 2, #1500).
//
// A contract is stored as data with the version, so a run uses the contract
// of the version it runs (it is frozen with it):
//
//   - agent: version meta `output_contract`;
//   - pipeline: the `output_contract` field of an `llm` node in the YAML.
//
// The shape is one of:
//
//	{"type": "json_schema", "schema": {...}}
//	{"type": "regex", "pattern": "..."}
//	{"type": "format", "format": "json" | "markdown" | "csv"}
//
// The Rust worker admits the same contract again before a model is bound and
// enforces it (libs/rust/agent-runtime/src/output_contract.rs). The bounds
// here are the worker's: keep them in step.
package outputcontract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

const (
	// MetaKey is the agent version meta key and the pipeline node field.
	MetaKey = "output_contract"
	// MaxSchemaBytes is the largest encoded JSON Schema.
	MaxSchemaBytes = 32 * 1024
	// MaxSchemaDepth is the deepest nesting of objects and arrays in a schema.
	MaxSchemaDepth = 32
	// MaxPatternBytes is the longest regex pattern.
	MaxPatternBytes = 1024
	// Code is the 422 code of a refused contract.
	Code = "OUTPUT_CONTRACT_INVALID"
	// ViolationCode is the public code of a run whose final answer did not
	// meet its contract after one retry.
	ViolationCode = "OUTPUT_CONTRACT_VIOLATION"
	// ViolationSafeMessage is the registered text the worker sends under
	// MODEL_RESPONSE_INVALID (retryable false) for a violation; Main maps
	// the pair to ViolationCode. It is the worker's
	// OUTPUT_CONTRACT_VIOLATION_MESSAGE (src/protocol/output.rs): keep them
	// equal.
	ViolationSafeMessage = "The answer did not have the format that the agent or pipeline node requires, and one retry did not correct it. Later nodes did not run. Change the instructions or the output contract, or select a different model."
)

// The refusal reasons. A reason names the rule, never the contract's content.
const (
	ReasonShape          = "shape"
	ReasonSchemaTooLarge = "schema_too_large"
	ReasonSchemaInvalid  = "schema_invalid"
	ReasonPatternTooLong = "pattern_too_long"
	ReasonPatternInvalid = "pattern_invalid"
	ReasonFormatUnknown  = "format_unknown"
	// ReasonStructuredOutput is a contract on an LLM node that also sets
	// structured_output: the node's output is then already a JSON object
	// built from the pipeline state, so the two cannot both apply.
	ReasonStructuredOutput = "structured_output"
)

var reasonMessages = map[string]string{
	ReasonShape:            "The output contract must be an object with type json_schema (and schema), regex (and pattern) or format (and format), and no other field.",
	ReasonSchemaTooLarge:   fmt.Sprintf("The output contract JSON Schema exceeds %d KiB or %d levels of nesting. Make it smaller.", MaxSchemaBytes/1024, MaxSchemaDepth),
	ReasonSchemaInvalid:    "The output contract JSON Schema is not a valid JSON Schema, or it refers to a schema outside itself. Use only local references (#/...).",
	ReasonPatternTooLong:   fmt.Sprintf("The output contract regex exceeds %d bytes. Make it shorter.", MaxPatternBytes),
	ReasonPatternInvalid:   "The output contract regex is empty or is not a valid RE2 expression. Back references and look-around are not supported.",
	ReasonFormatUnknown:    "The output contract format must be json, markdown or csv.",
	ReasonStructuredOutput: "An LLM node with structured output cannot also have an output contract. Turn off structured output or remove the contract.",
}

// Error is a refused contract.
type Error struct {
	// Reason is one of the Reason constants.
	Reason string
	// NodeID is the pipeline node of the contract; empty for an agent.
	NodeID string
}

func (e *Error) Error() string {
	message := reasonMessages[e.Reason]
	if e.NodeID != "" {
		return fmt.Sprintf("Node %q: %s", e.NodeID, message)
	}
	return message
}

// Check checks one stored contract. Nil (absent or JSON null) is no contract.
func Check(raw any) error {
	if raw == nil {
		return nil
	}
	object, ok := raw.(map[string]any)
	if !ok {
		return &Error{Reason: ReasonShape}
	}
	kind, _ := object["type"].(string)
	field := map[string]string{"json_schema": "schema", "regex": "pattern", "format": "format"}[kind]
	if field == "" || len(object) != 2 {
		return &Error{Reason: ReasonShape}
	}
	value, present := object[field]
	if !present {
		return &Error{Reason: ReasonShape}
	}
	switch kind {
	case "json_schema":
		return checkSchema(value)
	case "regex":
		pattern, ok := value.(string)
		if !ok {
			return &Error{Reason: ReasonShape}
		}
		return checkPattern(pattern)
	default:
		format, _ := value.(string)
		switch format {
		case "json", "markdown", "csv":
			return nil
		}
		return &Error{Reason: ReasonFormatUnknown}
	}
}

func checkSchema(value any) error {
	schema, ok := value.(map[string]any)
	if !ok {
		return &Error{Reason: ReasonShape}
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		return &Error{Reason: ReasonShape}
	}
	if len(encoded) > MaxSchemaBytes || depth(schema, 1) > MaxSchemaDepth {
		return &Error{Reason: ReasonSchemaTooLarge}
	}
	if refersOutside(schema) {
		return &Error{Reason: ReasonSchemaInvalid}
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return &Error{Reason: ReasonSchemaInvalid}
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(noLoader{})
	const location = "urn:elitea:output-contract"
	if err := compiler.AddResource(location, document); err != nil {
		return &Error{Reason: ReasonSchemaInvalid}
	}
	if _, err := compiler.Compile(location); err != nil {
		return &Error{Reason: ReasonSchemaInvalid}
	}
	return nil
}

// noLoader loads nothing: a contract only refers to its own definitions.
type noLoader struct{}

func (noLoader) Load(string) (any, error) {
	return nil, fmt.Errorf("output contract: external references are not loaded")
}

func checkPattern(pattern string) error {
	if pattern == "" || strings.ContainsRune(pattern, 0) {
		return &Error{Reason: ReasonPatternInvalid}
	}
	if len(pattern) > MaxPatternBytes {
		return &Error{Reason: ReasonPatternTooLong}
	}
	// The worker matches the whole answer: ^(?:pattern)$.
	if _, err := regexp.Compile("^(?:" + pattern + ")$"); err != nil {
		return &Error{Reason: ReasonPatternInvalid}
	}
	return nil
}

func depth(value any, level int) int {
	if level > MaxSchemaDepth {
		return level
	}
	deepest := level
	switch typed := value.(type) {
	case map[string]any:
		for _, child := range typed {
			deepest = max(deepest, depth(child, level+1))
		}
	case []any:
		for _, child := range typed {
			deepest = max(deepest, depth(child, level+1))
		}
	}
	return deepest
}

func refersOutside(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "$ref" || key == "$dynamicRef" || key == "$recursiveRef" {
				target, ok := child.(string)
				if !ok || !strings.HasPrefix(target, "#") {
					return true
				}
			}
			if refersOutside(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if refersOutside(child) {
				return true
			}
		}
	}
	return false
}

// CheckPipeline checks the output contract of every `llm` node of a pipeline
// YAML. A document that does not parse, or has no `nodes` list, is left to
// the worker compiler (as pipelinelimits.Check does).
func CheckPipeline(instructions string) error {
	if !strings.Contains(instructions, MetaKey) {
		return nil
	}
	var document struct {
		Nodes []map[string]any `yaml:"nodes"`
	}
	if yaml.Unmarshal([]byte(instructions), &document) != nil {
		return nil
	}
	for _, node := range document.Nodes {
		raw, present := node[MetaKey]
		if !present || raw == nil {
			continue
		}
		id := fmt.Sprint(node["id"])
		if structured, _ := node["structured_output"].(bool); structured {
			return &Error{Reason: ReasonStructuredOutput, NodeID: id}
		}
		// A YAML mapping decodes with string keys here, so the JSON check
		// applies unchanged once nested maps are normalised.
		if err := Check(normalise(raw)); err != nil {
			if refusal, ok := err.(*Error); ok {
				refusal.NodeID = id
			}
			return err
		}
	}
	return nil
}

// HasContract reports whether a pipeline YAML or an agent version meta holds
// any contract. Main uses it to mark the command (WorkerCommandV1 field 70).
func HasContract(meta map[string]any, instructions string) bool {
	if meta != nil && meta[MetaKey] != nil {
		return true
	}
	if !strings.Contains(instructions, MetaKey) {
		return false
	}
	var document struct {
		Nodes []map[string]any `yaml:"nodes"`
	}
	if yaml.Unmarshal([]byte(instructions), &document) != nil {
		return false
	}
	for _, node := range document.Nodes {
		if node[MetaKey] != nil {
			return true
		}
	}
	return false
}

// normalise turns the map[any]any and integer values a YAML decoder may
// produce into the JSON shapes Check reads.
func normalise(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			out[key] = normalise(child)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			out[fmt.Sprint(key)] = normalise(child)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, child := range typed {
			out[i] = normalise(child)
		}
		return out
	case int:
		return float64(typed)
	default:
		return value
	}
}
