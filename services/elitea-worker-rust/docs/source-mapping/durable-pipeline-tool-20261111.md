# A fan-out member's saved Agent calls a saved pipeline, durably (FU-291), 2026-11-11

Plan and review: `handoffs/point5-wave1-20261008/fu291/PLAN.md` and `REVIEW-PLAN.md` (C1–C7, and the D8 decision
(C) + (B), which wins). Stacked on W3-2b (`feat/w3-2b-saved-code-children`, #1925). Paths: `W/` =
`services/elitea-worker-rust/src`, `M/` = `services/elitea-main/internal`.

Decisions applied: D1 (b) narrowed by D8 (B) to **members only**; D2 (a) derivation + fenced writer row + stored call,
no migration; D3 (a) 8 nested calls per member; D4 (a) LLM, State Modifier, Router, direct toolkit/MCP; D5 (a) a new
precise code, the old text kept registered; D6 (a) a re-call after a typed failure is a new call on a new thread.

## Business behaviour

Taken from the current platform: an Agent may call a saved pipeline as a tool; the pipeline runs its own graph on
the task it is given and its final output is the tool's result.

New on the replatform: the same Agent as a Map worker or Parallel branch. Before this change such a member was
refused before the run (`graph.fanout.agent_calls_saved_pipeline`). Each call now runs on a **durable nested thread of
the member** and survives a Worker loss: the call is replayed from its stored batch with no new model completion, the
nested pipeline re-enters where it stopped, and a finished nested pipeline answers its stored result.

Not ported, on purpose: "lost, run again from scratch". A nested effect that may have started is never repeated.

Not in this slice (refused before the run, readably, `graph.fanout.agent_tool_pipeline_unsupported` with a cause):
Code in the pipeline tool (FU-319), an Agent node in it, a pipeline reached through another Agent. Already refused:
a fan-out or a static pause inside it; a `hitl` gate under a graph root is skipped (as before).

A **root** Agent node (not in a Map or Parallel) calling a pipeline tool keeps its in-memory run: a Worker lost there
ends typed `RECOVERY_FRONTIER_UNSUPPORTED` (side PR #1933). That is F, not in scope; the fix is FU-323. An ordinary
(non-graph) Agent's pipeline tool is unchanged (follow-up below).

## Design as built

- **Nested thread (D-2).** `NT = A + "/pt1:" + b64url(SHA256("elitea.pipeline.tool-thread.v1\0", len‖A,
  len‖lineage))` (`W/agents/graph/tool_pipeline_threads.rs:76`). `A` is the member Agent node's own admitted path,
  `<member thread>/<agent node>` (the activation receipt's thread and node, `W/agents/pipeline/scoped_runtime.rs:349`):
  the path the family admits for the node, on which a saved Agent writes no graph. The plan named the receipt's
  thread as `A`; the receipt's thread is the member thread itself, which the admission refuses as a root, so the
  node path is used. `NT ≤ 480` bytes; an `A` holding `/pt1:` is refused, so nothing nests below a nested thread.
- **Admission (D-3).** `ApplicationCheckpointers::admit_tool_pipeline` (`W/state/postgres_checkpointer/application_children.rs:452`):
  `A` must be one of the family's own threads and not its root; `NT` must not collide with an admitted thread; per
  member one admission at a time; the bound is checked (`tool_pipeline_threads.rs:161`); the writer is activated under
  the `A` writer's exact claim and fence (`activate_fanout_child`), **then** registered (`:173`). Re-admission in the
  process returns the registered writer; after a takeover the same derivation re-activates the same thread.
- **One object (C1).** The registry `ToolPipelineThreads` is owned by the member's checkpointer family and handed to
  the member's event scope through `ToolPipelineAdmission::tool_threads` (`ParallelChildCheckpoint.tool_pipelines` →
  `PreparedParallelBranch` → `ParallelBranchExecution` → `ParallelEventScope.tool_threads`,
  `W/agents/graph/node_events_parallel.rs:84`). The test implementations of the child factory lend none.
- **Member authority (item 3).** The resolver rebinds each member's scoped Agent participants with the family's
  admission (`for_tool_pipelines`, `W/agents/graph/parallel_compiler.rs` `branch_journal_resolver`); the scoped
  runtime scopes `MemberToolPipelines` (admission, `A`) around the Agent call next to its event channel
  (`W/agents/application_tools.rs:329-350`), and `DrivenTask` carries it onto a spawned continuation
  (`W/agents/driven.rs:94`).
- **Durable call (D-4).** `ApplicationPipelineTool::invoke_durable` (`W/agents/application_pipeline.rs:1418` region):
  lineage from the coordinator (none: nothing runs); admit and read the latest row with an in-process retry of a
  transient store failure; a completed row answers its stored result (`completed_text`, the completion text the
  uninterrupted run's output mapper computes from the same final state) and is never re-entered or written; otherwise
  compile on the nested writer and run. Every forwarded event carries the private receipt
  (`stamp_durable_tool_thread`, stripped before output with the other private keys).
- **Replayable call (D-5, C4).** The member Agent's model scope lists its pipeline tools; they are durable only while
  `MemberToolPipelines` is in scope (`W/agents/model_scope.rs:473`). Their batch is recorded as
  `durable_delegation_pending` (737a3cf15) with the batch's original event id (C2, below).
- **Journals (D-6).** The nested thread's writer is lent as the direct tool nodes' journal, with a visit derived from
  `NT` seeded into the first frontier (`with_tool_journals`, `W/agents/graph/compiler.rs:498`).
- **Refusal (D-7, D5).** `reject_agent_calling_pipeline` (`W/agents/graph/parallel_compiler.rs:587`) reads
  `fanout_tool_pipeline_refusal` (`W/agents/events.rs:725`): a pipeline tool's cause is read from its definition when
  it is materialized (`PipelineDefinition::durable_tool_refusal`), a pipeline below another Agent is `NestedAgents`.
  New kind `FanoutAgentToolPipelineUnsupported`, its older-Main fallback `FanoutAgentCallsPipeline` (same wire code).

## Item 5, the lineage map (decision)

The coordinator's call-lineage map stays shared by the node and capped at 1024 (`W/agents/application_tools.rs:578`).
It already held every call of every member's Agent before FU-291 (it records whole batches), so its exposure is
unchanged; FU-291 adds at most 8 durable pipeline calls per member, 512 for a 64-member Map, inside the cap. A
per-member map would multiply the worst case by 64 (65 536 entries). A Map whose 64 Agents make more than 16 calls
each already hits the cap today (typed `resource_exhausted`); that is a pre-existing bound, registered below.

## C1–C7

- **C1, one source.** The projector admits a nested thread only by re-deriving it from the event's receipt (`A` and
  lineage), requiring `A` to be a non-root thread of this member and that exact thread to be registered for that `A`
  (`node_events_parallel.rs:297`, used at `:168`). Tests (seen failing first): `a_nested_pipeline_thread_is_proved_by_its_receipt_and_the_member_registry`
  (a correct derivation without a writer row, the string alone, another member's receipt, a changed lineage, the
  member root, no registry). The browser projector re-derives the same way for the active call
  (`durable_thread_for_projected_call`, `tool_pipeline_threads.rs:284`; `W/agents/events.rs` `descendant_thread`).
- **C2, replay asks the model nothing.** Replay re-emits the stored batch through `DelegationReplayModel`
  (`W/agents/model_checkpoint/delegation.rs:297`). **Finding:** ADK mints a new event for it, so the batch id in the
  lineage, and so `NT`, changed on every replay. Fixed: the record stores `batch_event_id`
  (`W/agents/model_checkpoint.rs:68`, set at `delegation.rs:173`), the replay carries it
  (`DURABLE_REPLAY_BATCH_KEY`), `original_application_batch_id` honours it (`W/agents/application_tools.rs:1906`).
  Tests: `a_replayed_durable_batch_keeps_its_original_event_and_call_lineage` (seen failing first); the kill tests
  assert each member's first request after the kill is its nested node's, and the same `NT`. A call whose lineage was
  never captured runs nothing (`a_call_without_its_captured_lineage_admits_and_runs_nothing`); a changed lineage
  derives another thread, which the projector refuses under the old receipt.
- **C3, receipt before repeat.** `a_completed_nested_thread_answers_its_stored_result_and_is_never_written` (seen
  failing first: without the rule the executor saves one more row) and the two-process
  `…killed_after_its_pipeline_tool_finished_reads_the_stored_result` (Map, Parallel): no nested model call, the Agent
  reads the stored text, no new nested row.
- **C4.** 737a3cf15 plus C2: an older Worker's strict decode refuses the record on its phase and on `batch_event_id`
  (`an_older_worker_refuses_a_durable_delegation_record_typed`). Forward: an older `tool_may_have_started` stays F.
- **C5.** Admission, activation and the latest-row read: `checkpoint.storage_unavailable` / `storage_failure` are
  retried in process (3 attempts) and then answered retryable (`pipeline.tool_thread_unavailable` →
  `DEPENDENCY_UNAVAILABLE`, `retryable: true`); only an absent or denied thread is refused. Tests:
  `a_locked_writer_table_answers_retryable_and_admits_once_released` (real PG, T6),
  `a_transient_store_failure_is_retried_then_answered_retryable`.
- **C6, node recovery off.** See the rows below.
- **C7.** Bounds: `NT ≤ 480` (480 admitted, 481 refused), 8 per member (8 admitted, the 9th refused typed and
  without a row), 129 total (129 admitted, the 130th refused), 64 members (unchanged). Logs: ids, stages and codes
  only (`durable_member_calls_log_no_task_result_lineage_or_thread`). Cost: see Performance.

## Performance

- New per call: one activation transaction and one latest-row read; the nested graph's rows equal a root run of the
  same pipeline, and `durable_delegation_pending` replaces `tool_may_have_started` one for one.
- `the_per_call_nested_admission_cost_is_within_the_child_overhead_budget` (real PG, min of 3 runs, 20 samples, gate
  p99 ≤ `CHILD_OVERHEAD_P99` = 30 ms): p50 2.7–3.5 ms, p99 10.9–22.3 ms over four runs on a shared machine at load
  ~16. A per-event receipt check is one SHA-256 over at most 4 KiB.

## Durability

Mechanisms as above; proofs: the admission real-PG tests (one row, idempotent, takeover re-derives and fences the
older writer), the replay test (C2), the two-process kills below.

## Resilience

Bounded inputs (receipt ≤ 4 KiB, `NT` ≤ 480, 8 per member, 129 per member family); typed refusals; transient store
failures retried then retryable; a pause inside a durable nested pipeline is refused (D-7 admits none).

## Security

- Authority: only the member's fenced family admits a thread, under its exact claim; the name grants nothing; the
  projector never admits from the string (C1). Main has no authorization change (D-1).
- Data: the receipt (lineage digest) is private metadata, stripped before output; nothing of the task, arguments,
  lineage or result is logged (C7 test). No secrets, SQL or URLs in errors.
- Version skew: C4.

## Recovery guarantee rows (rules §2), members only

| Component × phase | Recovery on (production profile) | Recovery off (Helm default) | Proven by |
|---|---|---|---|
| Worker × member Agent model call, before its call batch | R (model scope, existing) | F, run not resumed | existing |
| Worker × response stored, call batch not yet recorded | **F** `invalid_checkpoint` (pre-existing, also for saved-Agent tools) | F | seen in a non-gated kill; follow-up |
| Worker × call batch recorded, before the nested first save | R: replay, fresh nested start | F | replay test; kill tests (admission before effects) |
| Worker × nested pipeline between nodes | R: re-enters the nested frontier | F | risk-1 test (8c3f6086e), kill tests |
| Worker × nested LLM node, mid-model | R | F | `…killed_inside_…` (Map, Parallel) |
| Worker × nested LLM node's own tool in flight | I read-only / F effectful (existing rule) | F | not proven here (T1d) |
| Worker × nested direct toolkit/MCP node | R committed / I not started / F started (journal) | F; the nested node still journals | not proven here (T1e) |
| Worker × nested pipeline completed, Agent not answered | R: stored result, never re-run | F | `…killed_after_…` (Map, Parallel) |
| Main restart / lease loss × above | as the Worker rows (takeover re-derives `NT`) | F | admission takeover test; not as a two-process T2 |
| PostgreSQL × admission or read | retryable dependency failure | same | T6 tests |
| Supervisor | n/a (no Code in scope) | n/a | — |
| Root Agent node × pipeline tool | F `RECOVERY_FRONTIER_UNSUPPORTED` (#1933) | F | FU-323 |
| Code / Agent node / tier ≥ 2 in the pipeline tool | F before start, nothing runs | same | T5 tests (Code, Map and Parallel) |

## Tests

New or changed, all green on PostgreSQL 18 (disposable `postgres:18`):

- `W/agents/graph/tool_pipeline_threads_tests.rs`: 7 unit tests (derivation, 480/481, nesting, 8/9, 129/130,
  append-only, receipt).
- `node_events_parallel_tests.rs`: C1 projector test (1).
- `model_checkpoint/delegation_tests.rs`: C2 replay test (1); the C4 rollback test now also refuses the batch id.
- `application_pipeline_tests.rs` `durable_member_calls`: 7 (run, C3, D3, lineage, C5, log capture and its child).
- `execution/native_agent_lifecycle.rs` (1) and `protocol/output.rs` (1) for the new kind; the older-Main fallback
  table test gains the new pair (extended, not weakened).
- Real PG, `state::postgres_checkpointer_tests::tool_pipeline_admission` (CI prefix, `#[ignore]`): 6 (admission,
  foreign threads, 8/9, takeover, T6, cost budget).
- Real PG, `pipeline_fanout_effect_pg_tests` (production assembler; runs whenever `ELITEA_TEST_DATABASE_URL` is set):
  Map and Parallel runs (2), T5 Code refusals (2), two-process kills inside the nested LLM node (2) and after the
  nested pipeline finished (2), plus their child process test.
- Go: `TestFanoutAgentToolPipelineUnsupportedShowsItsReadableText`, the registry-parity table extended; faults gate.

Deleted (they only pinned the lifted refusal): `postgres_a_map_item_agent_calling_a_saved_pipeline_is_refused_readably`,
`postgres_a_parallel_branch_agent_calling_a_saved_pipeline_is_refused_readably` and their helper.

Fixtures: the real-PG tests build their pipelines from YAML in the test and fake Main only as the claim and fence
source (`test_session_authority_for_reclaim`) and the model gateway; the reclaim is the production inspect →
authorize → assemble. No claim rests on Main logic. Real Main is proven in the browser (FU-321 is the harness gap).

## Not done, limits

- T1d (nested LLM node's own tool in flight), T1e (nested direct toolkit/MCP node kill, with the lent journal), T2 as
  a two-process takeover between inspect and assembly, T4 (8 items, 4 in flight) are not written.
- The window between a member Agent's stored model response and its recorded batch is F (pre-existing, saved-Agent
  tools too). Closing it means treating an all-replayable stored response as a delegation replay on restore: a
  recovery-ordering change for every Agent tool, left for a decision.
- With recovery off, a nested direct node journals one row per node (it is lent the journal in both profiles).

## Real-browser evidence

Pending (orchestrating session).

## Follow-ups

- **FU-319**: Code in a pipeline tool (Main needs the tool-lineage edge).
- **FU-322**: as registered by the orchestrator for this slice.
- **FU-323**: a root saved-Agent node as a recovery frontier (the root pipeline tool stays F until then).
- New, to register: the ordinary (non-graph) root's pipeline tool; the response-before-batch window; T1d/T1e/T2/T4;
  the shared 1024-call lineage cap per Agent node; an Agent node inside a pipeline tool (D4 b).
