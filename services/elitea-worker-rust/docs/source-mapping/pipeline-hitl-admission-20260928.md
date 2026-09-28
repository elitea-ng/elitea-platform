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

## Remaining implementation

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
