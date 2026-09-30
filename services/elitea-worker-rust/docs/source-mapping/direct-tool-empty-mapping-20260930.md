# Direct tools with explicit empty input mappings

## Current-platform behavior

`elitea-sdk/elitea_sdk/runtime/langchain/langraph_agent.py` constructs direct FunctionTool nodes near line 1342.
Its dictionary lookup defaults missing `input_mapping` to a variable mapping for `messages`.
An explicit empty dictionary remains empty.
`runtime/tools/function.py` invokes `runtime/langchain/utils.py::propagate_the_input_mapping`.
That function starts with an empty argument dictionary and fills only declared entries.
An empty mapping therefore passes no arguments.

## Rust implementation

`src/agents/graph/direct_tool.rs::RawDirectToolNodeDefinition` now applies the legacy default during deserialization.
`validate_input_mapping` preserves explicitly empty mappings.
Both Toolkit and MCP nodes use this contract.
Omitted mappings retain their existing behavior. Null mappings remain invalid.
The selected tool identity, authorization, confirmation, and output boundaries remain unchanged.

The configuration digest includes the admitted mappings.
Changing an explicit empty mapping from the previous implicit default changes its digest.
An incompatible checkpoint must not silently resume with different tool arguments.
No database migration is required.

## Verification

`explicit_empty_direct_tool_mapping_sends_no_arguments` executes both node kinds with nonempty message state.
It checks one call with `{}` and a configuration digest different from the omitted default.
The existing legacy-default test checks omitted mappings.
All 16 focused direct-tool tests pass.
The acceptance MCP fixture adds `/empty`, which rejects all argument keys.
Its four focused response checks preserve the legacy fixture contracts.
Persistent browser acceptance passes on Kubernetes rehearsal chat 760, pipeline 134, and saved MCP 95.
Execution `1e4fa2b2cd93ab2afdc00afa02ad6b7e` returns `EMPTY-ARGUMENTS-731` without an LLM call.
The result remains visible after reload.
The fixture records one successful call with no argument keys.
The deployed worker contains commit `2f3520c4a`.

The first assembly attempt fails because the worker lacks the isolated fixture CA.
The rehearsal adds that public CA to outbound trust only. Platform mTLS trust remains unchanged.
The next attempt exposes a fixture error: MCP permits omitted arguments for zero-argument calls.
The fixture now accepts omission or `{}`. It rejects null and injected `messages`.
See the [MCP request schema](https://modelcontextprotocol.io/specification/2025-11-25/schema#calltoolrequestparams).
Four focused fixture checks pass.

## Assembly failure diagnostics

`src/agents/pipeline.rs` previously maps tool materialization failures to an unavailable LLM runtime.
This also affects direct MCP pipelines with no LLM nodes.
Both root and nested pipeline bindings now reuse the ordinary-agent error adapters.
They preserve configuration, dependency, capacity, and authorization categories, including sanitized authorization requirements.
No endpoint, credential, response body, or tool arguments enter the error message.
`direct_mcp_setup_failure_preserves_configuration_category_without_model` verifies failure before connection.
Five direct-MCP identity and legacy-name regression tests pass. Clippy with tests and denied warnings passes.
The diagnostic correction is deployed from source `dbd2629b3`.
The cached worker manifest is `sha256:ada4f119a21b321c8b80d69b180f80cb86f5f90f931294ee71489c3af271c8c1`.
Chat 760 verifies failure after confirmed removal of the disposable MCP fixture Pod.
Execution `c59ff95712fbc6117f023330fa3c5503` reports `native_agent.dependency_unavailable` with the MCP assembly reason.
The UI shows `DEPENDENCY_UNAVAILABLE`, an operator-only support reference, and the same failure after reload.
An earlier attempt completes before fixture shutdown and is not negative-test evidence.
The fixture is restored after the check. A new browser turn returns `EMPTY-ARGUMENTS-731` with no argument keys.
Assembly failures still log at WARN. Aligning terminal assembly severity with the ERROR policy remains open.
The current SDK remains the functional reference for mapping behavior described above.
No current-platform error wording is copied.

The worker release target now accepts the same optional `CARGO_BUILD_JOBS` setting as the supervisor target.
This bounds build concurrency without changing runtime limits.
The isolated rehearsal stops during the release build because the shared Docker VM previously exhausted memory.
