# Inline LLM nodes as fixed Parallel branches (Point 5, FU-247)

Branch `feat/graph-fanout-inline-llm-branches`, stacked on `feat/graph-fanout-parallel-admitted` (Wave 2a-2,
[mapping](fanout-parallel-admitted-20261027.md)). Contract: `libs/proto/contracts/fanout-interrupt-decisions-v1.md`
v1.1 (unchanged: an inline branch raises and resumes cards exactly like an Agent branch).

## Business behaviour

Taken from the current platform (EliteaUI pipelines + elitea-sdk parallel execution), kept as observable behaviour:

- A `parallel` branch may name an inline `llm` node as well as an Agent node. The node runs alone as the branch: its
  own system instruction, task and chat history mapping, its own tool selection, and its outputs join the Parallel
  output like an Agent branch's.
- The branch's `ask_user` question and sensitive-tool approval raise their own cards beside the other branches'
  cards; each is answered on its own, in any order and tab, and only that branch continues.
- Any other node kind as a branch (state modifier, printer, toolkit, Code …) is refused at save, start and assembly,
  naming the node and its kind, with the way out (put it in a saved pipeline and run that from an Agent node).

Deliberately not ported: the current platform re-reading parent state when a branch resumes (an inline branch's
mapped inputs are frozen once, when the Parallel activates), and "lost, run again from scratch" after a Worker loss.

## Design

`validate_parallel_ownership` admits `Application` (Agent) and `Llm` owned nodes and refuses the rest with
`FanoutChildRefusal::Kind` (`src/agents/graph/parallel_compiler.rs:382`). An `OwnedBranch::Llm`
(`parallel_compiler.rs:437`) is compiled by `compile_llm_branch` (`:477`) into a one-node child graph that runs the
owned `LlmNode` in parallel-input mode (`LlmNode::with_parallel_inputs`, `src/agents/graph/llm.rs:1100`), with a model
factory obtained from `for_fanout_branch` (`parallel_compiler.rs:149`, `src/agents/pipeline.rs:1851`); without one
the compile is refused (`parallel_compiler.rs:260`). At activation the branch's input mapping is evaluated once
against the parent state and frozen into the private key `__elitea_parallel_llm_inputs_v1`
(`freeze_parallel_input`, `llm.rs:368`); every run of the branch, first or resumed, reads only that frozen map
(`frozen_execution_input`, `llm.rs:386`). The branch runs in its own checkpoint family: its path is registered with
the session's scope chain (`src/agents/session.rs:1287`) and counted against the composition's checkpoint-path bound
(`src/agents/pipeline/composition.rs:317`). Everything after that (receipts, cards, ledger, park, wake, recovery
frontier) is the Wave 2a-2 machinery, unchanged.

Main admits `llm` branches at save and start (`services/elitea-main/internal/domain/pipelinelimits/limits.go`);
the Web editor offers LLM nodes as branches and names any other kind
(`apps/elitea-web/src/features/pipelines/lib/graphParallelAdmission.helpers.ts`).

## Changed paths

- Worker: `src/agents/graph/{parallel_compiler.rs, llm.rs, compiler.rs}`, `src/agents/pipeline.rs`,
  `src/agents/pipeline/composition.rs`, `src/agents/session.rs`, `src/execution/native_agent_lifecycle.rs`; tests
  `parallel_compiler_tests.rs`, `parallel_cards_tests.rs`, `llm_tests.rs`, `pipeline_fanout_effect_pg_tests.rs`.
- Main: `internal/domain/pipelinelimits/limits.go`, `internal/infra/storage/runtime_saved_code_input_policy.go`;
  tests `node_types_test.go`, `http_action_snapshot_test.go`, `runtime_saved_code_input_policy_test.go`.
- Web: `features/pipelines/lib/{graphParallelAdmission.helpers.ts, graphParallel.helpers.ts,
  flow-editor/constants/runtimeContract.constants.ts}`, `ui/settings/parallel/ParallelBranches.tsx`, `en.json`
  (reworded branch strings; the 17 translations of the two reworded keys are dropped so they fall back to English
  until retranslated); tests beside each.
- Docs: this file, the README entry, `docs/recovery-guarantees.md` (Worker × P13 inline row; shifted test lines).

## Tests

Final runs on the branch tree (logs `handoffs/point5-wave1-20261008/2a2/logs/*-fu247*.log`), real PostgreSQL 18
(`ELITEA_TEST_DATABASE_URL`), production profile code paths:

| Suite | Result |
|---|---|
| Worker `cargo test --offline --locked --all-targets --all-features` | 2794 passed, 0 failed, 103 ignored (pre-existing live/opt-in tests) |
| Main `go test -race ./...` | 257 packages ok, 0 failed |
| Web `vitest run` | 19401 passed, 6 failed — all 6 pre-existing and identical on a clean `origin/main` worktree (themePreference needs Node ≥ 26; 4 WorkspaceComposer steering; 1 workspacePages); 2 more failed only because the run read a half-edited file and pass on rerun (76/76) |

New or changed for FU-247: compile limits and topology (16/17, a compile without the fan-out model factory refused),
the frozen-input bound (limit and limit+1), the private-key reservation (Worker, Main, Web), real-PG live
ask/approve/reject with per-branch system and tool asserted on every model request, and three two-process crash tests
through the production recovery path (mid-model R, read-only tool I, effectful tool may-have-started).

## Performance

| Mechanism | Path | Proving test | Measured |
|---|---|---|---|
| Branch count unchanged: 16 branches compile (any mix of Agent and LLM), 17 are refused | `parallel_compiler.rs` | `inline_llm_branches_are_owned_up_to_the_member_bound` | 16 / 17 |
| Inputs evaluated once per activation, never per resume (no parent re-read on a wake) | `llm.rs:368,386` | `postgres_inline_llm_branches_pause_with_their_own_cards_and_resume_live` (resumed requests carry the frozen system and task) | 1 evaluation |
| Frozen input bounded at 512 KiB (each text is 64 KiB, JSON escaping can grow the map) | `llm.rs:374` | `a_frozen_parallel_llm_input_is_bounded_at_its_limit_and_limit_plus_one` | exact |
| Checkpoint paths of inline branches counted against the composition bound | `composition.rs:317` | composition bound tests (unchanged bound) | unchanged |
| Parent writes per activation visit unchanged (an inline branch is a child like any other) | `parallel.rs` | `each_activation_visit_appends_at_most_two_parent_rows` | 2 |

## Durability

| Mechanism | Path | Proving test |
|---|---|---|
| The branch's inputs are durable with the branch (frozen key in the child's checkpoint), so a resume on another Worker sees the same system, task and tools | `llm.rs:368,386` | `postgres_inline_llm_branch_caught_mid_model_resumes_from_its_checkpoint` (production recovery path, two processes) |
| Own checkpoint family per inline branch; stable child identity across claims | `session.rs:1287`, `composition.rs:317` | the three inline crash tests below |
| Cards, decisions and ACKs: the 2a-2 ledger, unchanged | Main `execution_interrupts.go` | 2a-2 Main PG matrix |

## Resilience

| Mechanism | Path | Proving test |
|---|---|---|
| A branch of any other kind is refused typed and readable at save (Web, Main), start (Main) and assembly (Worker) | `parallel_compiler.rs:382`, `limits.go`, `graphParallelAdmission.helpers.ts` | `TestCheckRefusesAParallelBranchThatIsNotAnAgentOrLLMNode`, `an_unadmitted_parallel_branch_kind_ends_as_the_registered_deployment_message`, Web "names a branch node kind no branch can run…" |
| A compile without the fan-out model factory is refused, never run with a wrong factory | `parallel_compiler.rs:260` | `an_inline_llm_branch_without_a_fanout_model_factory_is_refused` |
| Effectful tool in flight at a crash: the branch ends `graph.fanout.may_have_started`, never repeated | 2a-2 marker | `postgres_inline_llm_branch_effect_in_flight_may_have_started` |
| Read-only tool in flight: runs again | `ReadOnlyRecordingToolset` | `postgres_inline_llm_branch_read_only_tool_in_flight_runs_again` |

## Security

| Category (`rules/security.md`) | Applies | How it was checked |
|---|---|---|
| Identity / object authorization | no new route or RPC | Cards and decisions go through the 2a-2 ledger and its authorization matrix unchanged. |
| Input bounds and strict schemas | yes | Branch kind allow-list in all three layers; frozen input bounded (limit test); text fields keep their 64 KiB bounds. |
| Private state channels | yes | `__elitea_parallel_llm_inputs_v1` is reserved: a pipeline cannot declare it as a state variable (Worker `compiler.rs:2933`, test in `parallel_compiler_tests.rs`; Main saved-Code policy, `TestSavedCodeReservedStateKeyHoldsTheFixedParallelPrivateChannels`; Web `ReservedStateKeys`, parity test and `graphParallelAdmission.helpers.test.ts`). |
| Tool scope | yes | An inline branch's model factory is a clone of the pipeline's own (`for_fanout_branch`, `src/agents/pipeline.rs:1851`): the same frozen toolsets, sensitive-tool policies, delegated-authorization catalog and claim-scoped context; nothing is widened. Its tools are the node's own `tool_names` selection (`StrictNodeToolset`); the PG test asserts each request offers the branch's selected tool. |
| Injection, egress | no new SQL, no new outbound call | — |
| Secrets and logs | no new logging of values | Spans log counts only (`selected_toolkit_count`, `selected_tool_count`). |
| Supply chain | no dependency change | no dependency file changed. `cargo deny`: 20 advisories, the same set as 2a-2 (pre-existing); `govulncheck`: GO-2026-6809 (Go toolchain, pre-existing); `npm audit`: 5 moderate in the Vitest dev toolchain (`vitest`, `@vitest/*`), newly published advisories, pre-existing dependency versions, not shipped |

`/security-review` (branch diff against `feat/graph-fanout-parallel-admitted`, done in this session): no findings. Checked: the branch-kind allow-list fails closed in all three layers; ownership rules are kind-agnostic (no entry, transition, second owner or parent route into an owned LLM node, `parallel_compiler.rs:382`); no tool-scope widening (above); frozen input bounded and its key reserved; no new route, query, outbound call or logged value.

## Recovery rows (replatform-delivery-gate §2)

Added to `docs/recovery-guarantees.md` (Worker × P13, inline LLM branch):

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Worker × inline LLM branch, mid-model (first run or wake) | R | own checkpoint family, frozen inputs `llm.rs:368,386` | `postgres_inline_llm_branch_caught_mid_model_resumes_from_its_checkpoint`; browser chat 28 turn 3 (killed 30 ms after a resumed branch's model request) |
| Worker × inline LLM branch, read-only tool in flight | I | `ReadOnlyRecordingToolset` | `postgres_inline_llm_branch_read_only_tool_in_flight_runs_again` |
| Worker × inline LLM branch, effectful tool in flight | F typed (`graph.fanout.may_have_started`) | 2a-2 marker | `postgres_inline_llm_branch_effect_in_flight_may_have_started` |
| HITL pause and decision (inline branch) | R | 2a-2 ledger | `postgres_inline_llm_branches_pause_with_their_own_cards_and_resume_live`; browser chat 28 |
| Main, Web × inline branch | R | unchanged from 2a-2 | browser chat 28 (two tabs, reloads) |

## Browser evidence (real Chrome, stack `fanpar`, production profile)

Images `fu247-a048969be`: Worker `sha256:153b305a…1956`, Main `sha256:896bae32…c0`, Web `sha256:71a09de6…ce`.
Binary check: the Worker binary contains `__elitea_parallel_llm_inputs_v1`, the Main binary contains
`or an LLM node. Put the` (both only on this branch); built on a base containing #1160. Pipeline "FP inline mix"
(app 15): inline ask (ask_user), inline approve and reject (sensitive `reverse`), saved "FP llm reverse" branch.
Full log: `handoffs/point5-wave1-20261008/2a2/evidence/browser-cases.md` (cases I1–I3).

| Case | Result |
|---|---|
| I1 "Plan my kite." | the model judged `reverse` irrelevant to the task and did not call it (inline and saved alike; it named the tool, so the tool was offered). The PG test now asserts each request's own system and tool. |
| I2 four branches pause at once | approve in tab A; tab B reloaded shows the 3 open cards; teal, block, authorize; joined: "You prefer teal.", `EVORPPA-ENILNI-PF`, declined, `ESREVER-MLL-PF`; reload keeps it. Ledger: 4 CONSUMED. |
| I3 Worker killed mid-wake inside the resumed inline ask branch | reclaim after the lease, "Fan-out child resumed from checkpoint", job SUCCEEDED; the answer applied once, no new card; the two remaining cards decided in reloaded tab B; joined "You prefer violet." …; reload keeps it. |

Fixtures, stated honestly: app 15 created in the UI on the real-model dump; its YAML is
`handoffs/point5-wave1-20261008/2a2/fixtures-fu247-inline.yaml`; `ask_user` internal tool set by SQL as in 2a-2
(`2a2/fixtures.sql`). No response mocks.

## Follow-ups

- Main: a fan-out response's last wake row stays ADMITTED after its run settles (lazy retirement; cosmetic, from 2a-2).
- The 17 translations of the two reworded branch strings.
- FU-248, FU-249 (separate branches). FU-246 landed on `feat/graph-fanout-agent-branch-pause`
  ([mapping](fanout-agent-branch-pause-20261028.md)).
