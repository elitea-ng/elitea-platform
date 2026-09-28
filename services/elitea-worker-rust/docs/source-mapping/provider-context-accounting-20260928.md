# Provider context accounting

Status: provider usage retention and context-meter integration implemented. Focused deployed browser acceptance passes in chat 717.

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
The integration passes 494 Rust agent tests with PostgreSQL enabled and 26 native Anthropic transport tests.
Strict Rust library and test Clippy checks pass.
Main context-domain tests, vet, and the PostgreSQL context projection test pass.
The projection test covers provider counts, child exclusion, fresh reads, and replacement by the next request estimate.
All 59 context UI tests, TypeScript, and the application build pass.
These are component checks, separate from the deployed acceptance below.
Focused transport tests cover usage on finish frames and trailing usage frames.
They cover repeated cumulative snapshots, cached-input breakdowns, reasoning breakdowns, and exclusion of arbitrary provider fields.
Malformed and absent usage must preserve successful completion without invented counts.

## Deployed browser acceptance

Main and Web deploy from `e86222c85`. Rust deploys from root-callback correction `488061706`.
The deployment preserves existing credentials, database selection, mounts, networks, and resource limits.
Main and Web deploy before the producer starts sending the optional fields.

Fresh headed Playwright sessions use chat 717. They do not intercept or mock browser responses.
The compatible case uses the private synthetic provider. The native case calls the configured Anthropic Haiku provider.

| Case | Provider input | Provider output | Displayed occupancy | Execution |
| --- | ---: | ---: | --- | --- |
| Compatible fixture | 1 | 5 | 6 / 126,720 | `b25c4e847af8ecc63581fd91cc3e060d` |
| Switch to native Haiku | 113 | 198 | 311 / 126,720 | `ec64ecac45da7969514100dd8d16dbf3` |
| Regenerate native response | 113 | 119 | 232 / 126,720 | `92746162d626c50e1434e8b48793cd40` |
| Fresh browser reload | 113 | 119 | 232 / 126,720 | Same regenerated execution |

The combined window is 128,000 tokens. The safety margin is 1,280 tokens.
The 256-token output cap is not subtracted again from completed-call occupancy.
Six live status events contain one estimate and one provider reading for each of the three executions.
The persisted read model and rendered panel agree with those events.
Regeneration has a new execution identity. Reload retains that identity and its counts.
Browser page errors remain empty. The final popup screenshot is inspected after its opening animation.
The native model refuses the synthetic marker request; this check proves accounting, not successful fulfillment of that request.

Evidence uses `/private/tmp/elitea-provider-meter-live-*` and `elitea-provider-meter-verified-reload.png`.
The initial missing-root observation remains under `elitea-provider-meter-root-missing-*`.
Deployment images:

- Main: `sha256:070655cd7dc93d29a7191aba7167fb880c26ea5d2f833a99622df4fcf1f0a238`.
- Web: `sha256:db8c59a73b03820f9e1ce694c4fe593bdf85b6bf380a35fef32f2c6984926eb3`.
- Rust: `sha256:d0bedbd7e3a269dacdc596df720f7173f15120f8966d16467ce170152999ecd9`.

Live cached-input reuse, provider measurements during continuation recovery, and large-window accounting acceptance remain separate checks.
Child isolation and per-continuation counts have component coverage. This acceptance does not close all Gate 4 requirements.


## Pipeline continuation accounting acceptance

Fresh headed Playwright chat 719 verifies the deployed pipeline model scope on 2026-09-28.
It uses existing application 93, version 100, and the isolated synthetic provider.
Browser responses are not mocked. No deployment or model default changes are required.

The browser receives six ordered context events for node `generate`:

| Call | Prepared input estimate | Provider input | Provider output |
| --- | ---: | ---: | ---: |
| Initial truncated response | 69 | 1 | 60 |
| Rejected continuation boundary | 489 | 1 | 1 |
| Successful repair | 549 | 1 | 36 |

Each estimate precedes its provider measurement. Counters describe individual calls, not accumulated usage.
All six events identify `model_scope=pipeline_node` and `node_name=generate`.
The execution generation is `24574d2a-010a-41cd-a4cf-bdd049608bd3`.
The root context indicator remains without a numeric measurement in the inspected screenshot.
This preserves the distinction between graph execution and each model-local context.
The final answer contains all 12 records once and `REPAIR_COMPLETE`.
It excludes the rejected fragment. Reload preserves the completed answer.
No failure events or browser page errors occur.
Evidence uses `elitea-provider-continuation-scope-*` in the local temporary directory.

The preceding chat 718 uses direct chat rather than a pipeline model scope.
It stops at the output cap, retains provider counts, and shows the user-driven Continue action.
The automatic-repair assertion fails because it targets the wrong execution scope.
That fixture does not implement the direct-chat continuation prompt. Do not use it to validate that action.
Its evidence remains under `elitea-provider-meter-continuation-*`.

This check closes uninterrupted pipeline continuation accounting only.
Cached-input reuse, accounting across process recovery, and large-window accounting remain open.
