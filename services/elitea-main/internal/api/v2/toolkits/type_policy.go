package toolkits

// The operator's toolkit TYPE policy, applied to the served catalogue.
//
// ═══════════════════════════════════════════════════════════════════════════
// THE CONTRACT, for whoever changes toolkitTypeCatalogue next
// ═══════════════════════════════════════════════════════════════════════════
//
// `toolkitTypeCatalogue()` decides WHICH TYPES EXIST on this deployment. That
// is its whole job, and it is deliberately not this file's. It reads the pinned
// SDK snapshot, the four elitea_core-native settings schemas, and — once the
// projections land — the pre-built MCP catalogue and the admitted provider-hub
// manifests.
//
// This file decides WHICH OF THOSE ONE PROJECT MAY SEE. It never adds a type,
// never edits a schema, and never reads a snapshot. It only removes keys.
//
// So the composition at the catalogue's RETURN SITE is fixed, and the ORDER is
// load-bearing:
//
//	catalogue, err := h.toolkitTypeCatalogue()          // 1. what exists
//	catalogue = h.applyTypePolicyToCatalogue(…)         // 2. what this project may see
//	catalogue = applyGuardrailsToCatalogue(…)           // 3. what nobody may use
//
// Three rules follow, and each has already been broken once in this repository:
//
//  1. STEP 3 STAYS LAST. The guardrails deny-list is TERMINAL. A policy row
//     saying `enabled` must never re-admit a type an administrator blocked.
//     Composing this filter after the guardrails would do exactly that, because
//     this filter only removes keys and the guardrail step would already have
//     run on a different map.
//
//  2. STEP 2 GOES AFTER EVERY MERGE, not inside one. A projection merged after
//     the filter is a set of types the policy never saw. If a later change adds
//     `mergeProjectedTypes` (or anything like it), it belongs INSIDE
//     `toolkitTypeCatalogue`, before its return — not between steps 2 and 3
//     here.
//
//  3. AN UNREADABLE POLICY SERVES EVERYTHING. See `typePolicyFilter` below.
//
// ═══════════════════════════════════════════════════════════════════════════
//
// The store, the three availabilities and the resolution order live in
// internal/application/toolkitcatalogue. Shared migration 0114 creates the
// tables. The admin surface that writes them is
// internal/api/v2/admin/toolkit_types.go.

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcatalogue"
)

// PolicyFilter answers whether one toolkit type is served to the project the
// filter was built for.
//
// The narrowest interface that does the job. The catalogue asks one question,
// so it depends on one method: a wider seam would let a later change reach the
// store from the request path and turn one listing into fifty queries.
type PolicyFilter interface {
	Allows(toolkitType string) bool
}

// ToolkitTypePolicySource builds one project's filter.
//
// An interface here, implemented in internal/application/toolkitcatalogue and
// injected by the composition root, for the reason `ToolkitArgumentSchemaSource`
// is one: the implementation owns a database pool, and this package must not
// grow one to answer a policy question.
type ToolkitTypePolicySource interface {
	ProjectFilter(ctx context.Context, projectID int64) (toolkitcatalogue.Filter, error)
}

// WithTypePolicy supplies the policy source.
//
// UNASSIGNED, EVERY TYPE IS SERVED — which is the behaviour every deployment
// had before this file existed, and the correct default. A Handler with no
// source is one no composition root gave a database to, which is the unit-test
// Handler; and a deployment that has not applied migration 0114 has no policy
// to apply. Neither is a reason to take toolkits away from anybody.
func WithTypePolicy(source ToolkitTypePolicySource) Option {
	return func(h *Handler) {
		if source == nil {
			return
		}
		h.typePolicy = source
	}
}

// allowAllTypes is the filter an unwired or unreadable source resolves to.
type allowAllTypes struct{}

func (allowAllTypes) Allows(string) bool { return true }

// typePolicyFilter resolves one request's filter, degrading to "allow" and
// logging.
//
// THE DEGRADATION IS THE DECISION, and it is the same one `guardrailPolicy`
// makes one file over. Refusing to serve the toolkit catalogue because a small
// policy table could not be read would take the create-toolkit form down to
// enforce a policy that is EMPTY on most deployments. The failure is recorded,
// never silent: a policy that stopped being applied and said nothing is
// indistinguishable from one that was never configured.
//
// A projectID that is not a number resolves the same way. The catalogue route
// takes the project from the URL, and a caller who asks about a project that
// cannot exist gets the default catalogue rather than a 500 — the per-project
// EXCEPTION is what needs the id, and there is no exception to find.
func (h *Handler) typePolicyFilter(ctx context.Context, surface, projectID string) PolicyFilter {
	if h == nil || h.typePolicy == nil {
		return allowAllTypes{}
	}
	numericProjectID, err := strconv.ParseInt(strings.TrimSpace(projectID), 10, 64)
	if err != nil || numericProjectID <= 0 {
		numericProjectID = 0
	}
	filter, err := h.typePolicy.ProjectFilter(ctx, numericProjectID)
	if err != nil {
		slog.ErrorContext(ctx,
			"toolkit_type_policy: policy read failed; serving this surface unfiltered",
			"surface", surface, "project_id", projectID, "err", err)
		return allowAllTypes{}
	}
	return filter
}

// applyTypePolicyToCatalogue removes the types this project may not see.
//
// It REBUILDS the map rather than deleting from the one it was handed:
// `toolkitTypeSchemas` is package-level state shared by every request, and the
// catalogue map may still alias parts of it. Deleting a key from an aliased map
// would remove a toolkit type from every later request on the process — the
// exact fault `withArgumentSchemas` rebuilds every node to avoid.
//
// The per-type SCHEMA is carried over untouched. This layer decides membership
// and nothing else; per-TOOL filtering is the guardrail step's business.
func applyTypePolicyToCatalogue(
	filter PolicyFilter,
	catalogue map[string]map[string]any,
) map[string]map[string]any {
	if filter == nil {
		return catalogue
	}
	withheld := 0
	for toolkitType := range catalogue {
		if !filter.Allows(toolkitType) {
			withheld++
		}
	}
	if withheld == 0 {
		// The common path on every deployment with no policy rows. Returning
		// the same map avoids copying fifty schema trees per request.
		return catalogue
	}
	kept := make(map[string]map[string]any, len(catalogue)-withheld)
	for toolkitType, typeSchema := range catalogue {
		if !filter.Allows(toolkitType) {
			continue
		}
		kept[toolkitType] = typeSchema
	}
	return kept
}

// filterTypesByPolicy is the same rule over the bare NAME list that
// `ListTypes` serves, preserving order.
//
// The two surfaces are filtered by one rule on purpose. A type withheld from
// the schema catalogue but still named by the type list is a tile the client
// offers and cannot configure, which reads as a broken form rather than as a
// policy.
func filterTypesByPolicy(filter PolicyFilter, types []string) []string {
	if filter == nil {
		return types
	}
	kept := make([]string, 0, len(types))
	for _, toolkitType := range types {
		if !filter.Allows(toolkitType) {
			continue
		}
		kept = append(kept, toolkitType)
	}
	return kept
}
