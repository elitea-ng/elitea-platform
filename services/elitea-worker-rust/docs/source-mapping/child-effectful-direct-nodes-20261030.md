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
- **Read-only calls.** A read-only direct node in a child is accepted by the walk and is expected to rerun on recovery
  (class I). This is not yet proven by a test (FU-266).
- **Not ported from the current platform:** its "lost, run again from scratch" behaviour for a crashed child.

## What was found on the way

- **Task message appended twice.** A child re-entered at its own unfinished frontier got its task message appended twice.
  The Agent node re-merged the task into the child's restored state through the `messages` append reducer. FU-249 had
  fixed this only for a child holding a Parallel.
  - The doubled message changes a journaled call's input digest, so a reclaim looked for a different journal. For a call
    that had started, that could have dispatched it a second time.
  - The rule now applies to every child that has an activation overlay, and to a Parallel's wake as before
    (`agents/graph/application.rs:797`). The overlay shows only this visit's frontier, so a completed earlier visit
    still starts fresh. Without an overlay (pipeline-as-agent-tool, the legacy unscoped path) the task is re-sent as
    before.
  - On re-entry nothing the child already holds is re-sent: the task message, its mapped variables and the appending
    `hitl_decisions` channel stay as restored (`:800-807`, REVIEW-1541 R1). A nested pipeline cannot declare typed
    reducers at all (`compiler.rs` `compile_subgraph_with_runtime`, pinned by
    `nested_child_pipelines_refuse_typed_reducers_in_v1`), so a mapped variable cannot be reduced twice today; dropping
    it keeps the input exactly the journaled one regardless.
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
  - `libs/rust/agent-runtime/src/graph/application_activation.rs`: `visit_digest` `:36`,
    `ApplicationActivationGuard::visit_digest`.
  - `src/agents/graph/application.rs`: `APPLICATION_VISIT_STATE_KEY` `:53`, injected at `:741`; an Agent node without an activation passes
    `null`, never its parent's visit, so an effectful journal below it is refused (REVIEW-1541 R4). It is a runtime channel in
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
- **Double append:** `src/agents/graph/application.rs:797-807`.
- **Other:** `src/sandbox/service.rs` boxes one future (`clippy::large_futures`, grown by the new fields).

## Tests

All against real PostgreSQL, except the pinned-identity test, which is a unit test. Runs were at 8 to 16 test threads.

- **`fanout_effect_pg_tests`:** 50 passed, 0 failed, 0 skipped (after REVIEW-1541). Full Worker suite
  (`--all-targets --all-features`, 13 test binaries): 2,888 passed, 0 failed, 105 ignored. agent-runtime crate: 1,627
  passed, 0 failed, 1 ignored. (An earlier "4,316" in this line was a summing error.) Clippy
  `-D warnings` is clean on both workspaces.
- **Two-process recovery** (`postgres_child_direct_effect_process` `:3809`, driver `child_direct_scenario` `:3919`, cases
  at `:4019` to `:4062`):
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
  - Mapped variables (REVIEW-1541 R1): the child also declares `items` and `count`, fed by the Agent node's mapped
    variables; depth 1 committed and started, depth 2 committed. Same assertions: 0 calls, the same journal thread.
- **Visit identity.**
  - `a_root_node_journal_keeps_its_identity_and_a_child_visit_changes_it` (unit): pins main's root journal id
    `0e7d4375…917d`.
  - `node_journals_are_visit_scoped_in_a_child_and_unscoped_on_the_root`: a child thread without a visit and the root
    with one are both refused `unscoped_child`.
  - `postgres_two_visits_of_one_child_in_one_execution_each_run_their_call` (`:4071`): two calls, two journals. It does
    not by itself prove the scope (see Durability).
- **Lent factory scope (REVIEW-1541 R2):** `lent_journal_factories_open_only_their_own_admitted_threads_under_the_claim`
  (`state/postgres_checkpointer_tests/run_root_fencing.rs:205`), through `Arc<dyn NodeRecoveryFactory>` as lent:
  - the root family refuses an unadmitted thread and a branch's thread;
  - a Parallel branch family refuses a root-family thread and an unadmitted branch thread;
  - no refused open writes a journal row;
  - each opens its own admitted thread;
  - after a newer claim takes the root, both refuse with `writer_not_current`.
- **Visit uniqueness (REVIEW-1541 R7):** `a_visit_digest_is_stable_for_its_stamp_and_unique_per_visit`
  (`libs/rust/agent-runtime/src/graph/application_activation.rs:347`): equal for an equal stamp; different for the
  parent step, parent thread, node, definition or inputs.
- **Child resume keeps one task message (REVIEW-1541 R5):** `pipeline_scoped_tests.rs:364`, in
  `an_ordinary_agent_in_a_saved_child_pipeline_asks_and_resumes_once_after_process_replacement`. It fails with the
  pre-FU-248 rule (the task appears twice).
- **Fan-out.**
  - `postgres_a_member_whose_child_calls_an_effectful_direct_tool_runs_it_once` (`:1710`): one call under `continue` and
    `stop`; all three branches succeed.
  - `postgres_a_member_child_sensitive_direct_call_pauses_then_runs_once_or_never` (`:4121`): a `tool_guard` card for
    the member and a park; approve runs once, reject runs 0, with a journal only for the approved call.

## Performance

- The visit adds no read: the Agent node derives it from its own activation and passes it in the child's state.
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
- A started call in a child ends with the registered `agent.recovery_refused` (`RECOVERY_REFUSED`), proven at depths 1
  and 2. In a fan-out member's child the path leads to the typed branch failure `graph.fanout.may_have_started`; no
  test loses a Worker there yet (FU-267).

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
    threads. Any other thread is refused (`lent_journal_factories_open_only_their_own_admitted_threads_under_the_claim`).
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
| Worker × read-only direct call in a saved child, lost mid-call | I (reruns), unproven | `session.rs:1710` | none yet: FU-266 |
| Worker × started call in a fan-out member's child, Worker lost | C → F (`graph.fanout.may_have_started`), unproven | `direct_tool.rs:837`, `node_events.rs:247-261`, `scoped_runtime.rs:398-405` | none yet: FU-267 |
| Worker × a child re-entered at its own unfinished frontier (reclaim, wake) | R (input preserved, task not appended twice) | `application.rs:797-807` | all recovery tests reopen the same journal thread, including the mapped-variable cases; the child resume pin |
| Worker × an approved child direct call, lost before the continuation's first root save | F− at FU-248; now F typed (`RECOVERY_FRONTIER_UNSUPPORTED`) after FU-265 (`continuation-lost-before-root-save-20261030.md`); R/C/I is FU-269 | `session.rs:1747-1758`, `checkpoint_recovery.rs:146-160` | browser: fanpar chat 44 kill; FU-265 |
| Worker × sensitive child direct call in a fan-out member | R (card, decision, one call) | `parallel_compiler.rs:1025`, branch family journals | `…_pauses_then_runs_once_or_never` |

## Browser evidence (fanpar)

- **Stack.** fanpar, project 118, Claude in Chrome, local mock-OIDC test identity.
  - Main and Worker images are `pm-68fb160ef`.
  - Binary check: the Worker binary contains `__elitea_application_visit_v1` and `child_effect_unknown`.
  - verify-profile: 49 passed, 1 failed. The failure is "shared migrations at the worktree head (ledger=210
    worktree=204)": the ledger is at the deployed image's head, and the profile compares it with an older worktree.
- **Call counting.** Calls are counted with `jsonrpc tools/call` lines in the mock MCP server's log; `echo` and
  `reverse` both count.
- **Stack preparation, stated honestly.**
  - The merged shared migration 0206 (#1536) fails on this database, which was restored from a current-platform dump:
    its `centry.audit_events` lacks the token columns. On this stack only, I added the five columns with Main's
    `001_initial` types, then migrated. This is not part of this change; the orchestrator raises it.
  - **Workaround for #1501 delegation narrowing.** A pipeline root's grants are only the toolkits its own nodes use, so
    a root that only delegates to "FP direct reverse" is refused `DELEGATION_GRANT_MISSING`. Each fixture root
    therefore ends with an `echo` node on the same toolkit, so it holds the grant legitimately. Delegation mode stays
    `enforce`.
- **Fixtures, created in the UI.**
  - App 16 "FP child direct": an Agent node runs "FP direct reverse" (a sensitive MCP `reverse` node), then `echo`.
  - App 17 "FP parallel child direct": a fixed Parallel with two members running "FP direct reverse", `on_error:
    continue`, then `echo`.
- **Agent node (chat 44).**
  - The card "Sensitive Action Authorization Required — fpmockmcp.reverse" shows under "FP direct reverse". Before
    this change, binding refused the node.
  - The card survives a reload, and no call is made while paused.
  - Authorize: 3 → 5 calls (`reverse` once, `echo` once). The run completes, and the result survives a reload. Each
    direct call has its own `n1:` journal, and the child rows carry the visit stamp.
  - Block: 5 → 6 calls (`echo` only); `reverse` never ran.
- **Parallel members (chat 45).**
  - Two member cards, "FP direct reverse (1)" and "(2)", replace the former failed join entry. Both survive a reload,
    and no call is made while parked.
  - Authorize (1): 6 → 7 calls. Block (2): the run completes, 7 → 8 calls (`echo`).
  - Ledger: two `tool_guard` cards, both CONSUMED (approve, reject).
- **Worker kill inside an approved child call (chat 44, execution `d44a3a3a8a5ec5ba70391556760c1412`).**
  - The Worker was killed at the approved call's tool start and restarted. Claim 1 expired (LEASE_EXPIRED), and claim 2
    settled 40 ms later.
  - Calls: 8 → 9. The call ran once and was never repeated.
  - The terminal is the generic Internal failure, "The runtime operation failed.", not the registered
    `RECOVERY_REFUSED`. Cause: the approval's continuation re-enters the paused execution's root row. Lost before its
    own first root save, recovery refuses that writer (`src/agents/session.rs:1747-1758`) before assembly
    (`src/execution/checkpoint_recovery.rs:146-160`).
  - This falls short of the F readability bar and is registered as FU-265 (see the recovery rows and follow-ups).
  - The two-process tests above crash in the first execution, where the writer is the execution itself, so they do
    not cover a decided continuation.
- **Root control.** Running "FP direct reverse" directly as a root pipeline (chat 46) did not exercise the kill: a root
  direct node emits no `agent_tool_start` progress, so the kill trigger never fired, and the run completed with one call.

## Follow-ups

- **FU-265 (next item).** A decided continuation lost before its first root save, for a root pipeline direct node
  and a saved child.
  - Main sends the paused execution and generation in the continuation, bound to its authorization.
  - The Worker accepts that writer for that continuation only, under the fence.
  - Real-PostgreSQL two-process tests crash inside the continuation.
  - The outcome is R or C, or at minimum the registered terminal.
- **Raised by the orchestrator, not this change:** migration 0206's upgrade from a current-platform database, and #1501
  counting only node-used toolkits as a pipeline root's grants.
- **FU-264:** an effectful direct node in a saved child two levels below a fan-out member. The inner child's threads
  belong to the member branch family, and the lent root journals refuse them typed, so the call never runs twice but
  does not run yet.
- Code nodes in saved children keep their pre-existing refusal ("Scoped Code requires its current fenced node writer").
  That is outside this change.
