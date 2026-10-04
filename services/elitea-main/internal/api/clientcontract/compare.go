package clientcontract

import (
	"fmt"
	"sort"
	"strings"
)

// Compare reports every way current breaks the promise recorded in locked.
// An empty result means current is the lock or an additive extension of it.
//
// The rules, side by side (ADR-0025 decision 6; API_CONTRACT.md "Client
// contract" repeats them for humans):
//
//	operation   removed, or no longer tagged `client`                 breaking
//	parameter   removed; optional -> required; a new required one     breaking
//	request     body made required; a property removed; a property
//	            made required; a new required property                breaking
//	response    a status, media type, header or property removed;
//	            a required property made optional                      breaking
//	any schema  type or format changed; an enum member removed        breaking
//	nullable    request: nullable -> not; response: not -> nullable   breaking
//
// Everything else — a new operation, an optional parameter or property, a new
// status, a new enum member, a looser request — is additive.
func Compare(locked, current *Surface) []string {
	var out []string
	keys := sortedKeys(locked.Operations)
	for _, key := range keys {
		was := locked.Operations[key]
		now, ok := current.Operations[key]
		if !ok {
			out = append(out, fmt.Sprintf("%s (%s): the operation is gone or no longer tagged `client`", key, was.OperationID))
			continue
		}
		c := &comparer{op: key}
		c.operation(was, now)
		out = append(out, c.problems...)
	}
	return out
}

type comparer struct {
	op       string
	problems []string
}

func (c *comparer) fail(where, format string, args ...any) {
	c.problems = append(c.problems, fmt.Sprintf("%s %s: %s", c.op, where, fmt.Sprintf(format, args...)))
}

// side says which way data flows through a schema.
type side int

const (
	request  side = iota // the client writes it
	response             // the client reads it
)

func (c *comparer) operation(was, now *Operation) {
	for _, key := range sortedKeys(was.Parameters) {
		wasParam := was.Parameters[key]
		nowParam, ok := now.Parameters[key]
		where := "parameter " + key
		if !ok {
			c.fail(where, "removed")
			continue
		}
		if !wasParam.Required && nowParam.Required {
			c.fail(where, "became required")
		}
		c.shape(where, request, wasParam.Schema, nowParam.Schema)
	}
	for _, key := range sortedKeys(now.Parameters) {
		if _, existed := was.Parameters[key]; !existed && now.Parameters[key].Required {
			c.fail("parameter "+key, "is new and required")
		}
	}

	switch {
	case was.RequestBody == nil && now.RequestBody != nil && now.RequestBody.Required:
		c.fail("request body", "is new and required")
	case was.RequestBody != nil && now.RequestBody == nil:
		c.fail("request body", "removed")
	case was.RequestBody != nil:
		if !was.RequestBody.Required && now.RequestBody.Required {
			c.fail("request body", "became required")
		}
		for _, media := range sortedKeys(was.RequestBody.Content) {
			nowShape, ok := now.RequestBody.Content[media]
			where := "request " + media
			if !ok {
				c.fail(where, "media type removed")
				continue
			}
			c.shape(where, request, was.RequestBody.Content[media], nowShape)
		}
	}

	for _, status := range sortedKeys(was.Responses) {
		wasResp := was.Responses[status]
		nowResp, ok := now.Responses[status]
		where := "response " + status
		if !ok {
			c.fail(where, "status removed")
			continue
		}
		for _, media := range sortedKeys(wasResp.Content) {
			nowShape, ok := nowResp.Content[media]
			if !ok {
				c.fail(where+" "+media, "media type removed")
				continue
			}
			c.shape(where+" "+media, response, wasResp.Content[media], nowShape)
		}
		for _, header := range sortedKeys(wasResp.Headers) {
			nowShape, ok := nowResp.Headers[header]
			if !ok {
				c.fail(where+" header "+header, "removed")
				continue
			}
			c.shape(where+" header "+header, response, wasResp.Headers[header], nowShape)
		}
	}
}

func (c *comparer) shape(where string, dir side, was, now *Shape) {
	if was == nil {
		return
	}
	if now == nil {
		c.fail(where, "schema removed")
		return
	}
	if was.Ref != "" || now.Ref != "" {
		// A recursion cut: the referenced schema is compared where it was
		// expanded, higher up the same walk.
		if was.Ref != now.Ref {
			c.fail(where, "recursive reference changed from %q to %q", was.Ref, now.Ref)
		}
		return
	}
	if was.Type != now.Type {
		c.fail(where, "type changed from %q to %q", was.Type, now.Type)
	}
	if was.Format != now.Format {
		c.fail(where, "format changed from %q to %q", was.Format, now.Format)
	}
	if dir == request && was.Nullable && !now.Nullable {
		c.fail(where, "no longer accepts null")
	}
	if dir == response && !was.Nullable && now.Nullable {
		c.fail(where, "may now be null")
	}
	// An enum that was LIFTED (no enum now) admits every value. That widens a
	// request, which is fine; on a response it is the unknown-value case the
	// contract already requires clients to tolerate. So only a member that a
	// still-closed enum dropped is breaking.
	if len(was.Enum) > 0 && len(now.Enum) > 0 {
		members := toSet(now.Enum)
		for _, value := range was.Enum {
			if !members[value] {
				c.fail(where, "enum member %q removed", value)
			}
		}
	}

	wasRequired := toSet(was.Required)
	nowRequired := toSet(now.Required)
	for _, name := range sortedKeys(was.Properties) {
		prop := where + "." + name
		nowProp, ok := now.Properties[name]
		if !ok {
			c.fail(prop, "property removed")
			continue
		}
		switch dir {
		case request:
			if !wasRequired[name] && nowRequired[name] {
				c.fail(prop, "became required")
			}
		case response:
			if wasRequired[name] && !nowRequired[name] {
				c.fail(prop, "is no longer guaranteed (required -> optional)")
			}
		}
		c.shape(prop, dir, was.Properties[name], nowProp)
	}
	if dir == request {
		for _, name := range sortedKeys(now.Properties) {
			if _, existed := was.Properties[name]; !existed && nowRequired[name] {
				c.fail(where+"."+name, "is a new required property")
			}
		}
	}
	if was.Open && !now.Open && dir == request {
		c.fail(where, "no longer admits keys it does not name")
	}

	if was.Items != nil {
		c.shape(where+"[]", dir, was.Items, now.Items)
	}

	// Alternatives are matched by position after sorting. A oneOf is rare in
	// the client subset; when its arms change at all, the gate asks a human.
	if len(was.OneOf) > 0 {
		if len(now.OneOf) < len(was.OneOf) {
			c.fail(where, "oneOf lost an alternative (%d -> %d)", len(was.OneOf), len(now.OneOf))
			return
		}
		nowArms := map[string]bool{}
		for _, arm := range now.OneOf {
			nowArms[canonical(arm)] = true
		}
		for i, arm := range was.OneOf {
			if !nowArms[canonical(arm)] {
				c.fail(fmt.Sprintf("%s.oneOf[%d]", where, i), "alternative changed or removed")
			}
		}
	}
}

func toSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		out[value] = true
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Summary is a one-line description of a surface, for test failure messages.
func Summary(surface *Surface) string {
	ids := make([]string, 0, len(surface.Operations))
	for _, key := range sortedKeys(surface.Operations) {
		ids = append(ids, surface.Operations[key].OperationID)
	}
	return fmt.Sprintf("client_contract %s, %d operations: %s", surface.ClientContract, len(ids), strings.Join(ids, ", "))
}
