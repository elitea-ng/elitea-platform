# Durable context compaction

Status: ordinary root, nested-agent, and pipeline model component implementation. Point 4 and deployed browser acceptance remain open.

## Functional source mapping

Sources are inspected on 2026-09-16. SDK revision: `18704a4070d098761fd1d35897dc53e412b4cbcc`.

| Current source or dependency | Behavior | Rust owner |
| --- | --- | --- |
| SDK `runtime/clients/client.py::_inject_summarization` | Reads conversation settings, retained messages, summary instructions, and a low-tier summary model. | `agents/context_management.rs` admits settings. `agents/context_compaction.rs` applies the admitted policy before model calls. Dedicated model selection remains open. |
| SDK `runtime/clients/client.py::_inject_context_editing` | Treats tool-output editing as a separate enabled strategy. | This change preserves complete tool groups. Tool-output editing remains separate point 4 work. |
| SDK `runtime/tools/application.py::formulate_query` and `Application._run` | Keeps the delegated task explicit. Removes parent execution state and separates parallel child checkpoint identities. | `application_tools.rs` preserves task and instruction ownership. `model_scope.rs` derives independent model sessions from existing child lineage. |
| SDK `runtime/langchain/langraph_agent.py::create_graph` and `runtime/tools/llm.py::invoke` | Applies context middleware to model nodes and carries message updates into graph execution. | `pipeline.rs::NativePipelineLlmAgentFactory` applies the policy inside each model loop. `graph/llm.rs::PipelineModelScope` isolates node visits. Exact graph state is never summarized. |
| ADK 2.2.0 `adk-agent/src/compaction.rs::LlmEventSummarizer` | Formats text events and returns an `EventCompaction` result. | The new compactor uses this summarizer with normalized structured tool records and a continuation-record prompt. |
| ADK 2.2.0 `adk-agent/src/llm_agent.rs` | Runs before-model callbacks inside the model/tool loop. | `agents/model_checkpoint.rs` composes preparation, summary persistence, and model-request persistence in this callback. |
| ADK 2.2.0 `adk-runner/src/runner.rs` | Runs its compactor before the agent invocation. | Existing Runner compaction remains for old bindings without model-limit snapshots. The new path also handles model-loop iterations. |
| Rust `agents/instruction_authority.rs` | Stores source identities, revisions, scope, and active instructions separately. | Rehydrates exact instructions before the compaction/checkpoint callback. Generated notes do not replace this authority. |
| Rust `state/postgres_session.rs` | Persists session events and state under existing claim and generation fencing. | Saves compaction state and the prepared model request in one existing session append. No database schema changes occur. |

The current SDK provides functional evidence. Its runtime implementation is not copied.

## Structured continuation record

User clarification requires structured working knowledge, not only a narrative summary.
`agents/context_summary.rs` defines a strict versioned JSON contract:

- Objective.
- Constraints and user corrections.
- Decisions and key facts.
- Completed work with evidence references.
- Open work and next steps.
- Unresolved issues.
- Descriptive resource and artifact references.

Unknown fields, missing sections, malformed JSON, oversized strings, and excess entries fail validation.
The response is limited to 32 KiB, 24 entries per array, and 2,048 UTF-8 bytes per string.
Reference values must occur in supplied source content, including previously compacted notes.
Completed-work evidence must reference an entry in that reference list.
These checks establish format and reference provenance. They do not prove semantic completeness or the truth of generated statements.
Live-model quality checks remain required.

Skill and project-context identities and revisions remain authoritative session state.
Current tool declarations remain bound runtime objects and exact checkpoint declarations.
Generated references do not grant authority, rebind tools, approve calls, or settle unfinished effects.
The first user request, latest user message, active system instructions, and retained recent complete exchanges remain exact model input.
Earlier user corrections enter the structured summary; original events remain stored.
The configured recent-message count is a minimum when a complete tool group crosses the boundary.

## Isolated native summary calls

`transport/summary_model.rs` adapts ADK's non-streaming summarizer request to the authorized streaming provider bindings.
Each summary call owns a fresh completion capture. The summary handle bounds total calls.
It retains the authorized model, generation controls, shared transport, project, and execution identity.
It uses summary-specific system instructions and exposes no tools.
It does not consume chat model turns or replace the captured chat answer.

ADK 2.2.0 takes the first content response and ignores subsequent stream errors in its summarizer.
The adapter consumes the complete bounded provider stream before returning one response to ADK.
It rejects truncation, missing termination, transport failure, unexpected tool content, empty output, and output beyond the bound.
Long summary prompts are split into exact UTF-8 text parts without changing their contents.
The complete request still passes the provider's token and byte limits.

## Durable preparation and coverage

The new path applies when an ordinary root invocation has an admitted summary plan and frozen model limits.
Main currently sends empty context settings, so deployed activation still requires settings delivery.
Old unversioned bindings retain their previous Runner path, now with an isolated summary binding.
Direct root HITL resume still needs equivalent integration.
Nested ordinary agents now use the same model checkpoint callback with independent session storage.
Pipeline LLM and model-backed Decision nodes use that callback within each model invocation.

The compactor uses the complete provider-request estimate and the existing 90-percent pressure trigger.
It summarizes the oldest eligible prefix while retaining the configured recent messages and complete tool groups.
The 70-percent target remains a target, not permission to remove protected content.
The resulting request must fit and must use fewer estimated input tokens.
Protected content passes a capacity check before any summary call starts.
An oversized protected request therefore fails without spending a summary call.

Before summary generation, the writer saves a `context_pending` checkpoint with the exact source request.
This phase authorizes context preparation only. Recovery refuses it without an active compactor.
After validation, the writer saves the `model_pending` request and `elitea.context.root.v1` in one session append.
The state contains the definition identity, original-task anchor, covered-source count and digest, and replacement contents.
The in-memory record changes only after persistence succeeds.
Task-model dispatch therefore cannot precede durable summary and request storage.

Recovery inspection runs before model credentials are redeemed.
`NativeSessionBackend::inspect_model_checkpoint` validates `context_pending` when the admitted plan enables summarization.
It returns only checkpoint evidence and does not construct a model or an executable writer.
Executable restoration still requires a bound compactor. Inspection does not authorize direct dispatch of the uncompacted request.
Both stages verify the same checkpoint digest before the recovery coordinator permits execution.

A replacement restores an interrupted preparation and can repeat the summary call.
It restores an already prepared request without generating that summary again.
This does not promise exactly-once provider billing when the summary response was lost before persistence.
Changed source history invalidates prior coverage instead of applying stale notes to different messages.
Repeated compaction extends the coverage digest while keeping original session events.
Coverage hashing sorts JSON object keys so database object ordering cannot invalidate unchanged content.
Generated summaries do not become protected user input during later compaction passes.

The implementation uses ADK's summarizer, callbacks, and session state/event append interfaces.
It does not publish a transcript-wide timestamp compaction marker to the Runner.
That marker cannot express the protected user messages or source-content coverage required here.
The private record extends the existing checkpoint mechanism; it does not introduce another storage service or scheduler.
Pipeline scopes derive durable lineage from existing graph checkpoints and parent call identities.

## Child and pipeline ownership trace

`application_tools.rs::LazyNestedAgent` binds a model separately for each child call.
`ApplicationToolInvocationContext` derives an instruction-session identity from the parent session, tool-call ID, and child name.
Its working session remains local memory. The parent forwards child events through the existing application event channel.
`model_scope.rs` adds an independent durable model session when the inherited policy enables compaction.
Its identity includes execution, generation, child instruction-session identity, and child agent name.
Provider call-ID reuse across executions cannot reuse another execution's summary.
A checkpoint writer binds to one identity. It rejects accidental reuse by a sibling.

The child retains its bound model's request measurement and isolated summary handle.
Its before-model callback runs after authoritative instruction preparation, inside the existing ADK model/tool loop.
The callback stores source coverage, summary state, and the prepared provider request before dispatch.
The child wrapper stores model and tool events before forwarding them to its parent.
Original events remain available after model-facing compaction. Parent and sibling checkpoints remain separate.
The existing compaction and checkpoint keys are relative to each model session.
Like the ADK Runner, scoped storage omits partial streaming events that share the final event ID.
The existing completion adapter enriches only the stored terminal copy with full generated text.
Streamed UI events retain their original contents; persisting a result does not emit the answer twice.

The root PostgreSQL service continues to reject other session identities.
An internal factory derives a separate child service under the same admitted claim and definition.
Activation and subsequent operations lock the root writer before the child writer.
Root-writer takeover therefore blocks stale child writes, even before local lease-loss detection.
This uses existing session and writer tables. No schema migration occurs.

A replacement can reload matching summary coverage through the derived child service.
Loading this state does not authorize replay of the parent's tool call or an unfinished child effect.
Automatic recovery of an in-flight nested call still requires parent recovery integration.
The existing parent recovery guard continues to reject an ambiguous tool boundary.

Authorized child guard replay keeps its first deterministic pending-call request intact.
Later provider requests remove the private replay marker before measurement, compaction, and checkpoint persistence.
When the authorized replay already has its result, the first real provider request is prepared immediately.
This preserves the existing replay adapter's ordering without treating control input as a user correction.
End-to-end compaction during nested guard resume still requires acceptance coverage.

`pipeline.rs` activates graph/session storage before binding runtime nodes and passes the existing model-session factory to them.
`NativePipelineLlmAgentFactory` retains each model's request measurement, isolated summary handle, and completion capture.
Its instruction callback runs before the scoped checkpoint callback. The graph Runner has no compactor of its own.
Pipelines without a model therefore accept the policy without making a summary call.

`graph/llm.rs::PipelineModelScope` hashes the graph thread, node definition, graph step, and optional parent event scope.
Existing graph checkpoints restore the same pending step; later visits and different parent calls receive separate identities.
The local invocation counter is excluded because it changes across process replacement.
The scoped storage identity also retains the execution and generation boundary.
LLM and model-backed Decision nodes share this factory. Nested saved agents receive the same storage factory with their own child identities.
Exact mapped graph values, output keys, and deterministic node state remain outside the compactor.

Pipeline guard replay validates the exact pending result before its first real provider request.
Once validated, later model-loop requests may use compacted history without repeatedly requiring that old result.
This does not authorize replay of an unfinished tool. Automatic recovery must still coordinate the model checkpoint with the graph boundary.

## Verification and remaining gates

Verification on 2026-09-16:

- Agent suite: 343 tests pass, with no failures or ignored tests.
- Session-storage suite: five tests pass, including the PostgreSQL component test.
- Provider suites: 36 tests pass, with no failures or ignored tests.
- PostgreSQL recovery uses three separate processes and an isolated test database.
- The first process leaves interrupted summary preparation. The second saves the validated summary and prepared request.
- The third process restores that request without another summary call.
- Both replacement processes validate checkpoint evidence before executable restoration.
- The production session-backend inspector admits interrupted summary preparation without model binding.
- Failed checkpoint persistence prevents task-model dispatch.
- An ordinary nested-agent fixture compacts older tool evidence while retaining its task and the recent complete tool group.
- The fixture retains both original tool results in child storage. Its parent receives only the child answer.
- Child fixtures verify separate sibling identities, execution/generation isolation, summary reload, and private replay-marker removal.
- A PostgreSQL fixture verifies child-summary reload and rejection of stale writes after root-writer takeover.
- The PostgreSQL fixture also verifies one complete stored terminal event for streamed chunks sharing its ID.
- A mixed graph compacts a long model tool loop, preserves original model events, and passes exact output to a deterministic node.
- A graph without models accepts the policy without requesting a model or summarizer.
- Scope identity checks cover replacement stability, graph steps, node definitions, graph threads, and parent call IDs.
- Replay checks require the exact pending result once, then permit later compacted history; completed child replay prepares its first real provider call.
- Coverage checks include repeated compaction, changed history, JSON object ordering, protected input, and complete tool groups.
- Both provider adapters reject incomplete and truncated summary streams without changing the captured chat answer.
- Rust formatting, Clippy with warnings denied, and Git whitespace checks pass.

The component fixtures test mechanics and use deterministic summary/model responses.
They do not replace browser acceptance or live-provider quality tests.
Local evidence uses `elitea-point4-pipeline-scope-postgres.log` and `elitea-point4-pipeline-scope-clippy.log`; earlier storage coverage is in `elitea-point4-child-scope-storage.log`.

Remaining work includes nested and graph-model recovery coordination, default policy delivery, summary-model selection, UI controls/status, and browser acceptance.
Large-input summary admission and representative live-model structured-output quality still need acceptance coverage.
Historical events remain available in storage; model-facing retrieval of omitted evidence needs explicit integration and verification.
Continuation, tool-output editing, same-name toolkit bindings, and runtime diagnostics remain separate point 4 requirements.
