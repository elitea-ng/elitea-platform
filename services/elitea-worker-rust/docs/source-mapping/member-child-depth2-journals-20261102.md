# An effectful direct node two levels below a fan-out member runs; a lost member wake never asks again (FU-264, FU-273), 2026-11-02

- **Branch:** `fix/fu264-member-child-depth2-journals`, off `main` `9f83b7a24`.
- **Plan:** `handoffs/point5-wave1-20261008/fu264/PLAN.md` (A1–A3), the orchestrator's projection decision (option A,
  P1–P5), and FU-273 folded in (D1–D6, E1–E3).
- **Scope:** Worker only, plus a Main real-PG test and a Web test. No contract, migration, Main code or Web code change.
- **Commits:**
  - `107fdd28f`: the FU-264 fix (journals at every saved-pipeline level, the member's own tier projected);
  - `ccd45f326`, `ae675d45f`: FU-264 crash and routing tests;
  - `d53d9cd58`: the FU-273 fix, on its own (D5), cherry-pickable;
  - `0f938e482`: the Main E1 test;
  - `049d945fb`: the Web live and reload test (P3).

## Business behaviour

- **The run.** A fixed Parallel member runs a saved pipeline. That pipeline's Agent node runs another saved pipeline,
  which holds an effectful direct toolkit/MCP node, possibly sensitive (approval card).
- **Before (FU-264).** At two Agent-node levels below the member, the direct node was refused typed and never ran, and
  a member whose saved child ran its own Agent node failed projection (`InvalidState`) at the inner call.
- **Now.** The node runs once, behind the member branch family's node journals, at one and two levels down. The inner
  Agent node's calls stream under the member's tier, live and after a reload.
- **Before (FU-273, pre-existing on main).** When the wake that runs a member's approved call was lost inside that
  call, the reclaim treated the decided card as already answered, re-ran the member without it, and raised a new card.
  Approving that card ran the effect again under a later wake execution: `reverse` was called 3 times for 2 members
  (fanpar, main's Worker `pm-8c7e1bb75`, chat 45).
- **Now.** The reclaim re-delivers the decision against the member's pause and the call's journal decides: lost inside
  the call ends `graph.fanout.may_have_started` with 0 repeats, lost before it runs it once, lost after its commit
  replays it. No card is raised again.
- **Current platform.** A lost run is lost and the user starts again. That is not ported.
- **Composition bound.** `MAX_PIPELINE_COMPOSITION_DEPTH` is 3 (`src/agents/pipeline/composition.rs:20`): root, the
  member's child and the inner child is the deepest admissible chain, so there is no depth-3 case below a member.

## Worker

### FU-264: one rebind at every level

- `PipelineNodeRuntimes::with_branch_journals` (`src/agents/graph/compiler.rs:429`) sets the branch family's journals
  and rebinds its own application resolver, so each nested resolver's `for_branch_journals` reaches the next level.
- `rebind_branch_journals` calls it (`src/agents/pipeline/parallel_application_resolver.rs:67`).
- Only participants that already hold journals get them; a pipeline bound without node recovery gains none.
- The member's own child events carry the member-thread routing key (`src/agents/graph/node_events_parallel.rs:213,374`,
  `FANOUT_MEMBER_THREAD_KEY`, read at `src/agents/events.rs:6782`), so the inner Agent node's calls project under the
  member's tier.

### FU-273: a decision is answered as applied only with proof

- **Cause.** The wake resumed the member's pause inside its saved pipeline, and the branch saved a row before the call
  returned. That row's receipt is carried from the pause (not its own), and the inner pipeline's own thread never saved
  past its paused checkpoint. On reclaim the runner restored from that newer row, found the decided card not open, read
  a "paused at a newer checkpoint" view and acknowledged the decision as applied without the member ever passing its
  pause.
- **The rule** (`unproven_resume`, `src/agents/graph/parallel.rs:1013`): when the branch's latest rows have no receipt
  of their own (`resumed_without_receipt`, `:1127`) and none of the pipelines the pause names
  (`nested_pause_checkpoints`, `src/agents/graph/parallel_cards.rs:149`) saved past its own paused checkpoint, the
  branch's truth is its pause row. It applies to:
  - the restore (`restored_replay`, `:1113`, called at `:829`): the member is restored parked at its pause;
  - the apply proof (`view`, `:1293`): the decided card is open and classifies `Apply`;
  - the child run (`run_child`, `:1514`): the decision is resolved against the pause row through a read-only view of
    the branch's checkpoints (`PauseRowView`, `:1069`).
- **E2.** The decision is acknowledged only by the settlement that follows the re-delivery, naming the member's own
  receipt row. If any named pipeline saved past its pause, the existing path applies (that row is the proof).
- **E3, which row the re-delivery resumes from.** The resume controls bind to the pause row; the branch graph then
  continues from its latest row (the lost attempt's row, same pending node), and the member's saved pipeline re-enters
  its own paused checkpoint with the decision, so the call goes through its node journal. The lost attempt's row is never
  a frontier the member continues from without its decision; the depth-1 and depth-2 tests assert no pause row after the
  decided one and exactly one settlement row.
- **Agent members are unchanged.** An Agent member's own pause keeps its existing reclaim (PM-1,
  `postgres_an_approved_agent_member_call_in_flight_at_a_crash_ends_typed`): its approval is durable and its reclaim
  already ends typed. A first version that included those pauses broke PM-1 and was narrowed.

### E1: wakes are serialized per response (no journal key change)

The node journal identity includes the execution and generation (`src/state/postgres_checkpointer/node_attempts.rs:62-99`,
`:77-78`). A different wake execution never runs a lost wake's decided card, because Main admits one wake at a time per
parked response:

- a decision POST queues a wake only when the response is parked; while a wake runs it queues nothing
  (`services/elitea-main/internal/infra/db/repos/execution_interrupts.go:447`);
- admitting a wake clears the parked execution with the queued insert (`execution_interrupts_wiring.go:366`), and only a
  parking execution sets it again; a park with a queued or admitted wake queues nothing (`:465`);
- an admitted wake is retired only when its job is terminal (`execution_interrupt_wakes.go:401`), and admission needs
  the response parked by the named execution (`:317`).

So a lost wake's decision is re-delivered only by the reclaim of the same wake execution, under the same journals.
Proven by `TestASecondDecisionWaitsForTheRunningWake`
(`services/elitea-main/internal/infra/db/repos/execution_interrupt_wakes_postgres_integration_test.go:237`). The
orchestrator accepted this in place of a journal key change.

### D3, D4

- **D3, no Main change.** A re-raised card is a new pause to Main (new key and id); matching it to the old one would be
  a heuristic over Worker-owned identity, and refusing it would leave the member unable to finish.
- **D4, other paths.** A sensitive root direct node or a saved child outside fan-out has no fan-out ledger: a decided
  continuation lost inside the call is reclaimed as the same execution with its own decision (FU-269,
  `continuation-rerun-20261102.md`), so its journal decides.

## Changed paths

- `src/agents/graph/compiler.rs`, `src/agents/pipeline/parallel_application_resolver.rs` (FU-264 rebind);
- `src/agents/graph/node_events_parallel.rs`, `src/agents/graph/node_events_parallel_tests.rs` (member tier);
- `src/agents/graph/parallel.rs`, `src/agents/graph/parallel_cards.rs` (FU-273);
- `src/agents/pipeline_fanout_effect_pg_tests.rs` (tests);
- `services/elitea-main/internal/infra/db/repos/execution_interrupt_wakes_postgres_integration_test.go` (E1);
- `apps/elitea-web/src/features/chat-messages/lib/chatStreamMemberDepth2.test.ts` and two fixtures (P3);
- docs: this mapping, `README.md`, `recovery-guarantees.md`, `fanout-parallel-admitted-20261027.md`,
  `child-effectful-direct-nodes-20261030.md`.

## Tests

- **Worker, full suite** (`cargo test --offline --locked --no-fail-fast --all-targets --all-features --
  --test-threads=4`, real PostgreSQL): 2977 passed, 1 failed, 105 ignored. The lib target: 2872 passed, 1 failed,
  105 ignored (FU-269's lib target: 2860 passed, 105 ignored; this branch adds 11 tests). The one failure,
  `lock_bound_tests::checkpoint_write_behind_a_stalled_writer_fails_typed_within_the_lock_bound`, measured an 8.8 s
  client wait against PostgreSQL's 10 s lock timeout while the Docker VM ran an image build (host load ~139). Rerun at
  normal load it passes (10.5 s). This branch does not touch the checkpointer.
- **Clippy:** `cargo clippy --offline --all-targets --all-features -- -D warnings`, clean.
- **New Worker tests** (real PostgreSQL, tool and model calls counted):
  - `branch_journals_reach_the_nested_saved_pipelines` (`src/agents/graph/compiler.rs` unit test): the level-2
    participant gets the branch factory, never the root's; fails without the change.
  - `postgres_a_member_whose_child_runs_an_agent_node_completes_and_projects_the_inner_tier`
    (`pipeline_fanout_effect_pg_tests.rs:1774`, P4): fails on main.
  - `postgres_a_member_whose_child_calls_an_effectful_direct_tool_runs_it_once` (`:1851`) and
    `postgres_a_member_child_sensitive_direct_call_pauses_then_runs_once_or_never` (`:4258`), now at depth 1 and 2.
  - Two-process crash cases (`member_direct_case!`, `:5234-5248`): a started call at depth 1 (FU-267) and 2 ends the
    member `graph.fanout.may_have_started`; a committed call at depth 2 replays with 0 calls.
  - FU-273 (`member_wake_case!`, `:5671-5700`), the wake under its own execution, depth 1 and 2: lost inside the call
    (C → F, 0 calls), before it (I, 1 call), after its commit (R, 0 calls, 1 model request). Each asserts no card raised
    again, one ACK `Applied` naming the member's own settlement row, 0 open decisions, and no pause row after the
    decided one. The started cases failed on main (scratch worktree, depth 1) and on the branch before the fix.
- **Main:** `TestASecondDecisionWaitsForTheRunningWake` (real PostgreSQL), passes.
- **Web:** `chatStreamMemberDepth2.test.ts`, live (park, decision, wake) and reload, from real fanpar frames and trace
  rows (chat 47); passes. Typecheck and lint pass. The full Web unit run on this machine reported 17 failures in 13
  files under heavy load; the branch adds only this test file. The PR's CI Web job is the comparison of record.

## Performance

- **Rebind:** one `Arc` clone per saved-pipeline level at assembly, bounded by the composition depth (3).
- **FU-273 rule:** nothing on the normal path. Only when a restored branch's latest row has no receipt of its own does
  it list the branch's history (bounded by `MAX_ANSWERED_PAUSE_SCAN`, 64) and load each pipeline the pause names (at
  most the composition depth). That happens on reclaim only.

## Durability

- No row is deleted or rewritten. The pause view is read-only; the re-delivered run appends after the lost attempt's
  rows.
- The decision stays DECIDED until the member settles; it is acknowledged with the member's own settlement row.

## Resilience

- A wake lost inside a member's approved call now ends the member typed (`graph.fanout.may_have_started`) instead of
  re-asking and repeating the effect. Lost before the call it runs once; lost after its commit it replays.
- Every refusal stays typed; nothing new maps to Internal.

## Security

- **Authorization and identity:** unchanged. No route, RPC, claim or admission changes. The re-delivered decision is the
  one Main recorded for that card under the response lock; the Worker only changes which of its own rows the decision
  is resolved against, and only to the pause the card names.
- **Effects:** no effect is dispatched outside its node journal; the change removes a path where an approved effect ran
  twice.
- **Parsing:** the pause bindings are the existing strict parsers (`pipeline_*_event_binding`).
- **SQL:** test SQL only, parameterized or constant.
- **Logging:** no new log fields.
- **Scanners:** no dependency manifest changed (`Cargo.toml`/`Cargo.lock`, `go.mod`/`go.sum`, `package*.json` are
  identical to `9f83b7a24`), so every finding is pre-existing on main:
  - `cargo deny --all-features check advisories` (Worker): 8 advisories, RUSTSEC-2024-0449, RUSTSEC-2026-0192, -0206,
    -0381, -0382, -0383, -0414 and -0433, none added;
  - `govulncheck ./...` (Main): 0 vulnerabilities in imported packages, 1 in a required module that the code does not
    call.

## Recovery rows (gate §2)

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Worker × an effectful direct node in a fan-out member's saved child, one or two Agent-node levels down: first run, sensitive pause and decision | I (runs once), R (card, decision, one call) | `compiler.rs:429`, `parallel_application_resolver.rs:67` | `:1851`, `:4258` at depth 1 and 2; browser chats 45 and 47 |
| Worker × crash inside that call (the run's own claim) | C → F (`graph.fanout.may_have_started`), R (committed: replayed) | the branch family's node journals | `member_direct_case!` (depth 1 started, depth 2 started and committed) |
| Worker × a member's wake lost inside / before / after its approved call (FU-273) | C → F / I / R; never re-asked | `parallel.rs:1013,1113,1293,1514` | `member_wake_case!` depth 1 and 2; browser chats 45 and 47 |
| Main × a decision while a wake runs | I (queues nothing; the running wake's reclaim delivers it) | `execution_interrupts.go:447`, `execution_interrupts_wiring.go:465` | `TestASecondDecisionWaitsForTheRunningWake` |
| Web × a member's inner Agent node, live and reload | R | no change | `chatStreamMemberDepth2.test.ts`; browser reload |

**Removed gap:** `Worker × P05`, "two levels below a fan-out member is refused typed (FU-264)".

## Real-browser evidence

- **Stack:** the shared fanpar stack. No migration, so a plain image swap: `profile.env` backed up
  (`.bak-pre-fu264`), restored afterwards, verify-profile 49/1 before and after (the known stale-worktree check).
- **Images:**
  - Worker `elitea-worker-rust:fu264-049d945fb` (branch HEAD), binary-checked: it contains `pause view is read-only`
    and `unproven_resume`; the earlier `fu264-ae675d45f` image does not.
  - Web `elitea-web:fu264-ae675d45f`, built from the branch with no Web diff against main `9f83b7a24` (main's Web).
  - Main `elitea-main:pm-8c7e1bb75`.
- **Stack note:** fanpar's Main had exited (code 1) at 18:52 UTC during heavy host load ("runtime outbox publisher
  stopped", restart policy `no`). It was started again before the check; nothing else was changed.
- **Fixtures,** made in the UI (project 118): app 17 "FP parallel child direct" (depth 1), app 18 "FP parallel deep"
  (depth 2), each two members running the sensitive MCP `reverse`, then a root `echo`.
- **Depth 1, chat 45:**
  1. Two member cards. Card (1) approved: one `tools/call` (19:54:33).
  2. Card (2) approved with a streamed kill on the MCP `tools/list`: `tools/call` at 19:55:00.877, Worker killed at
     19:55:01, started at 19:55:04.
  3. The reclaim (19:56:00): member 2 failed `agent.recovery_refused`, the fan-out joined, then the root `echo`
     `tools/call` (19:56:00.921). The wake job SUCCEEDED.
  4. Main's ledger for the response: 2 cards, both CONSUMED (card 2 at 19:56:00.914, after the member settled). No third
     card. After a reload: no card, the member tier shows `FP direct reverse` error, and the answer is the `echo` output.
- **Depth 2, chat 47:** the same: member 1's `reverse` and its child's `echo` (20:01:30), member 2's `reverse`
  (20:01:55.35), kill at 20:01:55, the reclaim joined at 20:02:55.443 and only the root `echo` followed (20:02:55.451).
  2 cards, both CONSUMED; both wake jobs SUCCEEDED. After a reload: two member tiers, member (2) shows
  `FP direct reverse` and `FP child direct` as errors, no card.
- **Before (main's Worker, chat 45 earlier):** the same kill re-raised a card, and approving it called `reverse` again.
- **Where the files are:** `handoffs/point5-wave1-20261008/fu264/evidence/`, outside git.

## Fixtures

- Built in code for the Worker tests; real pauses and real process loss (two processes for the crash cases, a dropped
  run for the wake cases). `committed` and `undispatched` are narrowed by deleting rows, as in FU-248.
- The Web fixtures are a real fanpar run's replayed frames and stored trace rows, scanned for credentials, emails and
  internal URLs.

## Follow-ups

- The member tier shows a bare "error" for a member that ended `graph.fanout.may_have_started`; the readable sentence
  the root path shows is not rendered for a member. Candidate follow-up.
- fanpar's Main exits on "runtime outbox publisher stopped" under heavy host load and is not restarted (compose restart
  policy `no`). Observed, not investigated here.
