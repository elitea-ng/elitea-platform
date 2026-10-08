# Direct tool node effect refusal (2026-10-08)

Status: partial. The refusal is now explicit. Admitting effects is proposed in
[`../direct-tool-effects-design.md`](../direct-tool-effects-design.md) and waits for a decision.

## Business behaviour

- Current platform: a sensitive tool in a pipeline toolkit/MCP node pauses with approve/block, and an
  auth-guarded MCP tool shows authorize/skip. Rust already does this for read-only tools.
- Not ported: running effectful direct tools without an effect receipt. Also not ported: fail-open
  confirmation decisions. See the design note.

## Change

- Errors raised for pipeline direct tool nodes now name the node kind instead of "LLM node":
  - out-of-scope: `invalid_direct_tool_scope`;
  - policy-blocked: `unsupported_direct_tool_scope`;
  - effectful tool: `unsupported_direct_tool_effect`, which says direct effects need durable
    confirmation and an effect receipt.
  
  All three are in `src/agents/pipeline.rs:2219-2238`, with the alias resolution at `:202-215`.
- Codes are unchanged (`invalid_input` / `unsupported_capability`), so the browser text and retry
  behaviour are unchanged.

## Tests

`src/agents/pipeline_tests.rs`:
- `toolkit_node_materializes_read_only_action_but_rejects_remote_effect` names the node kind and the
  reason. The message has no tool, toolkit or argument values.
- `toolkit_node_scope_is_exact_and_sensitive_read_is_bound_for_graph_confirmation` and
  `mcp_node_scope_is_exact_and_sensitive_read_uses_the_graph_confirmation` check the direct-node
  wording for out-of-scope and policy refusals.
- The LLM-node scope test still expects "LLM node".

`cargo test --all-features`: lib 2020 passed, 0 failed, 63 ignored (they need
`ELITEA_TEST_DATABASE_URL`). `cargo clippy --all-targets --all-features -D warnings` and
`cargo fmt --check` are clean.

## Performance / Durability / Resilience / Security

- **Performance:** no change. Static messages are chosen by function pointer, with no allocation.
- **Durability:** no runtime phase changes. An effectful direct node is still refused at admission
  (class F, before any effect).
- **Resilience:** the failure is typed, non-retryable and readable, and it names the node kind.
- **Security:** messages are `&'static str` and data-free. A test asserts that no tool name, toolkit
  alias or argument value appears in them. Authorization ordering is unchanged.

## Recovery guarantee rows

| Component × phase | Class | Evidence |
|---|---|---|
| Worker × admission of an effectful direct tool | F | `pipeline.rs:2122`, test above |

## Browser evidence

Not collected for this change: the browser text is code-mapped and unchanged. It will be collected
when the effect capability is enabled (see the design note's test list).

## Follow-ups

- Decide option A or B in the design note.
- Track the LLM-node receipt gap under Gate 6.
