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
  the path the family admits for the node, on which a saved Agent writes no graph.
  - **Deviation from the plan.** The plan took `A` = the activation receipt's thread. That thread is the member's own
    thread (the graph thread the Agent node runs on), and the admission refuses a family's root as an Agent thread
    (`application_children.rs:452`, the root filter), so every member call was refused. `A` is therefore
    `<receipt thread>/<node name>`.
  - **No collision.** `NT` never equals a member graph path: the admission refuses an `NT` that is an admitted thread
    (`application_children.rs:465`). Test: `a_nested_thread_never_collides_with_the_members_own_graph_paths` (real
    PG). It uses a member whose Agent node also owns `{thread}/{node}/inner`. Nested threads under the node and under
    `inner` are distinct and touch no graph writer. A family that admitted the derived `NT` as a graph path refuses
    the admission with no row (seen failing with the guard disabled).
  - `NT ≤ 480` bytes; an `A` holding `/pt1:` is refused, so nothing nests below a nested thread.
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

## Item 5, the lineage map

The coordinator's call-lineage map stays shared by the node and capped at `MAX_CALL_LINEAGE` = 1024
(`W/agents/application_tools.rs:614`). It already held every call of every member's Agent before FU-291 (it records
whole batches). FU-291 adds at most 8 durable pipeline calls per member, 512 for a 64-member Map. Raising or
reshaping the cap is **FU-330**.

**Call 1025, before this branch:** `observe_call_lineage` returned `resource_exhausted`. Both callers mapped every
error to `application_event_channel_error` (`ErrorCategory::Internal`, `elitea_nested_agent.event_channel_unavailable`):

- `application_tools.rs:5078`, a saved Agent's own event loop;
- `:5522`, an ordinary root's streaming Agent.

The run ended Internal, and a member ended as a broken scope with no code (both seen in the tests below, failing first).

**Now:** `lineage_observation_error` (`application_tools.rs:6282`) answers the bound with
`CALL_LINEAGE_LIMIT_CODE` = `elitea_nested_agent.call_lineage_limit`. Any other observation error (a changed or
malformed lineage) is still a broken channel.

- The run's terminal is the registered `ResourceExhausted` kind ("The execution exceeded an approved resource
  limit.", not retryable; `protocol/output.rs:852`).
- A nested Agent reports it to its parent model as a typed tool error (`child_failure_report`).
- A member's Agent node ends typed and sends that code (`ChildFailure::Refused`, `pipeline/scoped_runtime.rs:845`).

Tests, at the limit and past it:

- `the_call_lineage_store_admits_1024_calls_and_refuses_the_1025th_typed` (1024 admitted, a replayed call is not a
  new entry, the 1025th typed, a changed lineage still a broken channel);
- `the_call_lineage_limit_ends_the_node_with_its_typed_resource_limit`.

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
| Worker × response stored, call batch not yet recorded | **F** `invalid_checkpoint` (pre-existing, also for saved-Agent tools; **FU-329**) | F | seen in a non-gated kill |
| Worker × call batch recorded, before the nested first save | R: replay, fresh nested start | F | replay test; kill tests (admission before effects) |
| Worker × nested pipeline between nodes | R: re-enters the nested frontier | F | risk-1 test (8c3f6086e), kill tests |
| Worker × nested LLM node, mid-model | R | F | `…killed_inside_…` (Map, Parallel) |
| Worker × nested LLM node's own tool in flight | I read-only (replayed) / F effectful (never repeated, typed to the Agent) | F | T1d kills (Map, Parallel) |
| Worker × nested direct toolkit/MCP node, call started | F (journal Started; never repeated, typed to the Agent) | F; the nested node still journals | T1e kills (Map, Parallel) |
| Worker × call recorded, nested thread admitted, waiting for the shared tool | R: replay, fresh nested start | F | T4 kill |
| Worker × nested pipeline completed, Agent not answered | R: stored result, never re-run | F | `…killed_after_…` (Map, Parallel) |
| Main restart / lease loss × above | as the Worker rows (takeover re-derives `NT`; a superseded reclaim runs nothing) | F | admission takeover test; T2 kill |
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
- Real PG, `state::postgres_checkpointer_tests::tool_pipeline_admission` (CI prefix, `#[ignore]`): 7 (admission,
  foreign threads, 8/9, takeover, T6, cost budget, no collision).
- Real PG, `pipeline_fanout_effect_pg_tests` (production assembler; runs whenever `ELITEA_TEST_DATABASE_URL` is set):
  Map and Parallel runs (2), T5 Code refusals (2), and 12 two-process kills (see "The member kills"), plus their
  child process test.
- Real PG, `tool_pipeline_admission` also: `a_nested_thread_never_collides_with_the_members_own_graph_paths`.
- Unit: the 1024/1025 lineage-cap tests (2).
- Go: `TestFanoutAgentToolPipelineUnsupportedShowsItsReadableText`, the registry-parity table extended; faults gate.

Deleted (they only pinned the lifted refusal): `postgres_a_map_item_agent_calling_a_saved_pipeline_is_refused_readably`,
`postgres_a_parallel_branch_agent_calling_a_saved_pipeline_is_refused_readably` and their helper.

Fixtures: the real-PG tests build their pipelines from YAML in the test and fake Main only as the claim and fence
source (`test_session_authority_for_reclaim`) and the model gateway; the reclaim is the production inspect →
authorize → assemble. No claim rests on Main logic. Real Main is proven in the browser (FU-321 is the harness gap).

## The member kills (T1a–T1f, T2, T4)

All of them are two OS processes on real PostgreSQL, through the production assembler and the production reclaim
(`postgres_member_pipeline_tool_child`, `member_pipeline_tool_kill`). The recovery's model outcomes are routed by
what each request carries: a nested node's request, or a member Agent's report, which holds the nested answer.

**The gated kill wait (FU-329).** The crash process dies only once every member it kills is past its call batch:

- a watcher waits for the nested threads' durable frontier;
- or the nested tool waits until every member is inside it.

A member killed between its stored model response and its recorded batch is F today. That window is
`invalid_checkpoint` on restore, and it is pre-existing for saved-Agent tools too. It is **FU-329** (its own plan,
after FU-323), so the tests do not kill there; it is left as it is.

| Test | Kill point | Recovery | First run |
|---|---|---|---|
| `…killed_inside_its_pipeline_tool_resumes_on_the_same_nested_thread` (Map, Parallel) | nested LLM node mid-model | R, same `NT`, first request is the nested node's (C2) | failed until C2 |
| `…killed_after_its_pipeline_tool_finished_reads_the_stored_result` (Map, Parallel) | nested pipeline done, Agent report held | R, stored result, no nested run, no new nested row (C3) | — |
| `…killed_inside_its_nested_llm_effectful_tool_never_repeats_it` (Map, Parallel) | nested LLM node's effectful MCP call, boundary durable | F: not repeated, nested model not asked, the Agent reads `RECOVERY_REFUSED` | passed (the node's own boundary) |
| `…killed_inside_its_nested_llm_read_only_tool_replays_it` (Map, Parallel) | nested LLM node's read-only call | I: the stored nested batch replays, the read runs once more | **failed: F instead of I**; fixed below |
| `…killed_inside_its_nested_direct_tool_never_repeats_it` (Map, Parallel) | nested direct MCP node, lent journal Started durable | F: not repeated, typed to the Agent | passed; without the lent journal the nested direct node fails before its call |
| `postgres_a_superseded_reclaim_runs_nothing_and_the_newer_claim_resumes_the_nested_thread` (T2) | nested LLM node; claim 2 authorized at inspect, claim 3 takes the run before claim 2 assembles | claim 2's assembly refused (session writer moved), no model request, no nested row; claim 3 resumes and owns the nested writer | test expectation corrected only |
| `postgres_eight_map_items_killed_with_four_in_flight_keep_their_own_nested_threads` (T4) | 8 items at concurrency 4; 4 in flight | 8 distinct `NT`, the 4 killed replay (2 requests each), the 4 others run whole (3), every item its own answer | test expectation corrected only |

**T1d fix.** A durable nested pipeline's LLM nodes get the D2 recovery of an inline fan-out LLM branch: pending-model
recovery and read-only replay (`with_member_llm_recovery`, `W/agents/graph/compiler.rs:512`;
`for_member_recovery`, `W/agents/pipeline.rs:2062`, used at `:1929`).

- It is a new factory flag, `member_recovery`.
- `for_fanout_branch` would also silence the node's events (`event_sender`). That was tried, and it broke the member
  streaming tests.

**T4 finding.** One pipeline tool is shared by the members of one Agent node, and its child run holds the tool's
node-event drain (`W/agents/graph/node_events.rs:620`). So the members' calls of one pipeline tool run one at a time.
This is correct (events never cross calls), but a 4-wide Map runs its nested pipelines serially. New follow-up (perf).

## Not done, limits

- With recovery off, a nested direct node journals one row per node (it is lent the journal in both profiles).
- T1e covers the "started" journal state; the "committed" and "undispatched" states are the FU-248 journal's own
  tests at the root and at depth (`child_direct_scenario`), not repeated under a member.

## Code review (high, full diff against main), 2026-11-11

Each fix was written test-first; for 1–5 the test was seen failing on the code before the fix.

1. **Retry classification.** `checkpoint.storage_failure` was retried 3× and answered as a retryable
   `DEPENDENCY_UNAVAILABLE`, but the store says only `StorageUnavailable` is retryable. There is now one source:
   `PostgresCheckpointError::CODES` and `checkpoint_error_code` (`W/state/postgres_checkpointer.rs:174,189`).
   `is_transient_checkpoint_error` (`W/agents/application_pipeline.rs:1158`) reads that classification.
   - Tests: `a_permanent_storage_failure_is_refused_and_never_retried`. Before the fix it was answered retryable
     after 3 attempts; now it is refused after 1.
   - `the_code_table_is_each_errors_code_and_retryability` pins the table against each variant's `code()` and
     `retryable()`.
2. **A durable D3 bound** (one more read per admission; the budget re-measured at p50 2.6 ms, p99 4.5 ms). The 8-per-member and 129-total bounds counted only the in-memory registry, which a
   takeover rebuilds empty. Admission now counts the member's durable nested writer rows (one read,
   `durable_tool_threads`, `W/state/postgres_checkpointer/application_children.rs:445`). It checks them against the
   bounds before activating (`:524`); a thread that already has its writer re-admits.
   - Test: `a_refused_ninth_call_stays_refused_after_a_takeover` (real PG, two OS processes: claim 1, then claim 2
     in a fresh process). Before the fix, the second process admitted the 9th call.
   - After the fix it is refused again, an admitted call re-admits, and the rows stay 8.
   - The per-call budget test now takes a new member every 8 calls.
3. **No in-memory fallback in a member.** Every fan-out member's Agent call now carries `MemberToolPipelines`, with
   `admission: None` when its family lends none (`W/agents/pipeline/scoped_runtime.rs:350`). A pipeline call there is
   refused typed (`pipeline.tool_thread_unbound` → `UNSUPPORTED_CAPABILITY`, a tool error to the model,
   `application_pipeline.rs:1614`); it never runs in memory.
   - Test: `a_member_call_without_an_admission_refuses_typed_and_runs_nothing`. Before the fix the pipeline ran in
     memory ("HANDLED").
4. **No fresh batch id.** A durable batch whose replay key is malformed now refuses (`agent_recovery.invalid_checkpoint`)
   instead of taking the event's own id (`original_batch_id`, `W/agents/model_checkpoint/delegation.rs:296`). A
   non-durable batch needs no id and is unchanged.
   - Test: `a_malformed_durable_batch_key_refuses_and_runs_nothing` (an empty key and a 513-byte key). Before the
     fix the tool ran.
5. **The shared total's own answer.** `check_room_for` (`W/agents/graph/tool_pipeline_threads.rs:197`) refuses the
   129 total with `pipeline.tool_thread_total_limit`. Its text names the 129 checkpoint threads.
   - Tests: `the_shared_thread_total_reads_its_own_typed_tool_error` (the 129th thread admitted, the 130th refused
     with the total's code and text).
   - `each_bound_refuses_with_its_own_code`. Before the fix both were the per-member code and text.
6. **Perf: the receipt is built and parsed once.** It is serialized once per call (`durable_tool_receipt`,
   `tool_pipeline_threads.rs:247`) and inserted as is on every event. Each projector parses it once and derives once
   (`proved_receipt`, `:280`): the browser projector's check went from 2 parses and 2 SHA-256 derivations to 1 and
   1, and the member projector does 1 and 1. Covered by the existing receipt, projector and member-run tests.
7. **One source for checkpoint codes** (with 1): the operator line's `error_code` reads `checkpoint_error_code`, then
   the nested thread's own codes (`tool_pipeline_error_code`). There is no prefix matching of its own.
8. **The dead refusal is deleted.** `FanoutChildRefusal::AgentCallsPipeline` is gone, with its arms and its
   assembly mapping. Deleted test: `an_agent_member_calling_a_saved_pipeline_ends_as_its_registered_text`
   (`execution/native_agent_lifecycle.rs`), which only exercised that variant. The wire kind
   `RuntimeFailureKind::FanoutAgentCallsPipeline` and its text stay for spooled frames, as the older-Main fallback.
9. **One batch-id check.** `valid_batch_event_id` (`W/agents/application_tools.rs:120`) is shared by the record, the
   replay key and the lineage. `model_scope.rs:468` reads the member context once, not once per tool name.
10. **Mapping.** FU-322 appears once: the ordinary (non-graph) root's pipeline tool.

## Real-browser evidence

Pending (orchestrating session).

## Follow-ups

- **FU-319**: Code in a pipeline tool (Main needs the tool-lineage edge).
- **FU-323**: a root saved-Agent node as a recovery frontier (the root pipeline tool stays F until then).
- **FU-329**: the stored-response-before-batch window (polish, after FU-323).
- **FU-330**: raising or reshaping the 1024-call lineage cap (polish); call 1025 is typed here.
- **FU-322**: the ordinary (non-graph) root's pipeline tool, which still uses the in-memory checkpointer.
- **FU-331 (perf, polish)**: a shared pipeline tool's members run their calls one at a time, because each call drains
  the tool's node-event channel (`node_events.rs:620`).
- **FU-332**: an Agent node inside a pipeline tool (D4 b), refused here before the run.
