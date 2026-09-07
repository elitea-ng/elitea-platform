package toolkits

// Projected toolkit types: catalogue entries that no snapshot can hold.
//
// # The contract, for whoever owns toolkitTypeCatalogue
//
// `toolkitTypeCatalogue` builds the BUILT-IN half of the catalogue — the types
// this binary knows at compile time, from `toolkitTypeSchemas` and from the
// digest-pinned SDK snapshot. Nothing in this file reads or writes that
// function, and nothing in that function needs to know this file exists.
//
// The two halves meet in exactly one place: `ListTypeSchemas` calls
// `mergeProjectedTypes(base, sources…)` with whatever `toolkitTypeCatalogue`
// returned, BEFORE `applyGuardrailsToCatalogue`. So:
//
//   - a branch that rewrites `toolkitTypeCatalogue` changes `base` and needs no
//     edit here;
//   - a branch that adds a projection source changes this file and needs no
//     edit there;
//   - guardrails stay terminal, because the merge happens before them. A
//     projected type is filtered by exactly the same deny-list a built-in type
//     is, and a blocked type cannot re-enter through a projection.
//
// `mergeProjectedTypes` takes the served catalogue shape — a map of toolkit
// type name to its JSON Schema — rather than a slice of a named struct, because
// that map IS the response body and neither half has to agree on a DTO to
// compose. If `toolkitTypeCatalogue` later returns a slice, the adapter is one
// loop at the call site and this function does not move.
//
// # Why the base always wins
//
// A projection FILLS, it never overrides. Where both halves declare a type, the
// base keeps every key it already carries and the projection supplies only the
// keys the base left absent or empty. Two reasons:
//
//   - the pinned SDK snapshot is authoritative about built-in types, and a
//     database row must not be able to redefine one;
//   - the snapshot's `mcp` entry carries an EMPTY properties block (measured:
//     `{"args_schemas":{},"properties":{},"type":"mcp"}`), because the snapshot
//     projects annotated fields only and the Remote MCP settings carry no
//     annotations. Filling-not-overriding is what lets the Remote MCP settings
//     schema below land on that empty entry without the rule becoming "the
//     projection wins", which would let a row shadow `github`.
//
// # A failing source does not empty the catalogue
//
// Each source is read independently. A source that fails is logged at error and
// skipped; the other sources and the whole base catalogue still serve. This is
// `guardrailPolicy`'s own decision for the same reason: refusing to list toolkit
// types because one table could not be read takes the create-toolkit form down.
// The log line is the part that keeps it honest — a projection that stopped
// contributing and said nothing is indistinguishable from one that had nothing
// to contribute.

import (
	"context"
	"log/slog"
)

// ProjectedToolkitType is one catalogue entry contributed by a source outside
// the built-in half.
type ProjectedToolkitType struct {
	// Type is the catalogue key: `mcp`, `mcp_context7`,
	// `ImageGenServiceProvider_ImageGen`. It is the string a toolkit row's
	// `type` column carries.
	Type string
	// Schema is the type's JSON Schema, in the shape the web client reads:
	// `properties`, `metadata`, `required`, and the per-tool argument schemas
	// at `properties.selected_tools.args_schemas`.
	Schema map[string]any
}

// ToolkitTypeProjection contributes creatable toolkit types.
//
// An interface, injected, for the reason `ToolkitArgumentSchemaSource` is one:
// the implementations read tables that this package should not learn the shape
// of. Unlike that seam, the default implementations live in this package —
// they need nothing but the pool `NewHandler` already holds, which is why they
// can be constructed there instead of threaded through the router.
type ToolkitTypeProjection interface {
	// Name identifies the source in a log line. It is not served.
	Name() string
	// ProjectToolkitTypes answers this source's contribution. An empty slice
	// and a nil error mean "nothing to contribute", which is a correct answer
	// and is NOT the same as an error.
	ProjectToolkitTypes(ctx context.Context) ([]ProjectedToolkitType, error)
}

// WithToolkitTypeProjections replaces the projection sources.
//
// It replaces rather than appends so a test can state the exact set it drives,
// including the empty set. `NewHandler` installs the pool-backed defaults
// first, so a caller that passes this Option is deliberately taking them out.
func WithToolkitTypeProjections(sources ...ToolkitTypeProjection) Option {
	return func(h *Handler) {
		kept := make([]ToolkitTypeProjection, 0, len(sources))
		for _, source := range sources {
			if source != nil {
				kept = append(kept, source)
			}
		}
		h.projections = kept
	}
}

// projectedToolkitTypes reads every source, in order.
//
// Order decides which projection wins a collision between two sources: the
// first contribution of a type is kept. Sources are installed in a fixed order
// by `NewHandler`, so the answer does not depend on map iteration.
func (h *Handler) projectedToolkitTypes(ctx context.Context) [][]ProjectedToolkitType {
	if h == nil || len(h.projections) == 0 {
		return nil
	}
	contributions := make([][]ProjectedToolkitType, 0, len(h.projections))
	for _, source := range h.projections {
		projected, err := source.ProjectToolkitTypes(ctx)
		if err != nil {
			slog.ErrorContext(ctx,
				"toolkit catalogue: a projection source failed; serving the catalogue without it",
				"source", source.Name(), "err", err)
			continue
		}
		if len(projected) == 0 {
			continue
		}
		contributions = append(contributions, projected)
	}
	return contributions
}

// mergeProjectedTypes composes the built-in catalogue with the projected ones.
//
// THIS IS THE MERGE POINT. See this file's header for the contract it holds
// with `toolkitTypeCatalogue`.
//
// Rules, all pinned by tests:
//
//  1. Every base entry survives. A projection can never remove a type.
//  2. A type only the projection declares is added.
//  3. A type both declare is FILLED: the base keeps every non-empty key it has,
//     and the projection supplies the keys the base left absent or empty.
//     `properties` is filled property by property, on the same rule.
//  4. Earlier sources win over later ones for the same type.
//  5. Nothing handed in is mutated. `base` may still alias the package-level
//     `toolkitTypeSchemas`, and a projected schema may be reused across
//     requests by a caching source, so every node that changes is rebuilt.
func mergeProjectedTypes(
	base map[string]map[string]any,
	projected ...[]ProjectedToolkitType,
) map[string]map[string]any {
	if len(projected) == 0 {
		return base
	}
	merged := make(map[string]map[string]any, len(base)+len(projected))
	for toolkitType, schema := range base {
		merged[toolkitType] = schema
	}

	contributed := make(map[string]struct{})
	for _, contribution := range projected {
		for _, entry := range contribution {
			if entry.Type == "" || entry.Schema == nil {
				continue
			}
			if _, taken := contributed[entry.Type]; taken {
				continue
			}
			contributed[entry.Type] = struct{}{}

			existing, found := merged[entry.Type]
			if !found {
				merged[entry.Type] = entry.Schema
				continue
			}
			merged[entry.Type] = fillSchema(existing, entry.Schema)
		}
	}
	return merged
}

// fillSchema copies base, adding the filler's keys that base left empty.
func fillSchema(base, filler map[string]any) map[string]any {
	filled := make(map[string]any, len(base)+len(filler))
	for key, value := range base {
		filled[key] = value
	}
	for key, value := range filler {
		if key == "properties" {
			continue
		}
		if existing, present := filled[key]; present && !emptySchemaValue(existing) {
			continue
		}
		filled[key] = value
	}

	fillerProperties, hasFillerProperties := filler["properties"].(map[string]any)
	if !hasFillerProperties {
		return filled
	}
	baseProperties, _ := base["properties"].(map[string]any)
	properties := make(map[string]any, len(baseProperties)+len(fillerProperties))
	for key, value := range baseProperties {
		properties[key] = value
	}
	for key, value := range fillerProperties {
		if existing, present := properties[key]; present && !emptySchemaValue(existing) {
			continue
		}
		properties[key] = value
	}
	filled["properties"] = properties
	return filled
}

// emptySchemaValue reports whether a schema node carries no information.
//
// An empty map is the case that matters: the pinned snapshot's `mcp` entry has
// `"properties": {}`, and treating that as "already declared" would keep the
// Remote MCP settings out of the served schema and leave the create form blank.
func emptySchemaValue(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case string:
		return typed == ""
	case map[string]any:
		return len(typed) == 0
	case []any:
		return len(typed) == 0
	default:
		return false
	}
}
