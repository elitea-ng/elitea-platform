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
