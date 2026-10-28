# Fixed Parallel admitted: per-interrupt decisions, park and wake (Point 5, Wave 2a-2)

Branch `feat/graph-fanout-parallel-admitted`. Design: `handoffs/point5-wave1-20261008/T-2A-fanout-runtime.md` §2a-2,
contract `libs/proto/contracts/fanout-interrupt-decisions-v1.md` (v1.1). This mapping covers the whole branch; the
browser-found defects and the four follow-ups are listed at the end.

## Business behaviour

Taken from the current platform (EliteaUI + elitea-sdk parallel tool calls and HITL), kept as observable behaviour:

- A pipeline `parallel` node runs 2 to 16 declared branches (at most 8 at a time). Each branch is an Agent node that
  runs a saved pipeline (or a saved agent, see FU-246). The node joins the branch results into its output key
  (`append`, `keyed`, `merge`) with On Error `continue` (failed branches land as typed failed entries) or `stop`.
- A branch that asks the user (`ask_user`), needs a sensitive-tool approval (HITL approve/reject) or a delegated
  authorization raises its own card. A delegated-authorization card can be skipped; `authorize` from the card is
  refused typed until it carries its credential reference end to end (FU-250). Cards from several branches are open at the same time; each is answered on its
  own, in any order, from any tab, and the run continues each answered branch without waiting for the others.
- The parked response shows one accordion per member and its open cards; a card answered in one tab disappears from
  every other tab; a reload shows the same state from the server.

Deliberately not ported:

- The current platform's "one interrupt at a time, whole run blocked" HITL and its "lost, run again from scratch"
  behaviour after a worker loss.
- Running a branch inline as an LLM node (FU-247), a saved agent's own pause inside a branch (FU-246), effectful
  direct nodes inside saved child pipelines (FU-248), a Parallel inside a saved child pipeline (FU-249): each is
  refused typed and readable, naming the node, until its follow-up lands.

## Design in one paragraph

The Worker's fan-out runner (agent-runtime `graph/fanout/runner.rs`) owns the children. A paused child raises a card
frame; Main stores the card in `execution_interrupts` (PENDING). When no child can run, the run parks: its terminal
is `fanout_parked_v1`, Main settles the response as parked. A decision (DECIDED) admits one `fanout_wake` job under
the response-row lock; the wake is a new execution of the same response (frozen fresh turn, empty input, marker
`fanout_wake_v1`) that re-enters the parked node, fetches the decided cards over the claim-fenced private route,
resumes exactly those children, ACKs each decision with the child checkpoint it was applied on (CONSUMED), and parks
again or joins. Live frames (`agent_hitl_resolved`, `agent_fanout_wake_admitted`) reach every reader of the
conversation on the project event stream.

## Changed paths (by component)

- Contracts: `libs/proto/contracts/fanout-interrupt-decisions-v1.md` (v1.1; §7 claim rule updated 2026-10-27),
  `application-graph-extensions-v1.json` (parallel admitted), JSON fixtures under `libs/jsonschema/runtime/v1/`.
- agent-runtime: `src/graph/fanout/{plan,runner,frames}.rs` (plan, On Error/Join, admission limits, single-owner
  runner with per-interrupt apply, progress and card frames).
- Worker: `src/agents/graph/parallel*.rs` (compiler, checkpoint, cards, frames, branch runtime),
  `src/agents/graph/fanout_{wake,turn,control,frames}.rs`, `src/agents/session.rs` (production composition,
  `RecoveryFrontier`), `src/agents/pipeline.rs` (wake start, recovery, FU-249 tool refusal),
  `src/agents/assembly.rs` (`CommonProfileMode::FanoutWake`), `src/agents/events.rs` (member tier),
  `src/execution/{fanout_decisions,native_agent_lifecycle}.rs`, `src/protocol/output.rs`.
- Main: `internal/infra/db/repos/execution_interrupts*.go`, `execution_interrupt_wakes.go`, `agent_claim_authority.go`,
  `internal/application/agentexecution/{continue_fanout_wake,fanout_wake_admitter}.go`,
  `internal/api/v2/events/*`, `internal/api/middleware/otel.go` (SSE flush), `internal/domain/{executioninterrupt,
  pipelinelimits,applicationcomposition}/*`, migration `migrations/shared/0194_execution_interrupt_wakes.sql`.
- Web: `src/features/chat-messages/**` (ledger hook, fan-out frames, cards, shared project stream),
  `src/features/pipelines/lib/graphParallelAdmission.helpers.ts`, `src/pages/pipelines/**` (test-chat refusal).

## Tests (final run after the main merge, production profile code paths)

| Suite | Result |
|---|---|
| Main `go test -race ./...` (with `ELITEA_TEST_DATABASE_URL`) | all packages ok, 0 failed |
| Worker `cargo test --all-targets --all-features` | 2786 passed, 0 failed, 103 ignored (pre-existing `#[ignore]`) |
| agent-runtime `cargo test --all-features` | 1559 passed, 0 failed |
| Web `vitest run` | 19150 passed, 1 failed (`themePreference`, Node ≥ 26, see below), 6 expected fail, 1 skipped |
| Clippy (`-D warnings`, all targets) Worker and agent-runtime; `go vet`; oxlint; `tsc --noEmit` | clean |

Skips: the Worker's 103 ignored tests are the pre-existing `#[ignore]` crash-suite and remote-sandbox modules; none
was added. The single Web failure is `src/shared/api/themePreference.test.ts` ("keeps the mark in memory when storage
refuses the write"): it needs Node ≥ 26 (`package.json` engines, CI pins 26); this machine has Node 22 and 24; the
test and its code are not touched by this branch (it arrived with #1206).

Tests that fail without the fix they prove (checked by reverting the fix locally): the FU-249 Main matrix, the two
lost-wake PG tests, the ledger claim-mode PG test, the member-failure PG test, the SSE flush test.

## Performance

| Mechanism | Path | Proving test | Measured |
|---|---|---|---|
| At most two parent appends per activation visit (no O(children) parent writes) | `services/elitea-worker-rust/src/state/postgres_checkpointer/parallel_append.rs:25` (`append_after`) | `parallel_tests.rs:2556` `each_activation_visit_appends_at_most_two_parent_rows` | 2 |
| Bounded admission: 16 branches, concurrency 8, 16 open cards; limit and limit+1 | `libs/rust/agent-runtime/src/graph/fanout/plan.rs:198`, `runner.rs:349` (concurrency), `runner.rs:460` (open cards) | `plan_tests.rs` `child_count_limit_and_limit_plus_one`, `runner_tests.rs` `concurrency_limit_and_limit_plus_one`, `open_cards_limit_and_limit_plus_one`; `parallel_tests.rs:584`, `:1535` | exact |
| Bounded results: child result, joined result 8 MiB, join entry | `plan.rs:228`, `plan.rs:253` | `plan_tests.rs:172-235`; `parallel_tests.rs:2793` | exact |
| A fetch over 16 decisions is a typed ledger fault, never a truncation (REVIEW-1470 #7) | `runner.rs:611` | `runner_tests.rs` `a_fetch_at_its_bound_applies_and_one_more_is_a_typed_ledger_fault` | 16 applied / 17 refused |
| Each child resumed once whatever the answer order | `runner.rs:619` (`apply_decisions`) | `runner_tests.rs` `sixteen_cards_answered_in_reverse_resume_each_child_once` | 16 × 1 |
| Fetch bounded (16 entries, entry size) | `services/elitea-main/internal/domain/executioninterrupt/limits.go:29` | `TestFetchAtSixteenEntries`, `TestFetchEntryAtLimitAndLimitPlusOne` | exact |
| Decision latency (contract §11, p95 ≤ 150 ms) | `services/elitea-main/internal/infra/db/repos/execution_interrupts.go:323` (`Decide`, one transaction, statement budget 13) | `TestDecideLatencyBudget`, `TestDecideSingleTransaction` | p50 4.2–4.3 ms, p95 5.7–6.7 ms, p99 7.9–11.5 ms (3 rounds × 200, real PG, 2026-10-28) |
| Fan-out fetches on one Worker session do not serialize (share locks on session and command rows) (REVIEW-1470 #6) | `services/elitea-main/internal/infra/db/repos/agent_claim_authority.go:64` | `TestClaimAuthorityLetsFetchesOnOneSessionRunSideBySide` (fails with the old `FOR UPDATE`) | no wait |
| Composition check (FU-249 adds one string per version read, no new query) | Main `applicationcomposition` | existing read-budget tests (`TestEachVersionIsReadAtMostOnceWithinTheHopBudget`) unchanged | unchanged |
| One shared project event stream per tab (the ledger no longer opens a second connection) | `apps/elitea-web/src/features/chat-messages/model/useInterruptLedger.ts:167` | `useInterruptLedger.test.tsx` shared-connection test | 1 connection |
| SSE flushed at once behind the compression group | `services/elitea-main/internal/api/middleware/otel.go` | `TestOtelMiddleware_AnEventStreamBehindCompressFlushesAtOnce` | first frame immediate |

Parked wake ≤ 5 s and other tab ≤ 3 s (contract §11) are browser-observed; see the browser section.
Performance follow-ups (not in this PR): a progress coalescer for live member text; caching a held decision by child
checkpoint id instead of re-reading it each tick (REVIEW-1470 #14); `maxLiveAudience = 256`
(`execution_interrupts_wiring.go:217`) — a larger conversation gets no live copy and reconciles on the next stream
re-open or visibility change (REVIEW-1470 #15).

## Durability

| Mechanism | Path | Proving test |
|---|---|---|
| Card durable before the run parks; decision and wake job commit together under the response-row lock | `execution_interrupts.go:323` (`Decide`), `execution_interrupts_wiring.go:355` (`insertWake`), `:407` (`settleParkedResponse`) | `TestDecideVersusParkAdmitsExactlyOneWakeJob`, `TestParkSettlementAndDecisionAdmitExactlyOneWake`, `TestLostAdmitIsSweptExactlyOnce`, `TestTwoSweepersAndDuplicateSubmitsAdmitOnce` |
| A wake that parks again holding a decision (a child with two cards) records the park; the next decision wakes it (REVIEW-1470 #1) | `execution_interrupts_wiring.go:444` | `TestAWakeThatParksAgainWithAHeldDecisionRecordsThePark` (fails without the fix) |
| Child receipts appended atomically under the writer fence; stable child identity across claims and executions | `services/elitea-worker-rust/src/state/postgres_checkpointer/parallel_append.rs:25` | `parallel_tests.rs`, `postgres_checkpointer_tests.rs` |
| Decision crash seams on real PostgreSQL: after fetch, after apply, before ACK, before join, after a child receipt, a two-claim race, a zombie claim's save | runner `fetch_and_apply` (`runner.rs:598`), ACK `runner.rs:587` | `services/elitea-worker-rust/src/state/postgres_checkpointer_tests/fanout_resume.rs:1114,1119,1124,1129,1137,1201,1257` |
| A decision is ACKed with the child checkpoint it was applied on; an ACK from a superseded claim is refused | `runner.rs:587`, Main `execution_interrupts.go:574` (`Ack`) | `TestOldClaimAckRefusedAfterTakeover` |
| A wake lost before its first root save re-enters its park, and only the park its parked execution wrote (REVIEW-1470 #5) | `services/elitea-worker-rust/src/agents/session.rs:1616` (`RecoveryFrontier::WakePark`), `graph/fanout_wake.rs:103` (`is_parked_writer`) | `postgres_a_wake_lost_before_its_first_root_save_recovers_as_the_wake`; `a_wake_never_enters_a_frontier_another_execution_wrote`, `only_the_parked_execution_is_the_parked_writer` |
| An answered child resumed past its pause is never parked again on it: a receipt records its own checkpoint; a carried copy means "resumed" | `src/agents/graph/parallel_checkpoint.rs:425`, `parallel.rs:933` | `postgres_a_wake_lost_mid_model_resumes_the_answered_child` |
| The ledger serves a model-checkpoint reclaim (same ordinary claim) | Main `agent_claim_authority.go:28` | `TestTheLedgerAcceptsAnOrdinaryClaimFirstOrModelCheckpointReclaimed` |

## Resilience

| Mechanism | Path | Proving test |
|---|---|---|
| A member's failure fails its branch (failed join entry), never the run, under On Error `continue`; `stop` fails the node | `src/agents/graph/parallel.rs:1303` (child outcome), agent-runtime `runner.rs` admission | `postgres_a_member_whose_tool_cannot_run_fails_its_branch_not_the_run` |
| A run that ends without parking closes its cards: PENDING → CANCELLED, DECIDED → SUPERSEDED, audited with the run's claim, resolved frames; a later decision gets 409 (REVIEW-1470 #2) | Main `execution_interrupts_run_end.go:57,71,96`, Decide check `execution_interrupts.go:323`, sweep kicked after settlement (`runtimecomposition/composition.go`) | `TestARunThatEndsWithoutParkingClosesItsCards`, `TestADecisionAfterTheRunEndedClosesTheCardsAndIsRefused`, `TestAParkedOrWakingRunKeepsItsCards` |
| A wake that cannot run ends the response readably: cards CANCELLED, `fanout_parked_v1` removed, `graph.fanout.wake_failed.<code>` with a support reference; capacity and unavailable refusals give their attempt back (REVIEW-1470 #3) | `execution_interrupt_wakes.go:200` (`FailFanoutWake`), `:279` (`DeferFanoutWake`), admitter `fanout_wake_admitter.go:119` | `TestAFailedWakeClosesTheCardsAndEndsTheResponseReadably`, `TestCapacityRefusalsNeverExhaustAWake` (21 refusals, then admitted), `TestFanoutWakeAdmitterMapsRefusalsToTypedFailures` |
| A park lists only open cards (REVIEW-1470 #12) | `execution_interrupts_park.go:98` | `TestParkedTerminalSettlesInTheTerminalTransaction` "closed card" |
| `authorize` refused typed until it carries its credential reference (FU-250, REVIEW-1470 #8) | `services/elitea-main/internal/api/v2/agentexecution/interrupts.go:134` | `TestExecutionInterruptDecisionStrictAdmission` "authorize (FU-250)" |
| Typed readable failures: `graph.fanout.child_tool_unavailable` (FU-248), `graph.fanout.agent_pause_unsupported` (FU-246), `graph.fanout.child_kind_unsupported` (FU-247), `graph.fanout.parallel_in_child_pipeline` (FU-249), `graph.fanout.may_have_started`, `graph.fanout.ledger_fault` | `parallel.rs`, `compiler.rs`, `runner.rs:611` | PG and unit tests named in each follow-up below |
| Production Parallel without the claim's decision ledger is refused typed | `session.rs:2110` (`fanout_decisions_unavailable`) | `postgres_production_parallel_without_the_decision_ledger_is_refused_typed` |
| Lease loss stops the runner (control stop, never a business failure); Stop and Regenerate close open cards | `parallel.rs:1896`, Main `execution_interrupts.go:671` (`CancelAllForResponse`) | `TestStopAndRegenerateCloseCardsWithResolvedFrames`, runner lease tests |
| A resolved card is never shown again when re-announced; a parked response re-reads its cards each park and on every stream re-open; one malformed list entry refuses the list (REVIEW-1470 #10, #11) | `apps/elitea-web/src/features/chat-messages/model/useInterruptLedger.ts:156`, `lib/interruptLedger.ts` (`readInterruptList`) | `useInterruptLedger.test.tsx` re-open test, `interruptLedger.test.ts` |

## Security

| Category (`rules/security.md`) | Applies | How it was checked |
|---|---|---|
| Identity from verified credentials | yes (private routes) | The private ledger routes take the claim from the verified mTLS workload certificate and the claim/fence headers (`fenceFromClaim`); `TestFetchAndAckRefuseStaleFences` (wrong fence, identity, claim). |
| Object-level authorization | yes | Public route matrix `TestExecutionInterruptPublicRouteAuthorizationMatrix` (owner, member, foreign project, other user, unauthenticated, token vs session); the ledger matrix (NONE and AGENT_MODEL_CHECKPOINT accepted; NODE_RECOVERY running and suspended, model-checkpoint suspended, foreign projection, stale fence, released, expired lease, expired/revoked session, past deadline, terminal output, lost actor permission refused). Live frames reach conversation readers only (`TestStream_InterruptFramesReachOnlyConversationReaders`). The 2026-10-27 widening is limited to the recovery label of the same live, fenced ordinary claim; desired state stays RUNNING. The shared claim SQL also carries `j.actor_id ~ '^[1-9][0-9]{0,17}$'` (`agent_claim_authority.go:59`, from `779ed912f`), which now gates node recovery too: it only tightens the rule. Session and command rows are share-locked (REVIEW-1470 #6); revocation and retirement still wait for them. |
| Input bounds and strict schemas | yes | Cards, decisions, ACKs and fetch canonicalised and size-bounded (limits tests above); `fanout_wake_v1` admitted only with an empty input and no other HITL field (`a_pipeline_wake_is_admitted_with_its_empty_input_and_only_its_marker`, `TestSubmitRefusesAMalformedWakeTurn`, `TestBrowserContinuationCannotSelectFanoutWake`). |
| Injection | yes (SQL) | Parameterised only; the claim modes are a bound `text[]`. |
| Egress | no new outbound calls | — |
| Secrets and logs | yes | `TestWakeAdmissionLogsCarryNoValues`, `TestPrivateRouteLogsCarryNoValues`; decisions never log values; card values are not in live frames (`TestDecideEmitsAResolvedFrameWithoutTheValue`). |
| Supply chain | no dependency change | `cargo deny`: 23 findings (20 advisories), all pre-existing (no Cargo file changed); `govulncheck`: GO-2026-6809 (Go standard library, toolchain), pre-existing; `npm audit`: 0. |

`/security-review` pass: recorded in the PR body (local findings, if any, stay under
`/Users/romanmitusov/Documents/Work/security-findings/`).

## Recovery rows (replatform-delivery-gate §2)

Updated in `services/elitea-worker-rust/docs/recovery-guarantees.md` (matrix P13, footnote 4, rows Main × P13,
Worker × P13, browser × P13, gap 18):

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Worker × fan-out child (first run), mid-model | R | child model checkpoint, `ScopedCallKind::FanoutChildRecovery` | `postgres_fanout_child_caught_mid_model_resumes_from_its_checkpoint` (production recovery path) |
| Worker × fan-out child, read-only tool in flight | I | `ReadOnlyRecordingToolset` | `postgres_fanout_child_read_only_tool_in_flight_runs_again` |
| Worker × fan-out child, effectful tool in flight | F (typed `graph.fanout.may_have_started`, never repeated) | `tool_may_have_started` marker | `postgres_effectful_fanout_child_may_have_started_*` |
| Worker × crash mid-wake | R | `RecoveryFrontier::WakePark`, carried-receipt rule, ledger claim modes | the two lost-wake PG tests + browser chat 27 |
| Worker × crash with cards open (parked) | R | the park terminal and the ledger | browser chat 24 |
| Main × crash/restart with cards open | R | decision + wake in one transaction, lost admit sweep | Main PG tests above + browser chat 24 |
| Main × reclaim with model-checkpoint recovery | R | `interruptClaimMode` | ledger PG matrix |
| HITL pause and decision (Parallel) | R | ledger PENDING → DECIDED → CONSUMED with ACK | Main PG + browser chats 16, 18, 19 |
| Web × reload, second tab | R | ledger read on mount and at each park, shared stream | browser chats 16, 18, 19, 24, 27 |
| Map | — | still production-gated (G-WORKER-09 narrowed to Map and pod replacement) | — |

The Worker's decision seams run on real PostgreSQL (`fanout_resume.rs:1114-1257`: after fetch, after apply, before
ACK, before join, after a child receipt, a two-claim race, a zombie save) with the in-process ledger; Main's ledger has
its own real-PG matrix (settle, decide, fetch, ACK, close, wake). One test driving a Worker reclaim against Main's real
ledger was not practical (two services, two languages): the proof is that pair plus the browser rerun on images built
from the final head (see the browser section).

NATS × P13 and PostgreSQL × P13 stay `R·U` in `recovery-guarantees.md`: a lost live frame (NATS) is recovered by the
Web reconcile on every stream re-open, park and visibility change (REVIEW-1470 #10), and the ledger is the durable
record; a PostgreSQL failover during a decision is covered by the same transactions but has no failover test.

## Browser evidence (real Chrome, stack `fanpar`, production profile, verify-profile 50/0)

Images at the end: Main `2a2-473c27f27`, Worker `2a2-c3c072a1a`, Web `2a2-59295e5d5`, supervisor `2a2-40178539e`.
Each Worker/Main image was binary-checked for a string unique to its commit (`branch-receipt-checkpoint.v1`,
`parallel_in_child_pipeline`, `c.recovery_mode = ANY($6::text[])`) and built from a branch containing #1160. Two tabs,
reload each. Full log: `handoffs/point5-wave1-20261008/2a2/evidence/browser-cases.md` (screenshots there).

| Case | Chat | Result |
|---|---|---|
| Mixed: ask_user LLM, sensitive tool approve + reject, saved Agent (FU-246), direct node (FU-248) | 16 | answered/approved/rejected across tabs; reload; joined: "blue", reverse executed, "The call was declined.", direct `graph.fanout.child_tool_unavailable` readable; the saved Agent branch failed generic because every ordinary saved agent on this stack is refused by the model service (403, environment) — FU-246's typed refusal is proven by its PG test |
| 1 three asks, reverse order, two tabs | 18 | other tab drops a card within 3 s; joined; reload |
| 2 HITL + ask_user | 19 | joined; reload |
| 4 crash seams | 24–27 | Worker killed with cards open; Main restarted with cards open; Worker killed mid-wake: reclaim SUCCEEDED, answer applied once, no new card, joined after reload |
| 5 Stop / supersede | 24 | no Stop while parked (by design); Regenerate supersedes the open cards in both tabs |
| 6 Parallel in a saved child pipeline | 20 | named 422 at start, no execution; attach refused inline, nothing written (FU-249) |
| 8 nested fan-out | "FP nested" | editor names the node; test chat now shows Main's text |
| 7 branch failure | 16 | direct branch failed typed and readable after drain |

Fixtures, stated honestly: project 118 "Rust instruction revision acceptance" on the real-model dump `d5e966074`.
Created in the UI: every pipeline ("FP …"), the MCP toolkit (saved through the JSON view because MCP tool discovery
from the UI fails, pre-existing), all chats. Set by SQL (`2a2/fixtures.sql`): `meta.internal_tools=["ask_user"]` on
"FP ask", because the pipeline editor offers only Attachments (EliteaUI parity). No response mocks.

## Defects found by running it (all fixed here)

1. A member's own failure ended the root run (607c2d3fe). 2. Member calls shared one key (34aaef03f). 3. The ledger
opened a second event connection (150eab930). 4. The park was not the run's paused terminal (2ffcb69d4). 5. The wake
was refused by the profile (341677bb9). 6. The wake did not resume the answered pipeline branch (cbf7cab69). 7. A
resolved card re-appeared when re-announced (0d15dadd5). 8. A branch whose tool could not run failed generic
(bbca9e018, FU-248). 9. A parked response did not re-read its cards (7e25dcbfd). 10. Pre-existing on main: the project
SSE stream never flushed behind the compression group (ebf5b34d3). 11. A Parallel in a saved child pipeline failed
generic (997cebbbd, 59295e5d5, FU-249). 12–14. Crash mid-wake: the park was refused as foreign (26901e200), the ledger
refused the model-checkpoint reclaim (473c27f27), the answered child was parked again (c3c072a1a).

## Follow-ups

- FU-246: a saved Agent branch's own pause (refused typed `graph.fanout.agent_pause_unsupported`).
- FU-247: inline `llm` branches (refused typed at save, start and assembly; work on `wip/fu247-inline-llm-branches`).
- FU-248: effectful direct nodes inside saved child pipelines (refused typed `graph.fanout.child_tool_unavailable`).
- FU-249: a Parallel inside a saved child pipeline or an Agent's pipeline tool (refused typed
  `graph.fanout.parallel_in_child_pipeline` / Main bound `parallel_in_child_pipeline`); needs a capability-preserving
  appender through the activation/static-resume/scope chain, reused by Map-in-child.
- Web: Stop pressed before the start request answers only detaches (no cancel target yet).
- Web: a second tab does not attach a regenerated run live (shows it after reload).
- Web: "Thought for -1 h -1 min -1 sec" after a park in a reloaded tab; a stray "1. <question>" line above the result.
- Main: retire the last wake row at the run's terminal (stays ADMITTED, cosmetic).
- Main: steering (`agent_steering.go:445`) accepts only `NONE` claims; check whether a model-checkpoint reclaim should
  consume steering (not used by the fan-out runner).
- FU-250: `authorize` on a fan-out delegated-auth card carries its credential reference (refused typed until then).
- FU-251: a NODE_RECOVERY claim that reaches a Parallel node ends typed instead of a lease-lost control stop.
- Cache a held decision by child checkpoint id instead of re-reading it each tick (REVIEW-1470 #14).
- Live member text coalescer (performance).
- Kubernetes pod replacement for P13; Map (G-WORKER-09).
- Governance (ADR-0039, merged from main on 2026-10-27): a fan-out wake admission records no application version in
  its execution security context facts (as other continuation kinds on main); a governed run that parks and wakes is
  untested.

## Merge with main (2026-10-27)

`main` took migrations 0184–0188, 0190 and 0191 and claim field 18 (`governed_security_context_v1`): the park/wake migration is now
`migrations/shared/0192_execution_interrupt_wakes.sql` (manifest head 192; renumbered to 0194 below) and the Worker's opt-in is
`ClaimCommandRequestV1.fanout_interrupt_decisions = 19` (neither was released). Generated Go/Python protobuf code and
`api.gen.go` were regenerated with the pinned tools (`scripts/contract/generate_proto.sh`, oapi-codegen v2.7.2).
A later merge (2026-10-28) brought migration 0193. The base-branch check requires a new migration above main's
head, so the park/wake migration is renumbered to `0194_execution_interrupt_wakes.sql` (manifest head 194); 0192
stays a gap like 0189. Only the file name and its header line changed. main moved the turn checks into `validateAgentTurns`; the fan-out wake check
moved with them, and the admission carries both `CurrentFanoutWakeTurn` and main's `GovernedCredentials`.
`main` routed agent claims through a shared fresh-claim builder that dropped the fan-out opt-in; agent claims now use
an agent builder with both opt-ins (`ordinary_agent_claims_opt_into_fanout_interrupt_decisions` caught it).
