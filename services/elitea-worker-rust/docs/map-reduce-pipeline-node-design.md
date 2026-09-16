# Data-driven map and reduce pipeline design

Status: proposed and capability-disabled.

This document defines data-driven parallel processing for one pipeline state
list. It is separate from the fixed `parallel` node.

## Decision summary

Add a future `map` node for dynamic per-item fan-out.

The contract mirrors LangGraph `Send`. One activation creates item-local node
invocations, waits for them, and reduces their updates.

The node reads one bounded list from parent state. It repeats one owned worker
definition for every item.

The node limits active workers with `max_concurrency`. It admits another item
when one active worker settles.

Eight items with concurrency four use at most four workers. The scheduler does
not add a barrier between two rigid batches.

Each item uses one claim-fenced child checkpoint. A completed item is not run
again after worker loss.

Collect item outputs in input order. Apply one declared state reducer after all
required items settle.

## ADK-Rust boundary

ADK-Rust 2.0.0 `LoopAgent` runs a fixed subagent list sequentially. It repeats
that list until escalation or its iteration limit.

`LoopAgent` does not dispatch list items concurrently.

ADK-Rust 2.0.0 `ParallelAgent` runs a fixed subagent list concurrently. Every
subagent starts from the same invocation context.

`ParallelAgent` does not provide per-item input mapping or bounded dynamic
admission. Its optional `SharedState` is fresh process memory for each run.

ADK graph reducers can combine state updates. Static graph fan-out can run a
fixed frontier concurrently.

The inspected ADK version has no LangGraph `Send` equivalent. A dynamic item
cannot enqueue another invocation of one node with item-local state.

Implement the map scheduler as one custom ADK node. Compile each item worker as
one small child graph.

## Proposed YAML contract

This example converts one chat request into repeated calls to one saved
participant. The planner output becomes durable before the map activation.

```yaml
state:
  input: str
  work_items: list
  item_result: str
  processed_items:
    type: list
    value: []
    reducer: append
  final_answer: str

entry_point: plan_work

nodes:
  - id: plan_work
    type: llm
    input: [input]
    input_mapping:
      system:
        type: fixed
        value: Split the request into independent full-name tasks.
      task:
        type: variable
        value: input
    structured_output: true
    output: [work_items]
    transition: process_items

  - id: process_items
    type: map
    source:
      type: variable
      value: work_items
    item: work_item
    index: work_index
    worker: process_item
    collect:
      from: [item_result]
      into: processed_items
    max_concurrency: 4
    transition: summarize_results

  - id: process_item
    type: agent
    tool: full_name_resolver
    input: [work_item, work_index]
    input_mapping:
      task:
        type: fstring
        value: "Resolve name {work_index}: {work_item}"
    output: [item_result]

  - id: summarize_results
    type: llm
    input: [processed_items]
    input_mapping:
      task:
        type: fstring
        value: "Combine these ordered results: {processed_items}"
    output: [final_answer]
    transition: END
```

For the sample chat request, the planner returns two `work_items`. The map node
creates two child invocations of `full_name_resolver`.

The model does not run inside the map scheduler. A retry reuses the checkpointed
task list and does not silently replan admitted items.

The map node owns the worker node. Do not register that worker in the parent
graph frontier.

Do not use the worker as an entry point or route target. Do not set a worker
transition.

The compiler treats `item` and `index` as child-local state channels. They do
not become parent state channels.

Declare worker output keys in pipeline state for type validation. The map
worker keeps those values child-local until collection.

## Item input dispatch

Freeze the parent state when the map node starts. Resolve `source` from that
snapshot.

Reject a missing source, a non-list source, or a list above its configured
item bound.

Assign each item its zero-based input ordinal. Keep duplicate items as separate
work through that ordinal.

Create child-local state with these values:

- the current item under the configured `item` key;
- the current ordinal under the configured `index` key;
- approved broadcast inputs declared by the owned worker;
- required session, resume, event-scope, and checkpoint controls.

Evaluate the worker's existing input mapping against this child-local state.
Do not invent a second prompt-mapping implementation.

Use the normal worker behavior after mapping. An LLM worker keeps structured
output validation and its bounded tool loop.

An Agent worker keeps its saved participant, HITL, hierarchy, and checkpoint
behavior. A direct node keeps its exact tool contract.

## Item output collection

Capture only the worker's declared data outputs. Exclude messages and internal
runtime channels unless the contract selects them explicitly.

Validate every captured value against the declared pipeline state type. Retain
structured LLM output as an object with its declared keys.

Sort settled item outputs by input ordinal before collection. Never use
completion order.

For `append`, add one item result object for each input item. Include its ordinal
and selected worker outputs.

```json
[
  {"index": 0, "outputs": {"item_result": "Olivia result"}},
  {"index": 1, "outputs": {"item_result": "Sasha result"}}
]
```

For `sum`, require one numeric `from` key. Fold values in input order.

For `merge`, require one dictionary `from` key. Apply conflicts in input order,
with the later ordinal taking precedence.

Do not accept an executable custom reducer from stored YAML.

## State reducer ownership

Declare the reducer on the destination state channel. Do not hide reducer
semantics inside the worker node.

The current Rust compiler gives user state channels overwrite behavior. It
specializes reducers only for runtime-owned channels.

Extend the state descriptor with a validated optional `reducer` field before
map activation. Keep existing state declarations as overwrite by default.

Compile only approved reducer names. Match reducer input and output types before
runtime authority is created.

Do not let item workers update parent state concurrently. The map node first
collects deterministic item outputs.

Fold outputs by input ordinal. Emit one final `NodeOutput` update for the
destination channel.

The parent `StateSchema` then applies its declared reducer once. This keeps
checkpoint state independent from item completion timing.

A downstream LLM or data node can perform semantic reduction. Keep semantic
summarization outside the mechanical state reducer.

## Durability and HITL

### Model context ownership

An item worker can invoke an LLM, saved agent, or saved pipeline containing model nodes.
Each model conversation inherits the parent context policy and owns independent occupancy, summary coverage, and recovery state.
Use the item lineage below; do not key compaction by the reusable worker node name alone.
Retain exact declared item outputs during collection, regardless of whether the worker compacts its private history.

A downstream LLM reducer has its own model context and input-capacity check.
The combined item output can exceed that capacity even when every item request fits individually.
Use an explicit chunk/reduce graph or artifact references for that case.
Do not summarize, omit, or truncate item outputs inside the mechanical collector.
See [pipeline context ownership](context-continuation-design.md#fan-out-and-model-backed-reduction).

### Child lineage and decisions

Derive one child lineage from the map activation, source digest, item ordinal,
item digest, and owned worker digest.

Keep resume decisions outside the business-input digest. Reopen the same child
checkpoint after HITL or delegated authorization.

Treat each interrupted item as a paused child. Continue bounded admission while
the public pause-card limit permits it.

Stop new admission when sixteen public pause cards are pending. Publish one
aggregate standard graph interrupt for those cards.

Require one exact complete decision set for all published cards. Resume those
items before new admission continues.

Use the fixed parallel-node hierarchy metadata with these replacements:

- `map_node`;
- `map_activation`;
- `map_item_id`;
- `map_item_ordinal`.

Publish only a bounded digest label for `map_item_id`. Do not publish item data
or private checkpoint identity.

## Bounds and failure policy

Start with at most 64 source items and at most eight active workers. Keep both
limits configurable within deployment policy.

Limit one mapped item input to 512 KiB. Limit one selected item output to
512 KiB.

Limit the complete collected value to 8 MiB. Require artifact-backed results
for larger output.

Use fail-after-drain for the first version. Stop new admission after a blocked
or failed item.

Drain admitted workers. Report the first failed input ordinal after the drain.

Do not enable effectful workers until durable effect receipts exist.

## Verification gates

Add these tests before activation:

- eight items run with no more than four active workers;
- admission refills without a batch barrier;
- duplicate items receive different child lineages;
- reverse completion still produces input order;
- structured LLM outputs retain all declared fields;
- append, sum, and merge reducers are deterministic;
- an incompatible reducer fails during compilation;
- process loss reuses completed item checkpoints;
- two item HITL cards resume their exact children;
- sixteen pause cards stop new admission;
- changed business input creates a new child lineage;
- changed resume controls keep the original lineage;
- safe progress events contain no item content.

Use the PostgreSQL checkpointer for restart tests. Use a second worker process
for reclaim tests.

## Implementation order

1. Extend state descriptors with approved reducer names.
2. Add compiler ownership for one reusable worker definition.
3. Add upstream planner-to-list compiler fixtures.
4. Add bounded item dispatch and deterministic collection.
5. Add structured output selection and reducer folding.
6. Add item checkpoint, process-loss, and reclaim proofs.
7. Add aggregate item HITL and authorization.
8. Add UI progress grouping and live chat proof.

Implement the fixed `parallel` node first. Reuse its durable child runner,
interrupt aggregation, hierarchy projection, and observability in this node.
