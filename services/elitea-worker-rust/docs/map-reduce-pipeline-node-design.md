# Data-driven map and reduce pipeline design

Status: describes the shipped V1 code on main (2026-10-08). Gate: off.

All `path:line` citations are relative to `services/elitea-worker-rust/`
unless they start with `apps/`. The earlier proposal in this file differed
from the code. This document replaces it with the V1 contract. The proposal
is history in git.

## What V1 is

A `map` node reads one list from parent state. It runs one owned worker once
per item. It collects each item's selected outputs in input order. It writes
one ordered list to one destination channel.

Every item runs as its own child graph with its own child checkpoint thread.
The map node is a custom ADK node (`DurableMapNode`,
`src/agents/graph/map_reduce.rs:289`). It does not use ADK `LoopAgent` or
`ParallelAgent`.

V1 workers cannot pause. V1 has no per-item failure collection. Both are V2
(see the last section).

## Admission gate

| Layer | State | Evidence |
|-------|-------|----------|
| Worker compiler | `MAP_INTEGRATION_READY = false`. `MapCompilerBinding::new` returns `Unsupported` before any other check. | `src/agents/graph/map_compiler.rs:31`, `:44-48` |
| Test-only binding | `for_tests` skips the gate. | `map_compiler.rs:69-75` |
| Web node-type allow-list | `CompilerAdmittedNodeTypes` does not list `Map`. It lists `Parallel`, `Code`, `Decision`, `Agent`, `Toolkit`, `Mcp`, `Hitl`, `LLM`, `Printer`, `Router`, `StateModifier`. The Add-node menu is driven by this list. | `apps/elitea-web/src/features/pipelines/lib/flow-editor/constants/runtimeContract.constants.ts:73-85` |
| Web settings panel | `MapSettings.tsx` exists and is mounted by `MapNode.tsx`. | `apps/elitea-web/src/features/pipelines/ui/settings/graph-extensions/MapSettings.tsx`, `ui/nodes/MapNode.tsx:6-9` |

The YAML parser still accepts `type: map` (`src/agents/graph/compiler.rs:1976`).
Only the execution binding is gated. A document with a map node parses, but
cannot be bound for execution in production.

## YAML contract

Parsed by `RawMapNodeDefinition` (`src/agents/graph/map_yaml.rs:9-28`) with
`#[serde(deny_unknown_fields)]`. An unknown key is a parse error.

| Key | Type | Required | Default | Rule | Evidence |
|-----|------|----------|---------|------|----------|
| `id` | string | yes | none | valid graph id; differs from `worker` | `map_yaml.rs:12`, `map_reduce.rs:55-57` |
| `type` | string | yes | none | must equal `map` | `map_yaml.rs:13`, `:45` |
| `worker` | string | yes | none | valid graph id; names a node in the same document | `map_yaml.rs:15`, `map_compiler.rs:267-269` |
| `source` | string | yes | none | declared `list` state root; not a built-in key | `compiler.rs:2030-2038` |
| `item` | string | yes | none | local channel; must NOT be declared in `state` | `map_compiler.rs:250-253` |
| `index` | string | yes | none | local channel; must NOT be declared in `state`; differs from `item` | `map_compiler.rs:250-253`, `map_reduce.rs:78` |
| `broadcast` | list of strings | no | `[]` | at most 64; no duplicates; each is a declared state root | `map_yaml.rs:19-20`, `map_reduce.rs:60`, `:85`, `map_compiler.rs:254-257` |
| `outputs` | list of strings | yes | none | 1 to 64 keys; no duplicates; each declared in `state` and produced by the worker | `map_reduce.rs:61-62`, `:86`, `map_compiler.rs:258-262`, `:278-281` |
| `destination` | string | yes | none | declared `list` state root; differs from `item` and `index` | `compiler.rs:2031`, `map_reduce.rs:83-84` |
| `max_items` | integer | yes | none | 1 to 64 | `map_reduce.rs:58` |
| `max_concurrency` | integer | yes | none | 1 to 8 | `map_reduce.rs:59` |
| `reduction` | enum | yes | none | only `ordered_collection` | `map_reduce.rs:47-51` |
| `transition` | string | no | none | `END` or a valid graph id | `map_yaml.rs:26-27`, `:46-50` |

Further key rules (`map_reduce.rs:66-77`): every key passes
`valid_output_key` (non-empty, at most 256 bytes, no NUL/CR/LF,
`src/agents/graph/yaml.rs:374-381`), is not `messages`, and is not a reserved
user state key (`compiler.rs:2215-2245`).

The worker's `input` keys must be a subset of `{item, index} + broadcast`.
Any other input key is rejected (`map_compiler.rs:287-301`). For an Agent
worker, mapped variables must also stay inside that set (`:307-319`).

Errors:

- A map node YAML error becomes the generic pipeline error "a Map node is
  invalid" (`compiler.rs:1976-1978`). The specific code is dropped there.
- Runtime and validation errors use `GraphError::NodeExecutionFailed` with
  message `graph.map.<code>` (`map_reduce.rs:682-687`). Codes in use:
  `invalid_configuration`, `resource_exhausted`, `invalid_source`,
  `invalid_mapping`, `invalid_reducer`, `invalid_worker`, `invalid_result`,
  `invalid_candidate`, `invalid_child_scope`, `invalid_item_scope`,
  `invalid_turn_authority`, `invalid_activation`, `unsupported_parent_route`,
  `stale_activation`, `missing_occurrence`, `corrupt_occurrence`,
  `corrupt_receipt`, `immutable_receipt`, `missing_receipt`,
  `incomplete_collection`, `item_failed`, `stopped`, `invalid_value`,
  `missing_state_schema`.
- The YAML text is capped at 64 KiB (`map_yaml.rs:40-42`).

Example (from the test fixture, `src/agents/graph/map_compiler_tests.rs:216`):

```yaml
entry_point: map_entities
state:
  entities: {type: list, value: []}
  item_result: {type: str, value: ''}
  mapped: {type: list, value: []}
nodes:
  - {id: map_entities, type: map, worker: render, source: entities,
     item: entity, index: item_index, outputs: [item_result],
     destination: mapped, max_items: 64, max_concurrency: 4,
     reduction: ordered_collection, transition: END}
  - {id: render, type: state_modifier, input: [entity, item_index],
     output: [item_result], template: '{{ item_index }}'}
```

## Topology rules

The compiler enforces these in `validate_map_ownership`
(`map_compiler.rs:236-331`):

- The worker is a `state_modifier` or `agent` node
  (`map_compiler.rs:270-273`). Other families are rejected.
- The worker has no route targets, is not the entry point, is not owned by
  fixed Parallel, and is owned by at most one map (`:273-277`).
- The worker is not named in `interrupt_before` or `interrupt_after` (`:277`).
- No parent node may route to the worker (`:321-329`).
- A pipeline cannot contain both a map node and a fixed `parallel` node
  (`map_compiler.rs:124-128`).
- The worker must produce every key in `outputs` (`:278-281`).
- A worker's cleaned keys must be among its own outputs (`:293-296`).
- Declared-state checks run with `item` typed as opaque JSON and `index` as
  `int` (`:302-306`).

## Workers and pausing

| Worker family | V1 | Evidence |
|---------------|----|----------|
| StateModifier | allowed | `map_compiler.rs:272`, `:405-407` |
| Application (YAML `agent`) | allowed, with extra proofs below | `map_compiler.rs:272`, `:408-433` |
| Router | NOT a worker | `map_compiler.rs:270-273` |
| LLM, Code, Toolkit, HITL, Decision, Printer, nested Map/Parallel | not admitted | `map_compiler.rs:206-210`, `:434` |

Application worker proofs (`map_compiler.rs:181-211`):

- Fixed input values pass the structure guard
  (`src/agents/graph/application.rs:206-213`).
- The saved participant must resolve through
  `map_worker_definition_digest` (`src/agents/pipeline.rs:1260-1276`). This
  succeeds only when the saved participant is a pipeline and
  `map_nonpausing_effectfree()` is true. A zero fingerprint is refused
  (`map_compiler.rs:193-197`).
- `map_nonpausing_effectfree()` requires empty `interrupt_before` and
  `interrupt_after`, and every node of the nested pipeline to be a
  `state_modifier` or `router` (`map_compiler.rs:105-114`). Router may appear
  only here, inside a nested cohort. Nested Agent, LLM, Code, tool, HITL,
  Decision, Printer, Map and Parallel nodes all make the cohort ineligible.

Non-pausing by construction:

- The item graph is `START -> worker -> END` with max concurrency 1
  (`map_compiler.rs:436-445`).
- A node that returns an interrupt is captured as an `UnexpectedPause`
  receipt (`src/agents/graph/map_reduce_checkpoint.rs:462-475`, `:478-484`).
- `invoke_detailed` returning `GraphError::Interrupted` becomes
  `MapStop::UnexpectedPause` (`map_reduce.rs:620-622`). A saved
  `UnexpectedPause` receipt replays as the same stop (`:535-537`).
- A resumed turn is refused: `MapTurnAuthority::new` rejects `resume = true`
  with `invalid_turn_authority` (`src/agents/graph/map_turn.rs:33-45`).
- A worker that sets `goto_parent` fails with `unsupported_parent_route`
  (`map_reduce.rs:603-605`). The lifecycle wrapper also refuses `goto`
  (`map_reduce_checkpoint.rs:460`).

Child checkpoint minting by worker kind:

- `PostgresCheckpointer::for_item` accepts only `StateModifier` and refuses
  `Application` (`src/state/postgres_checkpointer/map_children.rs:41-45`).
- `ApplicationCheckpointers::for_item` is the path for `Application`. It adds
  the branch paths for the nested thread tree
  (`src/state/postgres_checkpointer/application_children.rs:335-371`).

## Limits

| Limit | Value | Enforced at |
|-------|-------|-------------|
| Items per map | 64 (`MAX_ITEMS`) | `map_reduce.rs:22`, `:58`, runtime `:112-114`, replay `map_reduce_checkpoint.rs:173`, child mint `map_children.rs:64` |
| Concurrent workers | 8 (`MAX_CONCURRENCY`) | `map_reduce.rs:23`, `:59`, admission `:427` |
| One item input, one item output, broadcast total | 512 KiB (`MAX_ITEM_BYTES`) | `map_reduce.rs:24`, `:134-144`, `:645` |
| Frozen plan (all item inputs) | 2 MiB (`MAX_PLAN_BYTES`) | `map_reduce.rs:25`, `:116`, `:152`, `:159` |
| Joined collection | 8 MiB (`MAX_COLLECTED_BYTES`); each item counts size + 32 bytes | `map_reduce.rs:26`, `:435-443`, `:481` |
| Whole checkpoint or state | 8 MiB (`MAX_CHECKPOINT_BYTES`) | `map_reduce.rs:27`, `:488`, `:772` |
| Item and output JSON shape | depth 48, 32,768 values | `map_reduce.rs:745-749` |
| Whole-state and checkpoint JSON shape | depth 64, 65,536 values | `map_reduce.rs:751-758`, `:760-773` |
| `broadcast` or `outputs` length | 64 each | `map_reduce.rs:60`, `:62` |
| Child threads per item | 129 | `map_reduce.rs:397` |
| Root thread id, execution id | 512 and 256 bytes | `map_reduce.rs:101`, `map_turn.rs:38-40` |

The 64 x 512 KiB product exceeds the 2 MiB plan cap. A full 64-item map is
bounded by the plan cap, not by the per-item cap. Exceeding a limit returns
`graph.map.resource_exhausted`. Collected-size overflow stops the map with
`MapStop::ResourceExhausted { index }` (`map_reduce.rs:442`).

## Execution

1. Freeze. `MapDefinition::freeze` (`map_reduce.rs:93-161`) validates config
   and state shape, reads `source` as an array (`invalid_source` otherwise),
   and snapshots `broadcast` values. An empty list is valid. It emits an empty
   collection and starts no children (test
   `empty_list_emits_empty_collection_without_children`).
2. Item input. Each item input is `broadcast` plus `{item: <value>, index:
   <position>}` (`:139-144`). The worker sees only these channels
   (`map_compiler.rs:367-377`). The item value is opaque JSON and is not typed
   against global state. Item inputs are hashed (`map_reduce.rs:145-146`).
3. Occurrence freeze. The plan is saved in the root checkpoint metadata under
   `elitea.graph.map.occurrence.v1` before any child runs
   (`map_reduce_checkpoint.rs:17`, `:31-77`). A replay at the same step
   returns the saved plan. If the live source differs, it fails with
   `stale_activation` (`:50-56`).
4. Reducer check. The destination channel must exist with reducer `Overwrite`
   or `Append`. Anything else fails with `graph.map.invalid_reducer` before any
   item is dispatched (`map_reduce.rs:355-364`; test
   `incompatible_reducer_fails_before_any_item_dispatch`).
5. Child admission. One child checkpointer is minted per item before dispatch,
   under the deadline (`:367-417`). Duplicate or out-of-scope child threads
   fail with `invalid_child_scope`.
6. Bounded dispatch. Up to `max_concurrency` workers start. When one settles,
   the next item starts (`:418-453`). There is no batch barrier (test
   `eight_items_refill_four_slots_without_batch_barrier`).
7. Item output. A completed item projects exactly the keys in `outputs`, each
   checked against the declared state type (`map_compiler.rs:460-482`,
   `map_reduce.rs:627-649`). A worker that sets `_pipeline_blocked` yields
   `MapStop::Blocked` (`map_compiler.rs:466-468`).
8. Collect. Results are sorted by item index (`map_reduce.rs:454`). The
   aggregate is `[{"index": i, "outputs": {...}}, ...]` in input order,
   whatever order items finished (`:471-479`; test
   `reverse_completion_collects_original_order_and_preserves_structured_values`).
9. Reduce. The node emits one update for `destination` (`:494-496`). The
   parent reducer applies it once (test
   `collector_returns_one_delta_and_parent_reducer_runs_once`). Before that, a
   candidate state is built with the same reducer and validated (`:482-492`).
   A failure keeps parent state unchanged (test
   `candidate_validation_failure_keeps_parent_state_and_committed_children`).
   Only `destination` is written. The `outputs` keys stay unchanged in parent
   state (also stated in the Web help, `MapSettings.tsx:65`).
10. Transition. The node edge goes to `transition` or `END`
    (`map_compiler.rs:229-231`). If `transition` is `END`, `destination` is a
    terminal JSON key (`map_compiler.rs:92-102`).

## Failure, cancellation, deadline

Fail-after-drain:

- On the first non-success, admission closes. Items already running finish
  (`map_reduce.rs:433-453`). Their receipts are kept.
- After the drain, the stop is chosen: a control stop (cancel or deadline)
  first, else the lowest-index item failure (`:455-461`).
- The stop is recorded in the occurrence checkpoint (`:462-466`,
  `map_reduce_checkpoint.rs:79-118`) and returned as
  `MapNodeOutcome::Stopped`. Through the plain `Node::execute` path it
  surfaces as `graph.map.stopped` (`map_reduce.rs:661-666`).
- No partial collection is written. Success means every item succeeded.

`MapStop` variants (`map_reduce.rs:204-214`): `Failed`, `ResourceExhausted`,
`UnexpectedPause`, `Blocked` (each with `index`), `Cancelled`,
`DeadlineExceeded`. A saved stop replays as the same stop. A different stop on
the same occurrence is `stale_activation` (`map_reduce_checkpoint.rs:90-96`).

Cancellation: checked before admission, before each item, and polled every
50 ms during a running item (`map_reduce.rs:324-335`, `:368-373`, `:543-545`,
`:811-815`). A cancelled item returns `MapStop::Cancelled`.

Deadline: the compiler passes the original execution deadline
(`map_compiler.rs:36`, `:227`). It is checked at admission, during child
minting (`timeout_at`, `map_reduce.rs:380-390`), and while an item runs
(biased `select!`, `:588-600`). On expiry the in-flight item future is dropped
and nothing is replayed (tests
`original_deadline_stops_and_drops_owned_effectfree_futures_without_replay`,
`map_expired_original_deadline_preserves_parent_business_state`). An already
expired deadline is refused at binding time (`map_compiler.rs:49-53`).

## Checkpointing and recovery

- Parent occurrence. One checkpoint append stores the frozen plan, guarded by
  the exact parent compare under the database writer lock
  (`map_reduce_checkpoint.rs:20-21`, `:75`). Stale parents are rejected (test
  `atomic_appender_rejects_stale_parent_and_preserves_exact_replay`).
- Item receipt. Each item's final child checkpoint carries a receipt
  (`Completed`, `Failed`, `UnexpectedPause`) under
  `elitea.graph.map.item-receipt.v1` (`map_reduce_checkpoint.rs:18`,
  `:258-266`, `:321-343`). On replay a `Completed` receipt returns the saved
  result without running the worker (`map_reduce.rs:525-542`; test
  `completed_item_receipts_survive_parent_loss_without_worker_replay`). A
  restored item graph gets an empty input delta, so append reducers do not
  replay input (`:582-587`).
- Child threads. `m1:<base64url sha256>` over tenant, project ids, capability,
  definition digest, execution id, generation, root thread, node id, step,
  config/worker/source digests, worker id, item ordinal and item input digest
  (`src/state/postgres_checkpointer/map_children.rs:90-134`).
- Fresh turn. `MapTurnAuthority` stamps the root checkpoint with
  `[execution_id, generation]` and keeps the physical parent compare on a new
  turn (`map_turn.rs:57-76`, `:108-145`; test
  `map_fresh_turn_keeps_exact_physical_parent_cas_and_old_receipts`).

Known lineage defect: the child thread hash includes `execution_id` and
`generation` (`map_children.rs:110-111`). A new execution or a new writer
generation mints different child threads, so completed item receipts from an
earlier generation are not found. Track C1 intends to freeze child identity so
that it no longer depends on these two fields. That work is planned and is NOT
merged. Until it lands, do not claim item reuse across generations.

## Verification today

Tests in the repo (all under `src/agents/graph/`):

| Area | Tests |
|------|-------|
| YAML and ownership | `map_yaml_child_channels_are_opaque_and_not_parent_descriptors`, `map_yaml_rejects_ownership_routes_pauses_and_local_global_collisions`, `map_keeps_explicit_root_declaration_order_and_selected_terminal_key_seam` (`map_compiler_tests.rs`) |
| Gate and authority | `map_production_binding_and_missing_authority_fail_before_dispatch`, `map_foreign_parent_pointer_refuses_before_child_activation`, `map_typed_turn_reclaim_retains_capabilities_and_exact_original_execution` |
| Real ADK graph | `map_actual_adk_graph_collects_unknown_items_and_terminal_replay_once`, `real_parent_adk_transition_applies_once_and_terminal_replay_skips_map`, `map_in_place_collection_and_overlapping_broadcast_use_original_frozen_values` |
| Saved Agent worker | `map_saved_agent_refuses_alias_and_zero_fingerprint_before_dispatch`, `map_saved_agent_rejects_deep_fixed_value_before_definition_hashing`, `map_nonpausing_cohort_refuses_static_pause_and_effectful_saved_definitions` |
| Scheduling and order | `eight_items_refill_four_slots_without_batch_barrier`, `reverse_completion_collects_original_order_and_preserves_structured_values`, `empty_list_emits_empty_collection_without_children` (`map_reduce_tests.rs`) |
| Failure and stops | `failed_item_drains_admitted_items_and_keeps_successful_receipts`, `unexpected_pause_is_typed_non_success_and_does_not_dispatch_on_replay`, `blocked_terminal_stays_non_success`, `compiler_pausing_admission_fails_closed` |
| Limits | `invalid_sources_and_resource_limits_fail_before_dispatch`, `collected_memory_limit_stops_admission_and_preserves_parent_state`, `adversarial_compact_source_and_broadcast_values_fail_structure_guard`, `map_checkpoint_json_roundtrip_reserves_codec_envelope_depth` |
| Deadline | `original_deadline_stops_and_drops_owned_effectfree_futures_without_replay`, `map_expired_original_deadline_preserves_parent_business_state` |
| Receipts and scope | `completed_item_receipts_survive_parent_loss_without_worker_replay`, `child_scope_collision_fails_before_worker_invocation`, `map_item_completion_receipt_belongs_only_to_root_checkpoint`, `map_event_scope_preserves_exact_descendant_call_lineage` (`map_item_scope_tests.rs`) |

Missing today:

- Any production path. The gate is off, so no deployed or browser acceptance
  of a map run exists. `docs/remaining-gates.md` has no Map acceptance entry.
- A test of Postgres child-thread lineage across an execution or generation
  change (the defect above).
- An end-to-end test of an Application worker against the Postgres
  authority. Application minting is covered only by
  `src/agents/pipeline/scope_receipts_map_tests.rs` and the
  `map_catalog_tests` module in `application_children.rs`.
- A Web test that `Map` appears in the Add-node menu. It does not, by design
  while the gate is off.
- A test that worker loss mid-item replays only the unfinished item against
  real Postgres. ASSUMPTION: the in-memory authority tests above are the only
  recovery evidence.

## V2 (pausing workers, any worker family, per-interrupt HITL)

These are V2 and live in
[`fanout-v2-runtime-design.md`](fanout-v2-runtime-design.md):

- Pausing workers and per-interrupt HITL.
- Workers from any family: LLM, Agent, Pipeline, Code (Code last).
- `on_item_failure: collect`.
- Status grouping for more than 16 items.

The earlier proposal's complete-set decision model and its pausing-worker
design are superseded. Their history is in git. Nothing in this document
depends on them.
