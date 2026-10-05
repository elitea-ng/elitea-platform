package pipelinetriggers

// The AGENT branch of the inbound trigger — legacy issue 6656.
//
// Legacy shipped webhooks for ordinary agents as well as for pipelines, and
// then lost them: its webhook endpoint required a PIPELINE trigger setting
// before it reached the agent fallback, so every agent call answered 400. The
// Go stack had only the pipeline half. This file is the other half, built on
// the SAME credential, the SAME signature modes, the SAME replay dedupe and the
// SAME admission path (run.go). Only two things differ, and both are here:
//
//   - WHAT THE AGENT READS. A pipeline runs from its entry node, so an empty
//     input is ordinary for it. An agent answers a message, so it needs text.
//     The text is the body's `input` when the caller wrote one, and otherwise
//     the delivery's own payload: a GitHub push or a GitLab merge request is
//     exactly what an agent wired to a repository is meant to read. A call
//     with neither is refused with 422 naming `input`.
//   - THE AGENT'S VARIABLES. The body's `variables` object re-values the
//     variables the version DECLARES (its `application_variables` rows, or
//     the `meta.variables` mirror for a version with none), the way a chat
//     participant's settings do. A name the version does not declare is
//     ignored: the request may re-value a variable, never declare one. That
//     is the SDK's own rule (`elitea_sdk/runtime/clients/client.py`, quoted
//     in services/elitea-worker-rust/src/agents/variables.rs).
//
// Who the run executes as, the permission it needs, and every refusal are the
// pipeline's, unchanged. See the package doc.

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// maxAgentPayloadInput bounds the provider payload an agent is given as its
// input. A signed delivery may be 1 MiB (maxSignedInboundBody); a model
// context is not the place for all of it. 64 KiB keeps an ordinary push or
// merge-request event whole and stays far below the start use case's own
// 256 KiB ceiling.
const maxAgentPayloadInput = 64 * 1024

// payloadTruncatedMarker ends a payload that was cut at maxAgentPayloadInput,
// so the agent can tell a cut payload from a short one.
const payloadTruncatedMarker = "\n[payload truncated]"

// maxAgentVariables and maxAgentVariableValue bound what a caller may write
// into the run through `variables`.
const (
	maxAgentVariables     = 64
	maxAgentVariableValue = 4 * 1024
)

// agentVariable is one row of the request-level variable list, in the shape
// the runtime reads (`{name, value}`).
type agentVariable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ownBodyKeys are the keys of this route's own body format. A JSON object
// made only of them was written FOR this route, so it is not a provider
// payload to hand to the agent.
var ownBodyKeys = map[string]bool{"input": true, "variables": true}

// agentRunInput is the text an agent run starts with.
//
// The caller's `input` wins. Without one, the raw payload is the input, unless
// the payload is this route's own format with nothing in it: `{}` or
// `{"variables": {...}}` says "run", not "read this".
func agentRunInput(input string, payload []byte) (string, error) {
	if strings.TrimSpace(input) != "" {
		return input, nil
	}
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || isOwnBodyFormat(trimmed) {
		return "", ErrAgentInputRequired
	}
	if !utf8.Valid(trimmed) || bytes.IndexByte(trimmed, 0) >= 0 {
		return "", ErrInvalidInput
	}
	if len(trimmed) <= maxAgentPayloadInput {
		return string(trimmed), nil
	}
	cut := maxAgentPayloadInput
	for cut > 0 && !utf8.RuneStart(trimmed[cut]) {
		cut--
	}
	return string(trimmed[:cut]) + payloadTruncatedMarker, nil
}

// isOwnBodyFormat reports whether the payload is a JSON object whose keys are
// all this route's own.
func isOwnBodyFormat(payload []byte) bool {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(payload, &object); err != nil || object == nil {
		return false
	}
	for key := range object {
		if !ownBodyKeys[key] {
			return false
		}
	}
	return true
}

// declaredVariableNames reads the names out of the declared-variable list
// resolveRunTarget projects: a list of `{name, ...}` rows. Anything else
// declares nothing.
func declaredVariableNames(raw []byte) []string {
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil
	}
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		if name, ok := row["name"].(string); ok && name != "" {
			names = append(names, name)
		}
	}
	return names
}

// agentVariables keeps the caller's values for DECLARED names only, in the
// order the version declares them, so the stored mapping is deterministic.
//
// A string value is used as it is. A number or a boolean is used as its JSON
// text. An object, an array, a null, or a value longer than
// maxAgentVariableValue is ignored: a variable is substituted into the
// instructions as text, and a structure there is a payload, not a value.
func agentVariables(declared []string, supplied map[string]json.RawMessage) []agentVariable {
	if len(declared) == 0 || len(supplied) == 0 {
		return nil
	}
	variables := make([]agentVariable, 0, len(declared))
	for _, name := range declared {
		if len(variables) == maxAgentVariables {
			break
		}
		raw, ok := supplied[name]
		if !ok {
			continue
		}
		value, ok := variableText(raw)
		if !ok {
			continue
		}
		variables = append(variables, agentVariable{Name: name, Value: value})
	}
	return variables
}

func variableText(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "", false
	}
	var value string
	switch trimmed[0] {
	case '"':
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return "", false
		}
	case 't', 'f', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		var scalar any
		if err := json.Unmarshal(trimmed, &scalar); err != nil {
			return "", false
		}
		value = string(trimmed)
	default:
		return "", false
	}
	if len(value) > maxAgentVariableValue || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return "", false
	}
	return value, true
}
