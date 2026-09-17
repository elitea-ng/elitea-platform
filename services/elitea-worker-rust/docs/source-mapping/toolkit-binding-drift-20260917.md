# Toolkit binding drift verification

Status: source audit and Rust component checks pass, 2026-09-17.
This is a point 4 verification slice, not complete point 4 acceptance.

## Current-platform evidence

The reference SDK is `765f6e1cd1fe5e9f70a741b9a73523224b350593`.
Its `elitea_sdk/runtime/langchain/langraph_agent.py::find_tool_by_name_or_metadata`
now delegates scoped direct-node selection to
`runtime/tools/tool_binding.py::select_tools_for_binding`. An unavailable
toolkit-qualified operation must not fall back to another toolkit's same-named
operation. A unique unscoped legacy tool remains a Python compatibility case.

`build_tool_binding_plan` in that module separately handles the flat model tool
namespace: preserve unique names, qualify collisions, and delegate each alias to
the original implementation. `tests/runtime/test_tool_binding.py` and
`tests/runtime/test_5925_5927_mcp_auth_routing.py` record those reference contracts.

## Rust mapping and implementation boundary

| Contract | Rust owner |
| --- | --- |
| Freeze and qualify model-callable tools | `src/toolkits/tool_binding.rs::{freeze_toolsets,bind_frozen_toolsets}` |
| Root and nested agent model binding | `src/agents/{ordinary,application_tools}.rs` |
| Pipeline model binding | `src/agents/pipeline.rs::NativePipelineLlmAgentFactory::bind_selected_tools` |
| Direct pipeline toolkit identity | `src/agents/pipeline.rs::{toolsets_by_alias,build_direct_tool_resolver}` and `NativePipelineDirectToolResolver` |
| Exact authorization and sensitivity after aliasing | `src/toolkits/delegated_auth.rs`, `src/agents/sensitive_tools.rs` |

Rust already implements the stricter admitted identity contract. Direct graph
selection looks up the toolkit alias first, then the operation within that
toolkit. The resolver retains the pair as its key. Duplicate aliases or operations
are refused. There is no global-name fallback and no unscoped legacy-tool path in
the admitted Rust snapshot, so the SDK change needs no new runtime implementation.
Provider aliases affect the flat model namespace only; deterministic direct
nodes retain the original toolkit and operation identity.

## Added proof and limits

Two regressions in `src/agents/pipeline_tests.rs` execute the production assembler
against controlled MCP connectors:

- Two direct graph nodes invoke the same operation name from different toolkits.
  Each implementation executes once, in graph order, and its own result is stored
  under the intended graph state key. No model is bound or invoked.
- One admitted toolkit no longer exposes that operation. Assembly fails before
  either tool is called; the other toolkit's operation is never substituted.

All 11 focused Rust checks pass: the two new direct-node cases, four binding-plan
checks, two root/pipeline model collision stories, and three exact-scope checks.
There are no ignored or environment-skipped checks in these runs. Cargo formatting
and all-target Clippy with warnings denied pass.

These are in-process contract checks with controlled connectors and model
responses. They do not claim deployed browser, external-MCP transport, or crash
recovery acceptance. No production Rust code, database schema, or dependency is
changed. Broader point 4 continuation, compaction, and diagnostics work remains
open.
