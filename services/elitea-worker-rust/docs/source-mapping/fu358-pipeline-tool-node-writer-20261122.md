# Effectful direct toolkit/MCP nodes in non-member pipeline tools (FU-358), 2026-11-22

Paths: `W/` = `services/elitea-worker-rust/src`.

## Business behaviour

Taken from the current platform: a non-member agent calls a saved pipeline as a tool. That pipeline runs its direct toolkit or MCP node and returns the result.

Before this change, both `root pipeline -> Agent node -> pipeline tool` and `ordinary chat agent -> pipeline tool` stopped on any effectful direct toolkit or MCP node with `PIPELINE_TOOL_UNAVAILABLE` ("A tool with external effects requires its current fenced node writer."). Fan-out members already ran the same pipeline tool durably through FU-291.

Deliberately not ported: unfenced external effects and "lost, run again from scratch". Every effectful direct tool node dispatches only after a claim-fenced `Started` append to PostgreSQL and never repeats on re-entry or process replacement.

## Design as built

- **Authority trait:** `PipelineToolJournalAuthority` (`W/agents/graph/node_recovery_runtime.rs:254-268`) lends a call-scoped `NodeRecoveryFactory` for one non-member pipeline-tool invocation.
- **Pipeline-root lending:** `ApplicationCheckpointers` implements `PipelineToolJournalAuthority` (`W/state/postgres_checkpointer/node_attempts.rs:256-277`). It anchors every `n1:` journal to `JournalRootFence::CheckpointRoot` (the run root row in `elitea_runtime.agent_graph_checkpoint_writers`).
- **Ordinary-chat-root lending:** `PostgresSessionService::pipeline_tool_journals` (`W/state/postgres_session.rs:607-617`) returns `SessionPipelineToolJournalAuthority` (`W/state/postgres_checkpointer/node_attempts.rs:279-319`). It anchors every `n1:` journal to `JournalRootFence::SessionRoot` (the 10-column primary key in `elitea_runtime.agent_session_writers`). Ordinary turns that never invoke a pipeline tool write zero checkpoint rows.
- **Strict fencing on open and write:** `PostgresCheckpointer::activate_pipeline_tool_journal` and `lock_current_writer` (`W/state/postgres_checkpointer.rs:483-582,1244-1255`) share-lock the run's root writer row before locking the `n1:` writer row. A newer claim that takes over the root writer fences both new opens and already-opened journal handles.
- **Domain separation and scope:** `pipeline_tool_journal_activation_id` (`W/state/postgres_checkpointer/node_attempts.rs:94-101`) hashes under domain `elitea.pipeline.tool-node-attempt-journal.v1\0`. `validate_pipeline_tool_call_threads` (`W/state/postgres_checkpointer/node_attempts.rs:146-184`) admits only `call_thread` strictly beneath `owner_thread/` and statically admitted subgraph descendants (`<= 512` bytes, no control bytes, no `..`, never `owner_thread` itself). Every open requires a non-empty `visit` (`tool_pipeline_visit(&thread_id)` at top level, `ApplicationNode` activation visit in subgraphs).
- **Per-call bound:** `MAX_PIPELINE_TOOL_JOURNALS_PER_CALL = 64` (`W/state/postgres_checkpointer/node_attempts.rs:26`) caps distinct journals per pipeline-tool call. Re-opening an already-opened journal consumes no new slot.
- **Wiring and compatibility (C4):** `ApplicationPipelineTool::invoke_child` (`W/agents/application_pipeline.rs:1877-1903`) mints one call-scoped factory per non-member call and binds it via `with_pipeline_tool_journals` (`W/agents/graph/compiler.rs:536-567`), including recursive subgraph participants (`W/agents/pipeline/parallel_application_resolver.rs:91-129`). Resumed pauses seed `APPLICATION_VISIT_STATE_KEY` when absent (`W/agents/graph/compiler.rs:1828-1832,3066-3074`). FU-291 member calls stay on `invoke_durable` unchanged. No database migration or wire-format change is added.

## Changed paths

- `services/elitea-worker-rust/src/agents/graph/node_recovery_runtime.rs`
- `services/elitea-worker-rust/src/agents/graph/application.rs`
- `services/elitea-worker-rust/src/agents/graph/compiler.rs`
- `services/elitea-worker-rust/src/agents/graph/mod.rs`
- `services/elitea-worker-rust/src/agents/pipeline/parallel_application_resolver.rs`
- `services/elitea-worker-rust/src/agents/application_pipeline.rs`
- `services/elitea-worker-rust/src/agents/application_tools.rs`
- `services/elitea-worker-rust/src/agents/pipeline.rs`
- `services/elitea-worker-rust/src/agents/ordinary.rs`
- `services/elitea-worker-rust/src/agents/session.rs`
- `services/elitea-worker-rust/src/agents/native_runtime.rs`
- `services/elitea-worker-rust/src/state/postgres_session.rs`
- `services/elitea-worker-rust/src/state/postgres_checkpointer.rs`
- `services/elitea-worker-rust/src/state/postgres_checkpointer/child_batch.rs`
- `services/elitea-worker-rust/src/state/postgres_checkpointer/application_children.rs`
- `services/elitea-worker-rust/src/state/postgres_checkpointer/node_attempts.rs`
- `services/elitea-worker-rust/src/agents/application_pipeline_tests.rs`
- `services/elitea-worker-rust/src/agents/application_tools/materialization_tests.rs`
- `services/elitea-worker-rust/src/agents/pipeline_fanout_effect_pg_tests.rs`
- `services/elitea-worker-rust/src/agents/pipeline_tool_node_writer_pg_tests.rs`
- `services/elitea-worker-rust/src/state/postgres_checkpointer_tests/run_root_fencing.rs`
- `services/elitea-worker-rust/docs/recovery-guarantees.md`
- `services/elitea-worker-rust/docs/source-mapping/README.md`
- `services/elitea-worker-rust/docs/source-mapping/fu358-pipeline-tool-node-writer-20261122.md`

## Performance

- Mechanism: zero extra transactions or checkpoint rows for ordinary turns that never invoke an effectful direct node in a pipeline tool (`W/agents/ordinary.rs:463-465`, `W/state/postgres_checkpointer/node_attempts.rs:279-319`). Per-call distinct journals are bounded by `MAX_PIPELINE_TOOL_JOURNALS_PER_CALL = 64` (`W/state/postgres_checkpointer/node_attempts.rs:26`). Each `lock_current_writer` adds one primary-key `FOR SHARE` lookup inside the existing transaction.
- Proving tests: `ordinary_session_pipeline_tool_journals_enforce_session_root_fence_and_takeover` asserts `0` rows in `agent_graph_checkpoint_writers` before any pipeline tool runs; `pipeline_tool_journals_enforce_scope_and_per_call_cap` proves the 64/65 cap on PostgreSQL 18.
- Measured result: all 7 end-to-end PG tests completed in 2.96 s; all 13 fencing/journal PG tests completed in 4.73 s.

## Durability

- Mechanism: `DirectToolNode` writes a claim-fenced `Started` journal row (`n1:<b64url>`) before external dispatch and `Completed` after projection (`W/agents/graph/direct_tool.rs:549-616`, `W/state/postgres_checkpointer/node_attempts.rs:196-254`). Parent `tool_may_have_started` boundaries plus `Started` journal state prevent repeat execution across Worker kills.
- Proving tests: `postgres_root_agent_node_killed_inside_effectful_direct_pipeline_tool_never_repeats_it` and `postgres_ordinary_agent_killed_inside_effectful_direct_pipeline_tool_never_repeats_it` (two OS processes on real PostgreSQL 18), plus `non_member_pipeline_tool_reentry_after_started_never_repeats_effectful_direct_node` (unit).
- Measured result: `1` external MCP call before kill, `1` durable `n1:` journal row, `0` repeats after Worker replacement (`RecoveryRefused` / `OrdinaryReclaimRefused`, `0` model calls on reclaim).

## Resilience

- Mechanism: bounded thread length (`<= 512` bytes), bounded descendants (`<= 128`, depth `<= 3`), bounded journals per call (`64`), explicit live-lease and root-writer fencing on open and write, typed failures (`PIPELINE_TOOL_UNAVAILABLE`, `PIPELINE_TOOL_FAILED`, `RECOVERY_REFUSED`), and fail-closed refusal when no fenced writer exists.
- Proving tests: `validate_pipeline_tool_call_threads_enforces_owner_prefix_traversal_and_bounds`, `non_member_pipeline_tool_without_lent_journal_still_refuses_effectful_direct_node`, `non_member_pipeline_tool_refuses_when_lent_journal_writer_is_superseded`, `pipeline_tool_journals_enforce_scope_and_per_call_cap`, `pipeline_tool_journals_fence_new_and_opened_handles_after_root_takeover`.
- Measured result: all limit, limit+1, lease-loss, and takeover assertions pass with `0` unpermitted dispatches.

## Security

- Mechanism: parameterized SQL only (`W/state/postgres_checkpointer.rs:509-582`); strict root-before-child lock order (`FOR SHARE` on root writer before child lock); fail-closed prefix/traversal/control-char validation (`W/state/postgres_checkpointer/node_attempts.rs:146-184`); domain-separated SHA-256 activation ID (`elitea.pipeline.tool-node-attempt-journal.v1\0`); non-empty visit required so a pipeline tool can never open an unscoped run-root journal or emit an operator reconcile interrupt; zero secrets in logs or errors.
- Proving tests: `pipeline_tool_journal_identity_is_domain_separated_from_root_and_subgraph_journals`, `validate_pipeline_tool_call_threads_enforces_owner_prefix_traversal_and_bounds`, `pipeline_tool_journals_enforce_scope_and_per_call_cap`, `pipeline_tool_journals_fence_new_and_opened_handles_after_root_takeover`, `ordinary_session_pipeline_tool_journals_enforce_session_root_fence_and_takeover`.
- Measured result: `0` out-of-scope or superseded writer rows created; `0` secret-scan hits on `git diff`.

## Recovery guarantee rows (rules §2)

| Component × phase | Class | Status | Mechanism (`path:line`) | Proof |
|---|---|---|---|---|
| Worker × P05 (effectful direct toolkit/MCP node in saved pipeline tool of non-member agent: pipeline root → Agent node → pipeline tool, or ordinary chat agent → pipeline tool), crash in flight | F (`RECOVERY_REFUSED`, never repeated) / I before `Started` | P | `W/agents/application_pipeline.rs:1877-1903`, `W/state/postgres_checkpointer/node_attempts.rs:196-319`, `W/state/postgres_checkpointer.rs:483-582,1244-1255` | Real PostgreSQL 18 two-process kills: `postgres_root_agent_node_killed_inside_effectful_direct_pipeline_tool_never_repeats_it`, `postgres_ordinary_agent_killed_inside_effectful_direct_pipeline_tool_never_repeats_it` (`1` call, `0` repeats); unit `non_member_pipeline_tool_reentry_after_started_never_repeats_effectful_direct_node` |
| Worker × P05 (takeover between journal open and `Started` append on non-member pipeline tool) | F (`writer_not_current`, `0` calls) | P | `W/state/postgres_checkpointer.rs:509-582,1244-1255` (`JournalRootFence::CheckpointRoot`, `JournalRootFence::SessionRoot`) | Real PostgreSQL 18: `pipeline_tool_journals_fence_new_and_opened_handles_after_root_takeover`, `ordinary_session_pipeline_tool_journals_enforce_session_root_fence_and_takeover` |

## Tests

All run against `services/elitea-worker-rust` with `--offline --locked --all-features`:

- `cargo fmt --all -- --check`: PASS.
- `cargo clippy --offline --locked --all-targets --all-features -- -D warnings`: PASS (`0` warnings).
- Unit suites (`application_pipeline_tests`, `node_attempts::tests`, `run_root_fencing` unit tests, `direct_tool_tests`, `node_recovery_direct_tool_tests`): **53 passed, 0 failed** (targeted direct-tool + FU-358) and **42 passed, 0 failed** (`application_pipeline_tests` + `node_attempts` + `run_root_fencing` unit).
- Real PostgreSQL 18 (`pgvector/pgvector:0.8.1-pg18`, disposable container torn down after run):
  - `pipeline_tool_node_writer` + `root_saved_agent_frontier` + FU-291 map direct-tool kill regression: **7 passed, 0 failed, 0 skipped**.
  - `run_root_fencing` + `direct_tool_journal` (`--ignored`): **13 passed, 0 failed, 0 skipped**.

## Fixtures and real-browser evidence

- Fixtures: synthetic pipeline YAML definitions, fake Main claim authority, and fake model gateway against real PostgreSQL 18 (`IsolatedPostgres`).
- Real-browser evidence: pending (no Docker image build or disposable stack run in this branch; deferred to PR / stack verification pass on shape 888 and 889).

## Follow-ups

- Resuming an in-flight saved pipeline tool of a root Agent node on its own durable nested thread (R instead of F) remains tracked under **FU-323b**.
