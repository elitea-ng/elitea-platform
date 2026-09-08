# Standalone toolkit discovery

Status: implementation gap `TKDISC-RUST-01`. Checked on 2026-09-08.

## Separate contracts

Rust already enumerates and binds supported native toolsets during agent and direct-read admission.
SharePoint and OpenAPI tool calls use that path.
The missing capability is the separate durable `toolkit.available_tools.v1` command.
It supplies a caller with the configured toolkit's available operation descriptors.
Generated message types alone do not implement command intake, execution, or fenced output.

| Evidence | Owner and state |
| --- | --- |
| Current `elitea_core/api/v2/toolkit_available_tools.py` and `rpc/application.py::get_toolkit_available_tools` | Expand the exact saved toolkit for the current actor and request live SDK/provider discovery. |
| `libs/proto/elitea/runtime/v1/toolkit.proto` | Defines `ToolkitAvailableToolsCommandV1` and its input-bound result artifact contract for every worker language. |
| Rust `src/protocol/command.rs::parse_and_verify_execution_command` | Accepts agent execution and `ToolkitExecuteRead`. Refuses other capabilities, including standalone discovery. |
| Rust `src/toolkits/tool_binding.rs::freeze_toolsets` | Enumerates native toolsets under time and size bounds. Existing agent and direct-read paths reuse this code. |
| Rust `src/toolkits/direct_execution.rs` and `src/agents/pipeline.rs` | Consume native enumeration without dispatching a standalone discovery job. |
| Main `internal/api/v2/toolkits/handler.go::pgRepo.AvailableTools` | Reads attachment rows. It does not implement the current platform's live saved-instance discovery contract. |
| Main `internal/api/v2/mcp/internal_toolkits_catalog.go` | Keeps `get_elitea_core_toolkit_available_tools` unpublished until its actual business operation is available. |

The current-platform source revision is `elitea_core` `b701a00aeff0af1a416916c4a537bfdd4b7d8337`.
This gap is separate from [toolkit Test](toolkit-test.md) and from [external MCP execution](external-elitea-mcp.md).
It does not mean all external MCP discovery is absent.
External MCP already publishes selected saved operations and has a separate dynamic-schema gate.

## Completion requirements

Reuse the existing language-neutral command and result schema.
Keep toolkit settings outside Redis and redeem credentials only after the current claim is admitted.
Reuse native toolset enumeration instead of implementing a second registry.
Preserve exact actor, project, configuration, selection, policy, cancellation, and error behavior.
Publish bounded descriptors through the input-bound, generation-fenced result artifact contract.
Prove redelivery and worker replacement without executing a protected toolkit operation.
Then expose saved-instance discovery through Main's shared business path and the internal MCP tool.

Keep this implementation gap separate from browser verification in [the test register](../testing-gaps.md).
