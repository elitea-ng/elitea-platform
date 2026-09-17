# Model context progress events

Status: Rust events, Main projection and UI components verified, 2026-09-17. Deployed model-loop acceptance remains open.

## Source and ownership mapping

| Functional source | Rust integration |
| --- | --- |
| Centry `elitea_core/utils/context_analytics.py` exposes runtime-written context analytics. | `agents/context_budget.rs` measures the actual prepared provider body and supplies the window, output reservation, safety margin, and usable input. These are per-request estimates, not cumulative billing counts. |
| Current EliteaUI context-budget components show utilization; the new user requirement adds visible compaction activity. | `agents/context_status.rs` defines a bounded content-free status contract, and `agents/events.rs` projects `agent_context_status` events. Root UI shows details; children need only brief compaction activity. |
| ADK before-model callbacks own request preparation; ADK events own the invocation stream. | `model_checkpoint.rs` publishes progress through the existing bounded `graph/node_events.rs` bridge. No detached task, unbounded channel, or ADK fork is added. |
| Elitea owns durable recovery and execution identity. | Existing checkpoint writes and accepted NodeEvent delivery retain their ownership. Main must derive a display projection from accepted events, without reading worker checkpoints. |

## Contract and ordering

`response_metadata.context_status` contains version 1, phase (`measured`, `compacting`, or `compacted`), budget mode, total window, usable input, reserved output, safety margin, estimated input, and the trigger/target token counts.
Mode `legacy` represents a previously admitted explicit window; it must not be presented as Full or Balanced without evidence.
The payload contains no instructions, tool data, summary text, credentials, or provider request body.
Parsing checks the closed schema, budget arithmetic, trigger/target ratios, phase-specific capacity, and the encoded-size bound.

The callback emits `compacting` only after the `context_pending` checkpoint is written, using the actual source measurement after any previous summary is applied.
It emits `compacted` only after the replacement summary and `model_pending` request are persisted together.
An ordinary measured request emits `measured` after its pending checkpoint is written.
Failure before the replacement write emits no successful completion notice.
Recovery remeasures the restored request; it does not fabricate a second completed compaction.

The bridge allows the notice to reach the caller while the summary model is still pending.
Events are marked as partial progress with no model content, preventing model completion adapters or transcript assembly from treating a notice as an answer.
The projector preserves the current model-turn boundary and existing child call hierarchy.
`model_scope` distinguishes an agent from a `pipeline_node`; `node_name` carries the graph node where applicable.
Existing execution/generation envelopes and `parent_agent_path` identify the invocation. Children do not update a parent meter.

## Verification and remaining integration

350 agent checks pass with real PostgreSQL fixtures and no environment skips.
Tests cover independent child scopes, replacement and claim fencing, malformed status refusal, exact nested hierarchy, pipeline scope, summary-write failure, and recovery without re-summarization.
A blocked-summary test observes `compacting` while the durable checkpoint remains `context_pending` and the task model has not run.
After releasing the summary, it observes `compacted` only with the saved `model_pending` request.
Clippy with warnings denied and Cargo formatting pass.

## Main presentation projection and UI integration

`services/elitea-main/internal/infra/db/repos/agent_context.go` consumes only
accepted root `agent_context_status` events inside the existing NodeEvents
transaction. It validates the closed measurement schema and arithmetic, matches
immutable stream/message/generation admission, and locks the bound response.
Duplicate and stale events remain subject to the existing sequence and claim
fences before this projection can run. Agent children and pipeline-node scopes
cannot replace the root measurement.

The projection uses `chat_message_group.meta.runtime_context`; no new table or
migration is needed. It records measurement, execution identity, client generation,
and database timestamp. `ConversationsRepo.GetContextState` reads the latest active
response, otherwise the latest updated response, with a deterministic ID tie-break.
It checks the current task and client generation before returning the snapshot.
A fresh/regenerated response without a measurement does not search older responses
for a convenient count. Main neither owns nor reads compaction checkpoints.

`domain/contextsettings/measurement.go` combines that record with the existing
status route. It derives activity from `is_streaming`, so a failed/cancelled
response cannot claim that compaction is still running. It preserves the actual
last phase rather than fabricating a successful compaction. Legacy frozen budgets
are labelled as an existing run, not guessed to be Balanced or Full. Message and
summary counters that this runtime does not supply remain explicitly unavailable.

`apps/elitea-web/src/widgets/context-budget` shows estimated input relative to
usable input, plus total window, reserved output, safety margin, and the 90%
trigger. The composer shows “Compacting context…” with accessible status text.
The details distinguish completed compaction from one stopped before completion.
The transcript itself is not removed by compaction.

`features/chat-messages/model/useChatStreamTransport.ts` invalidates the existing
context-status query on admission, root measurement, terminal response/failure,
and cancellation. Active status reads reconcile every five seconds while mounted;
idle conversations do not poll. This also covers missed events after refresh.
`chatStreamContextFrames.ts` uses exact generation and child-call/node hierarchy
for compact activity notices. It never resets answer text, tool results, or
interrupts. Parallel same-name children remain separate. Terminal/failure paths
settle pending notices. Child notices currently use live/replayed activity;
a durable child-activity projection beyond replay retention remains open.

Additional verification:

- 915 Main tests and eight real-PostgreSQL integration checks pass; no skips.
- PostgreSQL proves fresh-reader persistence, child/node exclusion, admission
  mismatch refusal, terminal inactivity, and regenerated-response isolation.
  Existing NodeEvent retention/terminal tests also pass. This does not claim a
  new full gRPC/SSE deployment test.
- 386 focused UI checks pass, including parallel child isolation, unchanged
  answer content, duplicate progress, lifecycle refresh, and stopped notices.
- TypeScript, changed-source Oxlint, and the app build pass.
- Fresh headed Playwright checks the accessible popup and the compacting,
  compacted and interrupted states, including reload. Screenshots are inspected.
  Context status and profile writes are intercepted; other reads use the rehearsal.
  This is browser presentation proof, not deployed end-to-end worker proof.

Temporary evidence uses the `elitea-context-progress-` and
`elitea-point4-context-progress-` prefixes under `/private/tmp`.
Required next work: compatible Rust/Main/UI deployment, actual model-loop
compaction and reconnect/restart acceptance, and durable nested activity beyond
replay retention. Do not aggregate model scopes or present estimates as exact
provider tokenizer measurements. No rehearsal deployment occurs in this slice.
