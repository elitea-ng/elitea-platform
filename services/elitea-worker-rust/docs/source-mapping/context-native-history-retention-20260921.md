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

## Repeated native loop verification

`native_tool_loop_compacts_repeatedly_with_persisted_coverage` executes 24 tool calls and 25 model calls in one native ADK run.
It uses the production compactor with fixture models and an in-memory session service.
The test writes summary coverage before it commits each prepared history replacement.
Seven summary calls complete. Every ordinary model request fits its configured input budget.
The original user task remains exact at every ordinary model call.
Serialized callback requests peak at 23,346 bytes instead of growing with all 24 tool results.
The final session contains the persisted compaction record.
This test proves repeated native-loop integration, not live-provider behavior or process RSS bounds.

## Live loop fixture

`tests/acceptance/compaction_mcp_fixture.py` serves twelve fictional archive records through a read-only MCP tool.
Each record includes an index, the next index, task facts, and bounded evidence text.
The fixture requires explicit test-only enablement and TLS certificate paths.
It does not read external data or change product records.
The rehearsal runs it on a separate port without restarting the existing OAuth fixture.
TLS tool discovery passes. Twelve valid indexes and four invalid index cases pass local checks.
Chat attachment and repeated live compaction acceptance remain open.

Chat 608 reads all twelve records in order through the real MCP endpoint.
Its real-provider run completes one compaction from 113,859 to 13,831 estimated input tokens.
The final answer preserves the delivery code, corrected color, archive status, remaining step, and record count.
The browser reports no page errors during this run.
The intended test configuration has a 32k window, but runtime events report 128k.
The run therefore fails the requirement for at least two live compactions.
The participant model selection and budget propagation require verification before another acceptance claim.

Chat 609 reproduces the 128k selection despite an explicit 32k model selection in the browser request.
Configuration 17 stores the intended limits, but its connection status is false.
The catalogue excludes that entry, and Main selects the configured default model.
The stored connection endpoint rejects `llm_model` with reason `unsupported_type`.
No configuration status is changed manually to bypass that missing verification path.
Chat 610 uses the validated default model and requests two independent passes through the twelve records.
Its two attempts fail acceptance.
Executions `2c8a53170706c15a6be9301c6fe16346` and `612d1970fd87dae74fa9811366cfc590` each record twelve actual tool calls.
Both settle as `FAILED`, despite showing only partial text without a visible error in the browser.
Worker diagnostics report `native_agent.event_failed` with upstream code `context_budget_exceeded`.
The settlement error-code columns are empty.
The continuation attempt completes one compaction from 151,523 to 1,077 estimated tokens before its later failure.
These results require investigation of grouped tool-result capacity and terminal error projection.
They do not prove a model refusal or successful repeated compaction.

## Completed tool batch capacity correction

The recent-history preference keeps at least one message during cutoff selection.
A single message can contain twelve completed tool results and exceed the entire input budget.
This prevents compaction before the summary model receives a request.

`src/agents/context_compaction.rs` now permits a complete-history cutoff when the retained tail exceeds the input budget.
The cutoff applies only when every tool call has a matching result.
Existing checks preserve pinned instructions and the latest user request.
The change does not truncate tool results or change the durable source ledger.
No database migration is required.

`completed_tool_batch_larger_than_recent_budget_can_be_compacted_whole` reproduces the failure with twelve results.
The corrected path produces a request within budget and retains the original authority messages.
Removing one result proves that an unfinished batch fails without calling the summarizer.
All 22 compaction tests pass locally. The PostgreSQL-enabled agent suite and strict Clippy checks also pass.
The PostgreSQL-enabled suite completes 376 agent tests.
The auditable release build succeeds and deploys to rehearsal with five existing mounts preserved.
The candidate digest is `sha256:a810298a44cde25ed047997da2a2b717e1f26c23345a630bb7d47a12b3402d1e`.
Execution `b04fa5111486c3a45afe41b978316246` compacts 152,219 estimated tokens to 1,150.
The subsequent chat-model request fails with `model_gateway.response_header_timeout` after 15 seconds.
This run does not prove repeated live compaction acceptance.

The earlier failed chat groups contain `is_error=true` and the expected resource-limit message.
A fresh browser renders that persisted message and the new generic runtime error.
The empty settlement error columns do not prove missing error persistence.
The settlement implementation does not populate those columns for ordinary worker failures.
The earlier browser observation can precede terminal-error rendering; terminal synchronization requires further verification.
Execution `dc032cc935f4e72171d5b523660d6585` retries from the compacted history with 1,219 estimated input tokens.
It encounters the same response-header timeout before further tool execution.
This confirms reuse of the compacted history, but leaves repeated live-loop acceptance open.
The lifecycle currently maps this timeout to `Internal`; the browser consequently shows a generic runtime error.
Provider timeout classification and the configured response-header deadline require follow-up under gate 4.

ADK 2.2.0 `adk-graph/src/delta.rs` provides separate checkpoint storage compression.
It reconstructs full state from snapshots and deltas; it does not summarize model context.
Its save path loads checkpoint history and reconstructs previous state before computing a delta.
Storage savings alone therefore do not prove lower peak process memory.
This correction does not introduce that checkpointer or change checkpoint ownership.

## Follow-up acceptance

Fresh chat 612 completes 24 tool calls and two compactions with real providers.
The final answer retains all required facts and remains equal after reload.
See [terminal answer snapshots](terminal-answer-snapshot-20260922.md) for deployed images and evidence.
This closes the repeated native-loop case, not all context-management gates.
