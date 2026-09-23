# Continuation failure diagnostics

## Behavior reference

Current SDK source: elitea_sdk/runtime/tools/llm.py, _continue_nested_output and parallel application result handling.
The SDK reports continuation attempts, failure reason, stop reason, and partial-output availability.
Parallel children return this information in an error ToolMessage.
The sequential tool loop raises OutputContinuationExhausted.
These paths do not have identical parent recovery behavior.

## Rust mapping

src/agents/model_checkpoint/output.rs owns payload-free typed continuation causes.
src/agents/model_scope_output.rs attaches a cause at each failed continuation condition.
src/agents/application_tools.rs preserves the typed cause through the nested fatal channel.
src/agents/runtime.rs exposes only the typed cause to logging.
src/execution/native_agent_lifecycle.rs logs unrecovered start and stream failures at ERROR.
Each event includes execution identity, generation, stable codes, a cause message, and the existing diagnostic stack.
Handled authorization challenges return before this terminal error log.
Recoverable retries retain their existing severity.

Raw provider messages and arbitrary error chains remain excluded.
The public error code and database schema do not change.
Release line tables remain enabled.

## Validation and remaining work

Focused continuation tests cover the four-call limit, repair, partial output, and graph recovery.
A cause test verifies safe extraction through wrapping and rejects arbitrary provider text.
The nested channel test verifies that the original typed cause reaches its receiver.

This change does not complete observability.
Original failure-site stacks and suspended asynchronous ancestry need further work.
General user error guidance and copyable UI correlation identity also remain open.

The user clarified that child-local failures must not terminate a healthy orchestrator.
Add a structured child error result, with recovery guidance and partial-output availability.
Distinguish parent recovery from permission to repeat the same operation.
Do not convert cancellation, authorization, or execution-wide limits into recoverable tool data.
The ordinary nested-agent continuation path now returns an error report instead of sending the root fatal signal.
The report includes the cause, available partial output, recoverable=true, and retryable=false.
Recovery means revising the task, not permission to repeat the same call or its side effects.
The error field remains a string for the existing tool-error UI projection.
The structured failure field gives the parent explicit recovery guidance.
A failed report has no successful response field.
The failed child does not create a completed model receipt.
The parent can persist the error tool result through its existing checkpoint flow.
Direct root continuation failure still uses the terminal failure contract.
Other child failure categories and graph propagation require separate classification and acceptance.

## Pipeline boundary clarification

An LLM orchestrator can reason about failed child output.
A direct graph Agent node cannot treat that output as valid downstream state.
The invocation-owned pipeline application marker distinguishes these callers.
A marked Agent node retains the typed fatal continuation signal and stops the graph.
The existing direct LLM-node path also stops on continuation exhaustion.
The focused suite includes downstream-state protection and the new Agent-node classification.
Pipeline-as-child propagation remains a separate verification item.

## Error propagation acceptance matrix

Use the execution boundary, not the application type name, to select behavior.

| Caller | Failed operation | Required result | Evidence |
| --- | --- | --- | --- |
| Chat orchestrator | Agent child continuation | Return a failed tool report; let the parent decide | Local tests and deployed browser acceptance pass |
| Direct pipeline | LLM node continuation | Stop before downstream state is written | Existing focused graph test passes |
| Direct pipeline | Agent node calling an Agent application | Stop with the typed failure | Caller classification test passes; live graph acceptance remains |
| Pipeline | Nested pipeline | Propagate failure; do not release partial state as success | Open |
| Pipeline Agent node | Pipeline application | Propagate failure to the owning graph | Open |
| Chat orchestrator | Pipeline child | Stop the child graph; report its contained failure to the parent | Open |
| Any scope | Cancellation or execution-wide limit | Preserve control semantics; do not offer automatic retry | Existing handling retained; combination tests remain |

The legacy platform does not establish acceptance for all nested combinations.
Verify each path against the intended state and error contract.
Keep graph expansion in gate 5, but track error propagation under gate 4.
Do not claim that all child error categories are handled by the continuation-specific correction.

## Deployed acceptance on 2026-09-23

Worker revision d705fa9a is deployed to rehearsal with existing configuration and mounts.
A fresh headed browser runs the original chat 637 without response fixtures.
The child exhausts continuation. The parent receives its error report and completes normally.
The parent explains the four-call limit, partial-output availability, and the prohibition on an identical automatic retry.
The response remains after reload. No root execution failure or browser error occurs.
The rendered screenshot is inspected.
Local evidence: elitea-nested-report-live.json, elitea-nested-report-live.png, and elitea-nested-report-frames.json.

The worker emits ERROR for nested_application_failed.
Its cause_message states that the answer still reached the output limit after four continuation calls.
The event includes application, version, invocation, tool-call identity, and the diagnostic trace.

The full PostgreSQL-enabled worker suite passes 1,212 tests before the pipeline-boundary refinement.
The refinement passes 31 focused continuation tests and strict all-target Clippy.
The refinement includes the direct Agent-node failure classifier and existing downstream graph-state checks.
Live direct Agent-node and wider nested-pipeline acceptance remain open.
