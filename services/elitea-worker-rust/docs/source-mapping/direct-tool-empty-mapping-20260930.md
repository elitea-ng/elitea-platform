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
The acceptance MCP fixture adds `/empty`, which rejects any argument beyond the explicit empty object.
Its four focused response checks preserve the legacy fixture contracts.
Deployed browser verification and strict zero-argument MCP acceptance remain pending.

The worker release target now accepts the same optional `CARGO_BUILD_JOBS` setting as the supervisor target.
This bounds build concurrency without changing runtime limits.
The isolated rehearsal stops during the release build because the shared Docker VM previously exhausted memory.
