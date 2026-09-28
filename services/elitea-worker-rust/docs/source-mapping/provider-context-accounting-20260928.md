# Provider context accounting

Status: compatible-provider usage retention implemented. Context-meter integration remains open.

## Source mapping

| Functional source | Rust integration |
| --- | --- |
| SDK `elitea_sdk/runtime/middleware/summarization/accounting.py` distinguishes provider counts from estimates. | `agents/context_status.rs` currently publishes prepared-request estimates only. Provider measurement integration remains required. |
| SDK `read_provider_usage` identifies the latest model response. Its counts describe that request, not a subsequently changed history. | `transport/openai_compatible_facade.rs` now preserves request usage in ADK `LlmResponse.usage_metadata`. |
| SDK `provider_prompt_tokens` uses a heuristic to identify cached-input conventions. | Native Anthropic and compatible transports have explicit protocol ownership. Do not copy this heuristic. |
| ADK `UsageMetadata` provides typed token counters and optional breakdowns. | The compatible facade retains only numeric counters. It discards arbitrary provider metadata. |
| Main `contextsettings/measurement.go` projects the latest accepted root measurement. | Extend this presentation contract before the worker emits provider-derived measurements. Main does not own worker checkpoints. |
| Web `widgets/context-budget` labels the existing measurement as an estimate. | Preserve that label until provider-derived measurements reach the browser. |

## Confirmed defect and correction

The compatible facade requests `stream_options.include_usage` but previously discards all usage frames.
It also discards usage attached to a content or finish frame.
The correction preserves the latest valid cumulative snapshot for each provider request.
The terminal ADK response carries that snapshot after stream completion.
Repeated snapshots replace earlier snapshots; they are not added together.
Cached input and reasoning counters remain breakdowns of input and output respectively.
Missing or malformed counters remain unavailable. They do not invalidate an otherwise valid answer.
The parser bounds integers and checks total arithmetic before accepting a snapshot.
Existing stream ordering, completion, tool-call, and allocation checks remain active.
This change adds no database tables, migrations, dependencies, or network requests.

## Remaining contract work

The context meter still shows the pre-call estimate. This correction alone does not change the meter.
Native Anthropic retains uncached input, cache creation, and cache reads separately, following its ADK adapter convention.
Its full input measurement must include those three counters exactly once.
Compatible prompt counts already include cached tokens under the compatible protocol contract.
Do not infer the convention by comparing the sizes of counters.

Keep prepared-request estimates for admission before provider usage exists.
Expose measurement provenance and the corresponding input and output counts.
Do not substitute cumulative billing usage for current context occupancy.
Bind measurements to the existing execution, generation, and model scope.
Do not let child or summarizer usage replace parent occupancy.
Do not carry a prior response's counts into regeneration or a different model request.
Account for continuation responses individually, including responses that exhaust their output allowance.
Retain existing compaction settings and protected-context semantics.

## Acceptance boundary

All 32 compatible-facade tests pass. Strict library and test Clippy checks pass.
These are local transport checks, not deployed browser acceptance.
Focused transport tests cover usage on finish frames and trailing usage frames.
They cover repeated cumulative snapshots, cached-input breakdowns, reasoning breakdowns, and exclusion of arbitrary provider fields.
Malformed and absent usage must preserve successful completion without invented counts.
Deployed provider-accounting and browser acceptance remain open.
Those checks must cover native Anthropic, compatible providers, regeneration, model switching, child isolation, continuation, and reload.
