# A terminal frame Main refuses ends the run typed (FU-290)

Date: 2026-11-06. Branch `fix/refused-terminal-ends-typed`. Plan and reviewer decisions:
`handoffs/point5-wave1-20261008/fu290/PLAN.md`.
- **D1:** no new Main authority path.
- **D2:** a real-path reproduction with no new switch.
- **D3:** a support reference only where no Main change is needed.
- The (g), (1) and (2) rules and the route conditions come from later review rounds.

## 1. Business behaviour, and what was not ported

- The current platform has no equivalent: a run whose result cannot be delivered is "lost, run again from scratch",
  and that is never ported.
- **The defect.** It was first seen 2026-11-05 on the shared stack, execution `c2ff9338…`:
  - A Worker spooled a terminal whose safe message was 257 bytes, over Main's 256-byte bound.
  - Main refused it (a bound PROTOCOL_VIOLATION, not retryable).
  - Every 60 s redelivery replayed the same spooled bytes.
  - The job stayed RUNNING, the chat spun forever, and Stop did not help.
- **The text itself** was fixed in #1576. This change closes every route by which a refused or unreadable pending
  terminal could keep a run alive forever.

## 2. What changed (all in the Worker; no Main code, no migration, no wire change, no flag)

| Step | Path | Rule |
|---|---|---|
| **A** | `src/execution/output_delivery.rs` `replay_terminal_with_replacement` | A **bound** `FrameRefused` of a non-canonical terminal is replaced **once**, through the spool's one-terminal swap (`replace_expected_terminal`). The replacement is Cancelled when the lease reports a stop, otherwise Internal. An unreadable lease stays retryable. |
| **B** | `src/execution/agent_delivery_processor.rs` (`terminal_frame_refused`), `src/transport/command_bus.rs` (`PoisonReason::TerminalRefused`) | A refused **canonical** terminal (version skew) is dead-lettered and terminated, not NAKed. ERROR event `agent_failure_terminal.canonical_refused`. |
| **Output-recovery route** (after a Stop) | `prepare_output_recovery` (was `prepare_empty_recovery`); processor `process_output_recovery` | When the spool holds exactly one pending terminal at watermark+1, it is replayed exactly through the existing `recover_accepted_terminal` (no third replay path). A Completed terminal Main accepts settles Completed once. A refused one gets A. A second terminal is never persisted beside it. |
| **(g)** stranded, unreadable terminal | `protocol/output.rs` `unrecognised_failure_terminal_kind`; `transport/output_grpc.rs` `retire_unrecognised_terminal` | Applies to a pending terminal at exactly watermark+1 whose only defect is a failure text this binary does not register (spooled by an older binary). It must pass every other check: identity, fence, stream, event type, logical output id, digest, single pending frame. It is retired as an encrypted record and replaced by one canonical failure. The kind is Cancelled on a stop; otherwise the old frame's kind when its code maps to exactly one registered kind; otherwise Internal. The deadline rule applies. |
| **Checkpoint route** (no Stop) | `prepare_checkpoint`, `prepare_checkpoint_pending`, `settle_rejected_checkpoint_progress`, shared `settle_replacement` | The same (g) qualification and replacement on the route Main answers a stranded run without a stop (see section 6). |
| **(1)** pending terminal ≤ watermark | `prepare_output_recovery` | Replayed exactly if it validates (Main acks the exact duplicate). Otherwise retained under `agent_delivery.output_recovery_terminal_unrecoverable`. Never followed by a fresh terminal (the two-terminals hazard). |
| **Unreachable routes** | `prepare` (fresh claim), `prepare_node_recovery` | A pending terminal there is retained under its own code, `agent_delivery.fresh_claim_terminal_unexpected` or `agent_delivery.node_recovery_terminal_unexpected`, not the generic NAK. Tampered or mis-bound terminals keep failing closed. |
| Live code | 10 `#[allow(dead_code)]` notes removed | The accepted-terminal recovery and its helpers are now reachable. |

Main tests added (no Main code change): `services/elitea-main/.../output/terminal_refusal_replacement_test.go` and
`.../repos/agent_execution_dispatch_postgres_integration_test.go`.

## 3. Tests (counts and skips)

**Worker** (`src/execution/output_delivery_tests.rs` unless noted):

- **A:**
  - `a_recovered_terminal_main_refuses_is_replaced_by_the_internal_failure` (:5113);
  - `a_refused_terminal_is_replaced_by_cancellation_when_a_stop_was_requested` (:5156);
  - `the_replacement_survives_a_crash_and_is_the_only_frame_replayed_afterwards` (:5191);
  - `an_authorization_failure_never_replaces_the_terminal` (:5292): only a bound refusal replaces.
- **B:**
  - `a_canonical_failure_terminal_main_refuses_is_not_replaced` (:5085);
  - `a_refused_canonical_failure_ends_the_replay_without_a_third_frame` (:5263);
  - `a_delivery_whose_canonical_terminal_is_refused_gets_a_terminating_verdict` (:5365);
  - `command_delivery_tests.rs:838` `a_refused_canonical_terminal_is_dead_lettered_and_terminated`;
  - `a_refused_pending_canonical_failure_on_the_output_recovery_route_is_dead_lettered` (:6662).
- **Output-recovery route:**
  - (a) `a_stop_replaces_a_pending_terminal_main_refuses_and_settles_cancelled_once` (:6358);
  - (b) `a_pending_terminal_main_refuses_is_replaced_by_internal_without_a_stop` (:6378);
  - (c) `a_pending_failure_terminal_is_replayed_and_settled_without_a_second_terminal` (:6396);
  - (d) `a_pending_success_terminal_is_replayed_and_settles_completed_once` (:6418);
  - (e) `the_pending_terminal_sequence_guard_matches_the_nonterminal_rule` (:6441);
  - (f) `a_repeated_recovery_claim_after_settlement_is_a_no_op` (:6474).
- **(g):**
  - `a_stop_replaces_a_pending_terminal_with_unrecognised_text_and_settles_cancelled_once` (:6543);
  - `a_pending_terminal_with_unrecognised_text_keeps_its_kind_or_ends_internal` (:6568);
  - `a_pending_terminal_failing_identity_validation_is_never_replaced` (:6605).
- **(1):** `an_unvalidated_terminal_at_or_below_the_watermark_is_retained_untouched` (:6639).
- **Checkpoint route:**
  - (g4) `a_stranded_terminal_with_unrecognised_text_settles_on_the_checkpoint_route_without_a_stop` (:6777),
    driving the whole processor;
  - `the_checkpoint_route_still_refuses_an_unrecognised_terminal_that_fails_other_checks` (:6810).
- **Named codes:**
  - `a_fresh_claim_meeting_a_pending_terminal_is_retained_under_its_own_code` (:6837);
  - `a_node_recovery_claim_meeting_a_pending_terminal_is_retained_under_its_own_code` (:6896).
- **Fail-before:** every replacement and settlement test above failed on the code before its step. The guard tests
  (g3, the authorization test, the checkpoint refusal) pin behaviour that was already right.
- **Fail-closed proof (existing tests, unchanged):** `restored_terminal_payload_digest_is_revalidated_before_recovery`
  and `restored_terminal_must_match_the_admitted_request_binding` keep a tampered or mis-bound terminal on
  `agent_output.invalid_durable_state`.

**Main:**
- `TestRefusedTerminalFailureIsFollowedByAcceptedCanonicalInternalFailure`, through the real output `Server` with
  stubs: the 257-byte terminal is refused, and the canonical Internal at the same sequence is accepted and projected.
- `TestPostgresAgentDispatchNeverRepublishesAClaimedRunningExecutionAfterItsLeaseLapses` (real PG; control
  included). The dispatch and claims package passed 76 tests.

**Existing tests:** no existing test assertion was changed or deleted. A test fake gained one `ReplayResult::FrameRefused`
variant.

**Full Worker suite.** Command: `cargo test --offline --locked --no-fail-fast --all-targets --all-features --
--test-threads=4` with a disposable PostgreSQL 18.
- At e7f0de1ff (base main 6914fbe37, before FU-287 merged), on a quiet machine: **3105 passed, 0 failed, 109
  ignored**.
- An earlier run at cdd751fec under load about 50 had 1 failure: the lock-bound timing test, which passed 3 of 3
  alone. That is FU-296.
- An earlier figure of 2842 came from a narrower command (`cargo test --offline` alone) and is not comparable.
- At the PR head 68611c6ba (main with FU-287 merged in), quiet machine: **3144 passed, 0 failed, 110 ignored**; clippy `-D warnings` with `--all-targets --all-features` clean.

**Lints:**
- clippy `-D warnings` is clean in every changed file. Pre-existing findings remain in untouched files
  (`agents/graph/code_remote.rs`, `sandbox/workspace.rs`, `test_pg.rs`, `workspace_run/store*.rs`).
- fmt, gofmt and go vet are clean.

## 4. Performance

- Nothing on a hot path changes.
- The added work runs only after Main has refused a terminal or when a pending terminal is found at recovery: one
  lease probe, one spool swap or one retirement.
- A stranded run now ends at its next redelivery instead of being redelivered every 60 s without end. On the shared
  stack, `c2ff9338` had been claimed 465 times before retirement.

## 5. Durability

- **Exactly one terminal per stream:**
  - A replaces through `replace_expected_terminal`;
  - (g) retires the old frame before persisting the canonical failure;
  - (1) never adds a terminal after one Main may hold.
- **Crash between retire and persist:** the `.retired` record stays, and recovery persists the canonical failure
  again (idempotent).
- **Crash after a swap, before the ack:** the replacement bytes are replayed (`the_replacement_survives_a_crash…`).
- **What is kept, and for how long:**
  - The retired frame stays as an encrypted `.retired` record until the run's settlement ack. It is then deleted
    with the run's other retired records (`discard_retired_records`, `transport/output_grpc.rs:885`), the same
    lifecycle as superseded progress frames.
  - The lasting trace is the ERROR log line `agent_output_unrecognised_terminal_retired`, which carries the
    execution id, the sequence, `frame_sha256` and the superseded claim attempt, never payload text.

## 6. Resilience and Security

| Rule | Enforcing code | Proving test |
|---|---|---|
| Only a bound refusal of this exact frame, or an unrecognised **text** on an otherwise fully valid frame, triggers a replacement. | `FrameRefused` is minted only after the ack's binding is validated (`transport/output_grpc.rs:1157`); `unrecognised_failure_terminal_kind` re-runs every restored check except the text | the authorization guard, g3, the checkpoint refusal test, and the two existing integrity tests |
| At most one replacement; then delivery stops. | `restored_terminal_failure_kind` guard; B's TERM verdict | B tests |
| A stop wins. | `lease.check_now()` → Cancelled; on the checkpoint route, a stop during replay gets `CancellationWon`, then the existing winner | (a), g1, `…cancellation_when_a_stop_was_requested` |
| Replacing on the checkpoint route without a lease check first is safe. The claim's fence makes Main refuse any frame from a superseded claim, and the spool is opened under an exclusive lock, so no other producer can write that sequence. | fence on every frame; spool ownership lock | g4 and the fence tests in `output_grpc.rs` |
| Main never re-dispatches a dead-lettered run. Worker side: `ClaimLeaseMonitor::close` only stops renewing (`agent_lease.rs:923-935`), and JetStream does not redeliver a TERMed message. Main side: republish needs no granted authority and a PENDING or DISPATCHED job (`db/queries/runtime_agent_execution.sql:384-430`; `agent_execution_dispatch.go:251`), and the first claim grants authority (`repos/claims.go:322`). | as listed | `TestPostgresAgentDispatchNeverRepublishes…`; the shared-stack data in section 8 |
| Which route a stranded run takes: with node recovery on (the production profile), a RUNNING, MAY_HAVE_STARTED run with no terminal in Main gets `ClaimRecoverAgentModelCheckpoint` (`node_recovery_claim.go:~77-91`, picked at `claims.go:~474-495`, `:512`). After a stop it gets output recovery. An accepted terminal goes to `ClaimRecoverTerminalACK` first (`claims.go:216-219`, `:464-468`), so (1) is defensive. A node visit requires no terminal in Main (`node_recovery_claim.go:~137`), and a fresh claim is PREPARING with nothing spooled, so those two routes only keep a named retained code. | as listed | the named-code tests |
| Logs carry codes, sequence and digest only. | the new events | event-capture assertions in the tests |

**Security categories** (`rules/security.md`):
- No route, RPC, identity, authorization, parser, SQL, egress, URL or template change, and no secret.
- Restored frames keep their integrity checks: only the text-equality step is relaxed, and only to choose a
  replacement, never to send the old text.
- Replacement texts are Main's registered canonical texts (parity-tested).

## 7. Recovery guarantee rows (section 2)

| Component x phase | Before | After | Enforcing code | Proving test / evidence |
|---|---|---|---|---|
| Worker x output delivery: terminal refused by Main (A) | **L** (RUNNING forever; 60 s loop) | **I/C**: exact replay first, then one bound replacement; the user sees one result | `replay_terminal_with_replacement` | A tests |
| Worker x output delivery: pending terminal after a stop (output-recovery route) | **L** (a second terminal refused locally; NAK forever) | **I/C**: exact replay of the pending terminal, then A | `prepare_output_recovery` → `recover_accepted_terminal` | (a)–(f); live run `87e453d9` (section 8) |
| Worker x output delivery: pending terminal this binary cannot read, no stop (checkpoint route) | **L** (preflight refusal; NAK forever) | **C**: retired as a record, replaced by one canonical failure | (g) on the checkpoint route | g4; live chat 889 (section 8) |
| Worker x output delivery: canonical terminal refused (version skew; parity tests keep it unreachable) | L | **C, escalated**: dead-lettered and TERMed; no re-dispatch (section 6). The job stays RUNNING and the chat keeps a running turn until FU-295 (Main quarantines or settles it; the UI shows a readable state). | B | B tests; `TestPostgresAgentDispatchNeverRepublishes…` |
| Worker x a pending terminal ≤ watermark, or on the fresh or node-recovery route (unreachable by Main's invariants) | L (generic NAK) | **F, retained** under a named code; never a second terminal | (1); named codes | (1) and named-code tests |
| Worker crash between the swap or retire and the ack | n/a | **I** | spool authority | `the_replacement_survives_a_crash…` |
| Main, NATS, PostgreSQL, sandbox supervisor, LLM gateway, Web | not touched | - | - | - |

A success terminal that Main refuses also ends with a failure (A). The user sees a failure for a run whose result
Main refused; the refused result is not shown.

## 8. Browser and real-stack evidence

The FU-290 proof comes from a disposable stack, real browser, no mocks, reloads.
- **Stack:** `fu290` at `http://fu290.localhost:19400`, from the real-model dump with the production profile (node
  recovery, model-checkpoint recovery and owner recovery on).
- **Dump workaround:** the dump's known migration gap (FU-279) was worked around on this throwaway database only.
- **Profile check:** verify-profile run from a 6914fbe37 checkout (the Main image's migration level) gave **50
  passed, 0 failed**. Run against the branch checkout it reports 2 ledger checks failed, only because that checkout
  holds main's newer migrations; FU-290 has no migration.

**Images:**
- Main `fu290-6914fbe37` (`sha256:a764bd7f2bbb…`) and Web `fu290-6914fbe37` (`sha256:daf2617af041…`), from main
  (no Main change in FU-290).
- **Strand Worker** `fu290-strand257` (`sha256:636c2cf0b2ae…`), a local throwaway, never committed or pushed: main
  6914fbe37 with one change, `FANOUT_MEMBER_FAILED_MESSAGE` in `src/protocol/output.rs` restored to e88cd7273's
  257-byte text. The binary holds that text and not the 203-byte one.
  - Why not e88cd7273 itself: that Worker refused current Main's input bundle (`agent_input.invalid_input`, "not
    canonical protocol v1").
- **Fix Workers** from this branch:
  - `fu290-1e9e046a4` (`sha256:fc1c553b2116…`);
  - `fu290-8956851ef` (`sha256:5502123a3c1c…`).
  - Binary check (docker create + cp): `agent_output_unrecognised_terminal_retired` 1,
    `agent_failure_terminal.canonical_refused` 1, the 203-byte text present, the 257-byte text absent.
- **Fixture:** app 170 "FU290 stop Map", created through `POST /api/v2/elitea_core/applications/prompt_lib/2` from
  the signed-in page. It is a Map over `[5, 2, 0, 4]` with a State Modifier `{{ 100 // n }}` and On Error `stop`,
  so item 2 fails and the run ends in the Map stop failure.
- **Run-to-completion:** off for both runs (`aria-pressed=false` before send).

**Run `87e453d9…` (with Stop; output-recovery route):**
1. With the strand Worker, the terminal was refused (`agent_terminal.output_rejected`; Main "runtime output frame
   rejected"). Redelivery and claims followed every 60 s.
2. Stop was pressed at 16:55 UTC. The job became RUNNING/CANCELLED and kept looping on the previous fix image. That
   loop is how the output-recovery gap was found.
3. With `fu290-1e9e046a4`, the next redelivery at 18:06:45 UTC retired the old frame (seq 10, digest only) and
   settled **CANCELLED once**. Claims went 59 → 60 (`SETTLED`), with none after.
4. **UI:** a stop before any output deletes the turn by product design (`db/queries/agent_cancel.sql`
   `deleted_response` / `deleted_question`), so this case has no chat card; the database is the proof.

**Run `435e99ab…`, chat 889 (no Stop; checkpoint route):**
1. Stranded from 18:11 to 18:47 UTC: 15 claims, all `LEASE_EXPIRED`, `agent_terminal(_recovery).output_rejected`.
   After a reload, the chat showed a spinner.
2. On `fu290-1e9e046a4` it still looped, now refused at preflight. That is how the checkpoint-route gap was found.
3. With `fu290-8956851ef`, swapped in at 18:47:56 with the database untouched, the first redelivery at 18:48:22
   retired the old frame (`agent_output_unrecognised_terminal_retired`, seq 10, digest only). It then settled
   **FAILED once**, kind Internal, because the fan-out code is shared and maps to no single kind.
4. Claims went 15 → 16 (`SETTLED`), with 0 open and none after. Main refused 0 frames.
5. **Chrome after a reload:** "The runtime operation failed." with "Details for support". The spinner is gone.

**The shared stack's `c2ff9338…` ("before" picture):**
- On 2026-11-06 it was still `RUNNING/CANCELLED/MAY_HAVE_STARTED`, with 465 claims since 2026-11-05 16:26 UTC. The
  loop survived a real Main crash (FU-275, 12:21 UTC) and a Docker restart.
- Main never re-dispatched it: `command_outbox` stayed at `publish_attempts = 1`.
- That stack was retired the same day (Roman's decision). Its final dump (`handoffs/real-model-stack/
  fanpar-final-20261106-*.dump`, local, mode 600) keeps this state checkable. The fix proof is the two runs above.

## 9. Fixtures

- Worker and Main test fixtures are code.
- The browser fixture was created through the UI's own REST endpoint, not the database.
- The strand Worker is a local image with the one-constant change described in section 8.

## 10. Follow-ups

- **FU-295:** Main quarantines or settles a RUNNING job whose terminal was dead-lettered (B), and the UI shows a
  readable state.
- **FU-296:** the load-sensitive lock-bound test. Also: `content_grpc_tests` need their own rustls provider when run
  alone.
- **FU-297:** canonical validation by exact text strands frames from the previous binary. Validate by kind/code
  with a bounded text, or keep the past texts.
- **Kind on replacement:** the old fan-out frame ends Internal, not with its own kind, because its code is shared
  with `PipelineToolFailed`. Keeping it would need a richer registered-failure lookup (covered by FU-297).
