package run

import (
	"context"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/engine"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

// DeepWiki over the engine sidecar (ADR-0023 H2). The analysis engine — the
// copied Python tool layer with its ~1.1 GB dependency closure — stays in
// Python and listens on a Unix socket next to this host. This host keeps
// the SPI, the parameter merge, the egress check, composition and upload.
//
// The socket protocol itself moved to internal/engine in stage H4c: it is
// the host's transport, not DeepWiki's. What is DeepWiki's, and stays here,
// is which tools the sidecar serves and how its results are composed.

// SidecarTools are the tools the Python sidecar runs itself: the three that
// need the analysis engine, plus the wiki resolver, which needs a model.
// Everything else is refused at the door, as the fixture table does.
var SidecarTools = []string{"generate_wiki", "ask", "deep_research", ResolveWikiTool, DeleteWikiIndexTool, DeleteProjectWikisTool}

// EngineTools are the tools a wired engine runner SERVES — the admission
// table's whole tool set (deepwiki.go), which until the wiki_query family
// was ported was three names while the table declared seven. A tool the
// table admits and the runner does not serve is admitted at the door and
// refused at the last gate, which is the regression this closes.
//
// Note ResolveWikiTool is deliberately NOT here: it is an internal step of
// resolve_and_ask / resolve_and_deep_research, not a tool any toolkit
// advertises, so it is reachable through those two and not on its own.
var EngineTools = append([]string{"generate_wiki", "ask", "deep_research"}, WikiQueryToolNames...)

// EngineLabel names the engine in the messages a caller reads.
const EngineLabel = "DeepWiki"

// EngineClient speaks the sidecar protocol over a Unix socket.
type EngineClient = engine.Client

// NewEngineClient builds the client for DeepWiki's sidecar.
func NewEngineClient(socket string) *EngineClient {
	return engine.NewClient(socket, EngineLabel)
}

// NewEngineRunner is the shared runner over the engine sidecar's tools (the
// Rust engine, ADR-0026), with the host's egress policy and callback CA. It
// reports itself as "native".
func NewEngineRunner(settings spi.Settings) *Runner {
	return NewNamedEngineRunner(settings, "native")
}

// NewNamedEngineRunner is NewEngineRunner reporting itself as name in
// GET /health.
func NewNamedEngineRunner(settings spi.Settings, name string) *Runner {
	client := NewEngineClient(settings.EngineSocket)
	runner := &Runner{engine: client}
	sidecar := map[string]Tool{}
	for _, name := range SidecarTools {
		tool := name
		sidecar[tool] = func(ctx context.Context, arguments map[string]any, tc *spi.Context) (map[string]any, error) {
			// Every call to the engine carries the invocation's
			// authenticated project, and only that one (project.go).
			stamped, err := StampProject(ctx, tool, arguments)
			if err != nil {
				return nil, err
			}
			return client.Invoke(ctx, tool, stamped, tc)
		}
	}
	transport := ArtifactClientFrom(settings.CallbackCA())
	tools := map[string]Tool{}
	for name, tool := range sidecar {
		if name == ResolveWikiTool || name == DeleteWikiIndexTool || name == DeleteProjectWikisTool {
			// Reachable only through the two resolve_and_* tools and the
			// wiki_query delete tools, which wrap them.
			continue
		}
		tools[name] = tool
	}
	// The wiki_query family: composed HERE, over the host's artifact
	// transport, delegating only the model-backed steps to the sidecar.
	for name, tool := range WikiQueryTools(transport, WikiQueryDeps{
		Resolve:      sidecar[ResolveWikiTool],
		Ask:          sidecar["ask"],
		DeepResearch: sidecar["deep_research"],
		DeleteIndex:  sidecar[DeleteWikiIndexTool],
		// delete_wiki stops the wiki's running generations before it
		// deletes anything, and asks the engine what it serves.
		StopGenerations: runner.StopGenerations,
		IndexDeletionServed: func(ctx context.Context) (bool, bool) {
			return client.Serves(ctx, DeleteWikiIndexTool)
		},
	}) {
		tools[name] = tool
	}
	runner.RunnerName = name
	runner.Tools = tools
	runner.Egress = spi.ParseEgressPolicy(settings.GitAllowlist)
	runner.Artifacts = transport
	runner.VerifiedIdentity = settings.IdentitySecret != ""
	runner.deleteProject = sidecar[DeleteProjectWikisTool]
	return runner
}
