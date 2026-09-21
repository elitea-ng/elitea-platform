# Context event retention investigation

Status: gate 4 remains open. Recovery-marker retention has component, deployed restart, and browser reload evidence.

## Current ownership

Rust `src/state/postgres_session.rs` owns fenced session writes, event identity checks, and event loading.
`SessionLimits` permits at most 4,096 events and 64 MiB of retained event payloads per session.
Before this candidate, `ensure_event_capacity` counts all stored session events before each append.
`load_events` rejects an unbounded read when the stored event count exceeds the configured maximum.

Rust `src/agents/model_checkpoint.rs` writes a complete prepared model request into recovery event state deltas.
The session state and recovery marker commit in one transaction.
Recovery requires an exact matching marker and rejects a completed model response after that marker.
Deleting or rewriting historical markers would change exact-redelivery checks.

Rust `src/agents/context_compaction.rs` replaces model-visible history through a durable coverage digest and replacement record.
It does not retire stored session events. Model compaction therefore does not release the separate event-retention budget.
Current-platform reference behavior remains mapped in `context-summary-compatibility-20260918.md`.
Crash-resumable worker checkpoint retention is a replatform extension.

## Observed pressure

A read-only rehearsal query measures the latest worker session after the chat 602 follow-up.
It contains 26 events totaling 5,238,759 payload bytes.
Four recovery events account for 3,162,803 bytes.
The measurement reads counts and byte lengths only, not conversation content.
A compacted model context can therefore coexist with growing retained checkpoint payloads.

## ADK source reference

Pinned `adk-runner` 2.2.0, `src/context.rs`, supports native `EventActions.compaction` history replacement.
Its history reader selects the latest compaction event before applying branch visibility filters.
Pinned `adk-agent` 2.2.0, `src/compaction.rs`, supplies `LlmEventSummarizer` and timestamp-based compaction boundaries.
These primitives require scope and durable-boundary verification before reuse in shared nested sessions.
Do not assume a timestamp cutoff has the same meaning as the worker's validated coverage digest.

## Required proof before a change

- Preserve immutable event identity and exact-redelivery checks.
- Preserve the latest checkpoint and all subsequent outcomes needed for recovery.
- Keep pending calls, approvals, and authoritative context outside generated summary authority.
- Bound the active session view without silently dropping unsummarized model history.
- Preserve branch isolation for nested agents and pipeline model nodes.
- Verify repeated compaction beyond the existing event count and byte ceilings.
- Verify replacement-process recovery and browser reload after the retention boundary advances.

A higher cumulative event cap alone does not satisfy these requirements.

## Candidate: superseded recovery markers

`postgres_session_active_events.sql` defines a scope-bound active event projection.
It retains every ordinary event and the latest content-free recovery marker in each branch.
Events with content, artifact changes, routing, compaction, approvals, or long-running tool IDs remain ordinary events.
The SQL projection supplies both event loading and active-capacity accounting.
Incoming same-branch recovery markers replace the previous marker's active capacity within the existing fenced transaction.

Stored event rows remain unchanged. Exact replay and conflict checks still use the full immutable payload.
Current session state retains the full latest checkpoint.
This candidate needs no migration and does not delete stored checkpoints or conversation history.
It does not yet bound ordinary history across repeated model compactions.
The remaining ordinary-event retention boundary stays open.

A PostgreSQL regression uses a two-event active limit and a 12,000-byte active budget.
It appends twelve 5,000-byte checkpoints while preserving the original user instruction.
It checks exact old-event replay, changed-payload refusal, latest state, branch isolation, and all thirteen stored ledger rows.
Verification on 21 September passes all seven session tests against isolated PostgreSQL databases.
All nineteen compaction tests pass, including PostgreSQL process replacement.
Clippy passes for the library and tests with warnings denied.
The boundary tests require `session.resource_exhausted` for sibling and content-bearing events.

The rehearsal evidence below records live deployment and fresh browser verification.
The projection still scans the immutable ledger; this candidate does not bound database storage or scan cost.
The in-memory invocation history also needs separate verification during long runs.
These limitations prevent treating the candidate as complete long-run retention support.

## Query cost check

A temporary PostgreSQL table holds 128 synthetic checkpoints with 512 KiB request bodies.
The fixture rolls back without changing application data.
Repeated JSON casts take 1,258.6 ms for active-capacity accounting.
The candidate parses each event once through PostgreSQL `jsonb_to_record`.
It materializes classification fields without retaining the parsed request body in the window operation.
Three repeated candidate queries take 24.3, 24.0, and 24.4 ms.
The query plan reports in-memory window storage and no temporary-disk spill.
This synthetic check does not establish production tail latency or bounded ledger growth.

## Rehearsal recovery evidence

Worker image `sha256:c4e92a0c99fa48e00a1fcde7fd1c2dbc732647f87a3b9d9f0eec7ff4364d10e1` deploys this candidate.
The deployment retains all five worker mounts and the existing environment and resource limits.
A fresh headed browser submits fictional project notes in chat 603.
Execution `c0f49f0a7fb41d0be0d5c5b192bc9133` starts with 353,853 estimated input tokens against a 272,000 input ceiling.
The test stops the worker during compaction. A replacement claim resumes the execution.
Compaction finishes at 991 estimated tokens and produces an answer.
The saved summary preserves the code, corrected color, completed archive verification, and handoff work item.

The first answer substitutes pending approval for the explicit handoff next step.
The first browser script also times out when clicking an already-open context popover.
A follow-up answer retrieves the handoff step, code, and corrected color from the saved context.
Its browser assertion incorrectly waits for a Send button after the composer clears.
These harness failures and the initial answer mismatch remain recorded; neither run is an unconditional acceptance pass.
The aggregate ledger contains 26 events, four recovery markers, and 4,292,233 payload bytes after the follow-up.
A separate read-only browser check verifies final rendering and reload without submitting another turn.

The final fresh-browser readback passes with no intercepted requests and no page errors.
The answer remains identical after reload and contains `CEDAR-731`, `teal`, and the explicit handoff work item.
The context panel reports 1,253 estimated tokens against the 272,000 input ceiling after the follow-up.
The browser opens context details through the control's keyboard interaction.
This proves persisted availability and reload, not perfect first-answer prioritization or complete gate 4 acceptance.
