# Terminal answer snapshot

## Observed behavior

Fresh chat 611 completes 24 tool calls and two compactions.
The streamed answer retains narration from earlier model calls in that execution.
After reload, the answer contains only the final model result.
This changes the visible transcript without another user action.

## Ownership and mapping

`src/agents/events.rs::finish_after_eos` emits the selected complete result in `pipeline_finish`, `agent_response`, and `full_message`.
Main's `internal/infra/db/repos/agent_execution_results.go` replaces provisional text with the `full_message` result.
The UI's `chatStreamTurnFrames.ts` previously ignored the `pipeline_finish` content.
It only stopped the streaming state and retained accumulated intermediate text.
The terminal observer can close before subsequent response events update that text.

The current UI reference is `projects/EliteaUI/src/common/convertChatConversationMessages.js`.
Its persisted message conversion defines the reload surface; it does not require copying the legacy streaming transport.
The replatform's durable final result is the authoritative answer snapshot.
Intermediate model and tool records retain their separate execution-trace ownership.

## Correction

The Rust terminal event now includes the existing `should_continue` metadata field.
The UI applies a string snapshot only when this field is explicitly false.
Continuation events and older events without that field retain their existing behavior.
Null content retains the complete result assembled from earlier chunk events.
Exact replay remains idempotent.
This change adds no database schema or new metadata field.

## Verification

The reducer suite passes 96 tests, including snapshot replacement, replay, continuation, and null-content cases.
The Rust event suite passes 34 tests, including the explicit terminal metadata assertion.
UI type checking and diff checks pass.
Release builds and browser reload acceptance pass as recorded below.

## Rehearsal acceptance

The worker deploys as `sha256:0c1c3a9673a0855142988cc5856f82f1ee297a68925fac34fd27d2b3070705ac`.
The UI deploys as `sha256:b89b73f939e15ffcb055fa0f11c83069d7c5f3b176c3ba23aac98e198555ec24`.
Fresh chat 612 runs execution `f3420c1127e814465ba192d1617c07c3` through two compactions.
The durable ledger confirms `SUCCEEDED`, exactly 24 tool calls, and zero tool errors.
The final report retains all required facts and does not request another instruction to finish.
The browser reports no page errors. The answer remains exactly equal after reload.
The screenshot is inspected visually.
This proves the ordinary terminal snapshot and checkpoint-orientation correction for this real-provider run.

A subsequent UI refinement applies the existing reasoning-tag parser to terminal snapshots.
The reducer regression covers that path, and UI type checking passes.
The final UI image is `sha256:84af8476b3ff188e2909552a748d180c622e0d2ac328f115331ee4924dc495a8`.
A fresh headed Playwright browser opens chat 612 with this image.
The persisted answer matches the earlier live answer and remains equal after reload.
No page errors occur. The screenshot is inspected visually.
This last check covers loading and reload; the reducer test covers reasoning-tag parsing.
