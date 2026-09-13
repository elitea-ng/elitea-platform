# Agent crash continuation

Status: implementation required. This document defines the next recovery change, not a passing gate.

## Confirmed failure

The [Main crash check](chat-restart-observer.md#main-process-crash-acceptance) separates browser observation from runtime continuation.
The browser reconnects correctly. The interrupted agent becomes a failed execution after reclaim.

The failure does not require a worker crash. Main also carries the active model transport used by this deployment.
The worker reports a model stream failure when Main stops. Control supervision loses contact at the same time.

## Current source mapping

| Owner | Source | Current contract |
| --- | --- | --- |
| Main | `internal/infra/db/repos/claims.go` | A running invocation beyond `PREPARING` receives ambiguous recovery authority. |
| Rust | `src/execution/agent_delivery.rs` | Output recovery receives no business input and cannot construct a fresh invocation. |
| Rust | `src/execution/agent_delivery_processor.rs::process_output_recovery` | A running ambiguous invocation becomes an internal terminal failure. |
| Rust | `src/agents/session.rs::assemble_ordinary_native_with_sessions_and_runtime_catalogs` | Restores ADK session history, then constructs an ordinary Runner invocation. |
| Rust | `src/agents/session.rs::RunnerSessionService` | Enriches completed model events before durable persistence. Partial model output is not a completed event. |
| Rust | `src/agents/replay_history.rs` | Normalizes exact call/result pairs and rejects conflicting replay history. |
| Rust | `src/agents/replay_history/recovery.rs` | Repairs one unavailable tool selection before semantic output. It does not recover a crashed invocation. |
| Rust | `src/agents/graph/turn_checkpointer.rs` | Retains graph checkpoints and isolates fresh turns from another execution's frontier. |
| Rust | `src/state/postgres_session.rs` | Persists sessions and events with writer fencing in the existing runtime state database. |
| ADK 2.0 | `adk-runner/src/runner.rs::run_with_config` | Accepts new user content and creates a new invocation ID. Reusing this entry point alone is not exact crash resume. |

The legacy platform remains the business reference for chat, tools, and pipeline behavior.
It does not supply the requested crash-continuation guarantee. Do not copy its restart limitations into this implementation.

## Required implementation boundary

Keep output-only recovery for completed output and uncertain external effects.
Add a separate, typed continuation path for a validated durable execution checkpoint.
Do not turn every ambiguous claim into an ordinary accepted claim.

Use the existing ADK session and graph checkpoint stores. Do not introduce a parallel product database schema.
Bind each recovery checkpoint to the execution, generation, frozen input revision, and current claim authority.
Persist the execution position before opening a model request or invoking a tool.
Preserve completed call/result pairs by their existing IDs. Never repeat them to reconstruct session history.

Distinguish these positions:

- A model request is pending. Restart that model step from its durable inputs, without resubmitting the user turn.
- A tool result is committed. Continue from that exact result.
- A tool invocation has an uncertain outcome. Require its recovery receipt or reconciliation contract before proceeding.
- A graph node is complete. Resume the saved graph frontier without running the completed node again.
- A terminal result is durable. Recover its publication and settlement without invoking the model.

Preserve authoritative skills, project context, persona, and frozen model settings during reconstruction.
Use a new output attempt identity to replace unfinished provisional text. Do not append a regenerated answer to its partial predecessor.
Do not confuse an execution replay cursor reset with replacement of model text.

The typed authority transition and checkpoint validation must precede business input hydration on a replacement worker.
Cancellation, deadline expiry, foreign scope, stale writers, and unsupported checkpoints must refuse execution.
Keep recovery attempts bounded by durable execution policy, not by an in-memory retry counter alone.

## Acceptance order

1. Interrupt a model step after partial text. Verify one user turn and one final answer after recovery.
2. Complete a tool call, then interrupt the next model step. Verify the tool executes exactly once.
3. Interrupt an in-flight tool. Verify no automatic duplicate effect occurs.
4. Replace the worker with no local spool. Verify the same checkpoint and output identity rules.
5. Repeat with a saved agent and an autonomous pipeline through an external MCP client.
6. Verify cancellation, stale claim rejection, and terminal output recovery during each transition.

The external MCP transport must also reconnect to the original execution. A repeated POST is a separate invocation today.
Browser reconnection, session persistence, and safe terminal failure remain separate evidence from successful continuation.
