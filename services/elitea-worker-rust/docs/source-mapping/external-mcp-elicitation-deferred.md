# External MCP elicitation: deferred option

Status: deferred by user agreement on 2026-09-14. This option does not block point 3.

## Current contract

External MCP executes autonomous agents, pipelines, and toolkit tools.
An execution that requires approval returns a clear non-success outcome.
External MCP does not expose Elitea interrupt approval or resume controls.
HITL nodes and direct pipeline history remain in point 5.
Platform-wide sensitive-tool policy remains a separate later gate.

A client permission to call an exported MCP tool does not approve every sensitive step inside that execution.
A PAT authenticates the caller. It does not represent a user's decision about a particular paused tool call.
Receiving tool result text also does not approve that call.

## Later integration option

MCP elicitation can collect a user's response through a compatible client.
The negotiated protocol revision and client capabilities determine the available interaction.
The 2025-11-25 contract supports form and URL modes, with accept, decline, and cancel responses.
Do not assume that each ChatGPT, Codex, or Claude client exposes the same capabilities.
Verify the target client's advertised capabilities and actual user interaction before enabling this integration.

A later implementation must bind the response to the authenticated actor and the exact pending tool call.
It must recheck current permission, preserve the decision durably, and prevent repeated execution after replay.
Unsupported clients, cancellation, and disconnect must not silently grant approval.
The language model's generated text must not substitute for the user's decision.
Secrets must not travel through a form elicitation response.

Protocol reference: [MCP elicitation, 2025-11-25](https://modelcontextprotocol.io/specification/2025-11-25/client/elicitation).
This note describes an option. It does not add elicitation support or claim compatibility with a specific client.

## Ownership and source mapping

- `services/elitea-main/internal/api/v2/mcp/server.go` owns the external MCP transport boundary.
- `services/elitea-main/internal/api/v2/mcp/execute.go` maps shared execution outcomes to MCP results.
- `services/elitea-worker-rust/src/agents/` retains agent and graph execution ownership.
- `services/elitea-worker-rust/src/toolkits/` retains toolkit invocation and policy enforcement.
- `external-mcp-fresh-browser-20260913.md` records autonomous execution and transport replay evidence.

Transport result replay retrieves the original outcome. It does not resume an approval pause.
Future elicitation must use existing execution authority and checkpoint ownership.
No Rust implementation, protocol field, database table, or migration changes in this deferred note.
