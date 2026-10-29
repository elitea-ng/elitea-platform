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
  - After the run ends, and after a reload, every member and sub-agent tier is still there with its own steps. Each
    step opens to its text, which is fetched on open as for every restored step (#951).
  - This also fixes a pre-existing gap from #1032. The transcript's reload source read only `tool_call` rows, so
    every chat's saved sub-agent tier lost its inner thinking on reload, and a Parallel member that called no tool
    lost its tier altogether.
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
- **The walk's deeper rows were checked less strictly than claimed (REVIEW-1533 R1).** Below the first level the walk
  accepted a leaf on any single pending node that carried the occurrence key, and checked the activation stamp for
  presence only. It now requires the leaf to be parked on an unowned Parallel node of the frozen leaf definition, and
  every deeper row's stamp to name its own thread with the frozen parent Agent node's digest. Two tamper tests prove
  it and fail without the checks.
- **Depth 3 was admitted by Main but refused opaquely by the Worker.** The Postgres checkpointer admits at most three
  path components, and an inline branch needs a path one level below its pipeline. The run failed "the claim-bound
  pipeline checkpoint is unavailable".
- **Inline streaming.** Giving the inline branch the member-scoped sender alone streamed its text as root events. The
  projector needed an explicit inline-member route.
- **Test timing.** The real-PG "lose mid model call" step dropped the run after a fixed 800 ms, which failed under load.
  It now waits for the model request itself.
- **A live decision can land after the activation parked.** That is correct, because Main then wakes the parked run.
  But the tests that decide a card while the run streams assumed the decision always arrives first, and flaked under
  load:
  - FU-246's `postgres_fanout_agent_branch_asks_and_resumes_with_the_answer` (#1511) failed in CI on #1528 with 3
    model requests where it expected 4;
  - locally, PM-1's crash child outlived the approval, and PM-6's third branch failed before its drop.

  The fix: the member that could finish first now answers only once the cards are decided
  (`TestModelGatewayOutcome::ResponseAfter`), and PM-6's dropped first process keeps that call in flight (`Pending`).
  The module then passed 5 of 5 runs at 16 test threads (34 passed each, 0 skipped); before, 2 of 3 runs failed.

## Changed paths

- **PM-1:** `src/agents/pipeline_fanout_effect_pg_tests.rs` (`approved_effect` crash mode).
- **PM-2:**
  - `src/agents/session.rs`: `walk_child_frontier` `:1637` (the deeper rows' stamp against the frozen Agent node
    digest, the leaf's unowned Parallel node), `inspect_checkpoint` `:1711`, causes `:1831`,
    `is_recovery_frontier_cause` `:1842`, `FrozenRecoveryLevel`;
  - `src/agents/pipeline.rs`: `RootRecoveryLevel` `:1969`;
  - `src/agents/pipeline/composition.rs`: `FrozenRecoveryLevel` for the frozen composition `:349`, depth refusal of
    inline branches;
  - `src/agents/graph/compiler.rs`: `has_node` `:1017`, `is_unowned_parallel` `:1023`, `application_config_digest`
    `:1034`;
  - `src/agents/graph/parallel_compiler.rs`: relative composition keys in `child_fanout` `:147` and
    `reject_nested_parallel` `:439`;
  - `libs/rust/agent-runtime/src/graph/application_activation.rs`: `ACTIVATION_METADATA_KEY` is now public;
    `stamped_definition_digest` `:39` reads a row's stamp for exactly its own thread;
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
  `src/execution/native_agent_lifecycle.rs:1594` (`stream_failure`); `llm.rs:368`, `parallel.rs:2269`,
  `runtime.rs:1141`.
- **PM-4:** Main `composition.go` (`ReadPipeline` returns `LLMBranches`; `checkpointPaths`), `repos/application_composition.go`,
  `repos/applications.go`, Worker `composition.rs` test.
- **PM-5:**
  - `src/agents/graph/node_events_parallel.rs:38` (`for_inline_llm_branch`, inline-member stamp);
  - `src/agents/graph/parallel_compiler.rs:248,681` (`InlineBranchEvents`, `compile_llm_branch`);
  - `src/agents/events.rs:1546,6454` (`project_inline_member`, `inline_member_tier`, reset on a member's status).
  - Web, the reload view:
    - `apps/elitea-web/src/entities/message/lib/traceSteps.ts:118,135,141` (`isMemberThinkingRow`,
      `memberThinkingSteps`, `withMemberThinkingSteps`; a restored thinking step keeps its owner call id and path);
    - `apps/elitea-web/src/entities/message/lib/toolActions.ts:277` (`restoredOwner`);
    - `apps/elitea-web/src/entities/message/lib/wire.ts:86`;
    - `apps/elitea-web/src/features/chat-messages/lib/convertMessagesToChatHistory.ts:313`.
- **PM-6:** `src/agents/pipeline_fanout_effect_pg_tests.rs` (`run_until_first_card` `:603`, its test and its
  negative control).
- **Tests only:** `src/transport/openai_compatible_facade.rs` (`#[cfg(test)]` replies `ResponseAfter` and
  `ResponseForRequestAfter`, answered once a test's gate opens).
- **PM-10:** 18 locale files; `fanout-inline-llm-branches-20261028.md`.

## Tests

- **Worker:**
  - real PostgreSQL, `fanout_effect_pg_tests`: 39 passed, 0 failed, 0 skipped per run, 5 of 5 runs at 16 test threads
    (after the REVIEW-1533 fixes);
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
      - `postgres_two_levels_down_a_leaf_parked_on_a_node_other_than_its_parallel_is_refused`
        (`child_frontier_unsupported`) and `postgres_two_levels_down_a_grandchild_stamped_by_another_definition_is_refused`
        (`child_definition_mismatch`), each refused at assembly with 0 model requests; both fail without the R1 checks;
      - `postgres_a_takeover_between_inspect_and_assembly_fences_the_older_claim`: the older claim is refused
        `AuthorizationFailed` (the lost writer lease) and writes no checkpoint, no session event and no ledger ACK;
      - `postgres_a_parallel_three_agent_levels_down_is_refused_typed`;
      - `a_branch_with_code_is_refused_inside_a_nested_child_too`;
      - `a_recovery_walk_refusal_ends_as_the_registered_frontier_terminal`.
    - PM-3: `a_frozen_parallel_llm_input_is_bounded_at_its_limit_and_limit_plus_one` (code and branch at limit+1),
      `a_parallel_resource_refusal_is_recognised_through_its_wrapping`.
    - PM-4: `inline_llm_branches_count_against_the_checkpoint_paths` (128 fits, 129 refused).
    - PM-5: `postgres_inline_llm_branches_stream_their_text_under_their_member_tier` (streaming, no loss).
      The inline card test is split: `postgres_inline_llm_branches_pause_with_their_own_cards_and_resume_live` is
      deterministically the live path (gated replies keep a branch running until every card is decided), and
      `postgres_inline_llm_branches_park_their_cards_and_resume_on_the_wake` parks undecided and wakes.
    - PM-6: `postgres_an_agent_member_lost_at_its_card_frame_is_decided_from_the_durable_confirmation` and its
      negative control `postgres_an_agent_member_decision_without_its_forwarded_confirmation_is_refused` (the
      confirmation deleted from the root session: `graph.parallel.decision_refused`, the Agent never resumes).
- **Main:**
  - new: `TestCheckpointPathsCountInlineLLMBranches`, `TestReadPipelineCountsOnlyParallelOwnedLLMNodes`,
    `TestInlineLLMBranchesInTheDeepestPipelineAreRefused`, `TestEveryRuntimeErrorCodeHasRegisteredTextOrIsNotATerminal`;
  - `go test -race ./...`: see the PR description.
- **Web:**
  - the i18n tests (5 files, 25 tests), and the `ApplicationAnswer` reload view of `RECOVERY_FRONTIER_UNSUPPORTED`;
  - `persistedMemberThinking.test.ts` (5 tests) over fanpar's real light rows for one "FP inline mix" turn
    (`__tests__/parallel-member-trace-rows.json`, hierarchy only, no text). It proves:
    - one tier per member, including inline branches that call no tool;
    - the saved sub-agent tier holds its wrapper and its inner thinking;
    - two concurrent instances of one saved agent stay apart;
    - the coordinator's steps are unchanged, and a turn with no member rows is unchanged.
  - The test is decisive. Without the merge the tier tests fail; without the restored owner fields the two-instance
    test fails.
  - `src/entities/message`, `src/features/chat-messages` and `src/widgets/chat-box`: 126 files, 1374 passed;
    `tsc --noEmit` and oxlint are clean.
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
- **PM-5 reload view:** no new request and no new route.
  - The member thinking rows come from the light trace rows the message page reader already fetches. That reader is
    paged at 500 rows and 200 message groups per request
    (`apps/elitea-web/src/entities/conversation/api/messageTraces.ts`).
  - The conversation-wide `tool_call` listing is unchanged.
  - Measured on fanpar, chat 41: 60 turns, 120 groups, 660 trace rows, a SQL copy of a real turn. Loading the first
    page of 50 groups, before and after:
    - 53 requests both times: messages 1, page trace rows 1, `tool_call` listing 1, step detail 25, feedback 25;
    - identical response bytes: 36,005 B; 236,449 B (250 rows); 119,769 B (120 rows).
  - Client work is one filter over the page's rows per message group.

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
- A takeover between inspect and assembly fences the older claim: its assembly is refused `AuthorizationFailed` (lost
  writer lease) and it writes no checkpoint, session event or ledger ACK.
- Depth 3 is refused before anything runs. PM-3's bound and PM-4's count are refused at the limit+1, not later.

## Security

**Option A trust argument (PM-2).**
- **What Main authorizes.** The checkpoint evidence digest covers the root row and the first-level child row of each
  frontier Agent node. The pre-assembly inspect cannot read deeper: the pre-authorization path allowlist admits only
  the root's own Agent-node thread paths, and it is not widened.
- **How deeper rows are protected.**
  - They are Worker-written under the claim's writer fencing, and every write carries the claim's `writer_lease_epoch`.
  - They are read only at assembly, after `open_with_application_paths` has made this claim the current writer. A
    superseded claim cannot assemble or write: the takeover test refuses it `AuthorizationFailed` with no checkpoint,
    session event or ledger write. The pre-assembly inspect never reads them.
  - The assembly-time walk validates every level against the Main-frozen definitions, failing closed with a distinct
    cause before any effect, model call or ledger write:
    - each intermediate level: one pending node, present in the frozen definition, a recovery-supported Agent node
      running a saved pipeline Main froze;
    - each deeper row: an activation stamp for exactly its own thread whose definition digest is the frozen parent
      Agent node's (`child_definition_mismatch` otherwise);
    - the leaf: parked on an unowned Parallel node of the frozen leaf definition (`child_frontier_unsupported`
      otherwise).
- **Residual risk.** Rows below the first level are not inside the digest Main signed off. Their integrity rests on the
  Worker's writer fencing, the database's access control and the frozen-definition checks above. Someone able to
  write the checkpoint tables directly with a valid fence could alter a deeper row's state (its graph state, not its
  node, its definition or its stamp) within what the frozen definition admits. The same holds today for every row the
  digest does cover after authorization, so A adds no new writer.

Other changes:
- **PM-5:** the inline-member marker is stamped only by the Parallel node's own branch sender (an explicit inline-LLM
  scope). The projector validates it strictly (an `fo1_` member call, an ordinal within the bound, a valid graph id,
  exactly three keys). A forged or malformed marker is an invalid state, never projected.
- **Registered terminal:** the text is data-free and registered on both sides. The internal cause never reaches the
  user.
- **PM-5 reload view:** the rows come from the existing listing, under its existing authorization. They are only
  regrouped client-side.
  - The owner path is bounded by the existing persisted-path limits: 3 tiers, 2048-char names, 512-char call ids.
  - Text is rendered as React text, as before.
- No new route, RPC, SQL built from strings, outbound call, credential or log field carrying user data. The PM-10
  locale strings are static.
- `rules/security.md`:
  - authorization unchanged (fail closed, in the same paths);
  - no identity, egress or parser surface;
  - the YAML read in Main is the existing bounded `ReadPipeline` pass;
  - secrets scanned before each commit.
- **Scanners (gate §4).** This PR changes no dependency manifest (no `Cargo.toml`/`Cargo.lock`, `go.mod`/`go.sum` or
  `package.json`/lock change against `main`), so `cargo deny`, `govulncheck` and `npm audit` report the same
  pre-existing set as `main`; no new findings.

## Recovery rows (replatform-delivery-gate §2)

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Worker × lost mid-wake inside a Parallel two Agent-node levels down | R | `session.rs:1637` (walk), `parallel_compiler.rs:147,439` (relative keys) | `postgres_a_wake_lost_two_agent_levels_down_resumes_the_answered_branch` |
| Worker × a recovery frontier the walk refuses (any level) | F (typed, readable, nothing repeated) | `session.rs:1637`, `native_agent_lifecycle.rs:1602` (terminal), `protocol/output.rs:982` | the seven depth-2 and depth-1 refusal tests, the takeover test |
| Main × save/start of a Parallel three Agent-node levels down | F at save (no run) | `composition.go:567`, Worker `composition.rs` | `TestInlineLLMBranchesInTheDeepestPipelineAreRefused`, `postgres_a_parallel_three_agent_levels_down_is_refused_typed` |
| Worker × approved Agent-member call in flight at a crash | C → F (`may_have_started`, never repeated) | `dispatch_journal.rs:680` (boundary in the member's model scope) | `postgres_an_approved_agent_member_call_in_flight_at_a_crash_ends_typed` (both interleavings of `third` accepted) |
| Worker × lost between a member's card frame and its decision | R | the member's FIFO channel into the root runner, appended before yielded; decision bound at `parallel_compiler.rs:596` | `postgres_an_agent_member_lost_at_its_card_frame_is_decided_from_the_durable_confirmation` and the negative control `postgres_an_agent_member_decision_without_its_forwarded_confirmation_is_refused` |
| Worker × lost at the resumed saved Agent branch's model call (wake) | R (only the killed model call runs again) | child checkpoint per branch, stable member call id | browser PM-1 (chat 40) |
| Worker × lost mid-stream in a resumed inline LLM branch (wake) | R | branch checkpoint + recorded decision | browser PM-5 (chat 28) only; a PG proof of this phase is FU-262 |
| Web × reload or end of a run with Parallel members and sub-agents | R (every tier and step restored from durable trace rows) | `traceSteps.ts:141`, `convertMessagesToChatHistory.ts:313` | `persistedMemberThinking.test.ts`; browser (chat 28) |
| Web × reload of a run whose recovery frontier was refused | F (registered text + support reference) | `protocol/output.rs` (RecoveryFrontierUnsupported), Main policy | `ApplicationAnswer.test.tsx`; browser (chat 20) |

## Browser evidence (fanpar, main-based images)

- **Stack and images.**
  - Main, Worker and supervisor run `pm-778e5beb8` (Worker sha256:64951e88faaa…, Main sha256:1ab3aa889bf1…).
  - Web and gateway run `pm-2d7f7d3e8` (Web sha256:3436d996a180…). The PM-5 reload proofs run Web `pm-07757110d`
    (sha256:9091e9efe164…).
  - `stack.sh binary-check`: the Worker image holds `pipeline.recovery.child_not_parked` and
    `elitea.fanout.inline_member.v1`; the Main image holds `RECOVERY_FRONTIER_UNSUPPORTED`.
  - verify-profile 50/0. The ledger renumber 0198 → 0202 is verified: 198..201 carry main's checksums.
  - Fixtures were created in the UI (pipelines and chats). The one exception is the forced-frontier case below, whose
    stored child frontier was edited in the stack's own database.
- **PM-1, chat 40, "FP agent branch".**
  - The saved Agent branch pauses with its own card next to the pipeline branch's card. Both cards are restored after
    a reload.
  - A second tab was open. The Agent card was answered with the Worker killed at the resumed saved agent's model
    request (wake `dc348c14…`).
  - The redelivery made exactly one model request: the killed call. The Agent node's own model step was re-projected
    from its checkpoint (same run id, stored timestamp), not re-requested, and its call id stayed stable. No card was
    re-raised and the other card re-parked.
  - Ledger: CONSUMED once per card. Answering the second card in the other tab joined both branches. The first tab
    showed the join live; a reload keeps it.
- **PM-5, chat 28, "FP inline mix".**
  - While the run is parked, the inline branches stream live into their own tiers (ask_llm, approve_llm,
    reject_llm) next to the saved pipeline's tier.
  - The ask_llm card was answered with the Worker killed at the resumed branch's model stream (wake `e83eeaa0…`,
    after the first chunk). The redelivery resumed ask_llm from its checkpoint with the decision and made one model
    request. The text streamed into the ask_llm tier, the run joined, and the result is unchanged after a reload.
  - On `pm-2d7f7d3e8`, once the run ended and after a reload, the panel showed only the tiers that hold a tool call.
    The cause was the transcript's `tool_call`-only reload source (since #1032).
  - Fixed in Web `pm-07757110d` (sha256:9091e9efe164…). Image check: the new bundle holds the member-row check
    (`usePipelineEditorLifecycle-BB0fSMS1.js`) and the old bundle does not; the tab ran `BB0fSMS1`.
    - After a reload of "PM-5 run 1": tiers FP llm reverse, reject_llm, approve_llm and ask_llm. The saved pipeline's
      tier holds its wrapper call and its inner thinking step, and opening that step fetches its text.
    - "PM-5 run 2", answered and ended live: all four tiers stay without a reload.
- **Registered frontier terminal, chat 20, "FP outer".**
  - The bell card was answered with the Worker killed mid-wake. Before the redelivery, the stored first-level child
    frontier was edited to two pending nodes; this is the PG negative test's edit, applied to the stack's own
    `agentstate` database.
  - The redelivery refused it with `pipeline.recovery.child_frontier_unsupported` and made no model request.
  - Live and after a reload the page shows the registered text. "Details for support" shows
    `RECOVERY_FRONTIER_UNSUPPORTED` and the message id.
  - Ledger: the decided card is SUPERSEDED and the open card is CANCELLED, both closed at the terminal.

## Follow-ups

- FU-255 to FU-259 (PM-7, PM-8, PM-11, PM-12, Map in a child), FU-260 (nested inline-branch streaming), FU-261 (replay
  retention 23514).
- FU-262: a real-PostgreSQL proof of the inline branch lost mid-stream during a wake (the row rests on the browser
  proof until then).
- FU-263: seen on the browser stack, not this change: the `start="fresh"` trace label on a redelivery past a decided
  pause, and a negative "Thought for" duration on a turn ending with the frontier terminal.
