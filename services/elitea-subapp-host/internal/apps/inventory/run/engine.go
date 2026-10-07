package run

import (
	"context"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/inventory"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/engine"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

// Inventory over the engine sidecar (ADR-0023 H4c stage I3). The
// knowledge-graph engine — the copied Python analysis layer with the ELITEA
// SDK's closure — stays in Python and listens on a Unix socket next to this
// host. This host keeps the SPI, the parameter merge, the deferred refusals,
// the source check, composition and upload.
//
// The socket protocol itself is internal/engine: it is the host's transport,
// not any application's. What is Inventory's, and stays here, is which tools
// the sidecar serves and how its results compose.

// EngineLabel names the engine in the messages a caller reads.
const EngineLabel = "Inventory"

// EngineClient speaks the sidecar protocol over a Unix socket.
type EngineClient = engine.Client

// NewEngineClient builds the client for Inventory's sidecar.
func NewEngineClient(socket string) *EngineClient {
	return engine.NewClient(socket, EngineLabel)
}

// EngineTools are the tools the sidecar serves: every tool the admission table
// admits, minus the ones DeferredTools refuses.
//
// Derived from the admission table rather than listed again. A second list
// would be a second answer to "which tools exist", and the two would disagree
// the first time the descriptor grew one — which is exactly what descriptor
// revision legacy-v1 does.
func EngineTools() []string {
	seen := map[string]bool{}
	var names []string
	for _, family := range inventory.Toolkits.Families {
		for _, tool := range family.Tools {
			if _, deferred := DeferredTools[family.Name][tool]; deferred {
				continue
			}
			if seen[tool] {
				continue
			}
			seen[tool] = true
			names = append(names, tool)
		}
	}
	return names
}

// NewEngineRunner is the runner over the sidecar's tools, with the host's
// callback CA for the artifact upload.
//
// No egress policy is threaded through, and that is a difference from
// DeepWiki's runner rather than an omission: DeepWiki CLONES with the git CLI
// from a URL in the request, so the destination is the host's to allow or
// refuse. Inventory reads its source through the ELITEA SDK's toolkit, whose
// destination is the toolkit's stored configuration — expanded by the facade,
// from a toolkit the caller can already see. There is no request-supplied clone
// URL for a host-side allowlist to check, and adding one that always passes
// would look like an egress control while being none.
func NewEngineRunner(settings spi.Settings) *Runner {
	client := NewEngineClient(settings.EngineSocket)
	tools := map[string]Tool{}
	for _, name := range EngineTools() {
		tool := name
		tools[tool] = func(ctx context.Context, arguments map[string]any, tc *spi.Context) (map[string]any, error) {
			return client.Invoke(ctx, tool, arguments, tc)
		}
	}
	return &Runner{
		RunnerName: "legacy",
		Tools:      tools,
		Artifacts:  ArtifactClientFrom(settings.CallbackCA()),
	}
}
