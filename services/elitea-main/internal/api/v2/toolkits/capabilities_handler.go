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
// keys them by, and the Rust availability is
// services/elitea-worker-rust/src/agents/internal_tools.rs's
// PLATFORM_INTERNAL_TOOLS — as of #866, that list recognizes all six below
// and SKIPS every one of them (agent_internal_tool_skipped), so every entry
// here is rustAvailable: false. ask_user, the Rust worker's one real internal
// tool, is not a user-facing Modules toggle and is intentionally not listed.
// attachments and pyodide are PLATFORM_INTERNAL_TOOLS entries too, but #866
// tracks them separately (pyodide runs a real Deno sandbox on the Python
// worker only — see the toolkit-catalogue doc's Python Sandbox note; both are
// out of scope for the six this endpoint reports on).
//
// A Rust port that lands one of these six moves it to rustAvailable: true
// HERE — the same change internal_tools.rs itself needs, so a partial port
// cannot silently under-report through this endpoint by one release lagging
// the other.
var platformInternalTools = []internalToolCapability{
	{name: "image_generation", pythonAvailable: true, rustAvailable: false},
	{name: "data_analysis", pythonAvailable: true, rustAvailable: false},
	{name: "internal_mcp", pythonAvailable: true, rustAvailable: false},
	{name: "planner", pythonAvailable: true, rustAvailable: false},
	{name: "swarm", pythonAvailable: true, rustAvailable: false},
	{name: "lazy_tools_mode", pythonAvailable: true, rustAvailable: false},
}

// RuntimeCapabilitiesResponse is RuntimeCapabilities' wire shape.
type RuntimeCapabilitiesResponse struct {
	// Worker is the plain worker name this deployment runs: "python" or
	// "rust", or "" when the deployment never stated one (see
	// WithWorkerImplementation).
	Worker string `json:"worker"`
	// InternalTools maps each of the six toggleable internal chat tools
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
// internal_tools defaults every one of the six tools to AVAILABLE when
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
