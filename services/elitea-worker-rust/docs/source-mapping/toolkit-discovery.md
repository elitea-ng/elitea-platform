# Standalone toolkit discovery

Status: shared command path implemented. Gate `TKDISC-RUST-01` awaits system and browser proof. Checked on 2026-09-09.

## Separate contracts

Rust already enumerates and binds supported native toolsets during agent and direct-read admission.
SharePoint and OpenAPI tool calls use that path.
The durable `toolkit.available_tools.v1` command now uses the same native enumeration path.
It supplies a caller with the configured toolkit's available operation descriptors.
Generated message types alone do not implement command intake, execution, or fenced output.

| Evidence | Owner and state |
| --- | --- |
| Current `elitea_core/api/v2/toolkit_available_tools.py` and `rpc/application.py::get_toolkit_available_tools` | Expand the exact saved toolkit for the current actor and request live SDK/provider discovery. |
| `libs/proto/elitea/runtime/v1/toolkit.proto` | Defines `ToolkitAvailableToolsCommandV1` and its input-bound result artifact contract for every worker language. |
| Rust `src/protocol/command.rs::parse_and_verify_execution_command` | Authenticates agent, read, shared Test, and standalone discovery commands before claim admission. |
| Rust `src/toolkits/tool_binding.rs::freeze_toolsets` | Enumerates native toolsets under time and size bounds. Existing agent and direct-read paths reuse this code. |
| Rust `src/toolkits/direct_execution.rs` and `src/agents/pipeline.rs` | Reuse native enumeration. `direct_runtime.rs::discover` supplies standalone descriptors. |
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

## Implementation history: 2026-09-09

The shared worker intake accepts the existing language-neutral discovery command.
`protocol/control.rs` admits settings and mandatory runtime context under the current claim.
`toolkits/direct_request.rs` parses bounded settings and authoritative toolkit policy.
`toolkits/direct_runtime.rs::discover` enumerates configured native toolsets without executing a protected operation.
It preserves selected operations and their original names, descriptions, and argument schemas.
Discovery uses a local snapshot row identifier because its command does not carry a saved row identifier.
This identifier grants no authority and never reaches a provider invocation.

`execution/toolkit_delivery_processor.rs` supervises materialization, enumeration, and artifact publication under the lease.
`transport/toolkit_discovery_artifact.rs` publishes bounded content through the authenticated claim-bound data plane.
The terminal result binds the artifact to the immutable settings entry and bundle digest.
Existing spool recovery validates these bindings before replay or claim replacement.
Redis carries references only.

The current Core revision remains `b701a00aeff0af1a416916c4a537bfdd4b7d8337`.
The SDK revision is `ecf49dfac73cd096da4c2297f3d91d13e526395a`.
SDK `tools/__init__.py::get_toolkit_available_tools` defines the descriptor envelope.
Core `rpc/application.py::get_toolkit_available_tools` defines standalone discovery dispatch.

The new Rust tests cover native OpenAPI enumeration and selected-operation preservation without network calls.
Live worker replacement, authenticated artifact recovery, and browser discovery remain system verification requirements.
Do not treat component tests as those proofs.

## Verification: 2026-09-09

`cargo clippy --all-targets --offline -- -D warnings` passes.
`cargo test --lib toolkit --offline` passes with 393 tests and no ignored tests.
The test run requires permission to bind local HTTP fixture listeners.
The initial sandbox run passes 388 tests and refuses three listener tests.
The authorized run includes two additional artifact transport tests added during integration.
These tests do not prove a deployed Main-to-worker request or browser flow.

The authorization follow-up test run passes with 401 toolkit tests and no ignored tests.
OpenAPI server-variable substitution now has a 128-expansion bound.
The recursive-default test verifies that malformed discovery settings cannot loop indefinitely.

## Isolated integration checkpoint: 2026-09-11

The isolated Rust candidate passes 410 toolkit tests with no failures or ignored tests.
The filter leaves 541 unrelated tests unexecuted. This is not a full worker-suite result.
Tests include shared command identity, authorization projection, result artifacts, and direct toolkit execution.
The candidate uses the committed shared protocol and excludes pending project-context snapshot changes.
Formatting and strict Clippy for the library and tests pass.
Deployed cancellation, replacement, and recovery acceptance remain open.
