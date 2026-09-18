# Compatible-provider summary and input limits

Status: admission and parser checks pass on 2026-09-18. Browser acceptance exposes a remaining summary-schema failure.
Gate 4 remains open.

## Source mapping

| Source | Behavioral evidence | Rust implementation |
| --- | --- | --- |
| Current SDK `runtime/clients/client.py::_inject_summarization` | Selects summary instructions and a separate summary model. | `transport/summary_model.rs` binds the authorized summary model and collects its complete response. |
| ADK 2.2.0 `adk-agent/src/compaction.rs::LlmEventSummarizer` | Formats source events and invokes the supplied model without a response schema. | `agents/context_compaction.rs` supplies the platform continuation contract and structured source records. |
| ADK `GenerateContentConfig.response_schema` | Provides a schema option for supporting adapters. | Existing Elitea adapters require capability-specific admission. A compatible endpoint alone does not prove enforcement. |
| Main `internal/application/agentexecution/input_bundle.go` and `internal/domain/execution` | Admit agent input bundles up to 1 MiB. | `protocol/control.rs`, `transport/input_content.rs`, and `config.rs` now use the same maximum. |
| Rust `agents/context_summary.rs` | Owns typed continuation notes and evidence validation. | Extracts one complete JSON object before the existing schema, size, reference, and evidence checks. |
| Rust `state/postgres_session.rs` and `agents/model_checkpoint.rs` | Persist worker-owned history and prepared requests under claim fencing. | Valid summaries and prepared requests still commit together before task-model dispatch. |

Current-platform code defines summary selection behavior. Its compaction algorithm is not the target for this implementation.
The [durable compaction mapping](durable-context-compaction.md) owns authority and recovery rules.

## Failure evidence

Chat 589 fails during summary validation on 2026-09-17.
The worker reports `context_summary_invalid` before it saves an accepted summary.
The saved phase is `context_pending`; the durable summary value is null.
The rejected model text was not persisted. Later regeneration replaces that session history.
The exact original validation failure cannot be reconstructed from this evidence.

The affected model uses the OpenAI-compatible adapter.
A separate real-provider probe sends `response_format.type=json_schema` with a strict schema.
The endpoint returns HTTP 200 and a JSON object inside Markdown fences.
This proves that this compatible route does not enforce the requested response shape.
It does not prove why the original summary failed.
Native Anthropic client support alone cannot correct this compatible route.

The repair uses Serde to parse one complete object within the bounded response.
It accepts surrounding prose or Markdown fences. It does not repair JSON or choose between multiple objects.
Truncated JSON, arrays, competing objects, wrong fields, oversized values, and invented references remain invalid.
The parser preserves braces and quoted text inside JSON strings.
Only an accepted record removes surrounding presentation text.
Static failure codes now identify parsing, schema, reference, output, and stream-completion failures.
Diagnostics contain no rejected response text.

## Input admission correction

The first browser fixture contains 18 synthetic historical messages, totaling 430,496 text bytes.
Main accepts the input, but the worker rejects materialization before context measurement.
The worker deployment uses a 256 KiB fetch limit despite the existing 1 MiB claim and content contracts.
`config.rs` now aligns that limit with the existing contracts.
The next browser run exposes the independent 256 KiB JSON-field limit in `agents/protocol.rs`.
History now uses the admitted 1 MiB envelope budget. Control fields retain their smaller limits.
History still rejects duplicate JSON members and invalid structure.
No database migration, table change, credential change, or Main deployment is required.
This does not establish capacity for every model's Full context window.
The broader input and output capacity checks remain gate 4 work.

## Independent output failure

A later chat 589 request completes at the provider with `Stop` and 58,452 text bytes.
Its persisted event contains 3,198 text fragments. The combined JSON string alone uses 61,001 bytes before envelope metadata.
The worker then reports `agent_event.resource_exhausted` during event projection.
Browser JSON has a 61,440-byte limit. The context indicator shows a separate input measurement.
This is an output-delivery boundary, not evidence that the model context is full.

The wire limit cannot be raised only in Rust.
Main validates the same event size, and durable output tables enforce 65,536-byte event limits.
Existing tool-result chunking provides a bounded delivery precedent without a new application schema.
Large model output, final-result assembly, replay, and reload require equivalent proof before this output gap closes.
This change does not claim to fix that output gap.

## Measurement and regeneration boundary

The context indicator currently estimates the complete encoded provider request using ADK's four-byte token approximation.
Provider-reported usage is a separate consumption record. It does not currently replace the displayed estimate.
Regeneration deletes the worker session and seeds it from Main's frozen visible chat history.
An ordinary model switch does not change the ad-hoc session identity.
The observed change from 103,833 to 42,207 estimated tokens has no accepted summary record.
It must not be reported as successful compaction.
Exact history parity across regeneration and provider-usage reconciliation remain open.

## Verification

- All 25 summary checks pass with isolated PostgreSQL databases available.
- The process-replacement check accepts wrapped summary output and reloads the committed summary and prepared request.
- All 12 content transport checks and five configuration checks pass.
- Content checks accept bodies above 256 KiB and at the existing 1 MiB maximum.
- All 16 agent-input contract checks pass, including a 430,000-byte history string and unchanged control-field limits.
- Rust formatting and Clippy with warnings denied pass.
- A parallel test run exposes a temporary database-name collision. A process-local atomic suffix removes that harness race.

The initial browser attempt proves the input-fetch failure and does not count as compaction acceptance.
The fresh headed Playwright run opens private synthetic chat 590 with no mocked requests.
The deployed image is `sha256:7a803eaea8c5f3cfbf56aee8b68e1b465c38d70cba1231686dca2246324bc2e0`.
It reaches compaction at 108,054 estimated input tokens, with a 110,720-token input budget.
The worker then reports `context_summary_schema`. No accepted summary or successful continuation is claimed.
A separate synthetic provider probe returns an object inside `open_work`, which requires strings.
It also returns evidence references that do not match its reference list.
This probe demonstrates schema compliance problems beyond Markdown wrapping. It does not reconstruct the earlier rejected response.

## Large-window acceptance remains open

Full mode uses the selected model window. A 1,000,000-token window with 128,000 reserved output leaves 863,808 input tokens.
The safety margin is 8,192 tokens. The compaction trigger is 777,427 input tokens.
The current single-call summarizer receives the original objective and the older complete history prefix.
Recent messages stay outside the summary. Later summaries reuse the previous summary instead of the original covered history.
A smaller summary model cannot accept this prefix without bounded batches.
The 1 MiB input envelope, provider request cap, and checkpoint limits also require large-window acceptance.
The history admission repair does not close these gaps.
