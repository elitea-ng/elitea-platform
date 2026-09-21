# Native model history retention

Status: deployed candidate. Single-compaction browser acceptance passes. Repeated live compaction remains open.

## Behavioral reference

This change implements the replatform compaction contract.
It does not port the current Python platform summarization mechanism.
The user requires compaction during long executions, preserved authority, and durable recovery.

## Source mapping

| Source or contract | Rust implementation |
| --- | --- |
| ADK 2.2.0 `adk-agent/src/llm_agent.rs`, model callback loop | `vendor/adk-agent/src/llm_agent.rs`, optional prepared-history retention |
| ADK 2.2.0 `adk-runner/src/runner.rs`, root and transfer streams | `vendor/adk-runner/src/runner.rs`, optional session refresh |
| Elitea durable prepared model request | `src/agents/model_checkpoint.rs`, history retention activation |
| Model-local child compaction | `src/agents/model_scope.rs`, scoped history retention activation |
| Elitea native invocation | `src/agents/runtime.rs`, session refresh activation |
| Durable active event projection | `src/agents/runner_history.rs` and `src/state/postgres_session_active_events.sql` |

## Implementation

The native agent clones its accumulated history before model callbacks.
Previously, callback compaction changed that request but left the accumulated history unchanged.
The extension retains the prepared request after successful callbacks and before provider dispatch.
Callback response overrides do not replace accumulated history.

The runner previously accumulated partial stream events in its mutable session.
The extension preserves stream delivery and reloads the active session view after durable event writes.
The worker retains checkpoint ownership and generation fencing.
The immutable event ledger remains unchanged.

No product database migration is required.
The original mutable-session snapshot remains allocated. Process memory measurements remain necessary.

## Delta checkpoint assessment

ADK graph `DeltaCheckpointer` stores changed state between periodic full snapshots.
Its default full snapshot interval is ten steps.
The pinned implementation lists checkpoints and reconstructs previous state when it calculates a delta.
It can reduce storage, but it does not release the agent model-history vector.
This change therefore does not activate graph delta checkpoints.

## Verification

The focused worker tests pass with the candidate dependencies.
Four tool calls previously produce callback history lengths `[1, 3, 5, 7, 9]`.
Prepared-history retention produces `[1, 3, 5, 5, 5]`.
The stream test retains two session events instead of 66.
Both modes deliver 64 partial events and one final event.
These tests prove retention behavior, not summarization quality or process memory consumption.

The isolated candidate and actual worktree each pass 374 agent tests with PostgreSQL.
The worktree suite reports zero failed or ignored tests.
Clippy, formatting, and the release build pass.
Repeated live compaction remains a separate acceptance requirement.

See `../../vendor/README.md` for package provenance and upgrade requirements.

## Rehearsal deployment

The release build uses `cargo auditable build --locked --release`.
The candidate image is `elitea-worker-rust:native-history-20260921`.
Its digest is `sha256:8c5d60e206e88da44ec9a1979ea1f8172c67c59c960aaee1c6eda461a8acb569`.
The replacement preserves five mounts, environment values, networks, and resource limits.
The deployment check confirms no active execution claim before replacement.
Chat 607 starts fresh browser verification with fictional project notes.
Chat 607 preserves all four facts and passes reload checks without page errors.
The input estimate falls from 353,853 to 866 tokens.
A fresh browser also verifies the persisted answer after reload.
Container memory samples reach 35.93 MiB during this run.
These samples are not a baseline comparison or a repeated-compaction memory bound.
The browser uses real providers and does not intercept model requests.
