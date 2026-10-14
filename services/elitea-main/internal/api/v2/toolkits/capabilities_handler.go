package toolkits

import "net/http"

// WithWorkerImplementation supplies the plain "python"/"rust" name of the
// worker image this deployment runs, for RuntimeCapabilities to serve as-is.
//
// It is a bare string, not the ToolkitCapabilitySource interface
// WithWorkerCapability takes, because RuntimeCapabilities' job is different:
// WithWorkerCapability answers "can THIS type run", per type, already folded
// into ListTypeSchemas' metadata.hidden. RuntimeCapabilities answers "which
// worker is this", once, so a client that already has a type's metadata can
// still tell a Python deployment from a Rust one without re-deriving it from
// which types happen to be hidden — and so it can answer for the internal
// chat tools below, which the toolkit catalogue does not cover at all.
//
// Left unset, the field stays "", and RuntimeCapabilities serves it as-is
// rather than guessing: an empty string is a visible "this deployment did not
// say" a client can flag, where a guessed default would misreport silently.
func WithWorkerImplementation(implementation string) Option {
	return func(h *Handler) { h.workerImplementation = implementation }
}

// internalToolCapability names one of the platform's toggleable internal chat
// tools and whether the CONFIGURED worker runs it for real.
type internalToolCapability struct {
	name            string
	pythonAvailable bool
	rustAvailable   bool
}

// platformInternalTools is the source of truth RuntimeCapabilities reports
// from, restated here rather than imported: the names are the agent-form
// toggle catalogue apps/elitea-web/src/features/agents/lib/internalTools.ts
// keys them by, and the Rust availability is what
// services/elitea-worker-rust/src/agents/internal_tools.rs actually serves:
//
//   - planner, lazy_tools_mode (Smart Tools Selection) and attachments are
//     served by BOTH workers (libs/rust/agent-runtime planner.rs and
//     lazy_tools.rs; the worker's attachment_tools.rs), so the form shows
//     them enabled on either deployment;
//   - image_generation, data_analysis, internal_mcp and swarm are still
//     recognized and SKIPPED there (PLATFORM_INTERNAL_TOOLS,
//     agent_internal_tool_skipped), so they are rustAvailable: false;
//   - skills_builder and project_context_builder run the other way round
//     (native only), below.
//
// ask_user, the Rust worker's clarification tool, is not a user-facing
// Modules toggle and is intentionally not listed. pyodide runs a real Deno
// sandbox on the Python worker only and is tracked separately.
//
// A Rust port that lands another of these moves it to rustAvailable: true
// HERE — the same change internal_tools.rs itself needs, so a partial port
// cannot silently under-report through this endpoint by one release lagging
// the other.
var platformInternalTools = []internalToolCapability{
	{name: "image_generation", pythonAvailable: true, rustAvailable: false},
	{name: "data_analysis", pythonAvailable: true, rustAvailable: false},
	{name: "internal_mcp", pythonAvailable: true, rustAvailable: false},
	{name: "planner", pythonAvailable: true, rustAvailable: true},
	{name: "swarm", pythonAvailable: true, rustAvailable: false},
	{name: "lazy_tools_mode", pythonAvailable: true, rustAvailable: true},
	{name: "attachments", pythonAvailable: true, rustAvailable: true},
	// #940 A8's two builder modules run the OTHER way round: they are
	// implemented in the native runtime (services/elitea-worker-rust/src/
	// agents/internal_tools.rs's SkillsBuilderTool/ProjectContextBuilderTool,
	// which write through the claim-bound content listener) and the Python
	// SDK worker has no port of them at all — that half is deliberately out
	// of this programme's scope. Reporting them as python-unavailable is
	// what makes the toggle render disabled-with-a-reason on a Python
	// deployment instead of silently doing nothing, the exact failure #866
	// existed to end.
	{name: "skills_builder", pythonAvailable: false, rustAvailable: true},
	{name: "project_context_builder", pythonAvailable: false, rustAvailable: true},
}

// RuntimeCapabilitiesResponse is RuntimeCapabilities' wire shape.
type RuntimeCapabilitiesResponse struct {
	// Worker is the plain worker name this deployment runs: "python" or
	// "rust", or "" when the deployment never stated one (see
	// WithWorkerImplementation).
	Worker string `json:"worker"`
	// InternalTools maps each toggleable internal chat tool
	// (apps/elitea-web's internalTools.ts catalogue) to whether the
	// CONFIGURED worker actually runs it, rather than skipping it silently.
	InternalTools map[string]bool `json:"internal_tools"`
	// HiddenToolkitTypes lists every catalogued toolkit type the configured
	// worker cannot build — the same verdict ListTypeSchemas already folds
	// into each type's metadata.hidden, restated here as one flat list so a
	// client can answer "what is hidden and why" without re-deriving it from
	// fifty-two individual type schemas.
	HiddenToolkitTypes []string `json:"hidden_toolkit_types"`
}

// RuntimeCapabilities serves which worker this deployment runs and what it
// cannot do — the answer #865 and #866 found nothing in the product could
// give. Before this endpoint, a type or an internal tool the worker could not
// run either disappeared (toolkit tiles, filtered by metadata.hidden client
// side) or ran silently as a no-op (internal tools,
// agent_internal_tool_skipped in the Rust worker's own logs, which no UI
// surfaces): a user could not tell "not offered" from "not deployed yet" from
// "misconfigured", and had no route to ask.
//
// It answers from the SAME two inputs ListTypeSchemas' worker-capability
// column already reads (WithWorkerImplementation, WithWorkerCapability), so
// this endpoint and that column cannot disagree about which worker is
// running or which types it hides.
//
// internal_tools defaults every listed tool to AVAILABLE when
// workerImplementation is "" (WithWorkerImplementation never wired) — the
// same "unset means most permissive, not most restrictive" rule
// WithWorkerCapability's own nil case follows (see its doc comment): only
// "rust", stated explicitly, turns a tool off. In every real deployment
// main.go always resolves and wires a non-empty implementation
// (runtimecomposition.WorkerImplementationFromEnv never returns ""), so ""
// here means the composition root left this Option out, not that an
// operator configured nothing.
func (h *Handler) RuntimeCapabilities(w http.ResponseWriter, r *http.Request) {
	internalTools := make(map[string]bool, len(platformInternalTools))
	for _, tool := range platformInternalTools {
		available := tool.pythonAvailable
		if h.workerImplementation == "rust" {
			available = tool.rustAvailable
		}
		internalTools[tool.name] = available
	}

	hidden := make([]string, 0, len(h.catalogueToolkitTypes()))
	for _, toolkitType := range h.catalogueToolkitTypes() {
		if supported, _ := h.supportsToolkitType(toolkitType); !supported {
			hidden = append(hidden, toolkitType)
		}
	}

	writeJSON(w, http.StatusOK, RuntimeCapabilitiesResponse{
		Worker:             h.workerImplementation,
		InternalTools:      internalTools,
		HiddenToolkitTypes: hidden,
	})
}
