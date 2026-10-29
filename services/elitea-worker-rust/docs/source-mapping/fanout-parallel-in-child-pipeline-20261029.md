# A fixed Parallel inside a saved child pipeline (Point 5, FU-249)

Branch `feat/graph-fanout-parallel-in-child-pipeline`, stacked on `feat/graph-fanout-agent-branch-pause` (FU-246),
`feat/graph-fanout-inline-llm-branches` (FU-247) and `feat/graph-fanout-parallel-admitted` (Wave 2a-2,
[mapping](fanout-parallel-admitted-20261027.md)). Contract `libs/proto/contracts/fanout-interrupt-decisions-v1.md` v1.1
is unchanged; no migration, no protobuf change.

## Business behaviour

From the current platform: pipelines are the native nesting primitive, so a saved pipeline run by a pipeline's Agent
node behaves like the same pipeline started on its own. A Parallel in it runs its branches, raises one card per
pausing branch, parks, and resumes only the answered branches when a decision arrives, in any order and from any tab.

Now allowed (was refused typed `graph.fanout.parallel_in_child_pipeline`): a Parallel in a saved pipeline that the
root reaches through pipeline Agent nodes only (the child runs as a subgraph of the root graph).

Still refused, typed and readable, at save, attach, start and Worker assembly:
- a Parallel in a pipeline run as an Agent's **tool** (beneath a saved Agent);
- a Parallel anywhere beneath another Parallel's **branch** (nested fan-out).

Not ported: nothing; the legacy platform has no durable park/wake to compare.

## Design

1. **Checkpoint authority through the child's chain.** A child graph saves through its Agent node's activation
   overlay. A Parallel's parent rows are atomic appends on the store, which would bypass the overlay and drop its
   stamp, so a restart could hide the child's progress. `ChildParallelAuthority`
   (`src/agents/graph/parallel_child_authority.rs:27`) reads and saves through the chain, and appends with exactly
   the stamp the chain's save writes (`ApplicationActivationCheckpointer::stamp`,
   `libs/rust/agent-runtime/src/graph/application_activation.rs:88`). Its probe hides the heads the overlay hides
   (`admits`, `:132`; `parallel_child_authority.rs:146`), and an append that expects no row follows a hidden earlier
   visit's row only (`physical_parent`, `:66`), as `FanoutTurnAuthority` does for the root thread.
2. **Binding.** The root binds the run's Parallel binding when it or a graph-path descendant holds a Parallel
   (`src/agents/session.rs:2268`). An Agent node whose child composition holds one gets the run's binding rebased on
   its child authority (`PipelineNodeRuntimes::child_fanout`, `src/agents/graph/parallel_compiler.rs:147`;
   `ParallelCompilerBinding::rebased`, `:103`; `ApplicationNode::new_in_fanout`, `src/agents/graph/application.rs:494`;
   `resolve_node_in_fanout`, `src/agents/pipeline.rs:1661`). A graph without a binding still refuses
   (`compiler.rs:1346`); the tool path is told apart by the materialization path (`is_graph_path`,
   `src/agents/application_tools/materialization.rs:141`; `pipeline.rs:2057`).
3. **Park.** The child's park is the run's park terminal: the nested-interrupt child projection leaves it to the root
   (`src/agents/events.rs:5407`), and the park keeps open the Agent-node calls on the parked path (`events.rs:1731`).
4. **Wake.** A wake re-enters the Agent node, which resumes the child at its parked Parallel. The node leaves the task
   message out when the child resumes its own unfinished frontier, since the subgraph merges its input through the
   reducers again (`application.rs:597`, `:763`); a text resume still sends its new message.
5. **Recovery.** Model-checkpoint recovery accepts an Agent node on the root frontier only when Main froze its
   attachment as a saved pipeline (`recovery_frontier_supported`, `src/agents/graph/compiler.rs:985`;
   `frozen_pipeline_aliases`, `pipeline.rs:1976`) **and** the child thread is parked inside a frozen Parallel occurrence
   (`holds_parallel_occurrence`, `src/agents/graph/parallel.rs:51`; `session.rs:1672`). That child checkpoint is bound
   into the recovery evidence digest (`elitea.pipeline.recovery.v2`). Any other child frontier (an effectful node
   included) is refused as before.
6. **No answered card raised again.** A re-entered occurrence re-raises its restored cards only after its restore
   fetch, and never one that fetch already holds a decision for (`libs/rust/agent-runtime/src/graph/fanout/runner.rs:425`,
   `decided` `:853`). This also fixes a root wake re-run from its park, which showed the answered card again.
7. **Turn isolation.** When only a child holds a Parallel, the root now uses `FanoutTurnAuthority` instead of
   `TurnCheckpointer`. It scoped only the root thread, so a second turn in the same conversation saw the first turn's
   child rows (same input: the old result reused; another input: the overlay refused it). It now stamps and scopes the
   root thread and every thread beneath it, as `TurnCheckpointer` does (`src/agents/graph/fanout_turn.rs:79`); branch
   threads still go through the branch factory.
8. **Main.** The composition check refuses a child Parallel only on the tool path or beneath a Parallel branch
   (`services/elitea-main/internal/domain/applicationcomposition/composition.go:496`; branch Agent nodes read by
   `branchNodes`, `:845`).

## Changed paths

- agent-runtime: `src/graph/application_activation.rs`, `src/graph/fanout/runner.rs` (+ `runner_tests.rs`).
- Worker: `src/agents/{application_tools/materialization.rs, events.rs, pipeline.rs, session.rs}`,
  `src/agents/graph/{application.rs, compiler.rs, mod.rs, parallel.rs, parallel_compiler.rs, parallel_child_authority.rs}`;
  tests `pipeline_fanout_effect_pg_tests.rs`, `pipeline_scoped_tests.rs`, `graph/compiler_tests.rs` and the frontier
  call sites in `graph/{map,parallel,shaping}_compiler_tests.rs`.
- Main: `internal/domain/applicationcomposition/{composition.go, composition_test.go}`,
  `internal/api/v2/agentexecution/route_test.go`, `internal/api/v2/eliteacore/application_composition_postgres_integration_test.go`.
- Web: the refusal sample text in two display tests.

## Tests

Suites on the branch (production test profile, real PostgreSQL; logs `handoffs/point5-wave1-20261008/2a2/logs/*-fu249{b,c}.log`):

- Worker `cargo test --lib`: 2482 passed, 0 failed, 16 ignored (pre-existing; `worker-fu249d.log`, after the
  turn-isolation fix). clippy `-D warnings` (all targets and
  features) and fmt clean.
- agent-runtime `cargo test`: 1404 passed, 0 failed, 1 ignored. clippy and fmt clean.
- Main `go test -race ./...`: all 266 packages ok.
- Web: the two display tests whose refusal sample text changed, 22/22; new `chatStreamNestedWake.test.ts` 1/1.

New tests (all pass): real PG `postgres_a_parallel_in_a_saved_child_pipeline_parks_and_a_wake_completes_it`,
`postgres_a_wake_lost_inside_a_child_pipeline_resumes_the_answered_branch`,
`postgres_a_child_pipeline_lost_outside_a_parallel_is_not_resumed`; unit
`an_agent_node_is_a_recovery_frontier_only_for_a_frozen_saved_pipeline`, `stamp_matches_save_and_admits_matches_load`,
`a_restore_re_raises_only_the_cards_without_a_decision`; Main `TestAPipelineWithAParallelRunsInTheRootGraphOnly`,
`TestReadPipelineMarksTheAgentNodesAParallelRunsAsBranches`, `TestAChildPipelineGainingAParallelIsRefusedThroughAnAgentParent`,
real-PG `TestAParallelInASavedChildPipelineIsCheckedAtAttachAndAtSave`.

## Performance

| Mechanism | Path | Proving test | Measured |
|---|---|---|---|
| The stamp borrows rows outside the child family; one clone only for a child row (as the overlay's save did) | `application_activation.rs:88` | `stamp_matches_save_and_admits_matches_load` | 0 extra clones |
| The probe decides visibility from the head (no state read) | `parallel_child_authority.rs:146`, `application_activation.rs:132` | same | 0 extra reads per save |
| An append expecting no row reads the head once, only then | `parallel_child_authority.rs:66` | child park test | 1 head read per occurrence freeze |
| The frontier read for a wake happens only for an Agent node whose child holds a Parallel | `application.rs:763` | child park/wake test | 1 child checkpoint read per such re-entry |
| Recovery reads one child checkpoint per Agent node on the frontier | `session.rs:1672` | lost-wake test | 1 read |
| Parent rows per visit and open cards stay the 2a-2 bounds | runner | 2a-2 limit tests | unchanged |

## Durability

| Mechanism | Path | Proving test |
|---|---|---|
| Every row the child's Parallel writes on the child thread carries the activation stamp | `parallel_child_authority.rs:120` | `postgres_a_parallel_in_a_saved_child_pipeline_parks_and_a_wake_completes_it` (`pipeline_fanout_effect_pg_tests.rs:1433`) |
| A wake in a new execution re-enters the child at its parked frontier and resumes only the answered branches | `application.rs:763` | same (3 decisions → 3 model steps → join of 3) |
| A wake lost mid-model inside the child resumes that branch from its model checkpoint | `compiler.rs:985`, `session.rs:1672` | `postgres_a_wake_lost_inside_a_child_pipeline_resumes_the_answered_branch` (`:1539`) |
| A fresh turn never sees an earlier turn's child rows | `fanout_turn.rs:79` | `postgres_a_second_turn_through_a_child_pipeline_parallel_runs`, `postgres_a_second_turn_through_the_same_child_pipeline_runs`, `every_new_row_of_the_turn_is_stamped_and_replays_are_not` |

## Resilience

| Mechanism | Path | Proving test |
|---|---|---|
| A child lost outside a Parallel is not resumed blind (the inspect refuses; the guard is decisive: with it off the test fails) | `session.rs:1672` | `postgres_a_child_pipeline_lost_outside_a_parallel_is_not_resumed` (`:1675`) |
| An Agent node running a saved Agent is never a recovery frontier | `compiler.rs:985` | `an_agent_node_is_a_recovery_frontier_only_for_a_frozen_saved_pipeline` |
| Tool path and nested fan-out stay refused, typed and readable | `composition.go:496`, `pipeline.rs:2057`, `reject_nested_parallel` | Main composition tests (unit + real PG + start), `a_parallel_pipeline_tool_of_an_agent_node_is_refused_typed_before_any_model_call` |
| An unproven or unfinished earlier visit is refused by the probe, as by `load` | `application_activation.rs:132` | `stamp_matches_save_and_admits_matches_load` |
| A restore never raises a card it already holds a decision for | `runner.rs:425` | `a_restore_re_raises_only_the_cards_without_a_decision` |

## Security

| Category (`rules/security.md`) | Applies | How it was checked |
|---|---|---|
| Identity / authorization | yes | No new route or RPC. Decisions still arrive only through the 2a-2 claim-fenced ledger. Recovery stays authorized by Main's checkpoint authorizer over the evidence digest; the digest now also binds the child checkpoint (`elitea.pipeline.recovery.v2`), so a recovery cannot be authorized for one child frontier and run another. |
| Recovery admission (fail closed) | yes | An Agent node is a frontier only for an attachment Main froze as a saved pipeline (the Main-built tool snapshot, never request text the Worker trusts alone) **and** a child parked in a frozen Parallel occurrence; anything else is refused (`invalid_configuration`). The negative PG test proves the guard decides (fails with it off). |
| Writer fencing | yes | Child appends go to the same claim-fenced store through the parent's authority; the turn authority's stamp and the store's expected-parent compare are unchanged. |
| Input bounds and parsing | yes | `ReadPipeline` still runs only after `pipelinelimits.Check`; `branchNodes` reads scalar `node` values only. |
| Injection, egress, secrets, logs | no new SQL, outbound call, credential or log field | — |
| Dependencies | none changed | — |

`/security-review`: no findings (delta `30cf7b61b..cd8b5fd58`, reviewed in this session without subagents). Checked:
- recovery admission takes the attachment kind from the Main-built frozen snapshot only, requires the child's frozen
  Parallel occurrence, and binds that child checkpoint into the evidence Main authorizes;
- the child authority fails closed on an unfinished or unproven earlier visit (`admits` errors propagate) and appends
  only through the parent's fenced authority;
- the projector's new allowance matches only Agent-node calls on the parked nested path, from the Worker's own graph
  events;
- Main's change relaxes an authoring bound only for the graph path the Worker now runs, with the tool path and nested
  fan-out still refused;
- no new SQL, outbound call, credential, log field or dependency; the diff was secret-scanned before each commit.
- delta `cd8b5fd58..73627a9f7`: the turn authority now hides an earlier turn's child rows from a fresh turn (it only
  narrows what a turn can read, as `TurnCheckpointer` already did) and a Web test with a captured, fake-data frame
  fixture; no findings.

## Recovery rows (replatform-delivery-gate §2)

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Worker × child Parallel park (cards raised, run parks) | R | `parallel_child_authority.rs:120`, `events.rs:5407` | child park/wake PG test |
| Main × decision while the child is parked → wake (new execution) | R | 2a-2 park/wake + `application.rs:763` | child park/wake PG test |
| Worker × lost during the wake, mid-model in a child branch | I → R | WakePark re-run of the wake; branch model checkpoint (`session.rs:1672`); restore re-raise rule (`runner.rs:425`) | lost-wake-in-child PG test (decision applied once, one model step, the answered card never raised again, `b` and `c` re-raised and parked) |
| Worker × lost inside a child outside a Parallel | F (unchanged) | `session.rs:1672` | negative PG test |

## Browser evidence

Stack `fanpar` (own standalone stack, project 118 with project context OFF), production profile, verify-profile 50/0
after every redeploy; migrations at head (fanpar's ledger renumbered with `handoffs/point5-wave1-20261008/2a2/fanpar-ledger-0198.sql`,
revert inside). Images: Main/Web `2a2-cd8b5fd58` (Main binary has the new refusal text), Worker `2a2-cd8b5fd58` then
`2a2-73627a9f7` (binary has `elitea.pipeline.recovery.v2`; the turn-isolation commit adds no string literal), supervisor
`2a2-40178539e`, code runners rebuilt from this head. Fixtures: UI-created pipelines "FP outer" (Agent node → "FP inner
parallel") and "FP inner parallel" (Parallel of two Agent nodes running saved pipeline "FP ask"), chat 20. Full notes:
`handoffs/point5-wave1-20261008/2a2/evidence/browser-cases.md` ("FU-249").

- Two cards ("FP inner parallel ▸ FP ask": bell, lights); the run parks; reload restores both from the ledger.
- Bell answered → a wake (new execution) resumes only bell; lights re-parks and stays visible live; answering lights
  joins; "Pipeline completed."; reload keeps it.
- Worker killed at the wake's model call inside the child (`midwake-kill.sh`): the reclaim's checkpoint recovery resumes
  lights from its checkpoint (one model call), joins; both cards CONSUMED, no card raised again.
- Second and third turns in the same conversation run (found and fixed on the way, design point 6).
- In one run the bell branch's resumed model call got an upstream 500 after 30 s and joined as a failed entry
  (on_error continue); in that run the live view lost the still-open card until a reload (follow-up below).
- Refusals (tool path, nested fan-out) are proven by Main unit, real-PG and start tests on this branch; their browser
  check was done on 2a-2 images with the earlier text, not repeated on these images.

## Follow-ups

- Map inside a saved child pipeline (the Map half of FU-249) stays refused typed.
- Web: when a branch FAILS during a wake of a nested park, the live view loses the still-open card until a reload
  (the Worker publishes it; a replay of the success path keeps it — `chatStreamNestedWake.test.ts`).
- Browser check of the two refusals with the new text on images from this branch.
- Recovery of a child parked deeper than one Agent node below the root (a grandchild's Parallel) is refused (F). Superseded by the post-merge fix PR (`fanout-postmerge-review-20261029.md`): two levels down is R; three levels down is refused at save and assembly.
