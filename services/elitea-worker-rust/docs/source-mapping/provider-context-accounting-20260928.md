# Provider context accounting

Status: provider usage retention and context-meter integration implemented. Deployed acceptance remains open.

## Source mapping

| Functional source | Rust integration |
| --- | --- |
| SDK `elitea_sdk/runtime/middleware/summarization/accounting.py` distinguishes provider counts from estimates. | `agents/context_status.rs` carries prepared-request estimates and optional provider input/output counts. |
| SDK `read_provider_usage` identifies the latest model response. Its counts describe that request, not a subsequently changed history. | `transport/openai_compatible_facade.rs` now preserves request usage in ADK `LlmResponse.usage_metadata`. |
| SDK `provider_prompt_tokens` uses a heuristic to identify cached-input conventions. | Native Anthropic and compatible transports have explicit protocol ownership. Do not copy this heuristic. |
| ADK `UsageMetadata` provides typed token counters and optional breakdowns. | The compatible facade retains only numeric counters. It discards arbitrary provider metadata. |
| Main `contextsettings/measurement.go` projects the latest accepted root measurement. | Its provider measurement uses combined occupancy. Main does not own worker checkpoints. |
| Web `widgets/context-budget` labels the existing measurement as an estimate. | Provider measurements show input, output, and explicit provenance. Estimates retain their existing label. |

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

## Context measurement contract

Native Anthropic now sums uncached input, cache creation, and cache reads into the ADK prompt count.
This deliberately corrects the stock adapter's uncached-only prompt count at the owned transport boundary.
The separate cache breakdown remains available. It must not be added again.
Compatible prompt counts already include cached tokens under the compatible protocol contract.
Do not infer the convention by comparing the sizes of counters.

Prepared-request estimates still control admission before provider usage exists.
`model_checkpoint.rs` retains only the small numeric admission measurement for the current call.
`model_scope_output.rs` publishes provider counts at each completed response, including output-limited continuation responses.
Root agents use the ADK after-model callback in `ModelCheckpointWriter::bind` instead of the nested scope wrapper.
The first deployed check in chat 717 exposes this distinct root path: execution succeeds, but only its estimate appears.
The root callback correction adds a dedicated Runner regression. It emits the estimate and then the provider measurement.
It does not serialize the request again or retain another copy of its contents.
Summary calls bypass this ordinary model scope and cannot replace its occupancy.
Existing event envelopes carry execution, generation, and child scope.
Main excludes child and pipeline-node measurements from the root meter.
The next prepared request replaces provider usage with a new estimate.

The optional `provider_usage` object contains `input_tokens` and `output_tokens`.
Absent usage means an estimate. Both provider fields are mandatory when the object exists; explicit zero remains valid.
Main calculates provider occupancy as input plus output divided by total window minus safety margin.
It does not subtract the output reservation again after counting actual output.
Estimate occupancy retains its usable-input denominator and existing compaction threshold.
The UI identifies the measurement source and shows separate provider input and output rows.
Counts describe one call, not cumulative billing or all future requests.
Compaction settings and protected-context semantics remain unchanged.

Deploy Main and Web before Rust starts publishing the optional fields.
Older closed-schema Main consumers reject the extension. No protobuf field numbers or database schema change.

## Acceptance boundary

All 32 compatible-facade tests pass for the transport correction.
The integration passes 493 Rust agent tests with PostgreSQL enabled and 26 native Anthropic transport tests.
Strict Rust library and test Clippy checks pass.
Main context-domain tests, vet, and the PostgreSQL context projection test pass.
The projection test covers provider counts, child exclusion, fresh reads, and replacement by the next request estimate.
All 59 context UI tests, TypeScript, and the application build pass.
These are local transport checks, not deployed browser acceptance.
Focused transport tests cover usage on finish frames and trailing usage frames.
They cover repeated cumulative snapshots, cached-input breakdowns, reasoning breakdowns, and exclusion of arbitrary provider fields.
Malformed and absent usage must preserve successful completion without invented counts.
Deployed provider-accounting and browser acceptance remain open.
Those checks must cover native Anthropic, compatible providers, regeneration, model switching, child isolation, continuation, and reload.
