# Code slots and deadline admission (W3-1), 2026-11-10

Plan and review: `handoffs/point5-wave1-20261008/w3-1/PLAN.md` and `REVIEW-PLAN.md`. The decisions are D1 one slot
per attempt per Supervisor target; D2 (a) default 4, set explicitly by Helm and compose; D3 the fan-out activation call
is wired in W3-2; D4 the 90 s margin is kept; D5 `FanoutAgentCallsPipeline` is registered here.

## C1: defaults, before any admission code

Read on `origin/main` `586c8470b`.

**Run deadlines (Main admission)**

| Run | TTL | Source |
|---|---|---|
| Agent execution (also pipelines: a pipeline is an application run as an agent execution) | 24 h | `services/elitea-main/internal/runtimecomposition/composition.go:99` `agentDeadlineTTL`, used by `agentDispatchPolicy` `:507-516` |
| Toolkit execute-read | 24 h | same constant, `toolkitDispatchPolicy` `:518-527` |
| Validation | 1 min | `:100`; it runs no Code |

**Code profile timeouts (the standalone production profile)**

The values come from `stack.sh`'s `code-material` output: `stacks/defaults/code/worker-runtime.json` plus the
Supervisor configs. They match `deploy/runtime/code-platform-startup.example.json`.

| Language | Target | `timeout_seconds` | Platform client | Preparation (own Supervisor) | Supervisor `concurrency` |
|---|---|---|---|---|---|
| python | `elitea-sandbox-deno:9446` | 60 | 60 | 120 | 4 |
| javascript | `elitea-sandbox-deno-native:9450` | 60 | 60 | 120 | 4 |
| typescript | `elitea-sandbox-deno-native:9450` (shared with javascript) | 60 | 60 | 120 | 4 |
| rust | `elitea-sandbox-rust:9447` | 180 | none | 300 | 4 |

- The Worker-side bound is `PreparedJob::new`, at 1..=3600 s (`W/sandbox/request.rs:107`).
- Preparation Supervisors have `concurrency` 1. The review's N3 sends any preparation bound to W3-2.

**Refused window.** A fresh attempt is refused in the last `timeout_seconds + OBSERVATION_MARGIN` (90 s) of the run.

| Profile | Window | Share of a default 24 h run | A first-node Code is admitted with |
|---|---|---|---|
| python / javascript / typescript (60 s) | 150 s | 0.17 % | 23 h 57 m 30 s of margin |
| rust (180 s) | 270 s | 0.31 % | 23 h 55 m 30 s of margin |
| worst allowed profile (3600 s) | 3690 s | 4.3 % | 22 h 58 m 30 s of margin |

No default combination refuses at the start of a run or in its first half, so C1 holds and the margin stays as
designed. Two Supervisor targets are shared by more than one language (javascript and typescript on `:9450`), so the
slot key must be the target, not the language (C4).

## Business behaviour

Taken from the current platform: nothing. The current platform has no Code slots and no deadline admission; a Code job
started near the end of a run is cut by the run's deadline. W3-1 is a durability and resilience change only.

Not ported, on purpose: running an attempt that cannot finish before the deadline (it ended unobservable, an
unknown-effect visit for owner recovery).

## Changed paths

- `libs/go/faults/registry/runtime.json`, `worker.json`, `multi-sentence-exemptions.txt`, generated `codes_gen.go` and
  `docs/errors/README.md` (registry first).
- `libs/rust/agent-runtime/src/graph/fanout/admission.rs`: `CodeSlots`, `required_window`, `admit_fanout_window`.
- Worker: `config.rs`, `bootstrap.rs`, `agents/{session,ordinary,pipeline}.rs`,
  `agents/graph/{code_remote,code_attempt_remote,code_runtime,code_tool}.rs`, `protocol/output.rs`,
  `execution/{output_delivery,toolkit_delivery_processor}.rs`, tests.
- Main: `internal/transport/runtimegrpc/output/server.go` and `deadline_admission_failure_test.go`.
- Deploy: Helm `values.yaml`, `templates/worker/configmap-runtime.yaml`, `tests/render-worker-sandbox.sh`;
  `deploy/runtime/code-platform-startup.example.json` and `.md`.
- `docs/upgrading/2026-11-10-0-code-slots-deadline-admission.md` (the issue number in the name is a placeholder).

## Design as built

- **Codes.** The Worker's parity test requires every wire kind to be `runtime.<snake kind>`, so the user-facing entries
  are `runtime.code_deadline_insufficient` (`CodeDeadlineInsufficient`, wire `PIPELINE_CODE_FAILED`) and
  `runtime.fanout_deadline_insufficient` (`FanoutDeadlineInsufficient`, wire `PIPELINE_TOOL_FAILED`). The plan's names
  `pipeline.code.deadline_insufficient` and `graph.fanout.deadline_insufficient` are registered as operator codes
  (`worker.json`): they are the upstream causes the runtime raises and the code of the WARN fault line. D5 adds
  `runtime.fanout_agent_calls_pipeline`.
- **Slots.** One `CodeSlots` per normalised Supervisor target (`config.rs:307` `code_slot_target`: host lower case, no
  trailing dot, port 443 when absent, as the client dials `https://{target}`), built once in
  `CodeRuntimeFactory::new`/`with_platform_client` (`code_remote.rs:79`). A broker `platform_client` target inherits
  its language entry's value. Preparation targets are not slotted (N3): preparation submits to its own Supervisor
  through `profile.preparation_client`, inside the attempt that already holds the run-target slot.
- **One acquire per attempt.** Journaled path `code_attempt_remote.rs:99`, before the original visit, preparation,
  `register_unsubmitted` and `mark_submission_started`. Raw path `code_remote.rs:410`, before preparation, the journal
  and submission. The slot is a local held to the end of the attempt.
- **Fresh vs started (C2).** `prior_dispatch` (the dispatch journal's proof, `true` when unreadable) selects
  `admit_started` (slot only, bounded by the run's deadline; expiry is the existing `AttemptTimeout` with an unknown
  effect, never a refusal) or `admit_fresh` (deadline admission).
- **Refusal class.** `NodeFailureClass` is persisted serde, so no new class: the refusal uses the non-retryable
  `InvalidConfiguration` with `NoExternalEffect`; `CodeAttemptFailure::deadline_insufficient` selects the registered
  upstream code (`code_runtime.rs:112`).
- **Version skew (C3).** Current Main refuses an unregistered (code, text) with a bound non-retryable
  `PROTOCOL_VIOLATION` and projects nothing (`server.go` `runtimeFailureFrame` compares the text with
  `runtimeFailurePolicyForError`). Before this change the Worker replaced only a non-canonical refused terminal, so a
  new canonical kind refused by an older Main would have been dead-lettered with the run unsettled until its deadline.
  `refused_terminal_replacement` (`protocol/output.rs:330`, used at `output_delivery.rs:2376`) now replaces a refused
  new kind once with the older kind of the same wire code (`older_main_fallback`, `:930`), which that Main admits.
  Main first stays the documented order.

## Tests

| Where | Tests | Result |
|---|---|---|
| `admission.rs` | `required_window_matches_the_formula` (N 0/1/4/5/8/9/64, concurrency 8 vs slots 1/4/8, zero lanes, u32 and `Duration::MAX` overflow), `fanout_window_admits_exactly_equal_and_refuses_one_ms_less`, `code_slots_limit_and_limit_plus_one_and_release_on_drop`, `a_cancelled_slot_wait_holds_nothing`, `slot_wait_ends_at_the_bound_typed`, `a_fresh_attempt_is_admitted_at_exactly_required_and_refused_one_ms_less`, `a_slot_wait_that_uses_up_the_margin_is_refused_as_slot_wait`, `a_started_attempt_takes_a_slot_without_the_deadline_check`, `code_slot_capacity_is_bounded` | 13 of 13 in the module pass (9 new) |
| Worker raw path (`code_slots_tests.rs`) | `raw_path_admits_exactly_required_and_refuses_one_ms_less`, `the_raw_graph_node_reports_the_registered_deadline_code`, `slots_bound_concurrent_attempts_per_target_and_not_across_targets`, `a_refusal_is_one_fault_line_with_its_cause_and_no_user_data` (+ child) | pass |
| Worker journaled path (`node_recovery_code_debug_caller_tests.rs`) | `journaled_path_admits_exactly_required_and_refuses_one_ms_less`, `a_started_attempt_recovered_with_too_little_time_is_reconciled_not_refused` (C2), `one_attempt_takes_one_slot_through_preparation` | pass |
| Worker config | `code_slots_are_bounded_defaulted_and_agree_per_normalised_target` (0/1/1024/1025, -1/"4"/4.5, default, unknown key, disagreement incl. absent vs explicit, error text), `code_slot_targets_are_normalised_like_the_client_dials_them` | pass |
| Worker protocol | `deadline_refusals_keep_their_readable_registered_text`, `fanout_agent_calls_pipeline_is_registered`, `a_refused_new_kind_is_replaced_by_the_kind_an_older_main_admits`, the FU-299 parity test (now incl. D5), `tests/agent_output_contract.rs` | pass |
| Worker real PostgreSQL (`node_recovery_code_wiring_tests.rs`, ignored) | `real_pg_root_code_with_too_little_time_is_refused_without_a_dispatch_row`, `real_pg_busy_from_another_worker_backs_off_without_releasing_the_slot`, `real_pg_one_slot_runs_one_attempt_through_preparation_and_run` | written, **not run** (need a disposable PostgreSQL) |
| Worker lib, whole suite | `cargo test --lib --all-features` | 3129 passed, 0 failed, 114 ignored |
| Main | `TestDeadlineAdmissionFailuresShowTheirReadableText`, `TestDeadlineAdmissionTextsMatchTheFaultRegistry`, `TestOutputServerAcceptsDeadlineAdmissionFailureFrames`, `TestAnUnknownFailureIsRefusedOnceAndItsSameCodeFallbackEndsTheRunTyped` (C3) | package: 55 top-level tests pass |
| FU-299 gates | `go test ./...` in `libs/go/faults` (well-formed, generated, guide, no orphans, legacy) | pass |

## Performance

- Mechanism: one `tokio::sync::Semaphore` per Supervisor target; acquire is one atomic permit take when uncontended
  (`admission.rs:294`), no allocation beyond the `OwnedSemaphorePermit`, no I/O. Bounded table: at most 4 profiles
  plus 4 broker targets (`sandbox_runtimes.len() <= 4`).
- Budget: the uncontended acquire adds no round trip, transaction or byte; measured value not recorded (no benchmark
  run in this change).

## Durability

- A slot is process memory and is acquired before any durable or external effect, so a crash while waiting leaves
  nothing to reconcile; a new process starts with every slot free.
- A refused attempt writes no dispatch row and marks nothing started (journaled path refuses before the original
  visit; the real-PG test pins "no `sandbox_dispatches` row", not run here).

## Resilience

- Every wait is bounded: fresh attempts by `execution_deadline − (timeout + 90 s)`, started attempts by the execution
  deadline. Dropping a waiting future holds nothing (`a_cancelled_slot_wait_holds_nothing`).
- Config bounded 1..=1024, unknown keys refused, disagreeing profiles on one Supervisor refused at startup with a
  readable error.
- Supervisor Busy/Pending from other Workers keeps the existing child-local backoff; the slot is held through it.

## Security

- The fault line carries code, `node_id`, `required_ms`, `remaining_ms`, `slots`, `support_ref` and, for N1,
  `cause`/`slot_wait_ms`; never source, inputs or outputs (log-capture test with marker strings).
- Main admits the new texts only exactly, only under their wire code, never retryable; an unregistered suffix is
  refused (`TestDeadlineAdmissionFailuresShowTheirReadableText`).
- Authorization, identity, egress and parsing: not touched.

## Recovery guarantee rows

| Component × phase | Class | Enforced by | Proven by |
|---|---|---|---|
| Worker × Code slot wait (fresh) | I (pre-dispatch, nothing started) | `code_attempt_remote.rs:99`, `code_remote.rs:410` | `journaled_path_admits_exactly_required_and_refuses_one_ms_less`, `raw_path_admits_…` |
| Worker × Code refused by deadline admission | F (typed; the Worker cannot extend the deadline) | `admission.rs:315` | as above; real-PG no-row test written, not run |
| Worker × started attempt recovered near the deadline | R/C unchanged: reconciled, never refused | `admit_started`, `code_attempt_remote.rs:99` | `a_started_attempt_recovered_with_too_little_time_is_reconciled_not_refused` |
| Worker × slot held across a Worker crash | R/I unchanged (FU-214, owner recovery) | slots are process memory | per-target test (a new factory starts with every slot free) |
| Supervisor × Busy from other Workers | unchanged backoff, slot kept | `code_attempt_remote.rs` submit loop | `real_pg_busy_from_another_worker_backs_off_without_releasing_the_slot` (not run) |
| Main older than Worker × new failure kind | typed failure (generic Code text), no hang, no loop | `output_delivery.rs:2376`, `protocol/output.rs:330` | `a_refused_new_kind_is_replaced_by_the_kind_an_older_main_admits`, Main `TestAnUnknownFailureIsRefusedOnce…` |

## Real-browser evidence

Not done in this change: no stack was started (the session ran with no Docker). PLAN §5 cases 1-3 remain.

## Fixtures

All unit fixtures are in-process fakes (never-connecting PostgreSQL pool, in-memory node journal, fixture Main RPC).
No database fixture was created.

## Follow-ups

- Browser acceptance (PLAN §5) and the real-PG tests on a disposable stack.
- The restored-terminal path (`report_restored_terminal_failure`) knows only the class, so a refusal restored from the
  node journal after a Worker restart shows the generic Code failure text.
- The raw path has no node id for the fault line (`node_id` is empty there).
- N2: a Supervisor capacity advertised to the Worker (D2 (d)).
- W3-2: call `admit_fanout_window` from the fan-out activation; a preparation bound if it proves needed (N3).
- Helm render scripts were not run locally (chart dependencies need `helm dependency build`).
