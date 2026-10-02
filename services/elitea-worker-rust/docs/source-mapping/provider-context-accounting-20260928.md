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
The synthetic case uses the private compatible provider. The Haiku case calls the configured model.
Those initial browser assertions identify model names, not the actual worker adapter.

| Case | Provider input | Provider output | Displayed occupancy | Execution |
| --- | ---: | ---: | --- | --- |
| Compatible fixture | 1 | 5 | 6 / 126,720 | `b25c4e847af8ecc63581fd91cc3e060d` |
| Switch to Haiku | 113 | 198 | 311 / 126,720 | `ec64ecac45da7969514100dd8d16dbf3` |
| Regenerate Haiku response | 113 | 119 | 232 / 126,720 | `92746162d626c50e1434e8b48793cd40` |
| Fresh browser reload | 113 | 119 | 232 / 126,720 | Same regenerated execution |

The combined window is 128,000 tokens. The safety margin is 1,280 tokens.
The 256-token output cap is not subtracted again from completed-call occupancy.
Six live status events contain one estimate and one provider reading for each of the three executions.
The persisted read model and rendered panel agree with those events.
Regeneration has a new execution identity. Reload retains that identity and its counts.
Browser page errors remain empty. The final popup screenshot is inspected after its opening animation.
The Haiku model refuses the synthetic marker request; this check proves accounting, not successful fulfillment of that request.

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
Cached-input reuse, recovery, and large-window accounting are pending at this stage. The later sections record additional acceptance.


## Haiku repeated-prefix check and cache boundary

Fresh headed Playwright chat 721 uses the configured Haiku model and a synthetic 300-record instruction prefix.
Both requests complete. Browser reload preserves the second measurement and answer.
No browser responses are mocked. Browser page errors remain empty.

| Request | Execution | Input | Output | Cache reads |
| --- | --- | ---: | ---: | ---: |
| First | `0f07ac4cd558e46983acd0aa1afc87c1` | 7,275 | 53 | 0 |
| Repeated prefix | `1d8dcefd4e4bb8c55e1a06def70c4f45` | 7,350 | 20 | 0 |

Persisted ADK `usage_metadata` matches both browser measurements.
The second panel shows 7,370 / 126,720 tokens and 6% occupancy.
The screenshot is inspected after the popup animation.
This proves repeated-prefix accounting, but not a live cache hit. The provider reports zero cache reads.
Evidence uses `elitea-native-cache-meter-bounded-*` and `elitea-native-cache-meter-usage.json` in the local temporary directory.

Gateway `internal/llmproxy/anthropic_usage_test.go` adds a separate protocol regression.
It exercises the actual HTTP handler and Bifrost converter with a synthetic router response.
Both unary and streamed responses preserve 500 uncached, 8,000 cache-read, and 1,500 cache-write tokens.
The 23 output tokens produce combined occupancy of 10,023 when the Rust facade reconstructs input.
Bifrost stores cache-inclusive input. Its Anthropic converter subtracts cache reads and writes before emitting `input_tokens`.
Rust `transport/anthropic_facade.rs` adds those separate wire counters exactly once.
The full gateway `internal/llmproxy` test package and `go vet` pass.
This component test does not replace the still-open live cache-hit check.

### Saved instruction limit finding

The initial 800-record fixture in chat 720 fails before any model invocation.
Execution `a9c60832dd5bd3c3028d6a4fde0228c5` reaches authoritative state `FAILED`.
Operator diagnostics identify `agent_input.resource_exhausted`: a JSON string exceeds its allowed limit.
`agents/protocol.rs` limits generic decoded strings to 64 KiB.
`agents/assembly.rs::bounded_instruction` and `bounded_adhoc_instruction` independently enforce the same instruction bound.
Main `application/agentexecution/projectcontext.go` uses a 60 KiB injection allowance against that worker contract.
Thus, changing only the JSON parser would still reject these instructions during assembly.
The history allowance is separate. A large model context window does not currently raise the saved-instruction allowance.
No instruction, parser, or context limits change during that initial verification.
The subsequent [instruction correction](instruction-content-capacity-20260928.md) closes the admission failure.

The browser only shows a generic platform processing-limit explanation.
Gate 4 must resolve instruction-size admission and actionable field-specific diagnostics before closing this finding.
The smaller chat 721 fixture validates provider accounting within the current contract; it does not close this admission finding.
Preserve chat 720 and its `elitea-native-cache-meter-*` evidence as the failed case.


## Confirmed native adapter acceptance

A routing audit finds public Haiku configuration 3 has `openai_compatible=true`.
The earlier chat 717/721 model labels alone do not prove native adapter use.
Keep their provider accounting evidence, but exclude them from native transport acceptance.

Fresh headed Playwright verifies chat 720 through the native adapter on 2026-09-28.
The bounded test temporarily changes only configuration 3's adapter flag and restores it in `finally`.
Worker request logs explicitly identify `transport::anthropic_facade` and `model_adapter=anthropic` for both calls.
No credentials, model limits, source instructions, or provider endpoints change.

| Execution | Provider input | Provider output |
| --- | ---: | ---: |
| `96f72f115db92690d30753ba294049e7` | 19,275 | 54 |
| `cd37604350923884cce9f6c715863ef1` | 19,275 | 62 |

Reload preserves the final answer and 19,337 / 126,720 reading, or 15% occupancy.
The screenshot is inspected. Browser page errors remain empty and browser responses are not mocked.
The final persisted ADK event agrees with the browser and reports zero cache reads and writes.
Regeneration replaces the first session evidence; the first measurement remains in browser evidence and worker logs.
Evidence uses `elitea-native-verified-720-*` in the local temporary directory.

The gateway regression now also checks system cache-control preservation before provider routing, for unary and streamed requests.
The real Bifrost conversion preserves `cache_control.type=ephemeral` at that boundary.
A live cache hit remains unproven. Do not infer one from repeated input or a successful request.

The earlier private fixture configuration 21 does not pass admission and is removed through the API.
Chat 722 selects the synthetic fallback; its two runs are excluded from native and real-provider acceptance.
Its evidence remains under `elitea-native-adapter-cache-*` for that failed setup.

## Provider accounting across worker recovery

Fresh headed Playwright verifies chat 724 on worker revision `6d5e2adf9`.
The first request establishes a completed baseline measurement. The next request selects the fixture's slow-stream behavior.
After partial text appears, the test confirms that this execution owns the only active worker claim.
The test sends SIGKILL to the rehearsal worker and starts the same container. Main and the browser remain running.

Execution `833851524c38a73434826c33ffcdcbc6` retains its identity and reaches `SUCCEEDED` under claim attempt 2.
Both claims release. The completed ADK event stores one input token and 84 output tokens.
The browser shows 85 / 126,720 tokens, with the same input and output breakdown.
It does not add the baseline request or interrupted attempt to the latest-call occupancy.
The final response contains each checked stream sentinel once. Reload preserves the measurement and execution identity.
The reload screenshot is inspected. Browser page errors and failure events remain empty.

Evidence uses `elitea-meter-recovery-*` in the local temporary directory.
The browser uses real application endpoints without response interception.
The provider is a synthetic HTTP fixture with deterministic usage, not a real-model token accuracy test.
This check proves root-agent latest-call accounting across in-flight worker loss and lease takeover.
It does not establish provider billing deduplication for an interrupted model call.

## Full-window accounting after compaction

Fresh headed Playwright submits a short follow-up in existing chat 708 on worker revision `6d5e2adf9`.
The saved Luna model uses Full mode with a 1,000,000-token window and Auto output.
The test preserves the previous large history and durable compaction state. It does not regenerate or reseed that history.
Execution `35832e2e52084f42bb1007fca0589a8d` returns all four required project facts.
These are delivery code CEDAR-731, approved color teal, completed archive verification, and the pending handoff note.

The provider reports 42,040 input tokens and 42 output tokens.
Persisted ADK usage contains the same counts and a combined total of 42,082.
The UI displays 42,082 / 991,808 tokens, or 4%.
The denominator subtracts the 8,192-token safety margin from the full window without subtracting output twice.
The Auto minimum allowance remains 1,024 tokens for request admission.
Reload preserves the exact measurement and execution identity.
The screenshot is inspected. Browser page errors and execution failure events remain empty, without browser-response mocks.
Evidence uses `elitea-meter-full-708-*` in the local temporary directory.

This closes the Full-window latest-call accounting check after durable compaction.
The earlier [Auto acceptance](auto-output-budget-20260928.md#deployed-auto-acceptance) supplies the separate near-window compaction evidence.
This follow-up does not repeat that expensive summarization or establish a live cache hit. The provider reports zero cache reads.

## Nonzero cached-usage provider fixture

`deploy/mock-llm/server.py` adds the explicit `[[mock:cached_usage]]` test mode.
It returns 10,000 input tokens, including 8,000 cached tokens, and 23 output tokens, including seven reasoning tokens.
Both detail counters are subsets. Correct combined occupancy is 10,023, not 18,030.
The mode affects only the synthetic provider and leaves normal fixture usage unchanged.
Six HTTP fixture tests pass, including unary/streamed counters and request isolation.
The gateway and worker accounting mappings above remain the production source references. No production runtime behavior changes here.
The deployed acceptance below verifies these nonzero counters.
This fixture does not establish a real-provider cache hit or its performance benefit.

### Nonzero cached-usage deployed acceptance

The fixture deploys revision `86cc3b7b3` with image `sha256:944c891050107c0fbe12f3ef32a0bdc54ddaa183af4f81ac195e29fe7532e8b8`.
The prior fixture container remains stopped and preserved. The replacement retains its private address and resource constraints, without host ports.
Main and Rust remain at revision `5179b5429`; the gateway and web deployment remain unchanged.

Fresh headed Playwright verifies chat 726 through the real application, gateway, and worker.
The first execution is `ea8ff9f51cfdb04cb75c8b17c70c92dd`.
Regeneration produces `fbf7b5f9b1d79e203187435f9d1981a4`.
Both display 10,023 / 126,720 tokens, or 8%. Reload preserves the regenerated measurement and identity.
The persisted ADK event contains 10,000 prompt tokens, 23 output tokens, 8,000 cached input tokens, and seven reasoning tokens.
Thus, both detail counters survive processing without being added again to occupancy.
The final panel screenshot is inspected after its animation. Browser page errors remain empty and browser responses are not mocked.
An initial login attempt fails with an invalid state cookie before creating a fixture or execution. A fresh login succeeds.
Evidence uses `elitea-cache-meter-*` in the local temporary directory.

This proves deployed compatible-provider cached/reasoning accounting. Native counter conversion retains its separate handler and worker contract tests.
A real-provider cache hit and its performance benefit remain unproven; repeated real requests report zero cache reads.
