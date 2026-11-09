// Package toolenvelope is the per-agent tool envelope (ADR-0050 decision 3,
// issue 1613): which of an agent's tools a run may call how often, which it
// may use with nobody present, and which a sub-agent may be given.
//
// # The block
//
// It is the `tool_envelope` key of the version's meta, so it is part of the
// version and its release digest like `context` and `memory`:
//
//	tool_envelope:
//	  tool_limits: {"github/create_issue": 3, "jira/*": 10}
//	  unattended_tools: ["github/get_issue", "github/create_comment"]
//	  subagent_tools: ["github/*"]
//
// A tool key is "<toolkit>/<tool>": the toolkit by its name in the agent or
// by its type, and "*" for every tool of the toolkit.
//
// Every control only narrows what the agent already has. None of them adds
// a toolkit or a tool, and none lifts an approval, a guardrail, the policy
// ceiling or a deny:
//
//   - tool_limits lowers the run's call budget for the tools it names;
//   - unattended_tools is the subset of the agent's tools a turn nobody waits
//     on may bind. Absent is the read-only tools. A tool that writes is
//     unattended only when the administrator's allowlist
//     (agents.unattended_tools_allowlist) names it too;
//   - subagent_tools is the subset a sub-agent may be given, writes
//     included. Absent is the read-only tools, the rule before the envelope.
//
// Main freezes the block and the administrator's ceilings into the run input
// (runtimev1.AgentToolEnvelopeV1) at admission; the worker enforces it.
package toolenvelope

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode"
)

// MetaKey is the version meta key of the block.
const MetaKey = "tool_envelope"

// Code is the 422 code of an invalid block.
const Code = "tool_envelope_invalid"

// Bounds of the block.
const (
	// MaxEntries is the most keys of tool_limits, and the most entries of
	// each list.
	MaxEntries = 256
	// MaxKeyBytes is the longest tool key.
	MaxKeyBytes = 512
	// MaxCalls is the highest per-tool limit.
	MaxCalls = 10_000
)

// Wildcard is the tool part that matches every tool of a toolkit.
const Wildcard = "*"

// ErrInvalid is a block that is not valid; the message names the field.
var ErrInvalid = errors.New("invalid tool envelope")

// Limit is one per-tool limit.
type Limit struct {
	Tool     string
	MaxCalls uint32
}

// Envelope is a parsed block. Lists are sorted and without duplicates; a nil
// list is a control the block does not set.
type Envelope struct {
	ToolLimits      []Limit
	UnattendedTools []string
	SubagentTools   []string
}

// Empty reports whether the block sets nothing.
func (e Envelope) Empty() bool {
	return len(e.ToolLimits) == 0 && e.UnattendedTools == nil && e.SubagentTools == nil
}

// Parse reads the block from a decoded meta value. Absent (nil) is the
// empty envelope. An unknown key or a bad value is refused.
func Parse(raw any) (Envelope, error) {
	if raw == nil {
		return Envelope{}, nil
	}
	object, ok := raw.(map[string]any)
	if !ok {
		return Envelope{}, fmt.Errorf("%w: tool_envelope must be an object", ErrInvalid)
	}
	var envelope Envelope
	for key, value := range object {
		var err error
		switch key {
		case "tool_limits":
			envelope.ToolLimits, err = parseLimits(value)
		case "unattended_tools":
			envelope.UnattendedTools, err = ParseToolList(value, "tool_envelope.unattended_tools")
		case "subagent_tools":
			envelope.SubagentTools, err = ParseToolList(value, "tool_envelope.subagent_tools")
		default:
			err = fmt.Errorf("%w: unknown key tool_envelope.%s", ErrInvalid, key)
		}
		if err != nil {
			return Envelope{}, err
		}
	}
	return envelope, nil
}

// Check checks meta.tool_envelope of a version meta. A nil meta and an
// absent key are valid.
func Check(meta map[string]any) error {
	if meta == nil {
		return nil
	}
	_, err := Parse(meta[MetaKey])
	return err
}

func parseLimits(raw any) ([]Limit, error) {
	if raw == nil {
		return nil, nil
	}
	object, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: tool_envelope.tool_limits must be an object", ErrInvalid)
	}
	if len(object) > MaxEntries {
		return nil, fmt.Errorf("%w: tool_envelope.tool_limits has more than %d keys", ErrInvalid, MaxEntries)
	}
	limits := make([]Limit, 0, len(object))
	for key, value := range object {
		if !ValidKey(key) {
			return nil, fmt.Errorf("%w: tool_envelope.tool_limits key %q is not \"<toolkit>/<tool>\"", ErrInvalid, key)
		}
		calls, ok := wholeNumber(value)
		if !ok || calls < 1 || calls > MaxCalls {
			return nil, fmt.Errorf("%w: tool_envelope.tool_limits.%s must be a whole number from 1 to %d",
				ErrInvalid, key, MaxCalls)
		}
		limits = append(limits, Limit{Tool: key, MaxCalls: uint32(calls)})
	}
	slices.SortFunc(limits, func(a, b Limit) int { return strings.Compare(a.Tool, b.Tool) })
	return limits, nil
}

// ParseToolList reads a list of tool keys, sorted and without duplicates.
// field names the list in the error. JSON null is the unset list (nil); an
// empty list is set and allows no tool.
func ParseToolList(raw any, field string) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%w: %s must be a list of tool keys", ErrInvalid, field)
	}
	if len(items) > MaxEntries {
		return nil, fmt.Errorf("%w: %s has more than %d entries", ErrInvalid, field, MaxEntries)
	}
	keys := make([]string, 0, len(items))
	for _, item := range items {
		key, ok := item.(string)
		if !ok || !ValidKey(key) {
			return nil, fmt.Errorf("%w: %s entry %v is not \"<toolkit>/<tool>\"", ErrInvalid, field, item)
		}
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return slices.Compact(keys), nil
}

// ValidKey reports whether key is "<toolkit>/<tool>": both parts non-empty,
// no further slash, no control character, at most MaxKeyBytes. The tool
// part may be Wildcard.
func ValidKey(key string) bool {
	if key == "" || len(key) > MaxKeyBytes || strings.ContainsFunc(key, unicode.IsControl) {
		return false
	}
	toolkit, tool, found := strings.Cut(key, "/")
	return found && strings.TrimSpace(toolkit) != "" && strings.TrimSpace(tool) != "" &&
		!strings.Contains(tool, "/") && (tool == Wildcard || !strings.Contains(tool, Wildcard))
}

// wholeNumber reads a JSON number that is a whole number. The applications
// handler decodes with float64, admission with json.Number.
func wholeNumber(raw any) (int64, bool) {
	switch typed := raw.(type) {
	case float64:
		if math.IsNaN(typed) || typed != math.Trunc(typed) || math.Abs(typed) > math.MaxInt32 {
			return 0, false
		}
		return int64(typed), true
	case interface{ Int64() (int64, error) }:
		value, err := typed.Int64()
		return value, err == nil
	}
	return 0, false
}
