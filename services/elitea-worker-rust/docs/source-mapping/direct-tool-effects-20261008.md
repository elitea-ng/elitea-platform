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
- `src/agents/graph/compiler.rs` `select_pipeline_result`: a set `_pipeline_blocked` message is the pipeline's answer.
- `src/agents/graph/node_recovery_runtime.rs`: the test module is visible inside the graph module
  (test-only).
- `deploy/runtime/worker-runtime.rust.json` (new; the shared file plus `agent_node_recovery: true`) and
  `deploy/docker-compose.standalone-rust-agent.yml` (mounts it for the Rust worker). The shared file is unchanged:
  the Python worker forbids unknown keys.
- `deploy/helm/elitea/values.yaml`, `templates/worker/configmap-runtime.yaml`: new
  `worker.runtime.agentNodeRecovery` (Rust-only, default false).
- `deploy/helm/tests/render-worker-sandbox.sh`: asserts that the toggle renders.

## Tests

New file `src/agents/graph/node_recovery_direct_tool_tests.rs`, with 8 tests:
- `effectful_direct_tool_runs_once_and_its_committed_result_is_replayed`
- `started_attempt_without_result_never_repeats_an_effectful_tool`
- `effectful_tool_failure_stops_with_its_error_and_is_not_called_again`
- `sensitive_effectful_pause_leaves_no_started_attempt_and_approval_runs_once`
- `blocked_sensitive_effectful_tool_stops_the_pipeline_without_a_journal_record`
- `effectful_authorization_challenge_pauses_then_skip_stops_without_a_journal_record`
- `effectful_tool_requires_the_fenced_writer_before_any_call`
- `effect_whose_result_cannot_be_recorded_is_reported_and_never_repeated`

`src/agents/graph/compiler_tests.rs`:
- `blocked_pipeline_answers_with_its_stop_message_not_unwritten_outputs`

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
- `cargo test --all-features`: lib 2031 passed, 0 failed, 63 ignored; all integration test targets pass. DB-gated tests are skipped without
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

Real browser (Claude desktop built-in browser), no response mocks, with reloads, on a separate local stack built
from this branch.

**Stack.** Compose project `elitea-dtfx` (`deploy/scripts/standalone-stack.sh` with `STANDALONE_WORKER=rust`), at
`http://dtfx.localhost:18140`.
- Images are tagged `dtfx-20261008`. Final worker image `sha256:fac953b5…`, built from commit `7740d591`. Main is
  `sha256:ae4e9479…` and web is `sha256:241e6128…`.
- `agent_node_recovery: true` came from the runtime file mounted for the Rust worker.
- The MCP server is `deploy/mock-mcp`. Its `echo` and `reverse` tools declare no `readOnlyHint`, so both are
  effectful.
- Every real effect was counted from the mock's `tools/call` log lines.

**Fixtures.**
- Created in the UI as `e2e-admin@autotest.local` in Default Project (1):
  - MCP "mock effects" (url, TLS verification on, selected tools `echo` and `reverse`);
  - pipelines "dtfx direct echo" (direct `mcp` node → `echo`), "dtfx sensitive reverse" (`reverse` → downstream
    `echo`), and "dtfx llm echo" (`llm` node with `tool_names: {mock effects: [echo]}`).
- The YAML was entered in the pipeline editor.
- Two settings were applied through the admin API from the same browser session:
  - **Guardrails.** `sensitive_tools: {mcp: [reverse]}` went through `PUT /api/v2/admin/plugin_config_values/administration/guardrails`,
    because the admin form's "Add toolkit" does not render a new row (a pre-existing UI defect; a saved row does
    render).
  - **Stale model row.** One seeded model row (configuration 2) was deleted, as `seed-llm` itself advises, so that
    project 1 resolves the credentialed mock model.

| Case | Result | `tools/call` delta |
|---|---|---|
| Direct `echo` (non-sensitive, effectful) | `{"output":"dtfx-echo-one"}` | +1 |
| Direct `echo` with a wrong output type | Typed "tool result does not match the node output mapping"; not re-run | +1 (the effect itself) |
| Sensitive `reverse`: pause | Approval card `mockeffects.reverse`, which is still there after a reload | 0 |
| Sensitive `reverse`: approve | `reverse` once, then downstream `echo`; result persists after reload, and reload adds no calls | +2 |
| Sensitive `reverse`: reject (with comment) | **Pipeline stopped** — the action **reverse** … was **blocked** by user. Downstream nodes skipped (exec `7743913a…`) | 0 |
| Approve in the turn after a block | Normal result; no stale stop message (exec `d9b3e7ea…` → `29ab358b…`) | +2 |
| LLM node calling effectful `echo` | `MOCK: tool echo said {"output":"dtfx-llm-echo"}`; persists after reload (exec `2114adab…`) | +1 |

**Found during verification and fixed in this PR.** A blocked pipeline answered with the `{}` default of a
terminal output instead of its stop message. `select_pipeline_result` now prefers `_pipeline_blocked`, proven by
`compiler_tests::blocked_pipeline_answers_with_its_stop_message_not_unwritten_outputs`.

**Found during verification, out of scope:**
- **Sensitive tools file.** The worker's `--toolkit-security-config` file is only the startup default. The
  run-time sensitive-tool policy comes from the platform Guardrails setting in the execution input.
- **Stale worker binary.** The worker image can contain another branch's binary: the build uses the shared
  `/cargo-target` cache mount, and BuildKit reuses the layer. A separate task has been opened. Workaround: touch the
  sources and build with `--no-cache`.
- **MCP tool discovery.** Main-side discovery (`mcp_sync_tools`) answers "MCP tool discovery failed" for the
  private `mcp-mock` host. Runtime calls go through the Worker and are not affected.
- **Admin Guardrails form.** "Add toolkit" does not render a new row.

## Follow-ups

Code review: findings fixed (unrecorded-result message, Helm refusal text). The others are recorded as gaps 3-6 in the design note.

- Wire operator retry/stop and result projection for direct-tool recovery cards.
- Move LLM-node effectful tools onto the same journal (Gate 6).
- Pre-probe the MCP authorization, so that a crash during an auth challenge does not leave a
  `Started` record.
