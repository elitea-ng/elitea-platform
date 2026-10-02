# Compaction-aware Runner history projection

Status: implemented and verified in rehearsal. Broader gate 4 acceptance remains open.

## Source and ownership

Rust `src/agents/runner_history.rs` already separates model history from durable approval-control records.
Rust `src/agents/session.rs::RunnerSessionService` applies that projection only when loading the Runner session.
Direct approval and recovery readers retain the underlying durable session.
Rust `src/agents/context_compaction.rs` owns the committed content anchor, coverage digest, and replacement records.
Pinned ADK runner 2.2.0 `MutableSession::conversation_history_for_agent_impl` maps event authors into model roles.
The candidate matches that role mapping before checking coverage.

Current-platform compatibility remains in [summary compatibility](context-summary-compatibility-20260918.md).
The user explicitly requires improved compaction instead of copying the legacy implementation.
This candidate extends the existing ADK session adapter; it adds no summarizer or database schema.

## Candidate behavior

The ordinary Runner receives its current definition digest and agent name.
A saved summary applies only when that definition, the original user anchor, and the covered content prefix match exactly.
Changed source content or a foreign definition keeps the original model history.
Shared branch history and existing native timestamp compaction also keep their current projection.
They require explicit coverage mapping before retirement.

The view clears only covered model content and inserts the saved replacement after the covered prefix.
The original anchor, recent messages, event identities, and control actions remain present.
Projected summary events stay local to the Runner view and never enter the durable ledger.
Storage events and current session state remain unchanged.

## Verification boundary

The focused regression passes the projected session into the native ADK MutableSession history reader.
It checks exact anchor, summary, and recent-message order, retained transfer controls, unchanged storage, changed-content refusal, foreign-definition refusal, repeated projection, and sibling-branch refusal.
The full agent suite passes 371 tests with PostgreSQL enabled and no reported skips.
Clippy passes for the library and tests with warnings denied.
These checks do not replace deployment and browser verification.

The wrapper now copies session identity and state, then releases the original event snapshot.
A lifetime regression verifies that release and preserved identity, timestamp, and state.
Active invocations can still accumulate new events.
It does not claim bounded process memory or repeated-compaction retention.
The storage integration below passes the documented rehearsal checks.
In-process history replacement remains open.

## Prepared-request snapshot candidate

`model_checkpoint.rs::before_model` already persists the complete prepared request before provider dispatch.
The candidate adds `elitea.agent.history_snapshot.v1` to that same fenced event.
The descriptor identifies the agent and format version.
It does not change the recovery checkpoint format or database schema.

`postgres_session_active_events.sql` retains the latest marked model snapshot and the latest recovery marker.
It also retains later events, control records, and events from other agents.
A later tool checkpoint cannot replace the retained history snapshot or its capacity charge.
The latest recovery state remains authoritative for replay permission.

`runner_history.rs::project_snapshot` restores the snapshot contents before later model events.
It checks the definition, agent, checkpoint phase, and invocation identity.
It excludes old system instructions because current instruction binding supplies that authority.
Repeated projection does not duplicate synthetic history events.
Synthetic events never enter the immutable ledger.

Branch history disables ordinary-event retirement.
Admission accounts for a new branch before accepting its event.
This prevents an accepted branch from exposing previously excluded history beyond the active capacity limit.

The PostgreSQL regression runs six snapshot cycles with a five-event active limit.
It checks retained controls, current tool-boundary state, capacity rejection, and exact replay after retirement.
All 25 original events remain in the immutable ledger.
Separate model-history checks verify ordering, current steering, and foreign-scope refusal.
The updated agent suite passes 372 tests with PostgreSQL enabled.
The session suite passes eight tests, including new-branch admission.
The synthetic query probe uses 128 snapshots with 512 KiB payloads.
Three candidate runs take 27.9–28.7 ms; the existing query takes 24.7–25.2 ms.
Neither query reports temporary spill.
This probe does not establish production latency or long-ledger scaling.
Clippy passes for the library and tests with warnings denied.

## Rehearsal verification

The worker image is `elitea-worker-rust:history-snapshot-20260921`.
Its digest is `sha256:27f161a2f9e3756f271ae74e3d3b4299a1a7b5bb04f5486eea56fc8b4ab0ed9d`.
The build uses base commit `1e2a7849` plus the ten Rust candidate files.
The build manifest matches the working source hashes before commit.
Deployment preserves the worker environment, all five mounts, networks, and resource limits.

Fresh headed Playwright sessions use real providers without request interception.
The task model is `gpt-5.4-mini`; the summary model is `global.openai.gpt-5.6-luna`.
The fixture contains 18 fictional project-note messages with 1,407,395 text bytes.

Chat 605 compacts 353,853 estimated input tokens to 913 tokens.
Its answer preserves the delivery code, corrected color, completed archive check, and handoff task.
The answer remains unchanged after reload.
The graceful restart produces one claim, so this run does not prove takeover.

Chat 606 compacts the same fixture to 878 estimated input tokens.
The harness sends SIGKILL after the committed-snapshot status reaches the browser, then starts the worker.
Execution `718f5a2a977b0c54f984e07135e6c953` uses two claims and finishes with no unreleased claim.
The resumed answer preserves all four facts and remains unchanged after reload.
The browser records no page errors, and the final screenshot is inspected.

These runs prove the observed model-boundary recovery case.
They do not prove repeated live compaction, nested browser recovery, or bounded memory during one long invocation.
The session component test proves repeated storage retirement separately.

## Remaining integration requirements

The next boundary must identify covered durable events by exact identity and order, not timestamp alone.
Commit the boundary with the validated replacement and prepared model request under the existing writer fence.
Keep the original anchor and pending control evidence available to recovery readers.
Verify cumulative coverage when a later summary replaces an earlier summary.
Count the active replacement and retained events against capacity while keeping immutable replay rows.
Do not let a model-generated summary choose event identities or authorize retirement.

Event-count pressure also needs admission coverage when token use remains below the compaction threshold.
A long sequence of small tool results can reach 4,096 events before the model window fills.
Raising that cumulative cap alone would postpone the failure rather than provide bounded retention.
In-process memory bounds remain unimplemented in this candidate.
