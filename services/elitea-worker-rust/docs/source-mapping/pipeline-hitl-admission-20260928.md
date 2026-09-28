# Direct pipeline HITL admission

Status: implementation in progress. This change does not complete history segmentation.

## Behavior and source mapping

Current Core `plugins/elitea_core/utils/pipeline_hitl_history.py` defines the business behavior.
`get_direct_pipeline_hitl_interrupt` excludes nested and parallel lineage.
`pipeline_hitl_decision_text` uses Approved, Rejected, or the exact submitted edit.
`create_pipeline_hitl_resume_segments` preserves the review and creates separate decision and response groups.
The new platform reuses this behavior, not its SQLAlchemy implementation.

Rust `agents/graph/hitl.rs` already supplies the versioned static-review contract.
Main's pending `directPipelineHITLReview` classifier reconstructs direct scope from persisted response metadata and the saved application type.
Main `application/agentexecution/continue.go` now carries this review through the immutable admission turn.
Target validation requires one matching interrupt and an application HITL continuation.
Admission validation also requires one matching Approve, Reject, or Edit decision without a tool-call identity.
Output continuation, authorization, and other guard actions cannot use this direct-history marker.
Cloning isolates the review object and encoded decisions from later mutation.
The review and edit content retain their exact whitespace.

## Verification

Focused continuation tests pass on 2026-09-28.
`pipeline_hitl_history_test.go` covers mismatched identities, multiple interrupts, incorrect continuation kinds, malformed content, and mutable aliases.
Existing application and ordinary continuation tests pass in the same command.
No browser acceptance or deployment occurs for this preparation.

### Transaction verification, 2026-09-28

The pending Main implementation creates the review, decision, and continuation segments within the admission transaction.
`db/queries/agent_pipeline_hitl.sql` owns segment writes through generated SQL bindings.
`infra/db/repos/agent_pipeline_hitl_history.go` checks the persisted review before those writes.
`CurrentContinueTurn.ProjectionResponseID` supplies the deterministic response identity for the resumed execution.
The implementation uses existing tables and requires no migration.

`TestPostgresDirectPipelineHITLHistoryAtomicSegments` passes against an isolated PostgreSQL test database.
Approve, Reject, and Edit each verify exact content, rollback, duplicate rejection, response ownership, and retained thread identity.
This result does not prove competing transactions, browser history, or restart recovery.
The transaction changes remain uncommitted and undeployed at this checkpoint.

The continuation service test verifies that direct review binds browser and execution projection to the same new response.
The nested case retains the existing response.
Both cases retain the graph thread and execution generation.
UI inspection finds that `useChatBoxHandlers.applyHitlOptimisticUpdate` marks the old response as streaming for direct review.
The pending transport change reconciles this optimistic state after admission returns a different response identity.
`useChatStreamTransport.reconcileResume` reads bounded persisted history before subscribing to the new response stream.
It restores the static review and exact user decision, while retaining the original review's question link and available trace details.
It does not copy partially generated response content into stream replay.
Nested resumes retain their existing response and do not request this history refresh.
A failed refresh reports reload guidance and still subscribes to the accepted execution, without repeating admission.
All 43 transport tests pass, including direct segmentation and failed-refresh coverage.
These are component tests with HTTP fixtures, not browser acceptance.

### Deployment and browser checkpoint

Commit `e90e1bef0` contains the Main transaction and UI reconciliation changes.
Main and web build from a clean archive of that commit and deploy to rehearsal with existing configuration preserved.
Main image: `sha256:1c2a555bc7d8c0f55321bb2d09980fe99e33f4f0b1b58037b45732a06a2bd1a4`.
Web image: `sha256:6f8d67e831c29d5d860ec0b40312ddeddb9ea28a72417676fdf3c61086cd5e2d`.
A fresh headed Playwright browser reaches the pipeline page without request mocks.
The application-list request returns HTTP 500 before a pipeline can be selected.

`infra/db/repos/applications.go` contains two stale query fragments.
The lateral join already selects one version, but `DISTINCT ON` conflicts with name or date ordering.
The query also selects `shared_id` after the corresponding scan destination was removed.
Removing both fragments restores the existing list contract without schema changes.
The existing `TestApplicationsRepoPostgres_List` suite passes against isolated PostgreSQL databases after this correction.
That suite covers filters, folder isolation, pagination, tags, author attribution, and fork metadata.
The corrected Main image and HITL browser decisions still require verification.

### First live review and admission correction

Main `1e930fbb5` deploys as `sha256:0d5e257f32770b2c6bcd063c4fc92dcb71a77cb41a4dd2d9a3ce387f21a23905`.
The fresh browser now loads the pipeline list and opens the existing `Hitl_node` application.
Its saved node IDs contain spaces, which the current graph validator rejects.
Application 130, version 137, copies that graph with normalized node IDs for isolated history verification.
The original application remains unchanged.
Legacy graph-ID compatibility remains a gate 5 requirement.

The editor Test chat reaches a real HITL pause in conversation 745 through the deployed worker and synthetic provider.
Execution `0aa41023436a76455ffc2acffa66e77b` supplies the static review and decision controls.
This proves test-chat wiring, not real-model quality.
The composer creates its test conversation on first focus; the browser waits for provisioning before submitting.

Approval returns HTTP 400 before decision consumption.
The admission service still compares the execution target with the paused response ID.
It must use `ProjectionResponseID`, as the repository admission check already does.
The corrected admission test exercises the real admission service, not only the continuation service's recording stub.
It rejects the old projection for direct review and accepts the new projection while retaining the paused identity.
Focused admission and HITL tests pass.
Deployment and another approval check remain pending.
The UI's socket fallback after this refusal produces misleading connection guidance and also needs correction.

## Original implementation checklist

The admission and segmentation items below are implemented. Full browser acceptance remains open.

Consume the pending interrupt and create all history segments in the same admission transaction.
Preserve the existing response as the static review.
Create the user decision and a new assistant response with deterministic identities for retries.
Bind the new execution and browser stream to the new response.
Retain the original graph thread and checkpoint identity.
Recheck the persisted review under the transaction lock before writing segments.
Verify Approve, Reject, and repeated Edit through direct participants and ephemeral Test chats.
Verify that nested interrupts retain their existing parent response.
Test competing decisions, duplicate delivery, rollback, restart, and reload before accepting this history contract.
No database migration is required by the current design.

## Real-model browser verification

Main revision `6d78ce4b2` fixes continuation admission. A fresh headed browser accepts approval in chat 745.
The synthetic result persists, but the live response stays empty until reload.

The isolated pipeline initially stored Haiku's display label instead of its canonical identifier.
Model resolution therefore selected the project's fixture default.
Selecting and saving Haiku through the pipeline editor corrects the saved identity.
The canonical model is `eu.anthropic.claude-haiku-4-5-20251001-v1:0`, owned by project 1.

Chat 748 uses the real model. It produces a computer joke.
The user edit requests a polar-bear joke. The resumed LLM produces that joke and returns to review.
The exact edit text appears as a separate user message. Approval then persists the revised joke as the final answer.
Reload shows review, edit, revised review, approval, and final result in order.
No browser responses are mocked.

The live final response remains empty before reload.
`chatStreamTurnFrames.ts` ignores terminal snapshot text when `should_continue` is true.
A direct HITL resume creates an empty response and can reach END without model chunks.
The reducer now consumes the terminal snapshot when the response is empty.
Existing nonempty continuation content remains unchanged.
The regression checks immediate rendering, settled controls, and duplicate terminal delivery.
Deployment and another fresh browser decision remain required for this renderer fix.

### Deployed renderer acceptance

Web revision `7dd806f81` passes all 98 focused reducer tests.
The deployed image is `sha256:37076c2e4729698888ba4bfe05c7bf64e1da99f8583d03cc09c419e9359d9ff2`.
Fresh headed Playwright runs generate real Haiku jokes, then exercise Approve and Reject in the pipeline test chat.
Both show the static review, separate user decision, and preserved final joke without reload.
The markers are `GATE5_HAIKU_FINAL_20260928` and `GATE5_HAIKU_REJECT_20260928`.
The editor correctly displays the saved Haiku model.
The test composer still displays the project default, although the saved pipeline model executes.
This model-display mismatch remains open. It does not invalidate the verified provider output.
Repeated edit, nested-scope isolation, concurrent decisions, and restart acceptance remain open.

## Main-chat acceptance and test-surface scope

Main chat is the primary acceptance surface for persisted history and recovery.
The pipeline editor test chat is ephemeral. It must retain run history separately, without requiring a permanent user conversation.
Execution checkpoints and pending decisions still need durable state for recovery during the test.

A fresh headed browser starts another Haiku turn in main chat 748 with marker `MAIN_HITL_20260928`.
The run generates a database joke, pauses for review, and accepts approval.
The final joke appears immediately. Reload preserves the review, user decision, and final joke.
This extends the earlier editor checks to the main-chat surface.

`usePipelineTestConversation.ts` currently creates a private conversation through the ordinary conversation API.
That implementation is not proof of the intended ephemeral lifecycle.
Gate 5 must separate test transcript retention from durable run history and recovery state.
The model-display mismatch also remains open.

## Paused-worker restart and repeated edit

Main chat 748 starts a real Haiku review with marker `HITL_RESTART_20260928`.
The rehearsal worker restarts after the database confirms zero active claims.
The worker start time changes from 14:18:58 UTC to 16:26:21 UTC on 2026-09-28. Its image remains unchanged.
A fresh browser submits an edit requesting a penguin joke.
Execution `c3398f51c5979476585b9293ae609605` produces the revision and returns to HITL.
A second edit requests two short lines. Execution `09560fd3df713a23c48d5370fe8cd56d` returns to HITL again.
Reject completes through execution `3f8c9cfac9cb4cbdbbe5d97962aa1645`.
The browser shows both exact edit requests, separate reviews, the rejection, and the preserved final answer.
This proves restart recovery from a persisted pause. It does not prove recovery during an active model request.

The editor test transcript may disappear on browser reload. This is an accepted lifecycle, not a defect.
Both chat surfaces require functional tests. Main chat retains priority for history and recovery verification.

## Competing decision transactions

`TestPostgresDirectPipelineHITLHistoryCompetingDecisions` verifies two independent transactions against real PostgreSQL.
The test observes `pg_blocking_pids` before releasing the first transaction.
This proves lock contention rather than relying on goroutine scheduling.

- If the first approval commits, the competing rejection returns `ErrCurrentAgentHITLAlreadyResolved`.
- If the first approval rolls back, the competing rejection commits successfully.
- Both cases retain four message groups: original question, static review, one decision, and one continuation response.
- The continuation task binding belongs to the committed decision.

The new test and existing atomic segmentation tests pass with `go test -race` on 2026-09-28.
Tests use isolated temporary databases. The rehearsal product database remains unchanged.
This evidence covers repository transactions. Concurrent HTTP admission and worker dispatch require separate end-to-end proof.
