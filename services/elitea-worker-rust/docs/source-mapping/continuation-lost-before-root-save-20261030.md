# A continuation lost before its first root save ends typed (FU-265), 2026-10-30

- **Branch:** `fix/fu265-continuation-paused-writer`, off `main` `9ba18a6ac`.
- **Plan:** `handoffs/point5-wave1-20261008/fu265/PLAN.md`. The orchestrator approved it with changes C1–C5
  (`PLAN-REVIEW.md`), then decided **option 2, narrowed**.
- **Scope:** Worker only.

## Business behaviour

- **The run.** A decision on a paused pipeline (an approval, an answer or an MCP authorization) starts continuation
  B. B re-enters the root checkpoint row written by the paused execution A.
- **Before.** When B was lost before its own first root save (for example inside an approved call in a saved child),
  the reclaim ended with the **generic Internal** failure. The call was never repeated, because its journal or
  dispatch record holds it.
- **Now.** The reclaim ends with the registered `RECOVERY_FRONTIER_UNSUPPORTED` terminal: "This run stopped at a point
  where it cannot be safely continued. Nothing was repeated." It never ends Internal, with or without a paused-writer
  marker. Nothing is repeated.
- **Wider scope (REVIEW-1544 D1).** The typed refusal is the `else` branch of `inspect_checkpoint`
  (`session.rs:1874-1882`), which `pipeline.rs:1380` calls on any recovery start. So every reclaim whose latest root
  row was written by another execution now ends with `RECOVERY_FRONTIER_UNSUPPORTED` (`root_writer_mismatch`) instead
  of Internal. That includes a fresh turn, a regeneration or a plain restore lost before its first root save, not only
  continuations. Re-running such a fresh turn from its input (class I) is registered as FU-270.
- **Not yet.** Re-running the continuation, so that the call's journal decides R / C / I, is **FU-269**. It needs an
  owner-proven session suffix, which every continuation's anti-stale guard currently refuses.
- **Rollout.**
  - The new Worker tolerates the paused-writer marker (`elitea.continuation.paused_writer.v1`) and parses it strictly.
  - No Main sends it yet; Main's marker is FU-269. The tolerant Worker therefore reaches a release before any Main
    sends it.
  - `UPGRADING.md` needs nothing in this PR.

## How the first attempt adopts the paused row today (C2)

- `src/agents/session.rs` `resolve_pipeline_start` (about `:2700`): the `StaticTools`, `Hitl`, `McpAuthorization` and
  `Text` arms prove the pause from the **root session events**.
  - The static-tools arm and the application-sensitive arm use `dispatch_journal::continuation_pause`: the pause is the
    last event, or this execution's `continuing` record follows it.
  - The other resolvers require the pause to be the last event.
- The graph then resumes from the latest root checkpoint. **No root-row writer check is made.**
- The reclaim (`inspect_checkpoint`) accepted only this execution's own row (`Own`), or a fan-out wake's parked writer
  (`WakePark`). A continuation's paused row was therefore refused untyped.
- **This change applies one proof to both,** `prove_continuation_row` (`session.rs:1784`):
  - the row is this execution's own; or
  - it was written by exactly the named paused writer, is a pause frontier on this run's root thread, and the root
    session's latest pause event names exactly that row.
- **The first attempt** runs it when the marker is present (`session.rs` about `:2739`). An older Main sends no marker,
  so today's behaviour is kept.
- **The reclaim** runs it when the marker is present, then ends typed `continuation_rerun_unsupported` until FU-269
  (`:1859-1873`). Without the marker, a foreign writer is `root_writer_mismatch` (`:1882`).

## Changed paths

- `src/agents/graph/continuation_writer.rs` (new): `PausedWriter`.
  - The marker must be an object with exactly `execution_id` (32 lowercase hex) and `generation` (at least 1).
  - `is_writer` is an exact match against the root row's `elitea.pipeline.execution.v1`.
- `src/agents/assembly.rs:600`: the continuation profile modes (continuation, guardrail continuation, MCP
  authorization) admit the marker key beside their allowed meta.
- `src/agents/graph/fanout_wake.rs:22`: a wake carrying the marker is refused.
- `src/agents/runtime.rs:455-464`: the plan carries the marker. A marker on a start that does not re-enter a paused
  row (a fresh run, a regeneration, a plain restore, a wake) is refused (`reenters_paused_row`, `:574`).
- `src/agents/session.rs`:
  - `prove_continuation_row`;
  - the reclaim branch;
  - the typed causes `root_writer_mismatch`, `continuation_pause_unbound` and `continuation_rerun_unsupported`, all
    recovery-frontier causes, which `assembly_failure` maps to `RECOVERY_FRONTIER_UNSUPPORTED`;
  - the ids-only `pipeline_recovery_frontier_refused` log.

## Tests

- **Two-process real PostgreSQL** (`src/agents/pipeline_fanout_effect_pg_tests.rs`: process `:4277`, driver `:4393`):
  - A pauses on a saved child's sensitive direct call (depth 1);
  - B approves it and dies inside the journaled call (`JournalCrashingConnector`: its Started record is durable);
  - B is reclaimed.

  The cases:
  - `postgres_an_approved_child_call_lost_before_the_continuation_saves_ends_typed_and_is_not_repeated` (`:4455`): with
    the marker, the row is proven and the reclaim ends `continuation_rerun_unsupported`;
  - `postgres_a_continuation_without_its_paused_writer_is_refused_typed_on_reclaim` (`:4465`): without the marker, it
    ends `root_writer_mismatch`.

  Both assert:
  - 0 repeated calls, and the journal-thread set is unchanged;
  - **C4:** the refusal log line carries the cause, B's execution and, with the marker, A's execution. It does not
    carry the interrupt id, the tool call id, the action, the tool name or the card message. The phase runs in its own
    process, so its global subscriber captures exactly its logs.
- **Unit tests:**
  - `continuation_writer`: the marker is optional and strict (an array, a wrong type, a zero or negative generation, an
    uppercase or short id, a missing field or an unknown field are refused). It matches only its own writer.
  - The three causes are recovery-frontier causes.
- **`pipeline_tests` `a_paused_writer_is_refused_on_a_fresh_run`:** a fresh run carrying the marker is refused.
- **Without this change:** the reclaim returns `invalid_configuration` with no cause, the generic Internal, so the
  cause assertions fail.

## Performance

- No change on the hot path.
- A reclaim with the marker reads the root session events once, which the continuation reads anyway.

## Durability

- No new state.
- The journal and dispatch records still hold the call, and the reclaim never runs it again.

## Resilience

- A continuation lost before its first root save now ends with a registered, readable terminal instead of the generic
  Internal.
- The run can be started again by the user. Nothing was repeated.

## Security

- **The marker grants nothing.** On a reclaim it only selects which typed refusal is given. On a first attempt it adds
  a refusal: an exact writer match is required.
- **Parsing is strict.** The marker is an object; unknown fields are refused; the id is 32 lowercase hex characters;
  the generation is a positive u64.
- **It is refused off a continuation:** on a fresh run, a regeneration, a plain restore and a wake.
- **The refusal log carries ids and generations only.** A test proves it.
- **No change** to authorization, SQL, egress or credentials.
- **Scanners:** no dependency manifest changed, so `cargo deny` reports the same results as main.

## Recovery rows (gate §2)

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Worker × a decided continuation (approved call in a saved child) lost before its first root save | F (typed `RECOVERY_FRONTIER_UNSUPPORTED`, never repeated). Known gap: R/C/I is possible and scheduled as FU-269. | `session.rs:1859-1882`, `prove_continuation_row` `:1784` | the two two-process tests above |
| Worker × a non-continuation run (fresh turn, regeneration, plain restore) lost before its first root save | F typed `RECOVERY_FRONTIER_UNSUPPORTED` (`root_writer_mismatch`); was Internal | `session.rs:1874-1882` (the `inspect_checkpoint` writer `else`), reached from `pipeline.rs:1380` on any recovery start | by construction: the same branch the no-marker two-process test exercises. Known gap: a fresh turn could re-run from its input (I), FU-270. |

## Follow-ups

- **FU-269 (next, before FU-264), with its own plan review:**
  - re-run the continuation (R/C/I) with an owner-bound `continuing` record on every continuation and a session view
    truncated at the pause for owner-proven suffixes;
  - Main's marker, with the settlement generation, C2/C3/C5 and R1/R2;
  - the crash matrix: root direct node, depth 1, depth 2, HITL node.

  Main's work in progress is on `fix/fu269-continuation-rerun-main-wip`.
