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
The current nested fatal behavior remains until that separate contract is implemented and verified.
