# Effectful direct tool nodes (2026-10-08)

Status: partial. Effectful direct tools now run, they are never dispatched twice, and block/skip
stops the pipeline. Operator actions on a direct-tool recovery card and LLM-node receipts are open.
Design and guarantees: [`../direct-tool-effects-design.md`](../direct-tool-effects-design.md).

## Business behaviour

**Ported from the current platform:**
- the sensitive approve / reject / block_with_comment pause;
- the "Pipeline stopped" block termination: `_pipeline_blocked`, no tool data in declared outputs,
  route to END;
- the MCP authorize / skip card, with its skip and refresh-failed terminations.

**Not ported:**
- fail-open decisions;
- re-running an approved effect after a crash;
- the step-less MCP auth identity.

## Changed paths

- `src/agents/pipeline.rs`:
  - removed the effectful-tool assembly refusal;
  - direct-node refusals name the node kind (`invalid_direct_tool_scope`,
    `unsupported_direct_tool_scope`), with alias resolution at `:202-215`.
- `src/agents/graph/direct_tool.rs`:
  - fenced-writer requirement at `:426-431`;
  - journaled `dispatch` at `:536-592`;
  - `DirectToolAttempt` at `:842-937`.
- `src/agents/graph/compiler.rs:1315-1319`: attaches the node-recovery authority.
- `src/agents/graph/node_recovery_runtime.rs`: the test module is visible inside the graph module
  (test-only).
- `deploy/runtime/worker-runtime.json`, `deploy/runtime/worker-runtime.nats-secure.json`:
  `agent_node_recovery: true`.
- `deploy/helm/elitea/values.yaml`, `templates/worker/configmap-runtime.yaml`: new
  `worker.runtime.agentNodeRecovery` (Rust-only, default false).
- `deploy/helm/tests/render-worker-sandbox.sh`: asserts that the toggle renders.

## Tests

New file `src/agents/graph/node_recovery_direct_tool_tests.rs`, with 7 tests:
- `effectful_direct_tool_runs_once_and_its_committed_result_is_replayed`
- `started_attempt_without_result_never_repeats_an_effectful_tool`
- `effectful_tool_failure_stops_with_its_error_and_is_not_called_again`
- `sensitive_effectful_pause_leaves_no_started_attempt_and_approval_runs_once`
- `blocked_sensitive_effectful_tool_stops_the_pipeline_without_a_journal_record`
- `effectful_authorization_challenge_pauses_then_skip_stops_without_a_journal_record`
- `effectful_tool_requires_the_fenced_writer_before_any_call`

`src/agents/graph/direct_tool_tests.rs`:
- `blocked_effectful_sensitive_tool_stops_the_whole_pipeline_under_node_recovery`
- `skipped_effectful_authorization_stops_the_whole_pipeline_under_node_recovery`

  Both run a full graph with a downstream node on the same tool and check: zero calls, END, no tool
  data in outputs, and the message present.
- `effect_or_wrong_structured_shape_fails_without_checkpoint_corruption` (updated).

`src/agents/pipeline_tests.rs`:
- `toolkit_node_materializes_read_only_and_effectful_actions` and
  `mcp_node_assembles_server_declared_effect_without_executing_it` (renamed);
- the direct-node wording assertions in the toolkit and MCP scope tests.

Results:
- `cargo test --all-features`: lib 2029 passed, 0 failed, 63 ignored; all integration test targets pass. DB-gated tests are skipped without
  `ELITEA_TEST_DATABASE_URL`, and the new tests use the in-memory journal fixture.
- `cargo clippy --all-targets --all-features -D warnings` and `cargo fmt --check` are clean.
- Helm: `render-worker-sandbox.sh` passes, and `render-worker.sh` ran 8 assertions, all passed.

## Performance / Durability / Resilience / Security

- **Performance.** Read-only direct tools are unchanged and are not journaled. An effectful call
  adds one fenced `Started` append and one result append. The result is bounded by the journal's
  1 MiB result limit.
- **Durability.** Intent is recorded before the effect. A committed result is replayed and never
  re-executed. A `Started` record without a result is never re-dispatched.
- **Resilience.**
  - Every failure is typed: a missing writer gives `pipeline.tool_unavailable`, a tool error gives
    `pipeline.tool_failed`.
  - Block and skip stop the whole pipeline.
  - Lease loss fails the append before the call.
- **Security.**
  - Authorization and policy blocklists run before dispatch.
  - Decisions are digest-bound and fail closed.
  - New error strings are `&'static str` and carry no data; arguments appear only as digests and the
    masked preview.

## Recovery guarantee rows

See the design note's "Recovery guarantees" table. Every row cites its enforcing code and proving
test.

## Browser evidence

Pending: a local rehearsal stack with `agent_node_recovery: true`. The check covers a direct MCP
`echo` node (non-sensitive and sensitive: approve, block, reload) and an LLM node with the same
tool.

## Follow-ups

- Wire operator retry/stop and result projection for direct-tool recovery cards.
- Move LLM-node effectful tools onto the same journal (Gate 6).
- Pre-probe the MCP authorization, so that a crash during an auth challenge does not leave a
  `Started` record.
