# Toolkit name compatibility

## Current-platform reference

SDK commit: `966526e8334354366dd161b606d73fe8e204b850`.

- `elitea_sdk/runtime/utils/toolkit_identity.py::toolkit_identity_key` removes whitespace and underscores and uses lowercase text.
- `elitea_sdk/runtime/tools/tool_binding.py::select_tools_for_binding` prefers exact toolkit and operation matches. A legacy spelling must resolve without ambiguity.

These files define compatibility behavior. The Rust worker does not copy the Python implementation or use an unscoped operation-name fallback.

## Rust mapping

- `src/agents/pipeline.rs::PipelineExecutionProfile::validate_tool_snapshot` resolves legacy names against the frozen, admitted toolkit snapshot before policy checks.
- `src/agents/pipeline.rs::legacy_toolkit_key` produces the compatibility key.
- `src/agents/graph/compiler.rs::PipelineDefinition::resolve_legacy_toolkit_aliases` applies the result to direct Toolkit/MCP nodes and LLM tool selections.
- `src/agents/graph/direct_tool.rs` and `src/agents/graph/llm.rs` retain the canonical alias in the execution definition.

Exact names win. For example, `release_intelligence` can resolve to `release intelligence` when there is one matching admitted toolkit. If both names exist, an exact reference selects its own toolkit. A third spelling that matches both is rejected before a connection or tool call.

Saved YAML, database schema, tool operation names, and application participant names do not change. Existing authorization, sensitivity, tool-kind, and selected-operation checks run against the canonical toolkit. A missing toolkit cannot fall back to another toolkit with the same operation name.

## Verification

The direct MCP legacy-name regression failed before the fix with `InvalidInput`.
After the fix, all 51 `agents::pipeline_tests` pass. Coverage includes direct MCP execution, exact-name precedence, ambiguous-name rejection before connection, LLM selection admission, and static toolkit compatibility. Strict library/test Clippy and formatting checks pass.

Deployment and fresh browser acceptance remain pending. Component checks do not close Gate 4.
