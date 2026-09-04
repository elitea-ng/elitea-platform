package mcp

// The three MCP endpoint scopes — issue 252 P2 and P3 — and how a URL becomes
// one.
//
//	/app/{pid}/mcp                       → everything the project exposes
//	/app/{pid}/mcp/{category}            → one category of it
//	/app/{pid}/mcp/{entity}/{versionID}  → one agent or one toolkit
//
// pylon registers those as three Werkzeug rules and lets the router rank them.
// chi does not rank overlapping wildcards, so the tail is matched once and
// classified here; the classification is the same one Werkzeug arrives at,
// because the two-segment "second part is an integer" case is exactly what
// `<string:entity>/<int:entity_version_id>` matches and nothing else is.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type scopeKind int

const (
	scopeAll scopeKind = iota
	scopeCategory
	scopeResource
)

type scope struct {
	kind scopeKind
	// category is one of the keys of mcpCategories, set when kind is
	// scopeCategory.
	category string
	// resourceType is "toolkit" or "application", set when kind is
	// scopeResource. Same normalisation as pylon's ENTITY_TYPE_MAP.
	resourceType string
	resourceID   int64
}

// mcpEntityTypes is pylon's `ENTITY_TYPE_MAP`
// (`legacy/plugins/elitea_core/models/enums/mappings.py`). `agent` and
// `pipeline` are aliases: both are rows of `applications`, distinguished by
// their version's `agent_type`, not by a separate table.
var mcpEntityTypes = map[string]string{
	"toolkit":     "toolkit",
	"application": "application",
	"agent":       "application",
	"pipeline":    "application",
}

// mcpCategories is the category vocabulary this server accepts.
//
// Pylon's tag-filtered endpoint takes an OpenAPI section name
// (`elitea_core/applications`, `secrets`, `artifacts`, …) and synthesizes tools
// from selected REST operations. Main admits one such category only after its
// operations have explicit schemas, permissions, project clamping, and an
// in-process executor. Unknown legacy categories remain refused rather than
// returning a plausible but unrelated project-tool listing.
//
// The project categories are the two real sources the project has, split the
// way pylon's own docstring says its tag endpoint splits them — "for
// elitea_core/applications, also includes agents tagged with 'mcp'. For
// elitea_core/toolkits, also includes toolkit instance tools marked
// available_by_mcp". The internal applications category is a separate fixed
// builder surface and does not change those external opt-in rules.
var mcpCategories = map[string]string{
	"applications":             "agents in this project whose version carries the `mcp` tag",
	"elitea_core/applications": "fixed internal application-builder operations",
	"toolkits":                 "toolkits in this project flagged meta.mcp_options.available_by_mcp",
}

// parseScope classifies the path tail after `/mcp`.
func parseScope(tail string) (scope, error) {
	tail = strings.Trim(tail, "/")
	if tail == "" {
		return scope{kind: scopeAll}, nil
	}
	segments := strings.Split(tail, "/")
	if _, known := mcpCategories[tail]; known {
		return scope{kind: scopeCategory, category: tail}, nil
	}

	if len(segments) == 2 {
		if id, err := strconv.ParseInt(segments[1], 10, 64); err == nil && id > 0 {
			resourceType, known := mcpEntityTypes[strings.ToLower(segments[0])]
			if !known {
				return scope{}, fmt.Errorf(
					"unknown entity type: %s. Valid types: %s", segments[0], sortedKeys(mcpEntityTypes))
			}
			return scope{kind: scopeResource, resourceType: resourceType, resourceID: id}, nil
		}
	}

	return scope{}, fmt.Errorf(
		"unknown category: %s. Valid categories: %s. This server serves the project's own agents and toolkits; "+
			"only explicitly ported internal categories are served", tail, sortedKeys(mcpCategories))
}

// titleCase upper-cases the first byte. The inputs are the fixed ASCII keys of
// mcpEntityTypes, so this reproduces pylon's `str.title()` on exactly the values
// that reach it without pulling in golang.org/x/text for a one-word cast.
func titleCase(value string) string {
	if value == "" {
		return value
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

func sortedKeys[V any](m map[string]V) string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// serverIdentity is the `serverInfo.name` / `instructions` pair a scope reports
// at initialize. pylon derives the same pair from the resource scope so that a
// client showing several Elitea endpoints side by side can tell them apart.
func (s scope) serverIdentity() (name, instructions string) {
	switch s.kind {
	case scopeResource:
		return fmt.Sprintf("ELITEA-%s-%d", strings.ToUpper(s.resourceType), s.resourceID),
			fmt.Sprintf("ELITEA %s (ID: %d)", titleCase(s.resourceType), s.resourceID)
	case scopeCategory:
		name := strings.NewReplacer("/", "-", "_", "-").Replace(strings.ToUpper(s.category))
		return fmt.Sprintf("ELITEA-%s", name), fmt.Sprintf("ELITEA %s", s.category)
	default:
		return "ELITEA MCP SERVER", "ELITEA"
	}
}
