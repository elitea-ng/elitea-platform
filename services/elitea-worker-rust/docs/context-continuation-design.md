# Context compaction and continuation

Status: implementation requirements; completion is not yet proved.

## Required continuation state

Compaction must leave enough information to continue the actual job.
Store a versioned checkpoint through the existing durable session and checkpoint infrastructure.
Do not add a table solely for a summary.

Keep these components distinct:

- Authoritative project context and skill content, with source IDs, revisions, scope, and activation state.
- Original user request and subsequent user instructions, linked to their immutable conversation events.
- Current objective, constraints, decisions, completed work, unfinished work, and the next action.
- Tool calls, results, artifact references, approvals, interrupts, and resume identities from durable execution records.
- A generated narrative summary of older conversation content, explicitly marked as derived information.

A summary must not replace authoritative instructions or invent successful work.
Completed operations must reference actual tool outcomes or durable receipts.
Pending and ambiguous operations remain pending until their state is reconciled.
Keep full conversation history available for retrieval and audit.

## Token budget

User steering on 2026-09-14 asks for a simple default and full-model option. The implementation plan uses two presets:

- Balanced: a default total budget of 272,000 tokens, capped by the model's supported context window.
- Full: the model's supported total context window.

Existing explicit conversation token limits remain overrides. Do not migrate or silently rewrite stored limits.
Store new selections in the existing context-strategy metadata if that contract can express them.
Main resolves provider limits from the authorized model catalogue and freezes the admitted limits for Rust.
Do not infer a model's capacity from its name or add its advertised input and output maxima together.
Keep a provider's separate input-only maximum distinct from its combined context-window maximum.

Reserve the invocation's admitted maximum output, including reasoning where the provider charges it to that limit.
When the user has not selected a smaller output cap, the model's supported maximum is the reservation.
Output capacity is inside the selected context budget, not additional to it.
For a hypothetical 400,000-token combined window and 64,000 output reservation, input can use at most 336,000 tokens before margin.
For a 1,000,000-token combined window and 128,000 output reservation, Full allows at most 872,000 input tokens before margin.
Balanced with that same output reservation allows at most 144,000 input tokens before margin.
If authoritative model limits are unavailable, disclose the configured fallback; do not label it Full capacity.

The context indicator reports current estimated input, output reservation, margin, total budget, and remaining input capacity.
Replace estimates with provider measurements where available and keep their provenance explicit.
Compaction frees current-request space; it does not reset cumulative usage or billing.
Changing the model or output cap recomputes the available input budget.

Make the decision before each model call, including calls within a tool loop.
Budget the final request after instruction rehydration and tool-schema assembly.
Include system instructions, active context and skills, user input, history, tool schemas, tool results, and provider framing.
Reserve the configured next-response output allowance and a bounded estimation margin.

The admission condition is:

`estimated full input + reserved output + margin <= effective context window`

Use provider token counts when available and a conservative estimator before submission.
Report estimated and measured values separately.
Track cumulative input and output usage separately from the per-request context window.
Repeated input tokens increase cost but do not accumulate into the next request's context window automatically.

An output cap limits one response. It does not implement context compaction.
When protected content alone exceeds the available input budget, return an explicit bounded failure.
Do not silently discard instructions or send an already known oversized request.

## Compaction sequence

1. Load authoritative instructions and the latest valid continuation checkpoint.
2. Reconcile tool outcomes and pending resume state against durable records.
3. Estimate the complete next request and reserve output tokens.
4. Compact only eligible older content, preserving complete tool call/result groups.
5. Validate the checkpoint's source coverage, bounds, and instruction references.
6. Persist the checkpoint before relying on it for another model call.
7. Rehydrate authoritative instructions and rebuild the request from the checkpoint and retained recent events.
8. Recheck the complete request budget before sending it.

Unresolved tool calls and authorization interrupts must not be summarized into successful outcomes.
Concurrent workers must use existing generation and claim fencing for checkpoint updates.
A replacement worker must load the same authoritative state without the original process or its memory.

## Existing source and ADK ownership

The installed dependency is ADK-Rust 2.2.0.
`adk-runner::compaction` supplies compaction strategies and event-token estimation.
`adk-runner::IntraInvocationCompactor` supplies per-call-cycle triggering and summarizer integration.
Its default trigger estimates event content and can fall back to uncompacted history after summary failure.
That default does not establish the full-request budget or durable checkpoint requirements above.
The 2.2.0 Runner calls `maybe_compact` before `agent.run`; it does not call it inside every `LlmAgent` model iteration.
The `LlmAgent` before-model callback does execute each iteration, after tool declarations and request contents are assembled.
Apply the final request check there, after instruction rehydration, with durable persistence before dispatch.

`adk-agent::LlmEventSummarizer` supplies the existing model-backed summarization primitive.
Its formatter reads only text parts. Normalize complete tool call/result groups into an explicit transcript for summarization.
Preserve tool identities and success, error, pending, and ambiguous outcomes; never omit them because they are structured parts.
ADK `EventsCompactionConfig` also supports invocation intervals, retained overlap, and a configurable summarizer.
The Runner persists the summarizer event through `SessionService::append_event_for_identity`.
ADK `EventCompaction` stores the covered timestamps and compacted content.
The Runner history projection uses this boundary without deleting the original stored events.
Elitea's `RunnerSessionService` forwards these events to the existing durable session service.
Use this path for durable summaries before considering a custom persistence mechanism.
Verify replacement-worker replay and summary bounds before enabling it.

Use these primitives with Elitea-owned authority, request budgeting, and failure handling.
Do not copy another framework's runtime or replace the current language-neutral contracts.

Current Rust `src/agents/context_management.rs` compacts the invocation's event view.
It does not persist the generated summary, so later invocations can repeat summarization.
Its history budget does not include all injected instructions, tool schemas, new user input, and output reservation.
`src/agents/instruction_authority.rs` already provides separate versioned instruction state and rehydration.
Complete integration with that state rather than asking a summary model to recreate instructions.
Main `agentexecution/start.go` and `adhoc.go` still dispatch empty context settings.
Complete authorized settings and model-limit delivery before claiming that UI compaction controls affect this worker.

## Point 4 baseline verification, 2026-09-14

The existing Rust context-management suite passes 21 tests with no ignored cases.
Those tests prove the existing history-only behavior, not this design's complete request budget or durable summary requirements.
The current SDK now uses a low-tier summary model and LangChain summarization/context-editing middleware through `runtime/clients/client.py`.
Use that implementation as functional evidence for model choice and controls, without copying its fallback behavior or storage design.

## Required proof

Test combined input/output boundaries and large tool schemas.
Verify compaction during a long tool loop without separating calls from results.
Verify original user intent and later corrections survive repeated compactions.
Verify skill and project-context revisions remain exact after source edits.
Verify completed work remains completed and ambiguous effects are not retried automatically.
Restart the worker after checkpoint persistence and resume through the UI.
Confirm the next action uses the retained objective, instructions, artifacts, and pending execution state.
Keep the gate open until these tests and the deployed continuation proof pass.
