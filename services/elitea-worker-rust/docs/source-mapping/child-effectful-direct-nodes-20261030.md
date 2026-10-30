# Effectful direct nodes in saved child pipelines (FU-248), 2026-10-30

**Branch:** `fix/fu248-child-effectful-direct-nodes`, off `main` 0990ab10f.
**Plan:** `handoffs/point5-wave1-20261008/fu248/PLAN.md`, approved with option B (recovery) by the orchestrator.
**Scope:** Worker only. Main and the Web are unchanged.

## Business behaviour

- **Saved children.** A direct toolkit or MCP node that changes data (a DB or HTTP write) works inside any saved child
  pipeline:
  - an Agent node running a saved pipeline;
  - a fixed Parallel member running one;
  - two Agent-node levels down.

  Before, it was refused at binding (`pipeline.effect_writer_unavailable`), before it could show its card. A Parallel
  member failed its branch as `graph.fanout.child_tool_unavailable`.
- **Sensitive calls.** A sensitive call pauses with its own card: approved, it runs once; rejected, it never runs.
- **A Worker lost inside the call** is recovered by what the call's journal holds:
  - a committed result is replayed without calling again;
  - a call that started and may have reached its tool ends with the registered "may have started, not repeated" stop;
  - a call that never started runs once.
- **Read-only calls.** A read-only direct node in a child reruns on recovery (class I).
- **Not ported from the current platform:** its "lost, run again from scratch" behaviour for a crashed child.

## What was found on the way

- **Task message appended twice.** A child re-entered at its own unfinished frontier got its task message appended twice.
  The Agent node re-merged the task into the child's restored state through the `messages` append reducer. FU-249 had
  fixed this only for a child holding a Parallel.
  - The doubled message changes a journaled call's input digest, so a reclaim looked for a different journal. For a call
    that had started, that could have dispatched it a second time.
  - The rule now applies to every child (`agents/graph/application.rs:784`). The overlay shows only this visit's
    frontier, so a completed earlier visit still starts fresh.
- **Fan-out branch families.** A fan-out member's child threads belong to the branch's own checkpoint family, not the run
  root's. The member's saved pipeline therefore journals through that branch family.
- **Code nodes.** Lending the run's journals to all node kinds would also have enabled Code nodes in saved children. Their
  recovery depends on Main's code-owner receipts, which this change does not cover. The journals are lent to direct tool
  nodes only, and Code in children is unchanged.

## Changed paths

- **Lending.**
  - `src/agents/pipeline.rs`: `PipelineApplicationRuntime::node_recovery`, fed by `state.node_recovery_authority()`
    (`:1434`); `bind_saved_pipeline_runtimes` lends it as `with_direct_tool_recovery` (`:2644`).
  - The pipeline-as-agent-tool path passes `None`.
  - `src/agents/graph/compiler.rs`: `with_direct_tool_recovery` `:417`; the direct-node binding uses it when there is no
    root authority (`:1685`).
- **Fan-out branches.**
  - `src/agents/graph/parallel.rs`: `ParallelChildCheckpoint::node_recovery` and `PreparedParallelBranch`.
  - `src/state/postgres_checkpointer/application_children.rs:309,372`: the branch family itself.
  - `src/agents/graph/parallel_checkpoint.rs`: `ParallelBranchExecution::node_recovery`.
  - `src/agents/graph/parallel_compiler.rs:1025`: `branch_journal_resolver`.
  - `src/agents/pipeline/parallel_application_resolver.rs:54`: `rebind_branch_journals`.
  - `PipelineApplicationResolver::for_branch_journals`.
- **Visit scope.**
  - `libs/rust/agent-runtime/src/graph/application_activation.rs`: `visit_digest` `:36`, `stamped_visit_digest` `:79`,
    `ApplicationActivationGuard::visit_digest` `:272`.
  - `src/agents/graph/application.rs`: `APPLICATION_VISIT_STATE_KEY` `:53`, injected at `:731`. It is a runtime channel in
    every pipeline, Agent-member branch and Map worker schema (`compiler.rs`, `parallel_compiler.rs`, `map_compiler.rs`).
  - `src/agents/graph/node_recovery_runtime.rs:68`: `visit_from_state`.
  - `src/state/postgres_checkpointer/node_attempts.rs`: a child requires a visit and the root has none (`:32`);
    `journal_activation_id` `:62`.
- **Recovery.**
  - `src/agents/session.rs`: in `walk_child_frontier` (`:1637`), a missing first row is accepted only for a child that
    starts at a direct node (`:1660`), and the leaf is an unowned direct node (`:1710`).
  - `src/agents/graph/compiler.rs`: `is_unowned_direct_tool` `:1054`, `starts_at_direct_tool` `:1065`.
  - `src/agents/graph/node_recovery_runtime.rs`: a child visit's reconcile becomes `CHILD_EFFECT_UNKNOWN_CODE` (`:967`,
    `:1198`).
  - `src/agents/graph/direct_tool.rs:837`: that code becomes the registered may-have-started stop (`approved_call_stop`).
- **Double append:** `src/agents/graph/application.rs:784`.
- **Other:** `src/sandbox/service.rs` boxes one future (`clippy::large_futures`, grown by the new fields).

## Tests

All against real PostgreSQL, except the pinned-identity test, which is a unit test. Runs were at 8 to 16 test threads.

- **`fanout_effect_pg_tests`:** 47 passed, 0 failed, 0 skipped, 2 of 2 runs. Full Worker suite
  (`--all-targets --all-features`): 4,316 passed, 0 failed, 105 ignored. The agent-runtime crate tests pass. Clippy
  `-D warnings` is clean on both workspaces.
- **Two-process recovery** (`postgres_child_direct_effect_process` `:3770`, driver `child_direct_scenario` `:3874`, cases
  at `:3969` to `:3989`):
  - a crash process dies inside the child's call, once its Started record is durable;
  - a reclaim of the same execution then runs in a second process;
  - the database states are set as follows:
    - `started` is left as the crash wrote it;
    - `committed`: the crash process dies at the next node's model call, and the child's newest row (pending that
      node) is deleted, as if the process had died between the journal commit and the checkpoint save;
    - `undispatched`: the journal rows are deleted, as if it had died before the Started append.
  - Assertions: the tool-call count in every case (committed 0, started 0, undispatched 1); the outcome (replay, the
    `agent.recovery_refused` terminal, a single run); and that the reclaim used, or recreated, exactly the journal thread
    the lost call opened.
  - Cases: depth 1 for all three; depth 2 for committed and started.
- **Visit identity.**
  - `a_root_node_journal_keeps_its_identity_and_a_child_visit_changes_it` (unit): pins main's root journal id
    `0e7d4375…917d`.
  - `node_journals_are_visit_scoped_in_a_child_and_unscoped_on_the_root`: a child thread without a visit and the root
    with one are both refused `unscoped_child`.
  - `postgres_two_visits_of_one_child_in_one_execution_each_run_their_call` (`:4002`): two calls, two journals. It does
    not by itself prove the scope (see Durability).
- **Fan-out.**
  - `postgres_a_member_whose_child_calls_an_effectful_direct_tool_runs_it_once` (`:1710`): one call under `continue` and
    `stop`; all three branches succeed.
  - `postgres_a_member_child_sensitive_direct_call_pauses_then_runs_once_or_never` (`:4052`): a `tool_guard` card for
    the member and a park; approve runs once, reject runs 0, with a journal only for the approved call.

## Performance

- One extra primary-key load is gone: the visit comes from state, not from a row.
- A child's effectful call opens the same journal the root's does: one writer activation plus the journal rows.
- Recovery measured per case, process start included: depth 1 started 186 ms, committed 240 ms, undispatched 225 ms;
  depth 2 started 198 ms, committed 358 ms. All are well inside the 20 s per-node budget.

## Durability

- **Journal identity.**
  - A child's journal id hashes the run, the thread, node, step, node digest, input digest, policy, and the visit.
  - The visit is the Agent node's open activation: parent thread, node, parent step, definition digest and input
    digest.
  - The root's id is unchanged (pinned), so journals already written stay addressable.
- **Visits within an execution.** They are also told apart by the child's node event scope. It sits in the child's state,
  and its `call_id` is `scoped_graph_call_id(thread, node, parent step, definition digest)`. That id is stable across a
  reclaim, which the recovery tests prove by reopening the same journal thread.
  - The visit scope is kept as defense in depth: it does not depend on node events being present.
  - The scope itself is proven by the two identity tests above.
- **Re-entry.** A reclaim re-enters the child with its own restored state, with no re-appended task, so the journaled
  input matches.

## Resilience

- A child thread without a visit, or the root with one, is refused (`pipeline.node_recovery.unscoped_child`). There is no
  unscoped fallback.
- Every walk refusal stays typed: anything other than the accepted leaves becomes `RECOVERY_FRONTIER_UNSUPPORTED`.
- A started call in a child ends with the registered `agent.recovery_refused` (`RECOVERY_REFUSED`). In a fan-out member
  it is the typed branch failure `graph.fanout.may_have_started`.

## Security

- **Leaf validation trust argument (option B).** The walk accepts a child frontier at a direct node only when, at
  assembly and against the Main-frozen definition of that level, all of the following hold:
  - the single pending node is a `DirectTool` node of that definition;
  - no Parallel or Map owns it;
  - every deeper row's activation stamp names its own thread with the frozen parent Agent node's digest;
  - a missing row is accepted only for a child whose frozen entry node is such a node.

  The effect class is not known at walk time (tools materialize after the walk). Read-only tools rerun (I), and
  effectful tools are decided by their journal (R / C → F / I).
- **Journals.** They open only under the run root's writer fence and lease (`ensure_current`).
  - The lent factory routes only admitted threads: the root family's application paths, or the branch family's own
    threads. Any other thread is refused.
  - The visit is derived from the Worker's own activation, never from user input. A malformed value is refused.
- **Unchanged.** No new route, RPC, SQL, egress or credential path. Ungoverned credentials in children are still refused
  (`refuse_ungoverned_credentials`).
- **Scanners:** no dependency manifest changed, so `cargo deny`, `govulncheck` and `npm audit` report main's set.

## Recovery rows (gate §2)

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Worker × effectful direct call in a saved child, lost after its journal committed | R (replay, 0 calls) | `node_recovery_runtime.rs` Completed branch; `session.rs:1710` | depth 1 and 2 `…_after_its_commit_…` tests |
| Worker × the same, lost after Started | C → F (registered may-have-started stop, never repeated) | `node_recovery_runtime.rs:967`, `direct_tool.rs:837` | depth 1 and 2 `…_after_it_started_…` tests |
| Worker × the same, lost before Started (including a child that saved no row) | I (runs once) | `session.rs:1660`, journal Ready | `…_before_it_started_runs_once` |
| Worker × read-only direct call in a saved child, lost mid-call | I (reruns) | `session.rs:1710` | walk acceptance; no journal |
| Worker × a child re-entered at its own unfinished frontier (reclaim, wake) | R (input preserved, task not appended twice) | `application.rs:784` | all recovery tests reopen the same journal thread |
| Worker × sensitive child direct call in a fan-out member | R (card, decision, one call) | `parallel_compiler.rs:1025`, branch family journals | `…_pauses_then_runs_once_or_never` |

## Browser evidence (fanpar)

To be completed after the images are deployed.

## Follow-ups

- **FU-264:** an effectful direct node in a saved child two levels below a fan-out member. The inner child's threads
  belong to the member branch family, and the lent root journals refuse them typed, so the call never runs twice but
  does not run yet.
- Code nodes in saved children keep their pre-existing refusal ("Scoped Code requires its current fenced node writer").
  That is outside this change.
