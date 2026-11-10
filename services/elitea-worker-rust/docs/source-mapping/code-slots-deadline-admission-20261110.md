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
  new kind once with the older kind of the same wire code, which that Main admits. Main first stays the documented
  order.
- **The fallback is a rule of the kind table.** `RuntimeFailureKind::older_main_fallback` (`protocol/output.rs:935`)
  is an exhaustive match, so a new kind does not compile until it is placed: a kind added after the FU-299 baseline
  names a baseline kind of the same wire code, every baseline kind is `None`. The baseline is pinned in the test module
  (`FU299_BASELINE_RUNTIME_FAILURES`, `:2416`: `1f799d714`'s registered kinds, 53, plus `FanoutAgentCallsPipeline`,
  whose text Main has admitted since FU-291). `post_baseline_kinds_fall_back_to_a_baseline_kind_of_the_same_wire_code`
  fails for a post-baseline kind without a fallback, with a post-baseline fallback, or with a different wire code or
  retryability. The kind table, not the registry, holds it: the table is what the replacement reads, and the parity
  test already ties each table kind to its registry entry.
- **Observability (plan §2.1).** An uncontended acquire logs nothing. A slot wait over 1 s logs one INFO fault line
  `pipeline.code.slot_waited` (`code_remote.rs:348`) with `slot_wait_ms`, `slots_capacity`, `supervisor_target` (the
  normalised target; `target` is the operator stream's own key) and `execution_id`, on both drive paths and for
  started attempts. A Supervisor Busy (`RESOURCE_EXHAUSTED`, `sandbox/service.rs:715`) logs one INFO fault line
  `pipeline.code.supervisor_busy` per attempt, however many retries (`SupervisorBusyNotice`, `code_remote.rs:728`,
  used at `code_remote.rs:582` and `code_attempt_remote.rs:536`); before this change Busy was not logged.

## Tests

| Where | Tests | Result |
|---|---|---|
| `admission.rs` | `required_window_matches_the_formula` (N 0/1/4/5/8/9/64, concurrency 8 vs slots 1/4/8, zero lanes, u32 and `Duration::MAX` overflow), `fanout_window_admits_exactly_equal_and_refuses_one_ms_less`, `code_slots_limit_and_limit_plus_one_and_release_on_drop`, `a_cancelled_slot_wait_holds_nothing`, `slot_wait_ends_at_the_bound_typed`, `a_fresh_attempt_is_admitted_at_exactly_required_and_refused_one_ms_less`, `a_slot_wait_that_uses_up_the_margin_is_refused_as_slot_wait`, `a_started_attempt_takes_a_slot_without_the_deadline_check`, `code_slot_capacity_is_bounded` | 13 of 13 in the module pass (9 new) |
| Worker raw path (`code_slots_tests.rs`) | `raw_path_admits_exactly_required_and_refuses_one_ms_less`, `the_raw_graph_node_reports_the_registered_deadline_code`, `slots_bound_concurrent_attempts_per_target_and_not_across_targets`, `a_refusal_is_one_fault_line_with_its_cause_and_no_user_data` (+ child) | pass |
| Worker journaled path (`node_recovery_code_debug_caller_tests.rs`) | `journaled_path_admits_exactly_required_and_refuses_one_ms_less`, `a_started_attempt_recovered_with_too_little_time_is_reconciled_not_refused` (C2), `one_attempt_takes_one_slot_through_preparation` | pass |
| Worker config | `code_slots_are_bounded_defaulted_and_agree_per_normalised_target` (0/1/1024/1025, -1/"4"/4.5, default, unknown key, disagreement incl. absent vs explicit, error text), `code_slot_targets_are_normalised_like_the_client_dials_them` | pass |
| Worker protocol | `deadline_refusals_keep_their_readable_registered_text`, `fanout_agent_calls_pipeline_is_registered`, `a_refused_new_kind_is_replaced_by_the_kind_an_older_main_admits`, the FU-299 parity test (now incl. D5), `tests/agent_output_contract.rs` | pass |
| Worker real PostgreSQL (`node_recovery_code_wiring_tests.rs`) | `real_pg_root_code_with_too_little_time_is_refused_without_a_dispatch_row`, `real_pg_busy_from_another_worker_backs_off_without_releasing_the_slot`, `real_pg_one_slot_runs_one_attempt_through_preparation_and_run`, `real_pg_supervisor_busy_is_one_info_line_per_attempt` | pass: the whole CI prefix `agents::graph::node_recovery_runtime::tests::code_owner_tests::` with `--ignored` gives 24 passed, 0 failed on a disposable PostgreSQL 17, including all four |
| Fallback rule and substitution | `post_baseline_kinds_fall_back_to_a_baseline_kind_of_the_same_wire_code`, `the_fallback_is_substituted_at_most_once` (`protocol/output.rs`); `a_refused_post_baseline_failure_is_replaced_once_by_its_fallback_and_settles`, `a_refused_fallback_is_not_substituted_again` (`execution/output_delivery_tests.rs`, through the recovery processor) | pass |
| Log lines | `slot_waits_and_supervisor_busy_are_one_info_line_each` (+ child): uncontended no line, 2 s wait one INFO line, Busy one line per attempt | pass |
| Worker lib, whole suite | `cargo test --lib --all-features` | 3129 passed, 0 failed, 114 ignored; after merging `main` (FU-299 as #1874) and the review fixes: 3143 passed, 0 failed, 114 ignored; clippy `-D warnings` clean |
| Main | `TestDeadlineAdmissionFailuresShowTheirReadableText`, `TestDeadlineAdmissionTextsMatchTheFaultRegistry`, `TestOutputServerAcceptsDeadlineAdmissionFailureFrames`, `TestAnUnknownFailureIsRefusedOnceAndItsSameCodeFallbackEndsTheRunTyped` (C3) | package: 55 top-level tests pass |
| FU-299 gates | `go test ./...` in `libs/go/faults` (well-formed, generated, guide, no orphans, legacy) | pass |
| Helm render scripts (not CI-wired; chart dependencies built under a scratch `HELM_*` config) | `render-worker-sandbox.sh` (codeSlots: absent not written, 4 rendered as integer `code_slots`, 0/1025/2.5/"four" refused, 1024 rendered), `render-worker-node-recovery.sh` 16/16, `render-worker.sh` 13/13, `render-compiled-snapshots.py` OK; then every `render-*.sh` | **44 of 44 pass.** `render-helm3-parity.sh` ran with `HELM3` set to the official Helm v3.17.3 darwin-arm64 release (`get.helm.sh`, its published SHA-256 checked, unpacked in the session scratchpad, not installed on PATH) and `HELM4` set to Helm v4.1.0: "Helm v3.17.3+ge4da497 and v4.1.0+g4553a0a render the same chart". |

**How the real-PG tests run in CI.** They follow the file's pattern exactly: `#[ignore = "requires disposable
PostgreSQL"]`, the harness reads `ELITEA_TEST_DATABASE_URL` through `crate::test_pg::database_url_var` and fails without
it. `ci-rust.yml` step "Verify Code owner recovery and journals with PostgreSQL" (`:385`) sets the URL and runs
`cargo test --locked --lib --all-features -- --ignored ... agents::graph::node_recovery_runtime::tests::code_owner_tests::`,
which selects all four (`cargo test -- --list --ignored` lists them under that prefix), and the ignored-test guard
(`check-worker-ignored-tests.mjs`) fails the job for an ignored test no step runs. Removing `#[ignore]` would make a
plain `cargo test` without a database fail, so they stay ignored; no workflow change is needed.

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
| Worker × Code refused by deadline admission | F (typed; the Worker cannot extend the deadline) | `admission.rs:315` | as above; `real_pg_root_code_with_too_little_time_is_refused_without_a_dispatch_row` (real PG) |
| Worker × started attempt recovered near the deadline | R/C unchanged: reconciled, never refused | `admit_started`, `code_attempt_remote.rs:99` | `a_started_attempt_recovered_with_too_little_time_is_reconciled_not_refused` |
| Worker × slot held across a Worker crash | R/I unchanged (FU-214, owner recovery) | slots are process memory | per-target test (a new factory starts with every slot free) |
| Supervisor × Busy from other Workers | unchanged backoff, slot kept | `code_attempt_remote.rs` submit loop | `real_pg_busy_from_another_worker_backs_off_without_releasing_the_slot` (real PG); browser case 3: 0 Busy answers |
| Main older than Worker (mixed rollout) × output delivery of a post-baseline failure | **I**: the refused terminal is replaced once, at the same sequence, by its baseline fallback; the older Main shows the generic Code or tool failure text; typed, not lost, never looped | `protocol/output.rs:330` (`refused_terminal_replacement`), `protocol/output.rs:935` (`older_main_fallback`), `execution/output_delivery.rs:2376` | `a_refused_post_baseline_failure_is_replaced_once_by_its_fallback_and_settles`, `a_refused_fallback_is_not_substituted_again`, `post_baseline_kinds_fall_back_to_a_baseline_kind_of_the_same_wire_code`, Main `TestAnUnknownFailureIsRefusedOnceAndItsSameCodeFallbackEndsTheRunTyped` |

## Real-browser evidence

Disposable stack `w31a` (`w31a.localhost:48600`, production profile: `stack.sh verify-profile` 50 passed, 0 failed,
before case 1 and again after the case 3 config change), real-model dump `product-real-models-main-ce2756fad`.

| Image | Tag | Identity | Branch check (§3b) |
|---|---|---|---|
| Worker | `elitea-worker-rust:w31-e996b024a` (this branch's head) | `sha256:54aad93a7e5b…` | binary extracted with `docker create` + `docker cp`: `pipeline.code.slot_waited`, `pipeline.code.supervisor_busy`, `code_slots_configured`, `supervisor_target`, `pipeline.code.deadline_insufficient` each found once |
| Main | `elitea-main:w31-f8d960003` | Main has no change after `f8d960003` (later commits touch the Worker and docs only) | built from this branch |

Fixture: pipeline 170 "W31 slot check", created in the UI: one Python Code node (`time.sleep(5)`, returns
`slot check done`), run from the designer's test chat. Python runs on `elitea-sandbox-deno:9446`, which no other
language uses.

1. **Defaults (case 1).** Startup logs one INFO `code_slots_configured` per Supervisor target, each `code_slots: 4`:
   `elitea-sandbox-deno:9446`, `elitea-sandbox-deno-native:9450` (JavaScript and TypeScript share it),
   `elitea-sandbox-rust:9447`. One run, execution `11af2e086716e776661e813101cf7e39`: SUCCEEDED in 12 s,
   "slot check done". Worker logs for it: no `slot_waited`, no `supervisor_busy`, no WARN, no ERROR (an uncontended
   acquire writes no line). After a reload the run history shows it SUCCEEDED with its preparation, hydration and
   execution trace.
2. **Too little time (case 2).** Proven with real PostgreSQL only (N4): the stack has no setting that shortens the
   24 h agent TTL, so a browser run cannot reach the 150 s window. See
   `real_pg_root_code_with_too_little_time_is_refused_without_a_dispatch_row`.
3. **One slot, two runs at once (case 3).** `"code_slots": 1` set on the Python entry of the stack's
   `material/worker-runtime.json`, Worker restarted. Startup then logs `elitea-sandbox-deno:9446` `code_slots: 1`, and the
   other targets stay at 4. Two runs were sent from two tabs within a second of each other (12:46:09.15 and 12:46:09.17).
   - `16729b89cda3266df2c9343160ce4453` took the slot: SUCCEEDED in 13 s.
   - `c927074e56650cf646a1199aab0f12be` waited for it. The Worker wrote exactly one INFO line:
     `code: pipeline.code.slot_waited`, `slot_wait_ms: 12213`, `slots_capacity: 1`,
     `supervisor_target: elitea-sandbox-deno:9446`, `execution_id: c927074e…`. Its trace shows preparation starting at
     12:46:21.57, 12.2 s after the run started. The run SUCCEEDED in 24 s with "slot check done".
   - The wait is longer than the 5 s sleep because one slot covers a whole attempt, preparation and run (D1).
   - Worker logs over the whole case: 0 `pipeline.code.supervisor_busy` lines (the Supervisor never answered Busy), 0 WARN,
     0 ERROR.
   - After a reload the run history lists both runs SUCCEEDED, with their traces.

## Fixtures

All unit fixtures are in-process fakes (never-connecting PostgreSQL pool, in-memory node journal, fixture Main RPC).
No database fixture was created. The browser fixture (pipeline 170) was created through the UI; the case 3
`code_slots: 1` was a hand edit of the disposable stack's generated Worker config, as PLAN §5 asks.

## Reviews and scans

- **code-review** (high, whole diff against `main`), nine findings.
  - Fixed in `5a4b9f101`:
    - Code admission no longer fails the assembly of an agent or pipeline without Code when the deadline has
      passed. `code_admission_deadline` binds it as "now", so every fresh Code attempt is refused and the
      lifecycle ends the run `DeadlineExceeded` as before. Test: `a_passed_deadline_admits_no_code_and_does_not_fail_assembly`.
    - The raw Code node no longer adds an ERROR line to the refusal's WARN fault line. The log-capture test now
      runs the node and asserts no ERROR.
    - `code_timing::OBSERVATION_MARGIN` is the admission constant.
    - values.yaml says the bound is per Worker process.
  - Justified:
    - Time spent inside the slot before the observation deadline (preparation, hydration) is not counted at
      admission: N3 moved that bound to W3-2.
    - The restored-terminal path shows the generic Code text: listed below; the run still ends typed and once (F).
    - Replicas × slots can exceed the Supervisor's concurrency: a per-Worker bound by design (D1, D2). Busy
      backoff covers it, and FU-307 shares the budget.
    - The two `fault!` arms and the `support_ref` field name: the FU-299 convention for fault lines. The INFO
      lines carry `execution_id` as the orchestrator specified; the value is the same.
    - `required_window(1, 1, …)` on a single attempt: one formula for the attempt and the fan-out.
    - `FanoutDeadlineInsufficient` has no caller yet: W3-2 wires it (D3).
- **The two user texts and the existing frame guard.** The first wording ("… could not start because …") put the bytes
  `cause` into the frame and loosened the existing assertion in `all_registered_runtime_failures_use_only_canonical_safe_policy`.
  As the orchestrator decided, both texts now use a colon ("The Code node could not start: too little time was left
  before the run's deadline."), and that assertion is exactly as on `main`. The test only gains the two new rows and a
  clippy allow.
- **security-review**: no finding. Surfaces checked against `rules/security.md`:
  - Main admits the new texts only exactly, under their wire code.
  - The fallback frame is built by the Worker, never copied from the refused frame, and is validated by Main again.
  - Log fields are ids, numbers and the normalised `host:port` only, with no Code source, inputs, results or
    credentials.
  - The user-facing texts are fixed sentences.
  - The slot is taken after the existing authority check.
  - Slots are per-Worker resource contention, with no shared data across runs.
  - The deadline comes from the signed command and can only make admission stricter.
  - Config is operator-trusted and bounded.
  - Not applicable: identity, egress, SQL, URL or path building, parsers of untrusted structure, secrets.
- **Scanners**: no dependency, lock or manifest changes, so nothing new.
  - `govulncheck` (Main): GO-2026-6809, pre-existing.
  - `cargo deny --all-features check advisories` (Worker): the same advisories as `main`.

## Follow-ups

- The restored-terminal path (`report_restored_terminal_failure`) knows only the class, so a refusal restored from the
  node journal after a Worker restart shows the generic Code failure text.
- The raw path has no node id for the fault line (`node_id` is empty there).
- Busy on the preparation Supervisor is not covered by `pipeline.code.supervisor_busy` (it keeps its own lookup retry).
- N2: a Supervisor capacity advertised to the Worker (D2 (d)).
- W3-2: call `admit_fanout_window` from the fan-out activation; a preparation bound if it proves needed (N3).
