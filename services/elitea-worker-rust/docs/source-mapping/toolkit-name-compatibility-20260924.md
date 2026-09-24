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

## Deployed acceptance

Commit `78a8dc80b` is deployed as worker image `sha256:e65812f7cdb3c543db5fa4c32024f1559ca6b498391db5f70b2b527592458bb5`.
The clean-commit release build passes and retains debug line tables and symbols.
The deployment preserves the worker environment, five mounts, network, and resource limits.

Fresh headed Playwright chat 683 uses saved pipeline 95, version 102.
Its direct MCP node requests `RUST-_COMPACTION-_RECORDS`; admitted toolkit 71 remains `rust-compaction-records`.
The real read-only `read_compaction_record` call returns `CEDAR-731`.
The browser renders the result and preserves it after reload, with no page errors or execution failures. No browser responses are mocked.
Execution `6c8d9d11219086b08acfbf7f563ac0d5` is `SUCCEEDED` in PostgreSQL.

The local acceptance artifacts are `elitea-toolkit-legacy-result.json`, `elitea-toolkit-legacy-frames.json`, and `elitea-toolkit-legacy.png` in the temporary evidence directory. The screenshot was inspected.
This closes deployed legacy-name acceptance for a direct MCP node. Live collision and LLM-loop compatibility acceptance remain open. Component checks do not close Gate 4.
