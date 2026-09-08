package toolkits

// Guardrail enforcement on the toolkit surfaces.
//
// The admin Configuration page's `guardrails` section names toolkit types and
// tools that this deployment refuses. Three of its four consumers are here; the
// fourth is the agent tool freeze in internal/application/agentexecution, which
// is the one a running agent cannot route around.
//
// # What is filtered, and what deliberately is not
//
// The TYPE surfaces are filtered — the type list, the type-schema catalogue, and
// the two per-project tool reads that feed the attach flow. A blocked type
// cannot be picked, and its tools do not appear in a schema.
//
// `Handler.List` — the toolkit INSTANCE list — is NOT filtered, and that is a
// decision rather than an omission. An administrator who blocks `github` must
// still be able to see the github toolkits that already exist in their projects,
// and delete them: those rows hold saved settings and references into the
// credential vault, and a list that hid them would strand both behind no surface
// at all. Blocking stops a toolkit working; it does not make it disappear. The
// product UI marks such an instance blocked from the `blocked_toolkits` list
// that `GET /elitea_core/platform_settings/prompt_lib` publishes.
//
// # Why a read failure does not block
//
// `guardrailPolicy` degrades to an empty policy and logs. Refusing to list
// toolkit types because one configuration row could not be read would take the
// create-toolkit form down to enforce a policy that is, in most deployments,
// empty — the same argument `ListTypes` already makes about its tenant read. The
// freeze path makes the opposite choice for the opposite reason, and
// `platformconfig.LoadGuardrails` returns the error precisely so the two can
// differ.

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/guardrails"
)

// GuardrailPolicySource resolves the live guardrails policy.
//
// An interface, injected by the composition root, for the reason
// `ToolkitArgumentSchemaSource` above is one: the implementation reads
// `centry.platform_config` through internal/platformconfig, and this package
// should not grow a database dependency of its own to answer a policy question.
type GuardrailPolicySource interface {
	GuardrailPolicy(ctx context.Context) (guardrails.Policy, error)
}

// WithGuardrails supplies the policy source. Unassigned, every surface behaves
// exactly as it did before guardrails existed — nothing is blocked. That is the
// correct unwired default here and only here: a Handler with no source is one no
// composition root gave a database to, which is the unit-test Handler.
func WithGuardrails(source GuardrailPolicySource) Option {
	return func(h *Handler) { h.guardrails = source }
}

func (h *Handler) guardrailPolicy(ctx context.Context, surface string) guardrails.Policy {
	if h == nil || h.guardrails == nil {
		return guardrails.Policy{}
	}
	policy, err := h.guardrails.GuardrailPolicy(ctx)
	if err != nil {
		// Recorded, never silent. A guardrail that stopped being applied and
		// said nothing is indistinguishable from one that was never configured.
		slog.ErrorContext(ctx, "guardrails: policy read failed; serving this surface unfiltered",
			"surface", surface, "err", err)
		return guardrails.Policy{}
	}
	return policy
}

// refuseBlockedToolkitType answers the toolkit WRITE paths.
//
// It returns true when it has written a refusal. The 403 NAMES the type: an
// operator who blocked `shell` and then hits this from a script needs to learn
// which rule fired, and a bare "forbidden" would read as a permissions problem
// and send them to the roles page.
func (h *Handler) refuseBlockedToolkitType(
	w http.ResponseWriter, r *http.Request, surface, toolkitType string,
) bool {
	if toolkitType == "" {
		return false
	}
	if !h.guardrailPolicy(r.Context(), surface).ToolkitBlocked(toolkitType) {
		return false
	}
	slog.WarnContext(r.Context(), "guardrails: refused a blocked toolkit type",
		"surface", surface, "toolkit_type", toolkitType)
	writeJSON(w, http.StatusForbidden, map[string]any{
		"error": "toolkit type \"" + toolkitType + "\" is blocked by this deployment's guardrails",
	})
	return true
}

// filterBlockedToolkitTypes drops blocked type names, preserving order.
func filterBlockedToolkitTypes(policy guardrails.Policy, types []string) []string {
	if policy.Empty() {
		return types
	}
	kept := make([]string, 0, len(types))
	for _, toolkitType := range types {
		if policy.ToolkitBlocked(toolkitType) {
			continue
		}
		kept = append(kept, toolkitType)
	}
	return kept
}

// filterBlockedTools drops toolkit rows whose TYPE is blocked.
//
// These two reads return `elitea_tools` rows — toolkit instances of a type, not
// the individual tools inside one — so the type check is the whole of the
// question they can answer. Per-tool blocking applies where per-tool names
// exist: the type-schema catalogue below, and the agent freeze.
func filterBlockedTools(policy guardrails.Policy, tools []Tool) []Tool {
	if policy.Empty() {
		return tools
	}
	kept := make([]Tool, 0, len(tools))
	for _, tool := range tools {
		if policy.ToolkitBlocked(tool.Type) {
			continue
		}
		kept = append(kept, tool)
	}
	return kept
}

// applyGuardrailsToCatalogue removes blocked types from the type-schema
// catalogue, and blocked tools from the types that survive.
//
// Tool names live as the KEYS of `selected_tools.args_schemas` — that is the
// authoritative per-type tool list, replaced from the pinned SDK snapshot by
// `withArgumentSchemas`. Removing a key there removes the tool from the create
// form, from the tool picker and from the argument schema the client would have
// used to call it.
//
// Every node this touches is rebuilt rather than edited: `toolkitTypeSchemas` is
// package-level state shared by every request, and the catalogue map handed in
// may still alias parts of it.
func applyGuardrailsToCatalogue(
	policy guardrails.Policy,
	catalogue map[string]map[string]any,
) map[string]map[string]any {
	if policy.Empty() {
		return catalogue
	}
	filtered := make(map[string]map[string]any, len(catalogue))
	for toolkitType, typeSchema := range catalogue {
		if policy.ToolkitBlocked(toolkitType) {
			continue
		}
		filtered[toolkitType] = withoutBlockedTools(policy, toolkitType, typeSchema)
	}
	return filtered
}

func withoutBlockedTools(
	policy guardrails.Policy,
	toolkitType string,
	typeSchema map[string]any,
) map[string]any {
	properties, ok := typeSchema["properties"].(map[string]any)
	if !ok {
		return typeSchema
	}
	selectedTools, ok := properties["selected_tools"].(map[string]any)
	if !ok {
		return typeSchema
	}
	kept, removed := withoutBlockedArgumentSchemas(policy, toolkitType, selectedTools["args_schemas"])
	if !removed {
		return typeSchema
	}

	rebuiltSelectedTools := make(map[string]any, len(selectedTools))
	for key, value := range selectedTools {
		rebuiltSelectedTools[key] = value
	}
	rebuiltSelectedTools["args_schemas"] = kept

	rebuiltProperties := make(map[string]any, len(properties))
	for key, value := range properties {
		rebuiltProperties[key] = value
	}
	rebuiltProperties["selected_tools"] = rebuiltSelectedTools

	rebuilt := make(map[string]any, len(typeSchema))
	for key, value := range typeSchema {
		rebuilt[key] = value
	}
	rebuilt["properties"] = rebuiltProperties
	return rebuilt
}

// withoutBlockedArgumentSchemas removes the blocked tool KEYS from one
// `selected_tools.args_schemas` node and reports whether anything went.
//
// IT ACCEPTS BOTH CONCRETE MAP TYPES THE CATALOGUE BUILDS THAT NODE FROM, and
// that is the whole reason it exists rather than being three lines inline.
//
// `handler.go`'s hand-written type overrides write it as a `map[string]any`
// literal, but `withArgumentSchemas` — the path every SDK-backed type takes —
// assigns the snapshot's own `map[string]map[string]any` straight in
// (handler.go:608, and `runtimecomposition.CurrentToolkitSchemaEntry.ArgsSchemas`
// declares that type). A single `.(map[string]any)` assertion therefore MISSED
// every real type: `github`, `jira`, `confluence` and the other 45 kept every
// tool an operator had blocked, while the eight hand-written stubs were filtered
// correctly — so the package's own tests passed and the deployment did not.
// Both branches are JSON objects on the wire; only the Go type differs.
//
// The kept map is returned as `any` and stored back unchanged, so each shape
// keeps the type it arrived with and nothing else in the tree is retyped.
func withoutBlockedArgumentSchemas(
	policy guardrails.Policy,
	toolkitType string,
	argsSchemas any,
) (any, bool) {
	switch schemas := argsSchemas.(type) {
	case map[string]any:
		kept := make(map[string]any, len(schemas))
		for toolName, argsSchema := range schemas {
			if policy.ToolBlocked(toolkitType, toolName) {
				continue
			}
			kept[toolName] = argsSchema
		}
		return kept, len(kept) != len(schemas)
	case map[string]map[string]any:
		kept := make(map[string]map[string]any, len(schemas))
		for toolName, argsSchema := range schemas {
			if policy.ToolBlocked(toolkitType, toolName) {
				continue
			}
			kept[toolName] = argsSchema
		}
		return kept, len(kept) != len(schemas)
	default:
		// No args_schemas node, or one this package does not build. Nothing to
		// remove, and rebuilding the tree around an unknown value would only
		// risk changing what is served.
		return nil, false
	}
}
