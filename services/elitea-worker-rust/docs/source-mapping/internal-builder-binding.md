# Internal builder chat binding

## Source mapping

| Current platform source | Replatform owner | Behavior |
| --- | --- | --- |
| `projects/EliteaUI/src/[fsd]/features/chat/lib/hooks/useInternalToolsConfig.hooks.js` | Web `useChatBoxInternalTools.ts` | Persist conversation module selection and restore it after failure. |
| Core `rpc/chat_all.py`, merged agent and conversation internal tools | Main `agentexecution/internal_mcp.go` and `tools.go` | Merge builder selection and create frozen MCP references. |
| Current internal MCP categories | Main `mcpregistry/internal_builders.go` and `runtimecomposition/agent_prebuilt_mcp.go` | Resolve fixed category URLs with the endpoint project and executing actor. |

Core paths are relative to `projects/centry/pylon_main/plugins/elitea_core`.
Web paths are under `apps/elitea-web/src/widgets/chat-box/ui/hooks`.
Main package paths are under `services/elitea-main/internal`.
Rust consumes the resulting MCP settings through its existing toolkit client.

## Implementation history

The September 11 binding preserves module selection before execution.
New chats include the selected modules in their creation metadata.
Existing chats serialize selection updates. Sending waits for the pending save.
A failed save restores persisted selection and prevents that send from starting.

Main converts builder flags into fixed MCP toolkit references for saved agents and ordinary chat.
Repeated selection does not duplicate references.
The general internal-MCP flag enables nine registered categories.
Selected skill and project-context builders also work independently.

Claim-time materialization supplies the endpoint project and executing actor token.
Client-provided URLs, project values, tokens, and headers cannot override this authority.
Missing actor-token authority fails materialization.
Unknown internal types do not become arbitrary remote endpoints.

## Verification

The isolated candidate passes 52 Go binding test events without skips.
The Web suite passes ten tests through real hooks and HTTP fixtures.
Tests cover new-chat selection, durable save ordering, rapid toggles, reload projection, and failed saves.
Go vet, Web TypeScript checking, and focused Web lint pass.

Project-context snapshots and compaction changes remain outside this checkpoint.
Deployed browser acceptance remains separate from these component checks.
