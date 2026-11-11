# A root saved-Agent node resumes from its model checkpoint (FU-323a)

- **Branch:** `feat/fu323a-root-saved-agent-frontier`, off `main` at `b6633e6e6` (after #1933 `cb6fd90cd` and #1934).
- **Plan and review:** FU-323 `PLAN.md` (approved), `REVIEW-PLAN.md` (D1–D9, conditions C1–C6). This is slice 323a
  (D3 split). 323b, a saved-pipeline tool in flight under a root Agent node, follows FU-291 (#1937).
- **Paths:** `W/` = `services/elitea-worker-rust/src`. Line numbers are on this branch's head.

## Business behaviour

- **Current platform (reference only).** A Python pipeline that loses its process mid-run is lost and started again from
  scratch. That behaviour is not ported.
- **Before (main, #1933).** A pipeline whose root Agent node runs a saved Agent, with the Worker lost anywhere inside that
  node, ended on reclaim with the typed `RECOVERY_FRONTIER_UNSUPPORTED` Agent-step text. Nothing was repeated and
  nothing resumed.
- **Now.** With recovery on (compose today; stock Helm after PLAN-REMAINING 0.7a, see C6 below), the reclaim proves
  where the Agent stopped and continues from there:
  - lost in the Agent's model call: the stored request is re-issued once, the answer lands once (R);
  - lost in a read-only tool: the batch is replayed, the tool runs once more, its result is used once (I);
  - lost in an Agent → Agent call: the outer Agent replays its delegated call, the nested Agent resumes from its own
    model scope (R);
  - lost after the answer was persisted, before the call receipt completed: the answer is served, no model call (R);
  - lost before the receipt or before the first model step: the Agent runs once, fresh (R; nothing had been sent);
  - lost in an effectful tool (MCP, toolkit, Code tool) or a saved-pipeline tool: the registered "may have started,
    not repeated" end, `RECOVERY_REFUSED` (F, never repeated; the pipeline tool becomes R in 323b);
  - a state that cannot be proven: `RECOVERY_FRONTIER_UNSUPPORTED` with the Agent-step text (F), and an operator cause.
- **Deliberately not ported / not in this slice:** fan-out members (already R), `WakePark` and `ContinuationPause`
  frontiers, HITL inside the Agent (`Paused`/`Resuming` receipts end typed), FU-324 kinds, drain-to-boundary (FU-325),
  Helm defaults.

## Changed paths

- Worker:
  - `W/agents/graph/compiler.rs` (`recovery_frontier_supported_with_agents` `:1120`, `saved_agent_frontier_node_with_agents` `:1165`)
  - `W/agents/session.rs` (inspect `:2300-2378`, `recovery_evidence_digest` `:2406` with `recovery.v3` `:2417`,
    `FrozenRecoveryLevel::admitted_agents` `:2489`, `PreAssemblyRecoveryLevel` `:2514`, transient load mapping
    `:1996,2168,2253`, activation mapping `:5410,5433`)
  - `W/agents/session_agent_frontier.rs` (new: `AgentFrontierProof`, `prove_agent_frontier` `:124`, causes, transient
    classification `:259-290`, unit tests)
  - `W/agents/model_scope.rs` (`scoped_identity` `:229`, `inspect_agent_scope` `:327`, `completed_answer` `:437`)
  - `W/agents/pipeline/scoped_runtime.rs` (`RootAgentReentry` `:154-185`, proof-bound re-entry `:351-366`)
  - `W/agents/pipeline/scoped_applications.rs` (`bind_application` takes the root re-entries)
  - `W/agents/pipeline.rs` (`frozen_agent_aliases` `:2145`, pre-assembly level `:1250`, assembly `:1446`, plumbing)
  - `W/agents/application_tools.rs` (`pipeline_call_marker_valid` `:5813`, `recovering_child_agent` `:5835`,
    `pipeline_child_call` `:5875`, `child_instruction_session_id` `:5899`, `application_tool_name` now `pub(crate)`)
  - `W/agents/graph/direct_tool.rs` (`ScopedCallKind::RootAgentRecovery` `:1315`, marker `:1349`)
  - `W/execution/checkpoint_recovery.rs` (transient inspect retained `:157`)
  - `W/execution/native_agent_lifecycle.rs` (new causes → `RecoveryFrontierAgentStep` `:1937`)
  - `W/execution/agent_delivery_processor.rs` (retained line once per window `:891,913`)
  - `W/diagnostics/cycle.rs` (new: `CycleWindows`), `W/diagnostics.rs`
  - `W/transport/openai_compatible_facade.rs` (test-only `ExitProcessPrintingRequest`)
- Registry: `libs/go/faults/registry/worker.json` (seven operator codes), generated `libs/go/faults/codes_gen.go`,
  `docs/errors/README.md`.
- Helm: `deploy/helm/elitea/values.yaml` (stale drain comment, D7).
- Tests: `W/agents/root_saved_agent_frontier_pg_tests.rs`, `W/agents/graph/compiler_tests.rs`,
  `W/agents/graph/parallel_compiler_tests.rs`, `W/agents/application_tools.rs` (tests), `W/execution/output_delivery_tests.rs`,
  `W/execution/agent_delivery_processor.rs` (fault-line test), `W/diagnostics/cycle.rs`.
- Docs: `services/elitea-worker-rust/docs/recovery-guarantees.md`, this mapping, the README entry,
  `docs/upgrading/2026-11-11-0-root-saved-agent-frontier-resume.md`.
- **Corrected plan refs (main moved since the plan):** `recovery_frontier_supported` is `compiler.rs:1104` (plan
  `:1056`); `inspect_checkpoint` `session.rs:2232` on the base (plan `:2218`); its refusal `:2286-2304` (plan
  `:2271-2290`); `recovery_evidence` `:2334` (plan `:2320`); `walk_child_frontier` `:1981` (as plan);
  `MAX_RECOVERY_LEVELS` `:2422` (plan `:2408`); pre-assembly inspect `pipeline.rs:1222-1258` (plan `:1206-1243`);
  assembly inspect `:1420-1443` (plan `:1402-1427`); `bind_scoped_agent` `:2699` (plan `:2656`); the scoped-runtime
  refusal `scoped_runtime.rs:297-301` (as plan); `fanout_child_agent` `application_tools.rs:5828` (as plan); marker
  validation `:5810-5823` (as plan).

## Design as built

- **D-1 gate.** `recovery_frontier_supported_with_agents` (`W/agents/graph/compiler.rs:1120`) admits an unowned Agent
  node whose alias is in `agent_aliases`. The two-argument form passes none, so every existing caller and pin is
  unchanged. Only `inspect_checkpoint` passes aliases, and only on an `Own` frontier (`W/agents/session.rs:2300`).
  `agent_aliases` are the attachments Main froze with `agent_type == "agent"`, with their frozen application version
  (`frozen_agent_aliases`, `W/agents/pipeline.rs:2145`), read from the command's tool snapshot in both inspect calls.
- **D-2 walk.** An admitted Agent node is proven (`prove_agent_frontier`) instead of walked; a saved-pipeline node is
  walked as before (`W/agents/session.rs:2343-2375`).
- **D-3 proof** (`W/agents/session_agent_frontier.rs:124`), read only:
  1. the receipt for the node at the row's step (`receipt_for_node`); an earlier visit's receipt or none is
     `NotStarted`;
  2. the receipt's activation must equal this root occurrence's (`PipelineApplicationScope::new("", definition)` →
     `activate(thread, node, step)`, which fixes the call id) and its call must name the frozen Agent
     (`elitea_agent_{app}_v_{version}`); otherwise `agent_receipt_mismatch`;
  3. `Completed` is served (existing); `Paused`/`Resuming` refuse `agent_step_paused`;
  4. `Started`: the Agent's child session is `child_instruction_session_id(root thread, call id, agent name)` (the one
     derivation the running child uses, `W/agents/application_tools.rs:5899`), its model scope
     `scoped_identity(...)` (`W/agents/model_scope.rs:229`, shared with the running scope). `inspect_agent_scope`
     (`:327`) classifies: no session or an event-less session → `Fresh`; a completion receipt → `Answered(event id)`;
     a restorable step of this execution, generation and definition → `Restorable(digest)`; `ToolMayHaveStarted` →
     `RECOVERY_REFUSED`; another execution's checkpoint → `agent_step_foreign_execution`; events with no checkpoint →
     `agent_step_session_without_checkpoint`; anything that does not validate → `agent_step_invalid`.
  5. Binding: `recovery.v3` (`W/agents/session.rs:2417`) adds each proof's `(node, alias, application, step, call_id,
     class, digest | event id)`; with no Agent proof the v1/v2 inputs are untouched (golden test).
- **D-4 re-entry.** Assembly turns the proven set into `RootAgentReentry` (`W/agents/pipeline.rs:1446`), handed only to
  the root registry's Agent-node runtimes. `GraphCallOutcome::Started` on a non-fresh, non-fan-out visit is admitted
  only when `take(node, step, call_id)` succeeds (`W/agents/pipeline/scoped_runtime.rs:351-366`), and then runs as
  `ScopedCallKind::RootAgentRecovery` (marker `root_recovery`, never with `fanout`). Otherwise today's
  `may_have_started` refusal stands.
- **D2 read-only replay at the root.** `recovering_child_agent` (`W/agents/application_tools.rs:5835`) gives every
  pipeline Agent-node call read-only replay (as fan-out members), so a crash-time read-only batch is written
  `DelegationPending`, and pending model recovery to a fan-out or root re-entry.
- **Marker is in-process only.** The marker lives in `PipelineToolContext.actions` (`W/agents/graph/direct_tool.rs:1376`),
  passed straight to `tool.execute`; the scoped runtime never builds an event from those actions, and the container
  events are built fresh (`pipeline_application_event`). No new durable record kind, so no C4/forward guard (D6 a).
- **D-5 one card.** Not by skipping `send_container_call` (a deviation, see below): the re-entry's start is replaced by
  the receipt's durable `original_start` (`drain_event`, `W/agents/pipeline/scoped_runtime.rs`), and the root session
  stores a re-send of the same event once (`with_replay_equivalence`, `W/state/postgres_session.rs:650`). Proven by
  STEP 0: after the resume the root session holds exactly one start for the call, the receipt's.
- **D-6 transient inspect.** See Resilience.
- **Main and Web:** no change (D-7). Main's one-digest-per-claim grant (`AuthorizeAgentModelCheckpoint`,
  `services/elitea-main/internal/infra/db/repos/model_checkpoint_authority.go:14-80`) is relied on as is.

## Conditions

| Condition | Enforced in code | Proving test |
|---|---|---|
| C1 single, proof-bound re-entry | `RootAgentReentry::take` admits a proven `(node, step, call_id)` once (`W/agents/pipeline/scoped_runtime.rs:178`); the scoped runtime refuses any other non-fresh `Started` (`:351-366`). A retried assembly in the same claim re-inspects: a restored call it already sent rewrote the Agent's checkpoint (new invocation id), so the v3 digest differs and Main's one-digest-per-claim grant refuses it. | `a_root_reentry_is_proof_bound_and_taken_once` (unit); `postgres_a_root_agent_step_changed_after_its_grant_is_refused` (a changed step after the grant is refused, 0 model calls) |
| C2 only transient errors retained, per-cycle line, bounded | The stores' own `storage_unavailable` (connection, I/O, pool timeout, 08xxx, 40001, 40P01, 55P03, 57P0x, 25P03, 57014; `W/state/postgres_session.rs:2252`, `W/state/postgres_checkpointer.rs:1765`) carries an internal cause (`STORAGE_UNAVAILABLE_CAUSE`, `W/agents/session_agent_frontier.rs:299`) that changes no terminal and no message. Only the reclaim inspect turns it into the retained cause (`reclaim_inspect_failure` `:327`, applied at `W/agents/pipeline.rs:1272,1284`), and `W/execution/checkpoint_recovery.rs:157` retains only that. Decode, constraint and data errors keep their terminal. "Every frontier kind" means only these C2 transient classes, on any frontier kind's reclaim inspect (root row load, walk loads, continuation load, the proof's scope open, the root activation). For this cause the retained line is written once per 60 s window for the result code, however many executions; when the window runs out its summary (the count kept quiet, the distinct executions, up to 8 of their `support_ref`s) is written once, outside any delivery span, at the next recorded outcome or at processor close, and the window is evicted unconditionally (`W/diagnostics/cycle.rs:80,101`, `W/execution/agent_delivery_processor.rs:929,952`; windows owned by `AgentDeliveryProcessor`). Every other retained code keeps one line per delivery. Bound: the command deadline, checked before every inspect (`W/execution/agent_preparation.rs:2226`), with one redelivery per AckWait (60 s, `services/elitea-main/internal/transport/commandbus/contract.go:120`) until then, then a typed `DeadlineExceeded`. | `postgres_a_transient_read_failure_at_a_root_agent_inspect_is_retained` (real PG lock on the root and the scope writer rows; the assembly path under the same lock keeps its terminal), `a_transient_inspect_failure_keeps_the_checkpoint_delivery_for_the_next_reclaim`, `only_a_transient_storage_failure_is_retained`, `activation_failures_keep_their_message_and_carry_no_reclaim_cause`, `only_transient_inspect_retains_are_windowed_with_a_summary`, `many_executions_in_one_window_are_one_line_and_one_summary`, `an_expired_window_with_nothing_quiet_is_evicted_without_a_summary`, `the_table_never_stays_full`, `postgres_a_transient_read_failure_at_a_direct_agent_reclaim_inspect_is_retained` (a direct agent's reclaim inspect, same classes and bound) |
| C3 one snapshot under the claim's authority (fence argument accepted) | Not one SQL transaction: the root row (with its receipt, one row) and the scope are read in two fenced reads. Argument: both are read after this claim activated the root session and root checkpoint writer rows (`activate_pipeline_postgres`, `W/agents/session.rs:1753`); a graph checkpoint write locks the root writer row and requires its claim (`W/state/postgres_checkpointer.rs:424-451`), and a model-scope write locks the root session writer row first (`lock_current_writer`, `W/state/postgres_session.rs:754-765`). An older claim therefore cannot change either between the reads; this claim writes nothing during inspect. | `postgres_a_takeover_between_a_root_agent_inspect_and_assembly_fences_the_older_claim` (the older claim writes nothing after the takeover); `postgres_a_takeover_of_the_agents_scope_during_the_proof_is_a_lost_writer` (a newer claim owning the scope when the proof opens it: `AuthorizationFailed`, as the root activation) |
| C4 the restored request is the stored one | `prepare_request` returns the saved request (`W/agents/model_checkpoint.rs`). | STEP 0 compares the re-issued request body with the lost one (JSON equal). The model gateway sees the call twice (the lost one and the restored one): a cost, not a repeated effect, as for an ordinary root. |
| C5 one card, keyed on the durable receipt | Re-entry forwards the receipt's `original_start` (same event id); the session stores it once. An answer served from the Agent's completion receipt is not forwarded again (round 1 #3). | STEP 0: one start event in the root session, the receipt's id; T3: no second copy of the answer |
| C6 operator note | This mapping and the upgrade note. | — |

## Tests

| Test | Proves | Failing first |
|---|---|---|
| `postgres_a_root_saved_agent_node_lost_mid_model_resumes_from_its_model_checkpoint` (STEP 0 / T1) | crash in the Agent's model call: one `model_pending` under claim 1, frontier `["delegate"]`; recover: exactly 1 model request, equal to the lost one; answer once; receipt `Completed`; frontier advanced; one start event (the receipt's); 0 advisory locks | On `b6633e6e6`: `inspect: … [pipeline.recovery.saved_agent_frontier]`, 0 model calls (commit `698aee74a`) |
| `postgres_a_root_saved_agent_lost_before_its_first_model_step_runs_once` (T2, T2b) | before the receipt, and after it before the first model step: one model call, answer, frontier advanced | — (DB fixture, see Fixtures) |
| `postgres_a_root_saved_agent_answered_before_its_receipt_completed_is_served` (T3) | answer persisted, receipt `Started`: 0 model calls, the answer | gate reverted: `saved_agent_frontier` |
| `postgres_a_root_saved_agent_lost_in_a_read_only_tool_replays_it` (T4) | crash-time boundary is `delegation_pending` (the crashing tool checks it); 1 tool call, 1 model call | gate reverted: fails |
| `postgres_a_root_saved_agent_lost_in_an_effectful_tool_is_refused_and_not_repeated` (T5) | `agent.recovery_refused`, 0 tool and 0 model calls, operator line `agent_step_may_have_started` | gate reverted: fails |
| `postgres_a_root_saved_agent_lost_in_its_nested_agents_model_call_resumes` (T6) | Agent → Agent: 2 model calls (nested restored, outer answer) | gate reverted: fails |
| `postgres_a_root_saved_agent_node_lost_mid_run_is_refused_typed_on_reclaim` (T7, #1933's test, changed) | pipeline tool in flight: now `RecoveryRefused RUNTIME_ERROR_CODE_V1_RECOVERY_REFUSED`, 0 model calls, one WARN `agent_step_may_have_started`, task never logged (was `RECOVERY_FRONTIER_UNSUPPORTED`) | gate reverted: the #1933 outcome |
| `postgres_a_takeover_between_a_root_agent_inspect_and_assembly_fences_the_older_claim` (T8) | `AuthorizationFailed`, nothing written, 0 model calls | gate reverted: fails |
| `postgres_a_root_saved_agent_lost_twice_mid_model_resumes_again` (T10) | claim 2 dies in the restored call; claim 3 resumes: 1 model call | gate reverted: fails |
| `postgres_a_forged_root_agent_frontier_is_refused_typed` | receipt naming another Agent, another execution's checkpoint, another generation, a session without checkpoint: each its own cause, fault line, 0 model calls, no task text in logs | gate reverted: fails |
| `postgres_a_root_agent_step_changed_after_its_grant_is_refused` | a consistent edit of the step after the grant: refused at `authorize the lifecycle`, 0 model calls | gate reverted: fails |
| `postgres_a_transient_read_failure_at_a_root_agent_inspect_is_retained` (T9) | a held root writer row, then a held scope writer row: each inspect fails with the retained cause; released: resumes, 1 model call | retain path reverted: fails |
| `postgres_the_root_agent_proof_cost_is_measured` | Budget (Performance) | — |
| `a_transient_inspect_failure_keeps_the_checkpoint_delivery_for_the_next_reclaim` (T9, lifecycle) | transient: no settlement, no retire; non-transient: settlement and retire | retain path reverted: fails |
| `a_saved_agent_node_is_a_frontier_only_for_an_admitted_agent_alias` (U1) + `parallel_compiler_tests` case | gate matrix | — |
| `v1_and_v2_evidence_digests_are_unchanged`, `v3_evidence_binds_every_field_of_the_agent_proof` (U3) | v1/v2 equal the base formula and golden values; every v3 field changes the digest | — |
| `a_root_reentry_is_proof_bound_and_taken_once` (U5, C1) | only Fresh/Restorable/Answered admit; once | — |
| `only_a_transient_storage_failure_is_retained`, `every_agent_frontier_refusal_ends_with_its_registered_kind` (D5) | classification; causes → `RecoveryFrontierAgentStep`, may-have-started → `RecoveryRefused` | — |
| `the_pipeline_call_marker_admits_only_the_fanout_flags` (U4, extended), `the_child_session_is_derived_from_parent_call_and_agent` | marker rules; one session derivation | — |
| `one_line_per_class_per_window_with_the_suppressed_count`, `retained_delivery_is_one_registered_fault_line` (extended) | per-window reporting | — |

V1 (version skew, gate reverted): with the agent aliases forced empty every new PG case fails with the #1933 refusal
`pipeline.recovery.saved_agent_frontier` (evidence kept locally), which is what an older Worker does during the
rollout: F typed, nothing repeated. V2: no durable record kind or key changed (receipts and model checkpoints are
written exactly as before; `root_recovery` is in-process only).

Not covered by a dedicated test:

- **T1c** was to prove the narrow window after the model returned tool calls and before the tool boundary was written
  (a completed model event after the `ModelPending` marker). The pre-existing D2 rule refuses it
  (`W/agents/model_checkpoint.rs` restore: a completed model event after the marker is not an unfinished step); at the
  root this now shows as `agent_step_invalid` at inspect, never a repeat. Left as a gap with its own plan: FU-329.
- **T5b** was to prove an approved (sensitive) call in flight inside the root Agent. That call's dispatch journal on
  the root session decides it (recorded outcome served, dispatched without outcome refused, never started runs once;
  `W/agents/dispatch_journal.rs`), unchanged by this slice; its member twin is proven by
  `postgres_an_approved_agent_member_call_in_flight_at_a_crash_ends_typed`. No root-level test here: a gap.
- **T5c** was to prove a Code tool in flight inside the root Agent. A Code tool is neither an agent tool nor read-only,
  so `before_tool` writes `ToolMayHaveStarted` and the reclaim ends `RECOVERY_REFUSED` exactly as T5 proves for an MCP
  tool; no Code journal applies to an Agent's Code tool. Covered by the T5 rule, no Code-specific test.
- Also unchanged and not re-tested at the root: the delegated-call limits (8/9, `delegation.rs`), the receipt limits
  (128 receipts, 256 KiB), tier 3/4 admission, and two Agent frontier nodes in one row.

## Review round 1 (code-review high on `origin/main...ecd3eaa91`)

| # | Finding | Verdict | Fix | Test (seen failing first) |
|---|---|---|---|---|
| 1 | a scope-open writer loss or invalid scope became a terminal `DependencyUnavailable` | real | the proof maps scope errors through `session_code_error` (`W/agents/session.rs:5420`), the one classification the root activation uses (`W/agents/session_agent_frontier.rs:242`) | `postgres_a_takeover_of_the_agents_scope_during_the_proof_is_a_lost_writer` (was `native_agent.dependency_unavailable`, now `native_agent.authorization_failed`), `a_scope_open_failure_is_classified_as_the_session_activation` |
| 2 | inspect accepted `ContextPending` that restore refuses | real | the inspect passes the restore's predicate: the run's admitted context plan, which a nested Agent takes as is (`W/agents/assembly.rs:517`, `W/agents/session_agent_frontier.rs:233`) | forged case "a context-preparation step while the run does not compact" (was a mid-run `agent.legacy` failure after the grant, now `agent_step_invalid` at inspect) |
| 3 | an `Answered` re-entry forwarded a second copy of the Agent's answer | real (live frames; the root session persists only the call result) | the copy served from the completion receipt is marked in process (`W/agents/model_scope.rs:60,512`) and never forwarded (`W/agents/application_tools.rs:5047`) | T3 now asserts the frames that carry the answer (was `agent_llm_chunk,agent_llm_end,partial_message` twice, now only the call result and the pipeline answer) |
| 4 | a process-wide window per result code hid other executions | real | windows owned by `AgentDeliveryProcessor`, only for the transient-inspect code (reworked in round 2: per result code with a summary); every other code one line per delivery as before | `only_transient_inspect_retains_are_windowed_per_execution` (was one line for three deliveries) |
| 5 | activation messages changed and every activation carried the reclaim cause | real | activation errors keep their messages and carry an internal storage cause; only the reclaim inspect adds `inspect_transient` | `activation_failures_keep_their_message_and_carry_no_reclaim_cause` (was the wrong message), T9 assembly-path assertion |
| 6 | module lines between a doc comment and its function | real | moved (`W/agents/session.rs:2448`) | — |
| 7 | duplicated fault block | done | `log_refusal` / `refused` helpers; `AgentFrontierClass` kept (folding it is not trivial) | — |
| (b) | `(application, version)` binding condition | see Deviations | receipt stamp + assembly check | `postgres_a_saved_agent_edited_between_the_kill_and_the_reclaim_is_refused_typed` (was a resume) |

Failing-first output: `handoffs/point5-wave1-20261008/fu323/evidence/round1-failing-first.txt` (local).

## Review round 2 (delta, `ecd3eaa91..4c1c5585b`)

| # | Item | Fix | Test (seen failing first) |
|---|---|---|---|
| 1 | definition check: Answered, unproven, ids | `RootAgentReentry` carries each proof's stamp and whether it serves an answer, and the execution ids; `require_unchanged_agent` (`W/agents/pipeline/scoped_runtime.rs:206`) skips answers, names a missing stamp or digest `agent_definition_unproven` (registered), and refuses through `agent_frontier_refusal` (`W/agents/session_agent_frontier.rs:140,154`), so the line carries `generation` | T3 `answered_edited` (was refused `agent_definition_changed`, now served), `postgres_a_receipt_without_its_definition_stamp_is_refused_unproven` (was `agent_definition_changed`), the edited-Agent test's `generation` assertion (was missing), `the_definition_check_spares_answers_and_names_unproven_apart` |
| 2 | retained-line window | one window per result code (inspect_transient only), a summary with suppressed / distinct executions / ≤ 8 support refs when it runs out, unconditional eviction, flush at processor close | `only_transient_inspect_retains_are_windowed_with_a_summary` (was two lines for two executions), the three `diagnostics::cycle` tests |
| 3 | one classifier | `session_activation_error` is an exhaustive match again (`W/agents/session.rs:5417`); `store_failure` (`W/agents/session_agent_frontier.rs:328`) is the one string classifier for scope opens, checkpoint reads and the direct agent's session read | `the_string_classifier_agrees_with_the_session_activation` (every `PostgresSessionError` variant: same kind, message, cause) |
| 4 | direct agents | `OrdinaryNativeAgentAssembler::inspect_checkpoint` applies `reclaim_inspect_failure` (`W/agents/ordinary.rs:1406`); its session read goes through `store_failure` | `postgres_a_transient_read_failure_at_a_direct_agent_reclaim_inspect_is_retained` (was not retained) |
| 5 | strict stamp hex | `hex_digest` reused; decode accepts only 64 lowercase hex digits | `a_definition_stamp_round_trips_and_refuses_any_other_form` (uppercase was accepted) |
| 6 | v3 redundancy | `agent_definition` dropped from the v3 proof evidence (bound via the receipt in the root row); the v3 tests keep their field list | `v3_evidence_binds_every_field_of_the_agent_proof` |

**Accepted trade-off (review finding 3, no code):** an answer served from the Agent's completion receipt is not
forwarded again. If the Worker was lost between the Agent appending its answer to its model scope and the parent
forwarding it, the Agent's live message frame is lost for that run; the call's result (the tool response) and the
durable history still carry the text. The orchestrating session checks this after a reload in the browser.

Failing-first output: `handoffs/point5-wave1-20261008/fu323/evidence/round2-failing-first.txt` (local).

## FU-342, fixed inline (browser case 6, no kill)

- **Defect (pre-existing on `main` b6633e6e6, not caused by FU-323a).** A root Agent node whose saved Agent calls a
  saved pipeline tool failed on a fresh run with `agent_event.invalid_state` at the nested pipeline's first projected
  event ("The runtime operation failed."). The writer names the tool's checkpoint thread
  `{conversation_thread_id}/{call_id}` (`W/agents/application_pipeline.rs:1283-1285`, static form `:1321`), and a
  pipeline root gives the writer its session as the conversation thread (`plan.session_id()`,
  `W/agents/pipeline.rs:1521`, through `with_conversation_thread` at `:2772`). The projector expected the chat thread
  (`context.thread_id`) instead.
- **Fix (fail closed).** The projection context carries the writer's conversation thread, `tool_thread_id`
  (`W/agents/events.rs:376`): the chat thread for an ordinary root (`:774`), the pipeline's session for a pipeline root,
  taken from the same `session_id` the pipeline projection is built with (`:803`; `W/agents/session.rs:1281`), and kept
  by every nested tier (`:831`). The descendant thread check and the static-thread check compare against it exactly
  (`W/agents/events.rs:1745,1765`); nothing else is accepted.
- **Tests.**
  - `postgres_a_root_saved_agent_calling_a_saved_pipeline_tool_completes_fresh` (real PostgreSQL, production assembler,
    browser projector): failed first on this branch and on `main` with `agent_event.invalid_state`; now completes with
    three model calls and the root answer.
  - `a_pipeline_root_accepts_only_its_session_thread_for_a_nested_pipeline_tool`: only `{session}/{call}`; the chat
    thread, another session, another call, a bare prefix and a longer path are refused.
  - `an_ordinary_root_still_accepts_only_its_chat_thread`: unchanged ordinary behaviour.
- Browser case 6 (no kill, and with a Worker kill during the nested pipeline: typed `RECOVERY_REFUSED` until 323b):
  re-run by the orchestrating session.

## Gates (branch head, local)

| Gate | Result |
|---|---|
| `cargo test --offline --locked --lib --all-features` with `ELITEA_TEST_DATABASE_URL` (PG18), `ELITEA_REQUIRE_POSTGRES_RECEIPT_TESTS=1`, `ELITEA_TEST_DATABASE_GUARD=disposable-pg18` | round 2 (final): 3253 passed, 0 failed, 134 ignored (the `#[ignore]` set other CI steps run; NATS-secure tests skip without the server, as locally always) |
| new/changed tests in this PR | 19 real-PG two-process tests in `root_saved_agent_frontier_pg_tests.rs` (16 parents + 3 child processes), 1 real-PG direct-agent test, 9 unit tests in `session_agent_frontier.rs`, 1 stamp test, 1 compiler matrix + 1 parallel case, 2 marker/session tests, 1 lifecycle test, 3 window tests, 1 window fault-line test |
| `cargo clippy --offline --locked --all-targets --all-features -- -D warnings` | clean |
| `cargo fmt --all -- --check` | clean |
| `RUSTDOCFLAGS="-D warnings" cargo doc --offline --locked --no-deps --all-features` | clean |
| ignored-test guard (`check-worker-ignored-tests.mjs`) | 134 ignored: 120 run by 10 CI invocations, 14 allowlisted, 0 run by nothing |
| `cd libs/go/faults && go generate ./... && go test ./...` | `faults` ok, `grpcrecover` ok, `httperr` ok, `logx` ok, `cmd/faultsgen` no test files |
| `cargo deny --offline --all-features check advisories` (Worker) | fails on pre-existing advisories only (`astral-tl`, `rustybuzz`, `ttf-parser`, `unsafe-libyaml` unmaintained; five vulnerability advisories in existing crates); no dependency or lockfile change in this PR, so no new finding |
| Not run here | `/code-review`, `/security-review`, browser cases 1–6, a stack (orchestrating session) |

## Fixtures

All PG fixtures are produced by the production assembler in a crash process (fake model gateway, fake MCP, Main faked
only as the claim/fence source `test_session_authority_for_reclaim` and the digest grant `test_checkpoint_authorizer`).
Five states are then edited in the database, stated here: T2 and T2b delete what the lost process wrote after the
point named (receipt revisions and/or the Agent's model-scope rows); T3 deletes the root rows after the `Started`
receipt; the forged cases and the post-grant edit change stored text in place. No fixture was created in a UI.

## Performance

- **Budget.** On a reclaim only, per root saved-Agent frontier node and per inspect (two per reclaim): one model-scope
  writer activation (one transaction) and one session read (one transaction); no extra model, tool or Main call.
  Nothing changes on a run that is not a reclaim.
- **Measured** (`postgres_the_root_agent_proof_cost_is_measured`, local PG18 container, debug test build, 20
  pre-assembly inspects each, three runs): with a restorable Agent step p50 13.3–15.0 ms (max of 20: 67–122 ms, the
  first, cold iteration); with no receipt (no scope read) p50 7.0–7.6 ms (max 18–35 ms). The proof adds about 6–7 ms
  per inspect at p50, two inspects per reclaim, on a reclaim only. A release build on a production database was not
  measured.
- The retained-line window bounds operator log volume to one line per result code per minute.

## Durability

- Every crash window inside the node is named in the recovery rows below; the proving tests replace the process.
- Stable identities: the call id is derived from `(scope, thread, node, step)`; the Agent's child session from
  `(root thread, call id, agent name)`; its model scope from `(execution, generation, child session, agent)`. A reclaim
  changes only the claim, so the replacement Worker opens the same scope (T1, T10).
- Writer fencing: unchanged; an older claim writes nothing after a takeover (T8).
- No new durable record. The marker is in-process only.

## Resilience

- Typed ends for every unprovable state, with an operator cause each (registered, `worker.json`), and the user text of
  `RECOVERY_FRONTIER_UNSUPPORTED` (Agent step) or `RECOVERY_REFUSED`.
- Transient inspect failures are retained, bounded by the command deadline, reported once per window (C2).
- Lease loss: the takeover case (T8).

## Security

- **Authorization.** The proof only reads, through the claim-fenced stores, records of this execution: the root row
  must be the run's own (`Own`), the receipt must be this occurrence's call of the frozen Agent, the scope identity
  includes execution and generation. The proof is bound into the digest Main grants; a changed step after the grant
  is refused (`postgres_a_root_agent_step_changed_after_its_grant_is_refused`). Admission widens only to aliases Main
  froze as saved Agents in the authorized command.
- **Forged frontiers.** Receipt, scope execution, generation, missing checkpoint: refused typed
  (`postgres_a_forged_root_agent_frontier_is_refused_typed`).
- **Logs.** Fault lines carry `execution_id`, `generation`, `node_id`, `support_ref` and the code only; the tests
  assert the Agent's task text is in no log line. Evidence carries ids, a class and a digest or event id, no content.
- **Effects.** An effectful call in flight is never repeated (T5, T7).
- **Dependencies.** None added.
- `rules/security.md` categories: trust boundaries (no new identity source), object authorization (above), input
  parsing (receipts and checkpoints keep their bounded, `deny_unknown_fields` codecs), injection (no SQL built from
  input; test fixtures use `AssertSqlSafe` on literals), egress (none), secrets (none), supply chain (no change).

## Recovery rows (root saved-Agent node; recovery on)

On stock Helm (`agentModelCheckpointRecovery: false`, `agentNodeRecovery: false`) every row stays F until PLAN-REMAINING
0.7a flips the defaults (scheduled right after FU-323). Compose deployments (`agent_node_recovery: true`) benefit now.

| Component × phase | Class | Enforced by | Proven by |
|---|---|---|---|
| Worker × before the receipt | R (runs once) | proof `NotStarted` `W/agents/session_agent_frontier.rs:156-160` | T2 |
| Worker × receipt `Started`, no model step | R (fresh, nothing sent) | `inspect_agent_scope` `Absent` `W/agents/model_scope.rs:349-360` | T2b |
| Worker × mid model call | R (stored request re-issued once) | `Restorable`, re-entry `W/agents/pipeline/scoped_runtime.rs:351-366` | T1 (STEP 0), T10 |
| Worker × model returned tool calls, before the tool boundary write | F typed (pre-existing D2 rule) | `W/agents/model_checkpoint.rs` restore | not tested here (FU-329) |
| Worker × read-only tool in flight | I (replayed; was F) | `recovering_child_agent` `W/agents/application_tools.rs:5835` | T4 |
| Worker × effectful / MCP / Code tool in flight | F typed `RECOVERY_REFUSED`, never repeated | `inspect_agent_scope` `MayHaveStarted` | T5 |
| Worker × saved-pipeline tool in flight | F typed `RECOVERY_REFUSED` (was `RECOVERY_FRONTIER_UNSUPPORTED`); R in 323b | same | T7 |
| Worker × agent tool in flight (Agent → Agent) | R | `DelegationPending` + nested scope restore | T6 |
| Worker × answer persisted, receipt not `Completed` | R (served, 0 model calls) | `Answered` | T3 |
| Worker × receipt `Completed` | R (served) | `scoped_runtime.rs` Completed branch | existing |
| Worker × unprovable step (receipt mismatch, paused, invalid, foreign, no checkpoint) | F typed `RECOVERY_FRONTIER_UNSUPPORTED` | `prove_agent_frontier` | forged-frontier test |
| Worker × planned stop past the 30 s drain | as the phase reached | same | browser case 4 (pending) |
| Main restart / lease loss | as the Worker rows (takeover) | writer fence, one digest per claim | T8; browser case 5 (pending) |
| PostgreSQL × transient error at inspect | retained (`RecoveryRequiredNoAck`), bounded by the deadline | `W/execution/checkpoint_recovery.rs:157` | T9 |
| Older Worker during the rollout | F typed (#1933), nothing repeated | `saved_agent_frontier` refusal | V1 (gate reverted) |

## Real-browser evidence

**Setup.**
- **Stack:** a disposable stack `f323` with the production profile (recovery and node recovery on). verify-profile:
  50 passed, 0 failed.
- **Images:** Worker, Main and Web `fu323a-d01577bc8`, built from this branch's head, which is main `b6633e6e6` plus
  FU-323a. The Worker binary was extracted and contains the branch-only `pipeline.recovery.agent_definition_unproven`,
  `pipeline.recovery.agent_definition_changed` and `pipeline.recovery.inspect_transient`.
- **Browser:** Chrome, main-chat conversations, each checked after a reload.

**Fixtures.**
- Pipelines and Agents were created through the app's own API from the signed-in page. Every tool attachment and
  internal-tool toggle was made in the UI and checked in `entity_tool_mapping` / version `meta`.
- The kill helper `docker kill`s the Worker a fixed delay after the run's Nth `agent_model_request_started`, restarts
  it, and reports the claims, the model-request counts, WARN/ERROR lines and locks.

| Case | Chat / execution | Result |
|---|---|---|
| 1. Root Agent node runs a saved Agent with no tools (long answer); Worker killed 3 s into the model call | 887 / `845c9fc75a59d3e6e19c6d603da1ec22` | **R.** Reclaim `AGENT_MODEL_CHECKPOINT`; exactly 1 model request after the kill (the restored call). One call, one response and one answer in the session events. The full answer persists after reload. 0 WARN/ERROR, 0 locks. |
| 3. Agent → Agent: killed 3 s into the child Agent's model call | 888 / `b515f88c0e463ad14970661214a2c6a0` | **R.** Reclaim `AGENT_MODEL_CHECKPOINT`: the child call was restored, then the parent's final step ran (2 requests). The parent's answer persists after reload. Only the pre-existing FU-336 WARN. |
| 5. Worker kill, then `docker restart` of Main 2 s into the restored model step | 890 / `f11a0280cc4a240117c7fa308ee32f39` | **R over two reclaims.** The Main restart cost claim 2 its lease (`claim_lease_control_failure`), the delivery was retained, and claim 3 (`AGENT_MODEL_CHECKPOINT`) completed. One answer persists after reload. The model call was paid twice (a cost, not a repeated effect; FU-302). Claim 2's lost stream logs ERROR `native_agent.event_failed` / `agent.legacy` although the run recovers: FU-340 (it should be one operator WARN). 0 locks. |
| 2. Built-in tools (Attachments read-only, memory effectful) | 889 / `5985af576ad62b24e56676a6a40972a6` | **Not run, blocked by FU-335 (probable).** The saved Agent at the root Agent node was offered **0 tools** (`tool_count: 0`) although Attachments and memory are enabled on its version. The model then *claimed* in text to have listed attachments and saved a note; it did neither (0 memory entries). The tool-call recovery rows are proven by the real-PG tests. |
| 6. Pipeline tool in flight under a root Agent node | 891 / `59ff027e125d8fd77d9dcff454ddb6c6` | **Not reached; pre-existing defect FU-342.** The fresh run fails before any kill: `agent_event.invalid_state` at the nested pipeline's first projected event. It fails identically on main `b6633e6e6` (`fu323/evidence/case6-on-main-b6633e6e6.txt`). The cause is a thread mismatch between `application_pipeline.rs:1283-1285` and `events.rs:6918-6920`. The typed refusal for a pipeline tool in flight is proven by the real-PG test. |
| 4. Planned stop: `docker stop -t 45` (stands in for the compose `stop_grace_period: 45s` override) 3 s into a turn | 892 / `a95156abf9c9a20c819a920bdf810e96`; 895 / `c0e46d6c76330da2b48be98d1136039b` | **The drain completed the turn.** Both turns finished within the 30 s drain (12 s and 19 s). A turn that outlasts the drain could not be produced: a single very long stream (about 6000 words) fails by itself at about 23 s with no stop (chats 893 and 894: `failure_diagnostic` `disabled_or_rate_limited` under `agent.legacy`, FU-341). A drain that expires mid-turn is the same drop-then-reclaim path as a crash (SHUTDOWN-FACTS), proven by case 1 and the real-PG tests. Not run as a drain expiry. |

No Supervisor kill: no Code is in scope. The negative "Thought for -1 h…" label after a failed run is FU-327.

## Deviations from the plan

- **D-5:** `send_container_call` is not skipped on re-entry. The scoped runtime already replaces the child's start with
  the receipt's durable `original_start`, which the root session stores once; skipping would lose the start when the
  crash came before its first forward. Proven in STEP 0 (one start event, the receipt's).
- **D-3.4 / D1 (accepted with a condition, round 1 (b)):** Main's `saved_agent_fingerprint` cannot be read before
  assembly, so the frozen `(application, version)` is bound instead, plus the definition digest the call started with.
  The condition asked to show that restore runs a frozen copy, or that a version row cannot change in place. Neither
  holds: every assembly re-reads the version from Main, which materializes its current content and recomputes the
  digest (`services/elitea-main/internal/infra/storage/runtime_application_version.go:239-252`), and only published or
  embedded versions are refused an in-place edit (`services/elitea-main/internal/infra/db/repos/applications.go:995-1000`;
  a draft can change). So the call's receipt is now stamped with Main's digest when the call starts
  (`prepare_graph_call_stamped`, `W/agents/pipeline/scope_receipts.rs:399`, key `:27`, read `:42`), the proof binds it
  into the v3 evidence, and assembly refuses a re-entry that runs the Agent again (fresh or restorable) when the current digest differs:
  typed `pipeline.recovery.agent_definition_changed`, the Agent-step text, before any model call
  (`RootAgentReentry::require_unchanged_agent`, `W/agents/pipeline/scoped_runtime.rs:206`, called at
  `W/agents/pipeline.rs:1114`). A missing stamp (an older Worker started the call) or a missing Main digest proves
  nothing and is refused with its own cause, `pipeline.recovery.agent_definition_unproven`. An `Answered` re-entry
  serves the stored answer and runs no model, so it is not checked. The stamp is a key in the start event's free
  provider metadata (64 lowercase hex digits, strictly decoded: `W/agents/pipeline/scope_receipts.rs:30,48`), so no
  codec changes; the v3 evidence binds it through the root row's receipt, not twice. Test:
  `postgres_a_saved_agent_edited_between_the_kill_and_the_reclaim_is_refused_typed` (the fake Main serves the edited
  content with its recomputed digest; refused, 0 model calls, the fault line carries `generation`, the edited text in
  no log), `postgres_a_receipt_without_its_definition_stamp_is_refused_unproven`, and T3's `answered_edited` case (an
  edited Agent's stored answer is served). The restored request equals
  the lost one in every resume test (C4, STEP 0).
- **D-3 "session without checkpoint":** a session with no event is `Fresh` (a Worker lost between creating the scope and
  its first checkpoint write sent nothing); only a session with events and no checkpoint is refused.
- **D-4:** read-only replay applies to every pipeline Agent-node call, not only on re-entry: the crash-time write must
  already classify a read-only batch as replayable.
- **C2 bound:** the bound is the command deadline (no separate redelivery counter: JetStream `MaxDeliver` is `-1` by
  contract). The per-window line covers only the transient-inspect cause, per execution (round 1 #4).
- **C3 (accepted):** two fenced reads with the stated fence argument (`W/state/postgres_checkpointer.rs:424-451`,
  `W/state/postgres_session.rs:754-765`), not one SQL snapshot.

## Follow-ups

- FU-334 (Main restart: a new model request sent while Main is down fails its node), FU-335 (delegation grant ceiling; 0 built-in tools for a saved Agent in a pipeline), FU-338/FU-339 (a draft edited between crash and reclaim), FU-340, FU-341, FU-342: registered from this PR's browser pass and reviews.

- FU-323b (after FU-291 #1937): a saved-pipeline tool in flight under a root Agent node (derive `NT` under
  `A + "/" + node`).
- FU-329: the model → tool write window (T1c).
- FU-325 / FU-326: drain to the next boundary; compose `stop_grace_period`.
- FU-334 (pre-existing, not this change): a model request sent while Main restarts fails its node.
- Not covered here: two Agent frontier nodes pending in one row (static fan-in) as a PG test; delegated-call and
  receipt limit tests at the root.
