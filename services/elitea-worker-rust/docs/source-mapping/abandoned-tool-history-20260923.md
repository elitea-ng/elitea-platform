# Historical tool calls without recorded results

## Evidence

Chat 637 contains two executions in one durable worker session.
The earlier execution records a parent tool call before the child exhausts automatic output continuation.
No function response exists for that call.
The later execution appends a new user message and receives `MODEL_REQUEST_REJECTED`.
Fresh chat 662 succeeds through the same provider binding until its expected continuation exhaustion.

The old root session contains user, model function call, and later user content in that order.
Recovery control events contain no model-visible function response.
The provider-history normalizer currently deduplicates replayed calls but leaves this unmatched historical call intact.

## Source mapping

The current SDK uses `runtime/langchain/assistant.py::filter_orphaned_tool_calls` inside the swarm model setup.
It removes tool calls without matching tool results before an Anthropic request.
This is a behavior reference for valid provider history, not a literal implementation template.

ADK Runner 2.2 filters conversation history by author and branch in `src/context.rs::conversation_history`.
It does not repair this abandoned call across user turns.
Rust `src/agents/replay_history.rs::model_history` already owns provider-history normalization and preflight measurement.
The candidate correction extends that owner without adding a dependency or changing storage.

## Candidate behavior

A function call without a recorded response receives a provider-only error response when a later user text turn exists.
The response retains the exact call identity and reports an unknown operation outcome.
It prohibits treating the missing result as success or repeating side effects without checking.
It does not create a completion receipt, change durable events, execute a tool, or authorize access.
Existing responses remain authoritative, including declined authorization results.
Active calls without a later user turn remain untouched for recovery and approval replay.
Empty recovery input and function-result content do not establish a new user-turn boundary.
Repeated normalization does not add another response.

The full Rust library suite passes 1,209 tests with PostgreSQL enabled. Clippy and formatting checks pass.
Coverage includes active recovery calls, recorded results, mixed batches, and repeated projection.
Deployed acceptance passes in the original chat 637 after worker replacement.
The real provider reaches expected continuation exhaustion instead of rejecting historical messages.
A fresh headed browser verifies partial-output copying, exact reload, and persisted terminal error identity.
The test uses no response fixtures. Database schemas and existing worker mounts remain unchanged.
Evidence: history-repair-live.json and history-repair-live.png in the local test artifacts.
