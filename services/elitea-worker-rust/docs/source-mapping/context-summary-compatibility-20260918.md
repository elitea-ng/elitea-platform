# Compatible-provider summary and input limits

Status: live compaction and reload pass on 2026-09-18. Full continuation-quality and capacity acceptance remain open.
Gate 4 remains open.

## Source mapping

| Source | Behavioral evidence | Rust implementation |
| --- | --- | --- |
| Current SDK `runtime/clients/client.py::_inject_summarization` | Selects summary instructions and a separate summary model. | `transport/summary_model.rs` binds the authorized summary model and collects its complete response. |
| ADK 2.2.0 `adk-agent/src/compaction.rs::LlmEventSummarizer` | Formats source events and invokes the supplied model without a response schema. | `agents/context_compaction.rs` supplies the platform continuation contract and structured source records. |
| ADK `GenerateContentConfig.response_schema` | Provides a schema option for supporting adapters. | The summary adapter freezes the platform schema and passes it through both provider adapters. A compatible endpoint does not prove enforcement. |
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

## Structured-output request controls

`context_summary.rs::response_schema` defines the platform continuation shape.
`SummaryModel` supplies this schema through ADK `GenerateContentConfig.response_schema`.
It also includes the platform contract in the system instruction.
Optional user guidance remains subordinate to that contract.

The OpenAI-compatible adapter sends `response_format` with a strict JSON schema.
This follows ADK 2.2.0 `adk-model/src/openai_compatible.rs`.
The native Anthropic adapter uses ADK `OutputConfig` and `OutputFormat::json_schema`.
It preserves an existing effort setting.
Ordinary chat calls do not request the summary schema.
The adapter rejects a request schema that differs from the frozen invocation.

Provider schemas do not replace local validation.
Byte limits, exact references, and evidence membership remain local checks.
Unsupported provider responses remain failures. No silent retry drops the schema.
Live compaction acceptance is still required after deployment.

## Required context acceptance matrix

All rows require valid continuation facts and preserved instruction identities.
Numerical budget tests alone do not establish these outcomes.

| Scope | Required cases | Remaining evidence |
| --- | --- | --- |
| Window | Balanced, model-capped Balanced, Full 400k, Full 1M | End-to-end admission, summary, checkpoint, and UI proof |
| Repeated work | Ten compactions within one tool loop | Existing regression passes; live long-run proof remains |
| Nested execution | Independent child agents and pipeline LLM nodes | Inherited policy and isolated history proof |
| Summary model | Same model and smaller dedicated model | Bounded source groups and validated merge |
| Payload | Large tool results and long model output | Compaction with combined payloads |
| Recovery | Restart during summary and after durable commit | No lost history or repeated committed effects |
| User actions | Regenerate and switch models | Consistent history and context measurement |

Current provider requests are capped at 1 MiB.
Worker session state is capped at 1 MiB; one stored event is capped at 2 MiB.
These caps can prevent a Full 1M request before compaction runs.
Raise or restructure these boundaries together, with bounded memory and durable recovery checks.
Do not report Full capacity from the model catalogue alone.

The 43 provider-adapter tests pass with the structured-output change.
They inspect both wire schemas and verify ordinary chat isolation.
They reject changed or omitted schemas after invocation admission.
Clippy passes for all targets with warnings denied.
These checks do not replace live provider or browser acceptance.

## Browser retest and bounded correction

The fresh headed Playwright run uses deployed image `sha256:b74a33071fc496a4bbbbfbb8fb1326c9cdb9ba35ce2ffd00fa2fa3656d812882`.
Synthetic chat 590 reaches 108,124 estimated input tokens on 2026-09-18.
The response passes shape validation but fails `context_summary_evidence`.
Execution `e9fac0084f3bff422e509f58f925e5a2` does not commit a summary.
This is not a successful compaction acceptance run.

`DurableContextCompaction` now permits one correction after local summary validation fails.
The correction receives the original records, the rejected candidate, and the static validation code.
The platform identifies the candidate as rejected data, not source evidence.
Validation of the correction uses only the original source records.
A second invalid candidate terminates preparation without replacing history.
Transport and stream failures do not enter this correction path.
The summary adapter permits at most two calls per admitted model step.
Correction remains subject to the summary model's input and output limits.

All 11 compaction checks and 43 provider-adapter checks pass after this correction change.
The checks include ten compactions in one tool loop and bounded correction failure.
All-target Clippy passes with warnings denied.
Live acceptance of the correction remains pending.

The correction image is `sha256:c5d3a85f20edeed6a63d411173db4e38b7a652faed7be9291dd3ce2ec9225314`.
A fresh browser test reaches 108,194 estimated tokens but fails evidence validation twice.
Execution `8b046f87ee9b50574ed37dfd7b373067` preserves the original history.
No live compaction success is claimed.

A separate full-fixture probe returns invented evidence labels instead of reference values.
For example, `Historical synthetic records 4-9 confirmation` has no matching reference entry.
The probe uses synthetic data and 74,223 provider-reported input tokens.
This differs from the worker's byte-based estimate and is not a measurement of the failed worker request.
The next correction must use validated reference values and preserve all non-evidence fields.
Resending the full source for this reference-only correction is unnecessary.

## Evidence-only correction

`context_summary.rs::evidence_correction_input` exposes only the candidate and its previously validated reference values.
For evidence-membership failures, `DurableContextCompaction` omits the original bulk history from the correction request.
Other validation failures retain the original bounded correction path.

`apply_evidence_correction` copies only evidence arrays onto the original candidate.
It checks the completed-work count and validates the merged record against the original source.
Model rewrites of facts, objectives, decisions, references, or pending work are discarded.
An invented reference cannot become evidence through the correction response.
The original candidate remains uncommitted until the merged record passes validation.

A synthetic provider probe reduces correction input from 74,223 to 1,246 reported tokens.
It fixes evidence links but also rewrites one result sentence.
The selective merge prevents that unrelated rewrite from changing the accepted record.
All five summary parser checks and 11 compaction checks pass.
The short correction input check also verifies that bulk historical text is absent.
Live browser acceptance remains pending for this refinement.

## Accepted compaction and separate answer-quality result

The deployed image is `sha256:0fe6501aabe1ce0d6dca7a85400f9a1a3370bf0b901714c7dc897f59422ba672`.
A fresh headed Playwright session runs synthetic chat 590 without intercepted requests.
Execution `b73f1645e3dd155cd0fc527f3b7d3b42` completes evidence correction and commits the summary before model dispatch.
Estimated input falls from 108,264 to 12,858 tokens.
The UI receives `compacting` and `compacted` events and displays 12% usage.

A read-only worker-state query confirms all four required facts in the accepted summary.
These include `CEDAR-731`, the teal correction, completed archive verification, and pending handoff preparation.
The query selects only this isolated test execution.
A second fresh browser session confirms stable answer text and the persisted compaction status.

The overall continuation-quality assertion does not pass.
Haiku rejects the repetitive synthetic scenario and omits the handoff step from its answer.
The persisted summary retains that step, so this failure is not evidence of compaction data loss.
Do not count this run as complete end-to-end acceptance of the requested four-line answer.
Use representative task history for the remaining continuation-quality and capacity checks.
Full-mode limits, smaller-summary-model batching, nested scopes, and crash recovery remain open.
