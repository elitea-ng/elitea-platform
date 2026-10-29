# Post-merge review fixes for #1508, #1511 and #1524 (2026-10-29)

Branch `fix/fanout-postmerge-review` off `main` 5e9c6dfa8. Review: `handoffs/point5-wave1-20261008/REVIEW-POSTMERGE-1508-1511-1524.md`.
One PR for PM-1 to PM-6 and PM-10 (PM-9's browser half and the editor test-chat refusal landed in #1528). Follow-ups
PM-7, PM-8, PM-11, PM-12 and the named gaps are FU-255 to FU-261 in `FOLLOWUPS-20261012.md`.

## Business behaviour

- **PM-1.** A saved Agent branch of a fixed Parallel pauses with its own card. An approved call that is in flight when
  the Worker dies is never repeated: the branch ends `graph.fanout.may_have_started` and the other branches keep their
  results.
- **PM-2.** A Parallel two Agent-node levels down (root → Agent node → saved pipeline → Agent node → saved pipeline
  holding the Parallel) parks, wakes and, when the Worker is lost mid-wake, resumes only the answered branch.
  - Three levels down is refused at save (Main) and at assembly (Worker), typed and readable.
  - A stored frontier that recovery cannot safely continue ends with the new registered terminal
    `RECOVERY_FRONTIER_UNSUPPORTED`: "This run stopped at a point where it cannot be safely continued. Nothing was
    repeated. Start it again, and share the support reference with your administrator if it happens again."
- **PM-3.** An inline LLM branch whose frozen input passes 512 KiB fails with `graph.parallel.resource_exhausted`
  naming the branch. The run's terminal is the registered `RESOURCE_EXHAUSTED`, not the generic failure.
- **PM-4.** Main's save and start check counts inline LLM branches against the 128 checkpoint paths the way the Worker
  does.
- **PM-5.** A root-level inline LLM branch streams its model text and tool calls under its member tier, like an Agent
  branch and like the current platform's pipeline LLM nodes. It never streams as the run's own text.
- **PM-6.** An Agent member's forwarded confirmation is durable in the root session before its card can be decided.
  This is proven; no code change was needed.
- **PM-10.** The three Parallel branch strings are translated in all 18 locales, and the #1508 mapping's citations are
  re-anchored.
- Not ported from the current platform (it has no Parallel node): nothing.

## What was found on the way

- **PM-2 did not even run before recovery.** A saved child's runtimes key their composition definitions relative to
  that child, but `child_fanout` and `reject_nested_parallel` matched them by the absolute scope path. As a result:
  - a Parallel two levels down could not compile;
  - inside a nested child, a branch whose saved pipeline holds Code or another Parallel was not refused by the Worker
    (Main still refused it).
- **Depth 3 was admitted by Main but refused opaquely by the Worker.** The Postgres checkpointer admits at most three
  path components, and an inline branch needs a path one level below its pipeline. The run failed "the claim-bound
  pipeline checkpoint is unavailable".
- **Inline streaming.** Giving the inline branch the member-scoped sender alone streamed its text as root events. The
  projector needed an explicit inline-member route.
- **Test timing.** The real-PG "lose mid model call" step dropped the run after a fixed 800 ms, which failed under load.
  It now waits for the model request itself.

## Changed paths

- **PM-1:** `src/agents/pipeline_fanout_effect_pg_tests.rs` (`approved_effect` crash mode).
- **PM-2:**
  - `src/agents/session.rs`: `walk_child_frontier` `:1634`, `inspect_checkpoint` `:1690`, causes `:1810`,
    `is_recovery_frontier_cause` `:1821`, `FrozenRecoveryLevel`;
  - `src/agents/pipeline.rs`: `RootRecoveryLevel` `:1969`;
  - `src/agents/pipeline/composition.rs`: `FrozenRecoveryLevel` for the frozen composition `:349`, depth refusal of
    inline branches;
  - `src/agents/graph/compiler.rs`: `has_node` `:1014`;
  - `src/agents/graph/parallel_compiler.rs`: relative composition keys in `child_fanout` and `reject_nested_parallel`;
  - `libs/rust/agent-runtime/src/graph/application_activation.rs`: `ACTIVATION_METADATA_KEY` is now public;
  - Main `internal/domain/applicationcomposition/composition.go:567`.
- **Registered terminal:**
  - `libs/proto/elitea/runtime/v1/errors.proto` (code 37), with the regenerated Go and Python `errors` files only;
  - Worker `src/protocol/output.rs` (kind, text, policy), `src/protocol/control.rs`,
    `src/execution/toolkit_delivery_processor.rs`, `src/execution/native_agent_lifecycle.rs` (assembly mapping and
    log of the cause);
  - Main `internal/transport/runtimegrpc/output/server.go`;
  - `testdata/proto/runtime/v1/model_failure_policies.json`, `conformance/governed/testdata/golden-vectors.sha256`.
- **PM-3:** `src/agents/graph/llm.rs` (`freeze_parallel_input`), `src/agents/graph/parallel.rs`
  (`is_parallel_resource_exhausted`), `src/agents/runtime.rs` (`parallel_resource_exhausted`),
  `src/execution/native_agent_lifecycle.rs:1594` (`stream_failure`).
- **PM-4:** Main `composition.go` (`ReadPipeline` returns `LLMBranches`; `checkpointPaths`), `repos/application_composition.go`,
  `repos/applications.go`, Worker `composition.rs` test.
- **PM-5:**
  - `src/agents/graph/node_events_parallel.rs:38` (`for_inline_llm_branch`, inline-member stamp);
  - `src/agents/graph/parallel_compiler.rs:248,681` (`InlineBranchEvents`, `compile_llm_branch`);
  - `src/agents/events.rs:1532,6440` (`project_inline_member`, `inline_member_tier`, reset on a member's status).
- **PM-6:** `src/agents/pipeline_fanout_effect_pg_tests.rs` (`run_until_first_card` and its test).
- **PM-10:** 18 locale files; `fanout-inline-llm-branches-20261028.md`.

## Tests

- **Worker:**
  - real PostgreSQL, `fanout_effect_pg_tests`: 34 passed, 0 failed, 0 skipped, run with load average above 30;
  - full suite `--all-targets --all-features`: see the PR description.
  - New tests:
    - PM-1: `postgres_an_approved_agent_member_call_in_flight_at_a_crash_ends_typed` (two-process).
    - PM-2:
      - `postgres_a_wake_lost_two_agent_levels_down_resumes_the_answered_branch`;
      - `postgres_two_levels_down_a_grandchild_row_without_its_activation_stamp_is_refused`;
      - `postgres_two_levels_down_a_middle_frontier_that_is_not_one_agent_node_is_refused`;
      - `postgres_two_levels_down_a_middle_pipeline_whose_frozen_definition_differs_is_refused`;
      - `postgres_a_grandchild_lost_outside_a_parallel_is_not_resumed` and
        `postgres_a_child_pipeline_lost_outside_a_parallel_is_not_resumed` (`child_not_parked`);
      - `postgres_a_takeover_between_inspect_and_assembly_fences_the_older_claim`;
      - `postgres_a_parallel_three_agent_levels_down_is_refused_typed`;
      - `a_branch_with_code_is_refused_inside_a_nested_child_too`;
      - `a_recovery_walk_refusal_ends_as_the_registered_frontier_terminal`.
    - PM-3: `a_frozen_parallel_llm_input_is_bounded_at_its_limit_and_limit_plus_one` (code and branch at limit+1),
      `a_parallel_resource_refusal_is_recognised_through_its_wrapping`.
    - PM-4: `inline_llm_branches_count_against_the_checkpoint_paths` (128 fits, 129 refused).
    - PM-5: `postgres_inline_llm_branches_stream_their_text_under_their_member_tier`.
    - PM-6: `postgres_an_agent_member_lost_at_its_card_frame_is_decided_from_the_durable_confirmation`. It is
      decisive: with the confirmation removed the same decision is refused.
- **Main:**
  - new: `TestCheckpointPathsCountInlineLLMBranches`, `TestReadPipelineCountsOnlyParallelOwnedLLMNodes`,
    `TestInlineLLMBranchesInTheDeepestPipelineAreRefused`, `TestEveryRuntimeErrorCodeHasRegisteredTextOrIsNotATerminal`;
  - `go test -race ./...`: see the PR description.
- **Web:** the i18n tests (5 files, 25 tests), and the `ApplicationAnswer` reload view of `RECOVERY_FRONTIER_UNSUPPORTED`.
- **Conformance:** the golden manifest passes with the new policy.

## Performance

- **PM-2:** the walk reads at most `MAX_RECOVERY_LEVELS` (3) child rows per frontier Agent node, only on a recovery,
  and only by primary-key load. Nothing changes on the normal run path.
- **PM-4:** one more integer per saved pipeline version, read in the same YAML pass.
- **PM-5:**
  - one child projector per streaming inline member, at most 16 (the branch bound), dropped when its member pauses or
    ends;
  - frames are the ones an LLM node already produces, with the member tier overlaid in place.
- **PM-3:** no new work on the success path.

## Durability

- **PM-2:**
  - recovery two levels down resumes the answered branch from its model checkpoint, once (R), and the other cards
    re-park;
  - the evidence binds the root row and the first-level child row.
- **PM-6:** the confirmation and the card are events of one root stream, appended in order before each is yielded, so
  the card is never decidable before its resume proof is durable.
- **PM-1:** an approved call in flight is classified C/F (`may_have_started`), never repeated; the tool count stays 1.

## Resilience

- Every walk refusal is typed and has a distinct cause:
  - `pipeline.recovery.child_missing`;
  - `child_unstamped`;
  - `child_frontier_unsupported`;
  - `child_definition_mismatch`;
  - `child_not_parked`.
- The cause is logged (`pipeline_recovery_frontier_refused`). The user sees the registered, data-free text with the
  support reference.
- A takeover between inspect and assembly fences the older claim: its assembly is refused and it writes nothing.
- Depth 3 is refused before anything runs. PM-3's bound and PM-4's count are refused at the limit+1, not later.

## Security

**Option A trust argument (PM-2).**
- **What Main authorizes.** The checkpoint evidence digest covers the root row and the first-level child row of each
  frontier Agent node. The pre-assembly inspect cannot read deeper: the pre-authorization path allowlist admits only
  the root's own Agent-node thread paths, and it is not widened.
- **How deeper rows are protected.**
  - They are Worker-written under the claim's writer fencing.
  - They stay unchanged between inspect and assembly under the exclusive lease. That is proven by the takeover test:
    a newer claim fences the older one out, and the older one writes nothing.
  - The assembly-time walk validates them against the Main-frozen definitions: a single recovery-supported Agent node
    per level, an alias Main froze, the node present in the frozen definition, the activation stamp on every deeper
    row, and a Parallel occurrence at the leaf. Any miss refuses before any effect, model call or ledger write.
- **Residual risk.** Rows below the first level are not inside the digest Main signed off. Their integrity rests on the
  Worker's writer fencing, the database's access control and the frozen-definition checks. Someone able to write the
  checkpoint tables directly with a valid fence could alter a deeper row's state within what the frozen definition
  admits. The same holds today for every row the digest does cover after authorization, so A adds no new writer.

Other changes:
- **PM-5:** the inline-member marker is stamped only by the Parallel node's own branch sender (an explicit inline-LLM
  scope). The projector validates it strictly (an `fo1_` member call, an ordinal within the bound, a valid graph id,
  exactly three keys). A forged or malformed marker is an invalid state, never projected.
- **Registered terminal:** the text is data-free and registered on both sides. The internal cause never reaches the
  user.
- No new route, RPC, SQL built from strings, outbound call, credential or log field carrying user data. The PM-10
  locale strings are static.
- `rules/security.md`:
  - authorization unchanged (fail closed, in the same paths);
  - no identity, egress or parser surface;
  - the YAML read in Main is the existing bounded `ReadPipeline` pass;
  - secrets scanned before each commit.

## Recovery rows (replatform-delivery-gate §2)

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Worker × lost mid-wake inside a Parallel two Agent-node levels down | R | `session.rs:1634` (walk), `parallel_compiler.rs` (relative keys) | `postgres_a_wake_lost_two_agent_levels_down_resumes_the_answered_branch` |
| Worker × a recovery frontier the walk refuses (any level) | F (typed, readable, nothing repeated) | `session.rs:1634`, `native_agent_lifecycle.rs` (terminal) | the five depth-2 and depth-1 refusal tests |
| Main × save/start of a Parallel three Agent-node levels down | F at save (no run) | `composition.go:567`, Worker `composition.rs` | `TestInlineLLMBranchesInTheDeepestPipelineAreRefused`, `postgres_a_parallel_three_agent_levels_down_is_refused_typed` |
| Worker × approved Agent-member call in flight at a crash | C → F (`may_have_started`, never repeated) | dispatch journal, member path | `postgres_an_approved_agent_member_call_in_flight_at_a_crash_ends_typed` |
| Worker × lost between a member's card frame and its decision | R | one ordered root stream (PM-6) | `postgres_an_agent_member_lost_at_its_card_frame_is_decided_from_the_durable_confirmation` |

## Browser evidence (fanpar, main-based images)

To be filled in once the main-based images are deployed. The deploy also verifies the 0202 ledger renumber.

## Follow-ups

- FU-255 to FU-259 (PM-7, PM-8, PM-11, PM-12, Map in a child), FU-260 (nested inline-branch streaming), FU-261 (replay
  retention 23514).
