# Model context budget

Status: component verification. Point 4 and deployed UI acceptance remain open.

## Current platform evidence

Sources are inspected on 2026-09-14.

| Current source | Behavior | New owner |
| --- | --- | --- |
| Centry `configurations/models/pd/llm_model.py` | Model metadata supplies `context_window` and `max_output_tokens`. Defaults are 128,000 and 16,000. | Main `application/configurations/models.go` preserves read-time fallback provenance. |
| SDK `runtime/clients/client.py`, `_required_provider_max_tokens` and model construction | Native Anthropic needs an output limit. Model name and project identify the configured model. | Main `agentexecution/model_context_limits.go` freezes the authorized catalogue limits. |
| SDK `runtime/clients/client.py`, `_inject_summarization` | Context thresholds and a low-tier summary model have separate responsibilities. | Rust `agents/context_budget.rs` owns request capacity. Durable summary integration remains open. |
| EliteaUI `src/[fsd]/widgets/llm-model-selector/lib/validation.js` | A selected output cap must fit the configured model maximum. | Rust admission validates explicit caps before provider binding. |

Source revisions: Configurations `906664480690620a79498232bfafa683ed205143`, SDK `18704a4070d098761fd1d35897dc53e412b4cbcc`, UI `fb805e6af02f5fc56662fa32ff29241488f78ca8`.
These sources define functional evidence. The implementation does not copy their runtime architecture.

## Contract and implementation

`AgentExecutionInputV1.model_context_limits`, field 65, carries `ModelContextLimitsV1`.
The snapshot contains the combined window, maximum output, fallback flags, and an optional input-only maximum.
Main replaces authored limits with values from the authorized catalogue selection.
Application and ad-hoc builders include the snapshot in their immutable input bundle.
Nested version snapshots use the same field shape and the existing authorized version freezer.
No migration, new table, or credential field is added.

Read-time defaults retain fallback flags through catalogue normalization.
Persisted configuration values remain configuration authority, including defaults previously materialized by the current platform.
The implementation cannot recover whether an operator originally typed those historical values.
It does not infer capacity from model names.

Rust resolves Balanced to 272,000 tokens, capped by the model window.
Full uses the configured window and refuses a read-time window fallback.
An explicit stored `max_context_tokens` remains an override.
The selected output cap becomes the reservation; absence reserves the model maximum.
A selected cap can supersede an unknown output maximum's fallback.
The existing provider Auto omission behavior remains unchanged.
Reasoning stays inside the admitted output cap.
The margin is one percent of the effective window, bounded between 1,024 and 8,192 tokens.
The optional input-only maximum further caps available input.
Invalid limits and exhausted input fail without unsigned subtraction overflow.

Nested models retain the parent's selection and recompute capacity against their own model limits.
Old frozen inputs without limits remain readable and retain their previous behavior.
They cannot request the new preset contract without limits.

Both provider adapters check the completed serialized request before network dispatch.
The check includes static instructions, authoritative instruction blocks, user input, history, tools, results, and provider framing.
It uses ADK 2.2.0's byte heuristic, with rounding and the reserved margin.
This is an estimate, not exact provider tokenization or a billing measurement.
Repeated requests do not accumulate occupancy in this check.
Encoded images can produce conservative estimates; multimodal calibration remains required.
Existing byte limits remain separate constraints, including the one-MiB provider request limit.
Full preset arithmetic does not prove that current transport limits admit every full-window workload.

The provider failure has the static code `context_budget_exceeded` and contains no prompt content.
The lifecycle maps this code to the existing non-retryable resource-limit terminal.
Point 4 diagnostics still need specific browser messages and the wider provider-error taxonomy.
The larger model plan crosses Clippy's future-size threshold on rejected delivery cleanup.
Boxing that cleanup future preserves its existing lifecycle and bounds its parent future size.

## Verification

An isolated staged candidate passes 624 Main checks without skips.
It excludes all pending HITL history and builder-catalogue edits.
They cover forged-limit replacement, fallback metadata, invalid configurations, protobuf round trips, and both dispatch builders.
Six budget tests cover presets, explicit limits, output reservation, input-only caps, nested models, fallback refusal, and arithmetic boundaries.
Both provider adapters reject oversized static instructions, active instructions, tool schemas, and user input before network dispatch.
Those failures contain no fixture content.
The agent regression suite passes 319 checks, including existing PostgreSQL process-replacement instruction recovery.
The database helper uses isolated temporary databases and does not change product data.
The additional application/ad-hoc budget profile test passes.
The input protocol suite passes 14 checks, including old-input compatibility and field 65 preservation.
Nine focused checks cover budget arithmetic, both provider adapters, and terminal classification.
Three Python wire checks pass; 67 unrelated tests are deselected.
The protobuf breaking-change check passes against the prior commit.

Logs use the `elitea-point4-model-limits-*`, `elitea-point4-budget-*`, and `elitea-point4-provider-budget-tests.log` prefixes in the temporary evidence directory.
Go vet and Rust Clippy pass after the bounded-future fix.

## Remaining integration

Main still sends empty context settings. New model snapshots select Balanced by default in Rust.
User defaults, conversation presets, status, and UI controls still need end-to-end wiring.
The final provider check prevents oversized dispatch; it does not generate or persist a summary.
Complete the before-model compaction callback and durable checkpoint integration before acceptance.
Preserve complete tool groups and authoritative instruction state across repeated compaction and replacement.
Keep cumulative usage separate from the current request estimate.

Deploy compatible workers before Main emits field 65, because older strict protobuf readers reject unknown fields.
Regenerated Python bindings recognize the field; the Python SDK adapter does not enforce this Rust budget policy.
No deployment or fresh browser acceptance is claimed for this slice.
Point 4 remains open under `remaining-gates.md`.

## Child and pipeline scope clarification, 2026-09-16

This section records source inspection and implementation requirements. It does not claim new runtime behavior.
The [context design](../context-continuation-design.md#child-agents-and-pipeline-model-requests) defines inherited policy and independent child compaction.
It also separates pipeline model-history projection from authoritative graph state.

SDK revision `18704a4070d098761fd1d35897dc53e412b4cbcc` provides the current functional reference.

| Source | Inspected behavior | Rust ownership and remaining work |
| --- | --- | --- |
| SDK `runtime/langchain/langraph_agent.py::create_graph` | Passes mapped inputs and context middleware to each `LLMNode`. | `agents/graph/llm.rs::map_execution_input` separates mapped system, task, and history content. Preserve this boundary during compaction. |
| SDK `runtime/tools/llm.py::invoke` and `_prepare_output_messages` | Runs context hooks and returns message-removal updates for checkpoint persistence. | Use existing durable Rust session and graph checkpoints. Keep original records and exact graph values rather than copying removal behavior. |
| Rust `agents/assembly.rs`, nested profile construction | Inherits the budget selection and recomputes child model capacity. Sets `context_management` to `Disabled`. | Complete child compaction-policy inheritance and independent summary persistence. Budget inheritance alone does not close this requirement. |
| Rust `agents/pipeline.rs::NativePipelineLlmAgentFactory::build` | Binds a node model with the pipeline profile's budget and authoritative instructions. | Add compaction before each model call within the correct node invocation. |
| Rust `agents/graph/decision.rs` | Uses the same model factory for model-backed routing without tools. | Include decision requests in budget enforcement; preserve exact route constraints. |
| Rust `agents/session.rs`, pipeline runner construction | Rejects an active transcript-wide context plan because the graph has no single summarization model. | Carry policy to model consumers without summarizing the graph itself. |

Purely deterministic pipelines must not acquire model calls solely because compaction is enabled.
Point 5 retains ownership of new map and parallel node capabilities.
Point 4 must define isolated model-history scopes that those nodes can reuse.
Required proofs include child isolation, mixed graphs, repeated node visits, branch separation, and replacement-worker summary recovery.

## Complete request measurement, 2026-09-16

The before-model integration needs an estimate before provider dispatch can reject an oversized request.
Counting only ADK contents would omit bound system instructions, provider tool conversion, and wire framing.

`agents/context_budget.rs::ModelRequestBudget` defines this consumer boundary for both provider adapters.
`BoundOrdinaryAgentModel::request_budget` exposes the measurement from an authorized model binding with frozen limits.
Old bindings without model limits return no measurement capability.
The provider-neutral facade delegates to the selected adapter.

Both adapters share their request encoder between measurement and dispatch.
Measurement validates the model and generation contract but permits an oversized body to be measured before compaction.
It returns numeric occupancy, reserved output, margin, input capacity, body bytes, and the transport byte limit.
It does not dispatch, consume an invocation turn, or change captured completion state.
Normal dispatch retains the existing byte and context checks.

The default trigger is the ceiling of 90 percent of usable input capacity.
The default target is the floor of 70 percent of that capacity.
Exceeding the transport byte limit also reports pressure even when the token estimate fits.
These methods do not generate summaries or activate a before-model callback by themselves.

`agents/model_checkpoint.rs` consumes this measurement before saving each ordinary pending-model checkpoint.
It measures after authoritative instruction preparation and pending-request restoration.
It uses the existing replay-history projection, matching the request normalization before provider dispatch.
The durable checkpoint retains its original replay records.
Oversized requests fail before checkpoint persistence or model dispatch.
The lifecycle classifies `model_request_bytes_exceeded` and `context_budget_exceeded` as resource-limit failures.
Diagnostics contain numeric estimates and limits, without request content.

## Native reasoning reservation, 2026-09-16

Current SDK `runtime/clients/client.py::get_llm` adds a reasoning allowance to a custom visible-output cap.
It does not add that allowance to the resolved model maximum.
SDK revision `18704a4070d098761fd1d35897dc53e412b4cbcc` provides this functional reference.

The Rust legacy Anthropic adapter already adds 2,048, 4,096, or 9,092 reasoning tokens according to effort.
Its request estimate previously reserved only the original output cap.
The regression test proves a 4,000-token reservation for a request that actually permits 6,048 output tokens.

`transport/anthropic_facade.rs::native_context_budget` now resolves capacity from the complete native output cap.
Both before-model measurement and provider dispatch use this calculation.
They preserve the inherited Balanced, Full, or explicit budget selection.
The compaction trigger and target therefore use the corrected input capacity.

A cap equal to the authoritative model maximum already includes reasoning.
The native adapter keeps that total unchanged instead of adding reasoning beyond the maximum.
An incompatible reasoning allowance or combined capacity fails with `context_budget_exceeded` before network dispatch.
Legacy inputs without authoritative limits retain their existing behavior.
Adaptive thinking retains its existing total-output contract and receives no additional reservation.
This change does not redefine model-selection or output-limit UI controls.

Validation: 18 native Anthropic tests pass. Rust formatting, strict library/test Clippy, and Git whitespace checks pass.
The fixtures cover each reasoning effort, adaptive output, the resolved maximum, and refusal before network dispatch.
These are component checks. Deployment and browser acceptance remain open.
Child and pipeline before-model integration still requires their scoped compaction implementation.

Both adapter suites pass 32 tests without ignored cases.
New tests measure twice, then successfully use a binding limited to one model turn.
Their measured body lengths match the dispatched OpenAI-compatible and native Anthropic bodies, including Unicode and provider framing.
Existing oversized-component cases now verify measurement before the unchanged dispatch refusal.
Ten focused budget checks pass, including different parent/child limits, rounding, and independent transport pressure.
The focused checks overlap two adapter cases; their counts are not additive.
The new Runner case proves that neither an oversized token estimate nor an oversized body creates a recoverable model checkpoint.
The final agent suite passes 322 checks without ignored cases, including PostgreSQL instruction recovery after process replacement.
This suite includes the checkpoint checks and the final replay-history projection.
Instruction recovery does not prove durable summary recovery; that integration remains open.
Production and test Clippy checks pass with warnings denied.
The stopped-coordinator test now boxes its rejected-delivery cleanup future, matching the production cleanup path.
The focused coordinator cleanup and resource-limit classification tests each pass without ignored cases.
Formatting and whitespace checks pass.

Evidence logs use the `elitea-point4-request-measurement-*` prefix in the temporary evidence directory.
This slice has no public API, schema, UI, or deployment changes.
Durable compaction, inherited/default policy delivery, browser status, and all other point 4 requirements remain open.

## Native ADK integration boundary

The pinned ADK version is 2.2.0. Its `LlmAgent` owns the model and tool loop.
Its before-model callbacks run during that loop and support request preparation without a second execution engine.
`adk-runner/src/runner.rs` invokes `IntraInvocationCompactor` before `agent.run`.
That placement alone does not check each model call inside a long tool loop.
`adk-agent/src/llm_agent.rs` runs before-model callbacks for those individual calls.
The current measurement uses that callback boundary through the existing model checkpoint writer.

ADK session, graph, summarizer, and compaction types remain the integration foundation.
Elitea owns complete provider-request admission, authoritative instructions, execution lineage, and generation-fenced persistence.
These responsibilities preserve the existing transport and recovery contracts.
Summary integration must persist scope and source coverage before the model relies on a compacted request.
Replacement must restore that summary without mixing child histories or replacing exact graph values.
Those summary guarantees remain implementation requirements, not results of the checks above.
