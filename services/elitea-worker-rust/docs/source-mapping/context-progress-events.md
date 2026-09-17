# Model context progress events

Status: Rust component implementation, 2026-09-17. Main's status read model and live UI handling remain open.

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

Required next work: Main's accepted-event status projection and restart-safe reads; UI invalidation/replay and nested activity text; terminal/error/cancellation reconciliation so a failed execution cannot remain labelled compacting; deployed browser/model-loop acceptance.
Do not aggregate different model scopes or show estimated counts as exact tokenizer measurements.
The current UI unknown-state reading is intentional until this projection is connected.
No protocol field, migration, new table, or rehearsal deployment changes in this slice.
