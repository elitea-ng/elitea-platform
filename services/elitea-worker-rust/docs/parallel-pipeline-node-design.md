# Explicit pipeline parallel node design

Status: proposed and capability-disabled.

This document defines the first production-shaped pipeline `parallel` node.
It replaces the disconnected prototype contract before compiler activation.

## Decision summary

The node is an explicit graph construct. It is not model-selected parallel tool
execution and it is not ADK `ParallelAgent`.

The first enabled form runs owned Agent nodes. An Agent node can select a saved
direct agent or a saved pipeline.

Each owned Agent node keeps its existing input mapping and declared outputs.
The parallel node evaluates all mappings from one parent-state snapshot.

Each branch runs as a small ADK `CompiledGraph`. Each child graph uses one
claim-fenced descendant checkpoint thread.

The node supports only `wait: all`. It returns branch results in declared YAML
order.

The node supports nested HITL and delegated authorization. It uses standard ADK
checkpoints and the existing session event lineage.

Do not add an interrupt table. Do not use `PARKED_CHILDREN` or the legacy
`parallel_reconcile` fields for this node.

## Product boundary

Current ad-hoc parallel execution is model-selected Application tool dispatch.
The provider selects the calls and their order.

The pipeline node is author-selected graph topology. Its branch set is frozen
before execution.

A raw chat prompt does not prepare branch tasks. The static node does not call
a model to infer those tasks.

The two capabilities share these runtime owners:

- saved Application resolution;
- nested agent and pipeline execution;
- call-bound hierarchy metadata;
- sensitive-tool confirmation;
- delegated authorization;
- `ask_user` clarification;
- session and checkpoint persistence;
- output delivery and settlement.

They do not share a scheduling contract. Keep their admission and replay logic
separate.

## Three parallel processing modes

Keep model planning, fixed topology, and data-driven fan-out as separate
runtime contracts.

| Mode | Work source | Input owner | Output owner |
| --- | --- | --- | --- |
| Model-selected calls | One `LlmAgent` turn | Each provider tool call | The parent model loop |
| `parallel` | A fixed YAML branch list | Each owned node mapping | One declared-order collector |
| Future `map` | Items from one state list | One item dispatcher | One reducer-backed collector |

The current ad-hoc runtime supports model-selected calls. The root model can
call one Application participant more than once in the same turn.

For example, the model can send separate Olivia and Sasha tasks to one saved
`full_name_resolver`. The runtime executes that pure Application batch with a
bounded parallel dispatcher.

The `parallel` node does not copy one input across every branch. Each owned node
maps its own task from the same parent-state snapshot.

The future `map` node repeats one owned worker definition for each list item.
It does not require one stored agent or pipeline for each item.

The future map contract is in `map-reduce-pipeline-node-design.md`.

## Chat-derived branch tasks

Use an explicit planner when a stored pipeline receives one unstructured chat
request.

For a fixed branch count, let an upstream LLM node produce named task fields.
Map each fixed branch from its named field.

For a dynamic branch count, let the planner produce one structured task list.
Pass that list to the future `map` node.

Do not hide an LLM inside the `parallel` scheduler. Keep model selection, cost,
retry, schema, checkpoint, and observability explicit.

The UI can later show a composite planned-parallel control. The compiler must
still expand it into an LLM planner and a map node.

Pipeline LLM nodes currently reject Application participants. Therefore, direct
LLM-to-Application fan-out inside a stored pipeline remains an activation gate.

## V1 YAML contract

The parallel node references Agent nodes from the same `nodes` list.

```yaml
state:
  parallel_results: list
entry_point: resolve_names
nodes:
  - id: resolve_names
    type: parallel
    branches:
      - id: olivia
        node: resolve_olivia
      - id: sasha
        node: resolve_sasha
    max_concurrency: 2
    wait: all
    error_policy: fail_after_drain
    output: [parallel_results]
    transition: END

  - id: resolve_olivia
    type: agent
    tool: full_name_resolver
    input_mapping:
      task:
        type: fixed
        value: Resolve Olivia Lovelace.
    output: [output]

  - id: resolve_sasha
    type: agent
    tool: full_name_resolver
    input_mapping:
      task:
        type: fixed
        value: Resolve Sasha Grey.
    output: [output]
```

Apply these V1 rules:

- Declare between two and sixteen branches.
- Set `max_concurrency` between one and eight.
- Limit all pending public pause cards to sixteen.
- Use unique branch identifiers.
- Reference each Agent node from one parallel node only.
- Do not use a branch Agent node as the pipeline entry point.
- Do not target a branch Agent node from another route.
- Do not set a transition on a branch Agent node.
- Declare exactly one aggregate output channel.
- Use only `wait: all` and `fail_after_drain`.
- Reject an explicit parallel node inside another explicit parallel branch.

An Agent branch can still select a saved direct agent. That agent can use the
existing bounded model-selected Application fan-out.

An Agent branch can also select one saved pipeline. The existing three-agent
tier and raw-hop limits remain authoritative.

## Compiler ownership

Treat each referenced Agent node as an owned child definition. Do not register
that node in the parent graph frontier.

Compile each owned definition as one child graph. Reuse the existing
`ApplicationNode` runtime and result projection.

Register one `DurableParallelNode` in the parent graph. Add only its declared
transition to the parent topology.

Include these values in the pipeline definition digest:

- the parallel node identifier;
- the output and transition;
- the wait and error policies;
- the concurrency bound;
- every branch identifier and ordinal;
- every owned Agent node definition digest.

Reject ownership conflicts before model, credential, MCP, or state authority is
created.

## Fixed branch input dispatch and output collection

Freeze one parent-state snapshot when the parallel node starts.

Evaluate every owned Agent node mapping against that snapshot. Preserve the
current `fixed`, `variable`, and `fstring` task mapping behavior.

Project only the mapping dependencies into each child graph. Add the required
session, resume, event-scope, and checkpoint control channels.

Do not copy the complete parent state by default. Do not expose one branch
result to another branch.

Keep resume controls separate from business input. A HITL decision must not
change the child checkpoint lineage.

Derive the child thread from these immutable values:

- claim and generation authority;
- pipeline definition digest;
- parent checkpoint thread;
- parent graph step;
- parallel node digest;
- branch identifier and ordinal;
- owned Agent node digest;
- canonical business-input digest.

Exclude these transient controls from the business-input digest:

- HITL decisions;
- sensitive-tool replay data;
- delegated-authorization decisions;
- `ask_user` answers;
- browser progress metadata.

Capture each branch node's declared data outputs after type validation. Keep
structured output fields as a keyed object.

V1 Agent branches copy their final response into their declared data outputs.
Direct LLM branches remain closed in V1.

The keyed collector shape permits later LLM structured outputs without changing
the parallel node output contract.

Return one array in declared branch order. Each entry has `branch_id`, `node`,
and `outputs`.

```json
[
  {
    "branch_id": "olivia",
    "node": "resolve_olivia",
    "outputs": {"output": "Olivia result"}
  },
  {
    "branch_id": "sasha",
    "node": "resolve_sasha",
    "outputs": {"output": "Sasha result"}
  }
]
```

The current disconnected prototype uses one untyped `result` value. Replace
that shape before compiler activation.

Limit one branch result to 512 KiB. Limit the joined result to 8 MiB.

Do not merge arbitrary branch state into the parent. A later state-modifier node
can select or transform collected outputs.

This node performs a wait-all collection. It does not apply a reducer across
different branch output keys.

## Durable branch outcomes

Each Agent branch can execute model calls directly or through its selected saved pipeline.
Inherit the parent context policy and keep each model conversation's occupancy and summaries inside its durable child lineage.
Apply the same boundary to future direct LLM branches when their compiler contract becomes available.
Keep branch result collection exact; a later LLM join has a separate request budget.
Do not add branch context capacities together or compact one branch using another branch's history.
See [pipeline context ownership](context-continuation-design.md#fan-out-and-model-backed-reduction).

Represent one branch with a typed internal outcome.

| Outcome | Parent behavior |
| --- | --- |
| Completed | Retain the terminal child checkpoint and joined result. |
| Paused | Retain the child interrupt and continue other admitted branches. |
| Blocked | Stop new admission, drain admitted siblings, and end the pipeline. |
| Failed | Stop new admission, drain admitted siblings, and return one stable failure. |
| Cancelled | Stop admission and cancel all owned branch futures. |

Select the first declared failed branch after the drain. Do not select the
branch which happens to fail first.

Do not cancel an admitted sibling after another branch fails. Its external
effect can already be in progress.

Approved effectful tools remain closed until durable effect receipts exist.
This rule also applies inside Agent branches.

## Nested HITL and authorization

Treat an inner `GraphError::Interrupted` as a paused branch. Do not convert it
to `graph.parallel.branch_failed`.

Let all admitted branches reach Completed, Paused, Blocked, or Failed. Aggregate
paused branches in declared order.

Return one outer `interrupt_with_data` value. Use the schema
`elitea.graph.parallel-interrupt.v1`.

The outer interrupt stores safe branch identities. Private child thread and
checkpoint identifiers stay inside the checkpoint and session binding.

Keep descendant HITL, authorization, and clarification cards as the public
events. Do not publish a duplicate generic parallel-node card.

The parent graph saves its normal checkpoint before output delivery publishes
the pause. The parent frontier remains on the parallel node.

Require one exact complete decision set for all published branch interrupts.
Reject partial, stale, duplicate, or foreign decisions before dispatch.

On resume, project each decision only into its matching child. Reopen the same
child checkpoint thread and run only unfinished work.

These crash cases need no new interrupt ledger:

1. A child pauses before the parent checkpoint is saved.
2. A child completes before the parent checkpoint is saved.
3. The parent checkpoint is saved before the browser event is acknowledged.
4. A new worker reclaims the same command after process loss.

In cases one and two, the new parent attempt reloads the child checkpoint. It
then recreates the same aggregate result or interrupt.

In case three, the existing output-delivery idempotency owns replay.

The effect-to-child-checkpoint crash window remains separate. Durable effect
intent and outcome receipts must close it.

## Scheduling and cancellation

Use one `FuturesUnordered` owner inside the parallel node. Do not detach branch
tasks.

Admit no more than `max_concurrency` branches. Stop new admission after a Blocked
or Failed outcome.

Propagate the root execution deadline and lease cancellation. Stop admission
before cancellation reaches child futures.

Await bounded cleanup for all owned futures. Treat cleanup timeout as an
internal cancellation failure.

Dropping an HTTP future does not prove that a remote effect did not occur.
Keep effectful branches disabled until receipts exist.

Do not use native deferred fan-in. ADK 2.0.0 does not persist its deferred
arrival tracker.

Do not use `NodeContext::run_node_with`. Its successful child ledger reaches the
checkpoint only after the parent node returns.

## Browser hierarchy and progress

Do not add the parallel node to `parent_agent_path`. That path remains the
three-tier saved-Application hierarchy.

Add these safe fields to branch progress metadata:

- `parallel_node`;
- `parallel_activation`;
- `parallel_branch_id`;
- `parallel_branch_ordinal`.

Derive `parallel_activation` from the private activation identity. Publish only
its bounded digest label.

Give each branch Agent call one stable synthetic call identifier. Extend the
existing Application hierarchy below that call.

Allow branch events to interleave. Preserve declared ordinal for stable UI
grouping and final result ordering.

Emit one container start event before branch admission. Emit one container end,
pause, or failure event after all admitted branches settle.

The UI can group by `parallel_activation` and branch identifier. It can shimmer
only the active branch accordion.

Extend `PipelineNodeEventScope` with the internal branch authority. Do not apply
the current `{parent}/{node}` thread assertion to hashed branch threads.

Validate the hashed thread against the compiler-supplied child checkpointer.
Never publish that thread value.

## Logs and traces

Create one `pipeline.parallel` span for each activation. Include safe execution,
node, step, branch-count, concurrency, and outcome fields.

Create one `pipeline.parallel.branch` span for each admitted branch. Include
branch identifier, ordinal, Agent node identifier, resume state, and outcome.

Emit short human-readable messages for these events:

- parallel node started;
- branch admitted;
- branch resumed from checkpoint;
- branch completed;
- branch paused;
- branch failed;
- parallel node joined;
- parallel node cancelled.

Record one safe error at the owner boundary. Include the stable error code and
the failed operation.

Do not log prompts, mapped tasks, tool arguments, results, URLs, credentials,
tokens, checkpoint state, or provider bodies.

Do not use `unwrap`, `expect`, or `panic` on the request path. The release
profile aborts on panic, so process supervision owns unexpected panic recovery.

## Verification gates

Add focused unit and component tests for these behaviors:

- strict YAML bounds and owned-node validation;
- one immutable parent-state snapshot for every branch mapping;
- different fixed, variable, and template mappings across sibling branches;
- structured branch outputs retained under their declared keys;
- declared result order under reverse completion;
- the eight-branch concurrency ceiling;
- deterministic failure selection after drain;
- no pending admission after Blocked or Failed;
- two simultaneous HITL branches;
- mixed sensitive, authorization, and clarification branches;
- complete-set resume without replanning;
- partial-set rejection before dispatch;
- changed resume controls reuse the original child lineage;
- changed business input creates a new lineage;
- process loss after child completion;
- process loss after child pause;
- process loss before output acknowledgement;
- lease loss and user cancellation;
- human-readable safe logs for success and failure.

Run the scheduler tests on Tokio's multi-thread runtime with four worker threads.
Use barriers and channels instead of sleeps.

Use the real PostgreSQL checkpointer for restart tests. Use a second worker
process for reclaim tests.

Add one UI chat proof with two Agent branches. Verify concurrent progress,
stable accordions, nested cards, final order, and persisted replay.

## Pull-request and CI plan

Open the current agent-runtime branch as a draft pull request. Do not mark it
ready for production.

Keep artifacts and indexing out of that pull request. They need separate
cross-language storage designs.

Add a required Rust CI workflow before review. Run these jobs:

1. `cargo fmt --all -- --check`;
2. `cargo clippy --locked --all-targets --all-features -- -D warnings`;
3. `cargo test --locked --all-targets --all-features`;
4. `cargo test --locked --doc`;
5. a release build and worker container build;
6. PostgreSQL session and checkpointer component tests;
7. a supervised process-loss and reclaim smoke test.

Rust has no direct stable equivalent to Go's race detector. Add targeted
multi-thread tests first.

Use Loom only if the scheduler adds shared synchronization state. Keep the
scheduler single-owned so Loom is probably unnecessary.

Treat ThreadSanitizer as an optional nightly signal. Do not make it a required
gate until it is stable for this dependency graph.

Keep the pull request in draft until these live gates pass:

- UI chat execution with an OpenAI-compatible model;
- ordinary model tool calling;
- sensitive-tool approve and block;
- delegated authorization;
- nested parallel HITL;
- worker termination and reclaim;
- human-readable success and failure logs.

## Implementation order

1. Replace the standalone YAML parser with compiler-owned node ownership.
2. Compile Agent-only child graphs and activate deterministic completion.
3. Add branch progress metadata and tracing.
4. Add aggregate child interrupts and exact complete-set resume.
5. Add PostgreSQL crash and reclaim proofs.
6. Add the UI node and live chat proof.
7. Consider other branch node families after V1 passes.

Do not activate `wait: one`, `wait: many`, distributed branch commands, or
effectful branches in V1.
