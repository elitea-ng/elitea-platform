# Map admitted for an inline State modifier worker; Parallel stays refused at bind — 2026-10-13

Branch `feat/pipeline-parallel-map`, from `main` `a7ece9443`. The Point 5 node families carry no feature flags
(owner decision 2026-10-12, [graph-shaping-admitted-20261012.md](graph-shaping-admitted-20261012.md)); this change
removes the last Map switch (`MAP_INTEGRATION_READY`) and wires the production composition. `FIXED_PARALLEL_INTEGRATION_READY`
stays false, for the reasons in section 4.

## 1. Business behaviour and what was not ported

- **Behaviour.** A pipeline may contain a `map` node. It reads one list from state, runs one owned `state_modifier` worker once
  per item (at most 64 items, at most 8 at a time), and writes the selected worker outputs as one ordered list
  (`{index, outputs}` records in input order). A failed item stops new admission, lets the admitted items finish, and fails the node
  with one stable code. The pipeline can pause (`hitl`, static interrupts) before or after the Map and continues in a later
  execution without running an item again.
- **The current platform** has no `map` or `parallel` node type (`langraph_agent.py` builds `function`, `toolkit`, `mcp`, `agent`,
  `subgraph`/`pipeline`, `loop`, `code`, `llm`, `router`, `decision`, `state_modifier`, `printer`, `hitl`, `custom` only; its
  "parallel" is model-selected tool parallelism and the HITL registry `runtime/_parallel_hitl_registry.py`). There is no YAML shape or
  reducer to match. The shape is the one in [../map-reduce-pipeline-node-design.md](../map-reduce-pipeline-node-design.md) and is unchanged.
- **Not ported / refused, with a named reason when the graph is bound (and at save in Main and the editor):**

| Form | Where it is refused | Reason |
|---|---|---|
| Worker that is not an inline `state_modifier` (an `agent`, `llm`, `toolkit`, `code` ...) | Worker bind, Main save (`PIPELINE_MAP_FORM_UNSUPPORTED`), editor | Only a pure, non-pausing, effect-free worker can be re-run safely after a crash. An `agent` worker also needs the saved-participant identity that exists only under `SCOPED_APPLICATION_CONTINUATION_READY` (false). |
| `map` in a pipeline that also has an `agent` node | Worker bind, Main save, editor | `MapTurnAuthority` stamps and filters the root thread only; an Agent child keeps per-turn threads of its own, so a second turn could resume the first turn's finished child. |
| `map` in a saved pipeline an `agent` node calls | Worker assembly (`bind_admitted_saved_pipeline`) | Map runs only in the pipeline a conversation starts. |
| `map` together with fixed `parallel` | Worker bind | Unchanged: no accepted overlay composition. |
| A pause inside an item | Worker (no pausing worker form exists) | Per-interrupt fan-out decisions (`fanout-interrupt-decisions-v1`) are not built. |

## 2. Recovery and cancellation decisions

| Question | Decision | Enforced by |
|---|---|---|
| Re-run of an item after a crash | Allowed: a `state_modifier` is a pure function of the frozen item, index and broadcast values. Completed items are restored from their receipts, not run again. | `map_reduce.rs` receipts; `completed_item_receipts_survive_parent_loss_without_worker_replay`; PG `postgres_map_runs_every_item_in_order_and_a_continuation_does_not_rerun_them` (item threads stay at 3 across the continuation) |
| Child identity across a continuation | Frozen at occurrence creation (Track C1), never derived from the continuation's `execution_id` | `fanout_lineage.rs` PG proofs; `map_resume_turn_reads_the_earlier_root_and_still_fences_identity` |
| New turn on the same conversation | Own occurrence and own item threads under the new execution; the first turn's receipts are not reused | PG `postgres_map_second_turn_runs_its_own_items` |
| Deadline | The original command deadline bounds the node; an already-expired deadline refuses before any item starts | `fanout_deadline` (`session.rs`); PG `postgres_map_with_an_expired_deadline_refuses_before_any_item_starts`; `deadline_stops_pending_items_at_the_deadline_and_is_still_recorded` |
| Stop / cancel | Dropping the run drops the owned item futures (pure work, no external effect). The latch (`FanoutCancellation`) stays unwired, as before. | `latch_cancel_stops_pending_items_promptly_without_recording_a_stop` (latch semantics only) |
| Lease loss | A control stop, not an item failure: siblings stop, nothing is recorded, a later claim restores | `a_lease_lost_item_is_a_control_stop_and_records_nothing`, `a_lease_lost_item_stops_its_admitted_siblings` |
| HITL inside an item | Refused by construction (no pausing worker form) | see section 1 |

## 3. Production composition (new)

- `AuthorizedNativeCommandBinding` and `OrdinaryNativeAgentPlan` carry `deadline_unix_millis` (the command's, already verified > 0).
- `PipelineStateServices` keeps the typed `MapCheckpointAuthority` beside the Parallel one; `assemble_pipeline_native` builds
  `MapTurnAuthority` (now accepting a continuation: it reads the latest root checkpoint of any earlier turn, still fenced by the
  claim's execution identity) and `MapCompilerBinding` for a definition with a `map` node. Every other pipeline keeps `TurnCheckpointer`.
- `MapCompilerBinding::new` no longer has a gate; it still refuses an expired deadline.

## 4. Why Parallel is not admitted

Its branches are `agent` nodes. (1) The saved-participant fingerprint the branch hashes exists only under the scoped Agent
continuation (`call_owners` are inserted only when `SCOPED_APPLICATION_CONTINUATION_READY`, false). (2) A branch that pauses needs
per-interrupt decisions; the complete-set resume (`parallel_published_resume.rs`) is unwired and superseded. (3) A branch that runs a
tool can repeat its effect after a crash until effect receipts cover Agent children. (4) Public BLOCKED delivery and real
PostgreSQL process/reclaim acceptance for Agent children are still open ([fixed-parallel-integration-20261004.md](fixed-parallel-integration-20261004.md)).
`pending_wave_2a` therefore keeps `parallel` for the Worker (parses, refused at bind), Main (admits) and Web (listed, authoring withheld).

## 5. Tests

- Worker graph: the existing 60+ `map_*` tests (ordering, refill under a concurrency bound, failure drain, receipts surviving parent
  loss, deadline, latch, lease loss, state-boundary limits) plus `map_resume_turn_reads_the_earlier_root_and_still_fences_identity`,
  `map_refuses_to_share_a_pipeline_with_an_agent_node_and_names_why`, and the updated production-binding test.
- Worker production assembly against real PostgreSQL (`pipeline_map_pg_tests.rs`, skipped without `ELITEA_TEST_DATABASE_URL`): the four
  tests named above, and `a_saved_pipeline_with_a_map_node_is_refused_where_an_agent_node_calls_it` (no database).
- Admission parity: `node_admission_parity_tests.rs` (Worker arms = `admitted` + `pending_wave_2a.worker`), Main
  `node_admission_parity_test.go`, Web `runtimeContract.admissionParity.test.ts`.
- Main: `TestCheckAdmitsAMapNodeWithAStateModifierWorker`, `TestCheckRefusesMapFormsTheRuntimeCannotRecoverSafely`,
  `TestCheckLeavesUnreadableMapWorkersToTheCompiler`.
- Web: Map in `CompilerAdmittedNodeTypes` (thirteen Add-node entries), the Map worker narrowed to State modifier, J16
  `ADMITTED_NODE_LABELS` gains `Map`.

## 6. Not done

Browser run of a Map pipeline against a deployed stack; a killed-process mid-Map reclaim against PostgreSQL (the in-memory receipt
proof and the lineage PG proofs cover the pieces); nested Map in a saved child pipeline; Agent/LLM/toolkit workers; Parallel.
