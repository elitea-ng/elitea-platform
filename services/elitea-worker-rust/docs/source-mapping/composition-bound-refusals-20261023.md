# Composition bound violations are refused, never dropped (FU-244, FU-243)

Branch `fix/fu244-typed-bound-refusals`, from `main` `6b0cea32c` (#1407). Step 0 of the 2a-2 brief
(`handoffs/point5-wave1-20261008/T-2A2-parallel-brief.md`).

## The rule (PLAN §8.8, user 2026-10-24, binding)

- **Cycles** — an agent reaching itself through agents or pipelines, including self-reference, a version switch and
  a saved pipeline — are always refused. Cycles are per **version** (a parent that reaches *another* version of
  itself is not a cycle; the #5719 lesson).
- **Ordinary agent nesting: at most 3 levels** (Orchestrator → Sub-orchestrator → Leaf, the root counted). A level may
  hold several sub-orchestrators. Pipelines are the native nesting primitive with their own bounds (3 levels of saved
  pipelines, 2 levels of pipeline Agent nodes, 25 attachments per run).
- **Refused at authoring time**, typed and readable, wherever a stack is composed, and again at assembly as defence in
  depth. Never dropped silently.
- **Ad-hoc chat (user decision, PLAN §8.8).** A conversation runs a participant as a root, so a 3-level agent can still
  be added and runs fully when selected. On an ad-hoc turn (no agent selected) it would sit one level deeper, below the
  chat model; there it is not offered to the chat model, and the participant list says so with EliteaUI's warning and
  wording. No per-turn notice (EliteaUI has none) and no per-turn refusal.

| Where a stack is composed | Main enforcement | Proof |
|---|---|---|
| Attach an agent or pipeline as a tool | `eliteacore/application_relation.go:326,373` (before any write, parent locked) | PG attach tests; browser B |
| Save a version: pipeline instructions, or a kind switch (agent ↔ pipeline, stored instructions walked) | `repos/applications.go:831,1014` | PG save test; `TestTurningAnAgentIntoAPipelineWalksItsStoredInstructions`; browser A |
| Internal MCP instruction patch and `agent_type` update (in their transaction, before the backup) | `mcp/internal_applications_version.go:84,189` → `repos/applications.go:1002` | `TestInternalWritesRefuseABrokenComposition` |
| Add to a conversation (incl. conversation create) | `repos/conversations.go:3181` → `repos/application_composition.go:324`; the participant's project authorized first (`:356`) | `TestAddingAParticipantWhoseCompositionBreaksABoundIsRefused`, `TestParticipantProjectIsAuthorizedBeforeItsCompositionIsRead` |
| Switch a participant's version | `repos/conversations.go:1373` | `TestSwitchingAParticipantToAVersionThatClosesACycleIsRefused` |
| Re-point references by deleting a version with a replacement (one transaction, parents locked, the replacement substituted in the whole walk) | `applications/handler.go:1771,1795` | `TestHandlerPostgres_DeleteVersionRefusesAReplacementThatClosesACycle`, `…WalksTheReplacementThroughEveryParent` |
| Publish | `eliteacore/handler.go:2008` (replaces the old walk, which counted pipeline hops as levels and never looked into pipelines) | `TestPublishValidationRefusesACycleThroughAPipelineAndAFourthLevel`; e2e `api.publish-subagents` |
| Ad-hoc turn (chat model): participants judged together against the chat model's shared budget, in turn order | `repos/agent_start.go:802` → `agent_nesting.go:395` → `applicationcomposition.ChatToolFits` (`composition.go:193`); the participant read sends the same answer (`repos/conversations.go:317`) and the Web shows EliteaUI's warning (`chat-participants/lib/helpers.ts:122`) | `TestChatToolFitsShareTheChatModelsHopBudget`, `TestFilterCurrentAdhocApplicationNestingOffersOnlyWhatFitsBelowTheChatModel`, `TestTheParticipantReadJudgesParticipantsTogetherAndOnlyInTheirOwnProject`; browser G |
| Run start (selected agent), same project and catalogue | `repos/agent_start.go:438,621` → `application_composition.go:434` (422 typed); the old start walk keeps its row checks only | route test; scripted start tests; browser C |
| Assembly (Worker) | `application.composition_bound` (below); the byte budget has its own outcome (`application.composition_size`) | Worker tests; browser C2 |

Current-app gaps not ported (#5680/#5778): unbounded nesting (bounded here); pipeline children skipped in the sub-agent
tree walk (the cycle search follows every attachment, `composition.go:219`); version-switch bypass (participant switch
and re-pointing are checked); name-only pipeline nodes (an Agent node's alias resolves to exactly one attachment by its
stored name, Worker and Main alike); depth-blind container checks (levels are counted, not "has children"); the Web's
depth-1 "container" guard is removed (`useAgentPipelineAssociation.tsx`), Main is authoritative; the current app's
participant warning was ported in its pre-#5778 form (every agent that uses other agents, never a pipeline) and is now
EliteaUI's depth-aware gate fed by Main.

## Business behaviour

- **Current platform (reference only).** Attaching an agent as a tool when the child already reaches the parent is
  refused at the attach with a readable message (the legacy runtime's `elitea_core` application tools,
  "Adding this agent would create a circular reference"); publish validation reports circular and too-deep sub-agent
  trees (`utils/publish_utils.py:422,668`). EliteaUI phrases the refusal (`mapAssociationError`); the Web port already
  carries that mapping (`apps/elitea-web/src/features/agents/lib/associationError.ts`), but Main had lost the backend
  half: the relation route attached anything.
- **Before this change (since #1407).** A cycle, graph-depth overflow, a third ordinary graph scope and a static-pausing
  saved pipeline beneath a pipeline root were silently dropped: the offending edge was not built and the run answered
  without it, with only a count in the Worker log (`pipeline_agent_attachment_skipped`). The user decided
  (2026-10-24) that this silent degrade is not acceptable: these are authoring errors.
- **Now.**
  - **Save time (Main).** Attaching an agent or pipeline (relation PATCH) and saving pipeline instructions are refused
    with `400 APPLICATION_COMPOSITION_INVALID` and a message naming the path (`"Root" → "Top" → "Middle" → "Leaf"`), the
    pipeline and the Agent node; nothing is written. Bounds: circular reference, 3 agent tiers, 3 levels of saved
    pipeline nesting, 2 levels of pipeline Agent nodes, 25 attached agents and pipelines, and a pipeline that pauses
    before/after a node used as a tool of an agent beneath a pipeline root.
  - **Run time (Worker).** The same violations, reached by a composition stored before this check or broken later by
    an edit of a descendant, are refused before any model call with the registered message
    "This agent or pipeline cannot run: its attachments form a circular reference, nest deeper than allowed, or include
    a pipeline that pauses where the pause cannot be continued. Review its attachments and change the one that breaks
    the rule." (public code `APPLICATION_COMPOSITION_INVALID`). Main's start validation, which already rejected a
    stored cycle, the raw hop bound and an over-tier container with the generic "This agent turn requires the current
    execution path.", now answers the same code and text (422).
  - **Genuinely unbuildable attachments still degrade** (a deleted frozen version, a pause kind the runtime cannot
    resume, an unavailable capability): the run answers without them, and now **every root names them**, pipeline roots
    included (FU-243): the `skipped_capabilities` notice of `agent_start` / `full_message` lists them as it does for an
    ordinary Agent.
- **Not ported.** The legacy depth-1 "container agent" rule (#5680) is gone (the Web guard is removed); the 3-level
  rule and cycles are Main's, equal to the Worker's bounds through the shared contract.

## Design

- **One typed cause.** Every bound violation carries `application.composition_bound` with a data-free detail
  (`cycle`, `agent_tiers`, `pipeline_depth`, `ordinary_graph_scopes`, `application_hops`, `checkpoint_paths`,
  `static_pause_under_graph_root`): `agents/application_tools/materialization.rs:21-48`. Tagged at
  every bound: `materialization.rs:120-224` (scopes, tiers, depth, cycle, hops, bytes), `application_tools.rs:3225-3254`
  (nested build hops, tiers, cycle), `pipeline/composition.rs:89-123,294-297` (saved pipeline composition),
  `pipeline.rs:1830-1838` (static pause under a graph root).
- **Refused, not skipped.** `materialize_pipeline_children` returns a bound violation instead of skipping the child
  (`application_tools.rs:3065`). The lifecycle maps the cause to `RuntimeFailureKind::ApplicationCompositionInvalid`
  whatever coarse code the bound used (`execution/native_agent_lifecycle.rs:1558`; registered at
  `protocol/output.rs:863`, wire `UNSUPPORTED_CAPABILITY` + registered text, like the node-type refusal).
- **Named degrade on every root (FU-243).** `ApplicationMaterializationPath` carries one shared, bounded skipped set
  (`materialization.rs:187-205`, at most `MAX_SKIPPED_APPLICATION_CHILDREN` = 8), filled by `pipeline::bind_scoped_agent`
  (`pipeline.rs:2137`) and the legacy direct path (`pipeline.rs:815`); a pipeline root reads it into
  `PipelineRuntimeBindings` (`pipeline.rs:753`) and its projection (`agents/session.rs:1952`); an ordinary root merges
  it (`application_tools.rs:2962`).
- **Main save check.** `internal/domain/applicationcomposition/composition.go` walks the saved composition with the
  Worker's path rules (`Check` `:127`, `enter` `:182`, `enterAgent` `:199`, `enterPipeline` `:210`, `walkAgent` `:220`,
  `walkPipeline` `:267`, `ReadPipeline` `:393`). The version being saved is walked as a root of its own kind, so a
  violation found there is one in every root that reaches it (embedding only adds depth): the save never refuses what
  the runtime runs. Read through `repos.CompositionSource` (`internal/infra/db/repos/application_composition.go:130`),
  which reads the same attachment rows as the relation route (the reader moved there: `ListApplicationToolReferences`
  `:61`). Wired at the relation attach, before anything is written, under the parent row lock
  (`internal/api/v2/eliteacore/application_relation.go:326,373`), and at a pipeline instructions save
  (`internal/infra/db/repos/applications.go:831,996`). Main maps the Worker's run-time refusal to the same code
  (`internal/transport/runtimegrpc/output/server.go:1172`).
- **Shared contract.** `libs/proto/contracts/application-composition-limits-v1.json` holds the four bounds, the public
  code and the registered message; the Worker (`materialization_tests.rs`
  `composition_bounds_match_the_shared_contract`) and Main (`applicationcomposition` `TestBoundsMatchTheSharedContract`,
  `output` `TestApplicationCompositionFailureAdmitsOnlyTheRegisteredMessage`) assert it.
- **Main start refusal.** `repos/agent_nesting.go:30` marks the start validator's cycle, raw-hop and over-tier-container
  refusals (`:175,183,206`) as composition bounds; `repos/agent_start.go:445` answers `applicationcomposition.ErrAtStart`
  (`composition.go:60`, 422 + `RuntimeMessage` `:54`), written by `api/v2/agentexecution/route.go:601`. Other nesting
  refusals (a malformed or missing row) stay the generic unsupported start.
- **Web.** The pipeline editor already shows a save refusal inline (`EditPipelineAlerts.tsx:59-66`). The agent editor's
  Tools menu dropped every failed attach silently (`ToolMenu` acted only on `outcome.ok`); it now closes the menu and
  shows the message inline (`features/agents/ui/ToolMenu.tsx:296,387`; EliteaUI toasted it, this app has no toasts and
  uses inline alerts). `mapAssociationError` turns a circular refusal into EliteaUI's own wording. The relation PATCH
  is a write and is no longer retried (`api/useAgentPipelineAssociation.tsx:215`): a refused attach went out four
  times.

### Review round 1 (orchestrator, `REVIEW-1409.md`)

- **The participant's project is authorized before anything is read in it** (`repos/application_composition.go:356`
  `participantSchema`). The conversation's own project is read as is (the conversation route authorized the caller);
  another project only when the caller may act inside it (`projectaccess.Membership`, the HTTP project gate's own rule)
  or for a version the catalogue project publishes; anything else answers 404 "The agent or pipeline was not found."
  before any read there, and every failure refuses. The ad-hoc turn and the participant read judge only participants of
  the conversation's own project, which are the only ones an ad-hoc turn offers.
- **Every writer and start path runs the check.** The internal MCP instruction patch and `agent_type` update
  (`mcp/internal_applications_version.go:84,189`, through `repos.CheckVersionComposition` `applications.go:1002`); a
  REST kind switch walks the stored instructions as the kind it becomes (`applications.go:1014`); the catalogue start
  (`agent_start.go:621`); a re-point runs in one transaction with the parents locked `FOR UPDATE` and the replacement
  substituted in the whole walk (`applications/handler.go:1795` `repointedSource`).
- **The chat model's shared budget.** The Worker builds every offered participant under one chat root sharing its hop
  budget, so `ChatToolFits` (`composition.go:193`) judges them together in turn (participant mapping) order; one that
  does not fit is not offered and consumes nothing.
- **Checkpoint paths** are a Main bound now (`composition.go:493`, 128, counted like the Worker's `collect_paths`).
  **Version bytes** are not: Main cannot measure the frozen documents the Worker loads. The Worker gives them their own
  cause, `application.composition_size` (`materialization.rs:54,242`, `pipeline/composition.rs:121,123`): an
  attachment past the 4 MiB budget is left out and named "too large" where the root offers its attachments to its own
  model (`application_tools.rs:3071,3186`; reason code `application_child_too_large`, `skipped_capabilities.rs:45`), so
  an ad-hoc turn keeps answering; a pipeline Agent node that selects it, or a pipeline root's own composition, ends with
  `RuntimeFailureKind::ApplicationCompositionTooLarge` (`native_agent_lifecycle.rs:1561`; registered
  `protocol/output.rs:809,874`; Main maps it, `output/server.go:1175`). The contract gained `max_checkpoint_paths`,
  `max_version_bytes` and `size_runtime_message`.
- **One read per level.** `CompositionSource` is per request: one legacy-table probe, a cache of every version read
  (misses too), and `Prefetch` (`application_composition.go:189`) reading one level of the walk in two statements;
  the walk prefetches level by level first (`composition.go:528`).

### Review round 2 (orchestrator, `REVIEW-1409-delta.md`)

- **A. The version switch authorizes and writes in one transaction** (`repos/conversations.go:1371`): the mapping row
  is locked `FOR UPDATE`, the participant's project authorized and the composition checked on that transaction, then
  the row is written and committed. A participant meta that does not decode refuses.
- **B. Re-points are serialized with attaches.** The re-point locks the version it replaces `FOR UPDATE` before it
  reads the parents (`applications/handler.go:1763`); the relation attach reads the child version `FOR SHARE OF version`
  (`eliteacore/application_relation.go:305`). An attach of that version in flight finishes first and its parent is
  walked; none can start until the move commits.
- **C. Ancestors are walked at authoring time** (`applicationcomposition.CheckWithAncestors`, `composition.go:189`):
  an attach, a version save or kind switch (REST and MCP) and every re-pointed parent are checked as roots, and so is
  every saved version that attaches them, transitively (up to 128), with the changed version as it will be stored. The
  ancestors are found a level per batch (`CompositionSource.Parents`, `application_composition.go:257`) and read in one
  batch, so twenty ancestors cost 8 statements. A chain linked top-down is now refused at the link that makes a fourth
  agent level, naming the path from the top.
- **D.** The MCP switch to a pipeline bounds the stored text first (`mcp/internal_applications_version.go:87`).

## Defects found by running it

- **The registered message did not fit the output plane.** Main admits a runtime failure's safe message up to 256 bytes
  (`application/output/runtime_failure.go` `RuntimeFailureFrame.Validate`). The first message was longer, so Main
  rejected the Worker's terminal (`PROTOCOL_VIOLATION`) and the run stayed retained without an ACK (spinner). Shortened
  in the shared contract; the Worker lifecycle test and Main's contract test now assert `≤ 256` bytes.
- **Main's start already refused a stored cycle, unreadably.** Found when the browser run of a stored cycle never
  reached the Worker. Now typed (above).
- **The Web dropped every failed attach** and retried the write on a 400 (above).
- **A test fixture passed for the wrong reason:** `TestValidateCurrentApplicationNestingEnforcesRawHopBackstop` built
  children whose application id belonged to the parent, so a mismatch refusal fired before the hop bound. Fixed; the test
  now asserts the bound marker.

- **Found by CI on the review round.** Three e2e journeys encoded the old behaviour: a cycle accepted at the link and
  refused only at publish, a four-agent chain accepted at publish (the old walk counted hops), and an agent attached to
  itself. They now assert the §8.8 rule. One journey attaches a draft agent of another project the persona belongs
  to; the first authorization rule (catalogue: published only) refused it, and is now the membership rule above.

## Changed paths

Worker (`services/elitea-worker-rust/src`): `agents/application_tools.rs`, `agents/application_tools/materialization.rs`,
`agents/application_tools/materialization_tests.rs`, `agents/pipeline.rs`, `agents/pipeline/composition.rs`,
`agents/pipeline_scoped_tests.rs`, `agents/ordinary_tests.rs`, `agents/session.rs`, `agents/session_tests.rs`,
`agents/events_tests.rs`, `agents/skipped_capabilities.rs`, `execution/native_agent_lifecycle.rs`,
`execution/toolkit_delivery_processor.rs`, `protocol/output.rs`. No `libs/rust` change.
Main (`services/elitea-main/internal`): `domain/applicationcomposition/{composition.go,composition_test.go,contract_test.go}`
(new); `infra/db/repos/{application_composition.go (new),applications.go,agent_nesting.go,agent_start.go,conversations.go}`
and their tests; `api/v2/eliteacore/{application_relation.go,handler.go}` and PG tests; `api/v2/applications/handler.go`
and the re-point PG tests; `api/v2/mcp/internal_applications_version.go` and a PG test; `api/v2/conversations` PG tests
(participant project authorization); `api/v2/agentexecution/{route.go,route_test.go}`;
`transport/runtimegrpc/output/{server.go,composition_failure_test.go}`.
Web (`apps/elitea-web`): `src/features/agents/{ui/ToolMenu.tsx,api/useAgentPipelineAssociation.tsx}` and tests;
`src/features/chat-participants/{lib/helpers.ts,ui/…}` and tests; the Chat buttons and edit actions of agents and
pipelines; `src/shared/i18n/en.json`; e2e `journeys/api/api.publish-subagents.spec.ts`, `streaming/chat.agent-tools.spec.ts`
(they built a cycle or a four-agent chain that is now refused where it is made).
Contract: `libs/proto/contracts/application-composition-limits-v1.json` (new).

## Tests

Counts from this branch head (after the review round and the merge of `main`), against a disposable `postgres:18`
with the CI environment (`ELITEA_TEST_DATABASE_URL`, `ELITEA_REQUIRE_POSTGRES_RECEIPT_TESTS=1`,
`ELITEA_TEST_DATABASE_GUARD=disposable-pg18`):

- **Worker** `cargo test --offline --locked --no-fail-fast --all-targets --all-features -- --test-threads=4`:
  **2646 passed, 0 failed, 103 ignored** (the ignored are the supervisor/preparation/hydration `#[ignore]` suites CI
  runs separately). `cargo fmt --check` and `cargo clippy --locked --all-targets --all-features -D warnings` clean.
  No `libs/rust` change.
- **Main** `go test -race -count=1 ./...`: **222 packages ok, 0 failed** (33 without tests); `go vet` clean. Rerun on
  the round-2 head `feb3c82a8`: 222 ok, 0 failed. Round 2 changed no Worker and no Web `src` file (Worker and Web
  counts are from `71083b119`); the e2e journey it changed type-checks and lints clean.
- **Web** `vitest run`: 1914 files / **19023 tests passed**, 6 expected fail, 1 skipped; 1 failed:
  `themePreference.test.ts` "keeps the mark in memory when storage refuses the write", which this branch does not touch
  (pre-existing, storage behaviour under node). `tsc --noEmit` and `oxlint` clean.

New or changed tests (red before the change where marked):

| Rule | Test | Before |
|---|---|---|
| Every bound violation ends as the registered composition terminal (all 8 details), ≤ 256 bytes; an unrelated unsupported capability stays generic | `native_agent_lifecycle::taxonomy_tests::composition_bound_violations_end_as_the_registered_composition_message` | new kind |
| A cycle / graph depth / third scope deep in a saved pipeline is refused with the typed cause before any model call | `mixed_graph_agent_cycle_is_refused_before_provider_effects`, `second_scope_mixed_cycle_is_refused_before_provider_effects`, `second_scope_mixed_graph_depth_is_refused_before_provider_effects`, `third_ordinary_graph_scope_is_refused_before_provider_effects` | red: the edge was dropped |
| Ordinary graph scopes: 2 assemble, 3 refused typed, the Agent past the bound never read | `the_ordinary_graph_scope_budget_admits_the_limit_and_refuses_one_more` | red: dropped |
| A static-pausing pipeline tool under a pipeline root is refused typed, 0 model calls | `a_static_saved_pipeline_tool_under_a_graph_root_is_refused_before_any_model_call` | red: dropped |
| The hop budget (25/26) carries the bound cause; the version-byte budget (4 MiB/+1) its own size cause, and a pipeline root ends with the size terminal | `the_application_hop_budget_admits_the_limit_and_refuses_one_more`, `the_version_byte_budget_admits_four_mib_and_refuses_one_more_byte_at_a_pipeline_root` | cause added |
| Tier limit and nested cycle carry the bound cause; saved-pipeline composition cycle/depth too | `nested_agent_tier_limit_fails_before_fetching_the_fourth_tier`, `nested_agent_cycle_fails_after_one_exact_resolution_and_before_model_dispatch`, `composition::tests::rejects_cycles_depth_unavailable_and_foreign_scope_before_binding` | cause added |
| FU-243: a pipeline root names the skipped attachment in `skipped_capabilities` | `an_unbuildable_attachment_of_a_pipeline_agent_node_is_skipped_and_the_node_answers` | red: not named |
| Bounds and message equal the shared contract | Worker `composition_bounds_match_the_shared_contract`; Main `TestBoundsMatchTheSharedContract`, `TestApplicationCompositionFailureAdmitsOnlyTheRegisteredMessage` | new |
| Save check: tiers 3/4, depth 3/4 (agent node at depth 3), scopes 2/3, hops 25/26, cycle (incl. self and through a pipeline root), static pause only beneath a pipeline root, unreadable attachment not followed, override = version being saved, YAML reading | `applicationcomposition` (9 tests) | new |
| Read budget: each version read once per walk; a refused walk reads ≤ 27 versions | `TestEachVersionIsReadAtMostOnceWithinTheHopBudget` | new |
| Real PG: a cycle-closing attach is refused, named, nothing written; tiers refused only past the limit; a pausing pipeline under a pipeline Agent node refused at attach and at save (row read back); 25 attachments walk in a measured time, the 26th refused | `TestAnAttachThatClosesACycleIsRefusedAndWritesNothing`, `TestAgentTiersAreRefusedOnlyPastTheLimit`, `TestAPausingPipelineUnderAPipelineAgentNodeIsRefusedAtAttachAndAtSave`, `TestTheCompositionCheckAtItsHopBudgetIsFast` | new |
| Start: a stored bound violation answers 422 `APPLICATION_COMPOSITION_INVALID` | `route_test` `TestCurrentApplicationStartRouteNamesThePipelineLimitItRefused/composition`, `TestARefusedStartIsTheTypedCompositionRefusal`, `agent_nesting_test` (bound marker on every refusal case) | red: generic |
| Ad-hoc: a participant fits below the chat model only with room for one more level (leaf 1, orchestrator 2, 3 levels → not offered, pipelines consume no tier, the chat model's hop counts, an unreadable version is left to the turn) | `TestChatToolFitOffersOnlyWhatFitsBelowTheChatModel`, `TestChatToolFitCountsTheChatModelsHop`, `TestChatToolFitLeavesAnUnreadableVersionToTheTurn` | new |
| Ad-hoc start offers only what fits (the nesting resolution alone would offer both) | `TestFilterCurrentAdhocApplicationNestingOffersOnlyWhatFitsBelowTheChatModel`; PG `TestPostgresCurrentNestedApplicationTurnPreservesThreeTierContract` (real source) | red: offered |
| Real PG: a 3-level agent is addable; the participant read sends `agent_subtree_tiers` 3/2/1, `is_container`, `max_agent_nesting_tiers` 3 | `TestTheParticipantReadFlagsAnAgentThatRunsOnlyWhenSelected` | new (Main sent none) |
| Web: EliteaUI's gate (`1 + tiers > max`, blunt fallback without the tier fields) | `helpers.test.ts` `isSkippedContainerParticipant` (4) | new |
| Web: a refused attach is shown, the menu closes, the write is sent once | `ToolMenu.test.tsx` "shows a server refusal of the attach …" | red: no alert, 4 requests |
| Review 1: the participant's project is authorized before it is read (own project, member of another project, published catalogue version allowed; foreign, nonexistent, malformed, unpublished catalogue refused; route answers for unauthenticated, other user, member without access; nothing named, nothing written) | `TestParticipantProjectIsAuthorizedBeforeItsCompositionIsRead` (12 cases) | red: a foreign project was read and named |
| Review 2: the MCP patch and `agent_type` update refuse a broken composition, nothing changed or backed up | `TestInternalWritesRefuseABrokenComposition` | red: written |
| Review 3/5: participants share the chat model's hop budget in turn order; a participant of another project is never judged on a colliding id | domain `TestChatToolFitsShareTheChatModelsHopBudget`; PG `TestTheParticipantReadJudgesParticipantsTogetherAndOnlyInTheirOwnProject` | red: each judged alone; wrong schema |
| Review 4: checkpoint paths 128/129 and through nested pipelines; the size skip names the attachment and the agent still answers; the size terminal and its contract | `TestCheckpointPathsAdmitTheLimitAndRefuseOneMore`, `TestCheckpointPathsCountNestedPipelines`; Worker `an_attachment_past_the_byte_budget_is_left_out_and_the_agent_still_answers`, `a_child_left_out_for_size_says_so`, the taxonomy and contract tests | new |
| Review 6: the catalogue start runs the shared check | `TestOriginalRootCatalogueSourceRetainsExactAdmittedReadAndOldWrapper` (scripted reads) | new reads |
| Review 7: an agent → pipeline switch walks the stored instructions | `TestTurningAnAgentIntoAPipelineWalksItsStoredInstructions` | red: stored |
| Review 8: a re-point is walked for the whole composition (a fourth level through another parent) | `TestHandlerPostgres_DeleteVersionWalksTheReplacementThroughEveryParent` | red: moved |
| Review 9: judging participants costs a fixed number of statements (5 for 4 and for 8 participants) | `TestJudgingParticipantsCostsAFixedNumberOfStatements` | red: per participant and version |
| Review 10: the depth refusal quotes the Agent node once | `TestTheDepthRefusalNamesTheAgentNodeOnce` | red: double quotes |
| e2e (CI): a cycle is refused at the link; a chain linked top-down is refused at the link that makes a fourth agent level (typed, naming the path), three publish; an agent attached to itself is refused | `api.publish-subagents.spec.ts`, `chat.agent-tools.spec.ts` | updated |
| Round 2 A: the version switch, create-with-participants and token principals authorize the participant's project (foreign, missing, unpublished catalogue refused; published catalogue allowed; token of the owner like its session, a token without an owner refused); a refused switch changes nothing, a refused create creates nothing | `TestParticipantVersionSwitchAndCreateAuthorizeTheParticipantsProject`, token rows in `TestParticipantProjectIsAuthorizedBeforeItsCompositionIsRead` | new |
| Round 2 B: an attach in flight when the re-point starts is waited for and walked (a cycle → 400, nothing moved) | `TestHandlerPostgres_DeleteVersionWaitsForAConcurrentAttachAndWalksIt` | red: moved unwalked |
| Round 2 C: an ancestor pushed past the limit refuses the change (domain); the 4th-level link refused on real PostgreSQL; the upward walk's statements (8 for 20 ancestors) | `TestCheckWithAncestorsRefusesAChangeThatPushesAnAncestorPastTheLimit`, `TestAnAttachIsRefusedWhenAnAncestorWouldPassTheLimit`, `TestTheAncestorWalkCostsAFixedNumberOfStatementsPerLevel` | red: 201 |
| Round 2 D: the MCP switch to a pipeline bounds the stored text | `TestInternalAgentTypeSwitchBoundsTheStoredText` | red: walked without nodes |

## Performance

- **Budget (Main checks).** One request reads each saved version at most once: `CompositionSource` caches every version
  it read (misses too) for the request, probes the legacy attachment table once, and reads a level of the walk in one
  batch (`Prefetch`: one statement for the version rows, one for their attachments, one more for legacy attachments on a
  legacy-migrated tenant). The walk prefetches the reachable versions level by level (at most `MaxCycleVersions` = 128)
  before it walks them, so a check costs about two statements per level of the composition, not per version. Proven by
  `TestEachVersionIsReadAtMostOnceWithinTheHopBudget` (domain) and, on real PostgreSQL,
  `TestJudgingParticipantsCostsAFixedNumberOfStatements`: judging 4 and 8 participants (each a 2-level agent) costs
  **5 statements** both times. The upward walk (round 2) adds one statement per level of ancestors and one batch for
  all of them: `TestTheAncestorWalkCostsAFixedNumberOfStatementsPerLevel` checks a version with twenty ancestors in
  **8 statements**. Only composing writes, a start, an ad-hoc start and the participant read pay it; an
  agent save that neither writes pipeline instructions nor changes the kind never walks.
- Pipeline text is parsed only within the save-time bounds (`pipelinelimits.Check`: 512 KiB, alias expansion budget)
  and only top-level `nodes` / `interrupt_*` are read; an over-bound stored text is not read past its length
  (`CASE … octet_length … <= $3` in the version query).
- **Worker.** No new work on the run path: the causes are fields on the existing error; the skipped set is a bounded
  `BTreeSet` (≤ 8 entries) behind an uncontended mutex touched only when something was skipped.
- **Participant read.** All agent participants of the conversation are judged in one `ChatToolFits` walk through one
  source (above). Measured before the batching on the stack, conversation 898 (two agent participants), 20 reads from
  the browser: p50 **8.3 ms**, p95 10.2 ms end to end; the batching only removes statements.
- **Measured** (real PostgreSQL 18, `TestTheCompositionCheckAtItsHopBudgetIsFast`): the worst admitted walk, 25
  attachments under one root, slowest of 20 checks = **2.0 ms** after the batching (54.7 ms before; bound asserted at
  2 s; the shape under test is the bound, not the machine).

## Durability

- Nothing new is persisted by the Worker. A refusal happens at assembly, before any model call, tool call or
  receipt (proven by the tests asserting zero model calls and no read past the bound).
- Main: a refused attach writes no row (checked before the insert, under the parent row lock); a refused save stores
  nothing (`TestAPausingPipelineUnderAPipelineAgentNodeIsRefusedAtAttachAndAtSave` reads the row back).
- The check reads in the attach transaction, so the new edge and the parent lock are consistent; a concurrent edit of a
  *descendant* is not locked (the walk would have to lock the whole tree): such a race can store a violating
  composition, which the Worker then refuses typed at run time (row below). Documented, not hidden.

## Resilience

- Bounded: the walk's hop budget, per-walk cache, label length (64 runes, printable, quotes replaced), YAML size and
  alias budget; the skipped set (8).
- Typed and readable everywhere: a bound violation is `application.composition_bound` + detail in the Worker, the
  registered `APPLICATION_COMPOSITION_INVALID` terminal for the user, and a 400 with the same code at save. No silent
  coercion: the previous "drop the edge, log a count" is gone (`mixed_graph_agent_cycle_is_refused_before_provider_effects`
  and siblings were the dropped-edge tests and now assert the refusal).
- Unbuildable ≠ bound: a deleted version or unavailable capability still degrades, named
  (`an_unbuildable_attachment_of_a_pipeline_agent_node_is_skipped_and_the_node_answers` now also asserts the pipeline
  root's notice; `TestAnUnreadableAttachmentIsNotFollowed` on the Main side).

## Security

Checked against `rules/security.md`:
- **Identity / authorization.** No new route. The relation route's existing project scoping and parent row lock apply;
  the walk reads only the request's tenant schema (quoted via `tenantschema.Quote`), so a foreign project's version is
  simply not found and not followed. A conversation participant's project is authorized before its composition is
  read (`participantSchema`, review item 1): the conversation's own project, a project the caller may act inside
  (`projectaccess.Membership`, the HTTP gate's rule), or a published catalogue version; anything else is 404 before
  any read there, and every failure refuses (proven by the 12-case matrix test). The Worker keeps its foreign-project refusal
  (`a_saved_participant_from_a_foreign_project_is_refused_before_any_effect`).
- **Input / amplification.** Stored YAML is parsed only after `pipelinelimits.Check` (size + alias-expansion budget);
  the walk is bounded by the hop budget and cache; cycles terminate (ancestor check).
- **Injection.** Parameterized SQL only; the schema identifier is the pre-quoted tenant schema.
- **Ad-hoc delta (participant read, ad-hoc start).** Read-only, inside the already-authorized conversation read and
  start; versions are read by the ids the participant row stores, in the conversation's own tenant schema, so another
  project's version is not found and not followed. The read adds only numbers and booleans to `meta`, never names.
  A client cannot plant the fields: the add stores only a server-built display name (`participantDisplayMeta`), and
  the start's decision never reads `meta`. A failed walk degrades the participant, never the conversation.
- **Data in messages.** A save refusal echoes the author's own application names and node ids (bounded, printable)
  back to the author of the same project, as the existing node-type refusal does; the run-time message is data-free
  (registered text only); nothing is logged except the existing data-free error lines.
- **Secrets / egress / supply chain.** Not applicable: no secret handled, no outbound call, no new dependency.
- Not applicable to this change: egress, delegated credentials, sandbox, CSP.
- **Audits (dependency scanners; no dependency or toolchain change in this PR).** `govulncheck ./...` (Main): one
  standard-library finding, GO-2026-6809 (`crypto/x509`, fixed in go1.26.10; the toolchain pin, pre-existing), no
  module finding reachable. `cargo deny --all-features check advisories` (Worker): 16 advisories, all pre-existing in
  the locked tree (RUSTSEC-2023-0071, 2024-0449, 2026-0192, -0206, -0346, -0347, -0348, -0349, -0367, -0368, -0369,
  -0381, -0382, -0383, -0385, -0386); none added.

## Recovery rows (replatform-delivery-gate §2)

No new durable state, no new effect. The rows this change touches:

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Main × admission (attach, save, kind switch, MCP patch/update, participant add/switch, re-point) | F (typed, readable, at save time): nothing is written; a re-point is checked and moved in one transaction | `application_relation.go:326,373`; `repos/applications.go:831,1014`; `mcp/internal_applications_version.go:84,189`; `applications/handler.go:1795` | PG attach/save/MCP/re-point tests (rows read back); browser A, B, E, F |
| Main × admission (run start, same project and catalogue) of a stored bound violation | F (`APPLICATION_COMPOSITION_INVALID`, 422): the run never starts, no execution row | `repos/agent_start.go:438,621`, `route.go` | route test; scripted start tests; browser C |
| Worker × assembly of a stored bound violation Main's start does not see (static pause under a pipeline root, pipeline depth, scopes) | F, settled and acked before any model call, persisted on the message | `native_agent_lifecycle.rs:1558`, `protocol/output.rs` | scoped/materialization tests; browser C2 (`executed_settled_acked`) |
| Worker × assembly past the version-byte budget | R where the root offers attachments to its model (left out and named "too large", the turn answers); F at a pipeline that needs the attachment (`ApplicationCompositionTooLarge`, registered, before any model call) | `application_tools.rs:3071,3186`, `native_agent_lifecycle.rs:1561` | `an_attachment_past_the_byte_budget_…`, `the_version_byte_budget_…_at_a_pipeline_root` |
| Worker × assembly of an unbuildable attachment (deleted version, unresumable pause kind) | R for every run: degraded and **named** on every root, pipeline roots included | `materialization.rs:187-205`, `pipeline.rs:753,815,2137`, `session.rs:1952` | `an_unbuildable_attachment_of_a_pipeline_agent_node_…`; browser D (reload) |
| Main × admission of an ad-hoc turn with a participant that does not fit below the chat model with the others | R: the turn runs; the participant is not offered (stated on the participant list, not dropped silently) | `agent_nesting.go:395`, `conversations.go:317` | filter unit + PG tests; browser G (Worker `tool_reference_count=1`) |
| Main × output delivery of the composition terminal | I: the registered text fits the 256-byte bound, so the frame is admitted and settled once | `runtime_failure.go` bound; contract | lifecycle + contract tests; browser C2 |

Gap kept (not this change): Main's start validation refuses a **deleted** attachment version generically
("requires the current execution path") instead of letting the Worker degrade it named — see Follow-ups.

## Browser evidence

Stack `fu244` (own; `production-profile.pre-defaults-20261019/stack.sh`, Traefik-less current-main tooling), front door
`http://fu244.localhost:19080`, dump `product-real-models-main-d5e966074.dump`, `verify-profile: 50 passed, 0 failed`
(after every redeploy). Final images (binary-checked, §3b: the registered message is present in both binaries):
Main `elitea-main:fu244-81a222eea` (cases A–D), `fu244-c3b44de34` (cases E, F: the §8.8 sites) and `fu244-83d394c2a`
(case G; binary contains `applicationcomposition.ChatToolFit` and `enrichParticipantNesting`), Worker
`elitea-worker-rust:fu244-81a222eea`, Web `elitea-web:fu244-a9ff0c48b` (A–D), `fu244-300b28c0c` (E, F) and
`fu244-83d394c2a` (G; bundle contains the EliteaUI wording), supervisor `2a1-1f617cfdb`, the rest `main-d5e966074-verify`. User's Chrome (Claude in Chrome),
own tab, mock OIDC `admin@centry.user`, project 2.

**Fixtures (honest).** Project 2 is legacy-created, so attaching through Main's route 500s there on a *valid* attach
(FU-242, `elitea_tools.owner_id`). The compositions were therefore created by SQL that mirrors Main's insert minus
`owner_id` (`handoffs/point5-wave1-20261008/fu244/fixtures.sql`): pipelines 171 "FU244 review flow" (v196, attaches agent
169 "S1 static caller" → static pipeline 168), 172 "FU244 cycle flow" (v197, Agent node → agent 157 v183), 173 "FU244 ask
flow" (v198, `ask_user`), 174 "FU244 notice agent" (v199, attaches 173), 175 "FU244 notice flow" (v200, Agent node →
174). The actions under test (save, attach, run) were done in the UI. The stored violations for C/C2 were written by
SQL on purpose, as a composition saved before the check would hold them (`fixture-c-cycle.sql`, `fixture-c2-static.sql`).

| Case | Steps (UI) | Observed | After reload |
|---|---|---|---|
| A. Save refused | Pipeline 171, YAML tab: add an Agent node running "S1 static caller", Save | Inline alert: `Pipeline "S1 static pipeline" pauses before or after a node, so it cannot be a tool of agent "S1 static caller" inside a pipeline: an Agent node cannot continue that pause ("FU244 review flow" → "S1 static caller" → "S1 static pipeline"). Remove the pause or the attachment before saving.` | Stored YAML unchanged (no Agent node); DB confirms |
| B. Attach refused | Agent 157, Tools → Pipeline → "FU244 cycle flow" | Exactly one `PATCH …/application_relation/prompt_lib/2/172/197` → 400 `APPLICATION_COMPOSITION_INVALID`; menu closes; inline: `Cannot add "FU244 cycle flow": this would create a circular agent reference. Remove the circular dependency first.` (Main's raw text names `"C1 sensitive record agent 20261008" → "FU244 cycle flow" → "C1 sensitive record agent 20261008"`) | Agent 157 has 0 application attachments |
| C. Stored cycle at run start | Chat with pipeline 172 (stored edge 157 → 172) | The registered message, 422 from Main's start (chat 894) | The chat is empty: a refused start creates no message group (as for every existing start refusal) |
| C2. Stored static pause at the Worker | Chat with pipeline 171 (stored Agent node → 169 → 168) | The registered message; Worker log `agent_native_assembly_failed … cause_code="application.composition_bound" cause_detail="static_pause_under_graph_root"`, outcome `executed_settled_acked`, execution `d689090294820c32fe422e9e26db0cc6` (chat 893) | Persisted, same message |
| E. A fourth agent level refused at attach (§8.8) | Agent 179 "FU244 Host", Tools → Agent → "FU244 Orchestrator" (176 → 177 → 178, a 3-level chain created by `fixtures-88.sql`) | One PATCH → 400; inline: `Agents are nested more than 3 levels deep: "FU244 Host" → "FU244 Orchestrator" → "FU244 Sub-orchestrator" → "FU244 Leaf". Attach fewer levels of agents before saving.` The removed Web guard would have refused locally with the depth-1 "uses other agents" text | Host has 0 attachments. (An earlier accidental type-ahead pick of a valid agent showed "relation write failed": FU-242's legacy-schema insert, now visible instead of silent) |
| F. Add to a conversation refused (§8.8) | Agent 157 (stored cycle 157 → 172 → 157), header "Chat" | `POST …/participants/prompt_lib/2/<conversation>` → 400; header alert shows the named cycle `"C1 sensitive record agent 20261008" → "FU244 cycle flow" → "C1 sensitive record agent 20261008"` (before: the generic "Failed to open a chat with this agent.") | The conversation created first is left with only the user (the button creates, then adds — follow-up) |
| G. Ad-hoc (§8.8) | New chat (898), chat model Claude Haiku; add "FU244 Top" (182 v207 → Middle 181 v206 → Leaf 180 v205, `fixture-g-adhoc.sql`) and "FU244 Middle" by `#` mention; (1) Top selected, send; (2) select nothing, send | Add accepted. (1) Top runs as the root and calls Middle, which has Leaf bound. Participant row under Top (not active): "Its sub-agent chain is at the nesting limit (3 tiers), so it runs only as the active agent here — select it to run."; nothing under Middle. (2) The turn completes; the chat model answers that it can call only "FU244 Middle"; Worker `execution_kind=Adhoc … tool_reference_count=1 nested_application_count=1`, no refusal. Read: Top `agent_subtree_tiers` 3, Middle 2, `max_agent_nesting_tiers` 3 | Collapsed rail: info dot, tooltip "Its sub-agent chain is at the nesting limit (3 tiers) — select it to run."; expanded: the same row warning |
| H. Review round re-check (images Main `fu244-3ee734d8f`, Worker and Web `fu244-f7b9be2dc`; binaries contain `repos.participantSchema`, `applicationcomposition.ChatToolFits`, `application.composition_size`, `application_child_too_large`) | Chat 898 after the redeploy: select nothing, send; then from the signed-in session add participants to chat 899 | The warning still on "FU244 Top"; the ad-hoc turn answers that it can call only "FU244 Middle". Add with a foreign project → 404 "The agent or pipeline was not found."; with a project id that does not exist → 404 (it was 500 for a platform administrator before `3ee734d8f`, found here); the own project's 3-level "FU244 Top" → 200 with its nesting fields | Persisted |
| I. Round 2 C: a fourth level reached through an ancestor (Main `fu244-feb3c82a8`, binary contains `applicationcomposition.CheckWithAncestors` and `(*CompositionSource).Parents`) | Agent 180 "FU244 Leaf" (attached by Middle, which Top attaches), Tools → Agent → "FU244 notice agent" | Inline alert: `Agents are nested more than 3 levels deep: "FU244 Top" → "FU244 Middle" → "FU244 Leaf" → "FU244 notice agent". Attach fewer levels of agents before saving.`; Leaf's version 205 has 0 application attachments (DB) | Unchanged |
| D. Named degrade on a pipeline root (FU-243) | Chat with pipeline 175 | The node answers ("Hello! 👋"); notice "Some tools were not available in this reply — Attached agent FU244 ask flow: this attached agent type is not available as a tool on this worker yet, so it was left out." (chat 892; message group 7518 meta carries `skipped_capabilities`) | Persisted, same notice |

Screenshots: `handoffs/point5-wave1-20261008/fu244/evidence/` (local; G1–G4 for case G).

**Current app (EliteaUI).** Placement and wording were taken from EliteaUI's `ParticipantWarning.jsx:44-45`
(participant row, neutral info) and `CollapsedPerticapantsList.jsx:206-216` (rail dot, tooltip), gate
`participants.helpers.js` `isSkippedContainerParticipant`. In the running current app (http://localhost, Chrome) the
conversation read returns the same fields (`is_container`, `agent_subtree_tiers`, `max_agent_nesting_tiers: 3`), e.g. a
2-tier agent `true, 2, 3`; no participant there sits at the limit, so the warning itself was not rendered there.

## Follow-ups

- **Plain delete vs a concurrent attach (pre-existing, outside the re-point).** The delete route checks whether a
  version is in use before taking any lock; an attach of it in flight is not seen, the delete waits for the attach's
  share lock and then removes the version, leaving the new reference dangling. Moving the in-use check under the
  version lock closes it (FU-245).

- The new Web composer's "Switch to model" control did not switch to the chat model on the stack (clicked and
  scripted); clicking the active participant in the list does. Pre-existing, unrelated to this change.
- `entities/participant` still exports a camelCase `isSkippedContainerParticipant` that no UI uses (the pre-#5778 rule);
  remove it or align it with the feature helper.
- A run retained by the earlier > 256-byte terminal (fixed here) keeps retrying on the stack
  (`agent_output.invalid_durable_state`, execution `e6f9d6e48bc7664ae560f90f342394db`); it is stack residue from before
  the fix, not produced by the final images.
- The agent/pipeline "Chat" button creates the conversation first and adds the participant second, so a refused add
  leaves an empty conversation; creating with the participant in one call (the create path checks it too) avoids it.

- **Start-time named check for upward violations.** The save check walks the saved version as a root, so a violation
  that appears only from an ancestor (a descendant edited after its parents were saved, e.g. a pause added to a pipeline
  an Agent node's agent already carries) is caught at run time with the data-free message, not at save. Main's start
  could run the same walk on the started version and name the path (one bounded walk per start).
- **Main start refuses a deleted attachment version generically** (`agent_nesting.go` "does not exist") where the Worker
  would degrade it named. Align with FU-243 (degrade, named) — product decision needed.
- The fixed skipped-application reason text ("this attached agent type is not available as a tool on this worker yet")
  is generic for every unbuildable attachment, including an unresumable pause kind; a reason per cause would be more
  precise.
- FU-242 (legacy tenant schemas lack `elitea_tools.owner_id`) still blocks valid attaches on dump projects.
- Main start validator vs Worker rules: Main's start still applies its own "container at tier 3" rule; the two rule sets
  should share the composition contract (Main's tier bound equals it today).
