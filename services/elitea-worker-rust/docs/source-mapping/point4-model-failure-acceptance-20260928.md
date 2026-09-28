# Point 4 model-failure acceptance

This matrix separates component contracts from deployed evidence.
A passing transport test does not establish browser or provider acceptance.

## Shared contract

`testdata/proto/runtime/v1/model_failure_policies.json` defines public codes, messages, and retryability.
Rust `protocol/output.rs::model_failure_contract_is_canonical_and_replayable` checks exact replay and rejects injected provider text.
Main `runtimegrpc/output/model_failure_test.go::TestModelFailurePoliciesCrossOutputBoundary` checks persistence admission for the same corpus.
Both tests pass on 2026-09-28.
The corpus includes all ten model/context categories below, plus delivery, pipeline, and input failures.

## Category evidence

| Public category | Component evidence | Deployed evidence and limit |
| --- | --- | --- |
| `MODEL_TIMEOUT` | Compatible header and idle timeout tests; lifecycle timeout classification; HTTP 408/504 cases | Chat 699 preserves partial output and reports the stream idle timeout. |
| `MODEL_RATE_LIMITED` | HTTP 429 classification before body consumption | Chat 697; child failure isolation in chat 728. |
| `MODEL_ACCESS_DENIED` | HTTP 401/403 classification before body consumption | Chat 696; child failure isolation in chat 727. |
| `MODEL_BUDGET_EXHAUSTED` | HTTP 402 classification before body consumption | Chat 729 verifies live failure, support reference, reload, and ERROR-level logging. |
| `MODEL_REQUEST_REJECTED` | HTTP 400 and native unsupported-sampling classification | Chat 730 verifies live failure, support reference, reload, and ERROR-level logging. |
| `MODEL_RESPONSE_INVALID` | Malformed SSE, premature DONE, incomplete native stream, and terminal-order checks | Chat 733 rejects a native response with the wrong model identity. Live failure, support reference, reload, and ERROR-level logging pass. |
| `CONTEXT_BUDGET_EXCEEDED` | Full-request context tests cover each input component before dispatch; lifecycle mapping | Chat 668 verifies a child context failure without terminating its healthy parent. Root admission has component evidence. |
| `MODEL_REQUEST_TOO_LARGE` | `request_profile_and_local_bounds_fail_before_network` verifies serialized request rejection before transport; lifecycle mapping | Chat 740 verifies the serialized model request limit, typed live event, support reference, reload, and ERROR-level logging. |
| `MODEL_UNAVAILABLE` | HTTP 409/503, transport failure, and protocol-version classification | Chat 698 verifies provider HTTP 503. |
| `MODEL_PROVIDER_FAILURE` | Compatible SSE error and native wire-error conversion; provider text exclusion | Chats 731/732 verify compatible/native streamed errors, typed UI guidance, support references, and reload. Chats 702/703 also verify gateway EOF propagation. |

All ten categories now have the deployed evidence and explicit scope limits listed above.
Do not use chat 695's delivery limit or chat 725's settings limit as substitutes.
The accepted native execution is `1444f90ccc6a281b35899d9e0b399059`; worker logs confirm the native adapter and `anthropic_gateway.provider_error`.

## Source mapping

Current SDK model failures propagate through `elitea_sdk/runtime/tools/llm.py` and nested application tool outcomes.
The current application provides the behavior reference for partial results and parent recovery.
Rust `transport/openai_compatible_facade.rs` and `transport/anthropic_facade.rs` classify provider failures.
Rust `protocol/output.rs` owns the safe public taxonomy.
Rust `execution/native_agent_lifecycle.rs` owns terminal reporting and ERROR-level logs.
Main's runtime output boundary validates canonical public messages before persistence.
Web `ApplicationAnswer` and its error components display partial results and support references.

The native provider-error correction translates Anthropic's `error` wire tag to ADK's `stream_error` enum tag.
It retains the existing ADK type, bounded decoder, and safe error mapping.
It does not introduce retries or change database schemas.

See [continuation diagnostics](continuation-diagnostics-20260923.md) for implementation history and browser evidence.
See [point 4 audit](point4-acceptance-audit-20260928.md) for the remaining closure work.

The five lifecycle taxonomy tests and 24 `ApplicationAnswer` UI component tests also pass on 2026-09-28.

## Native response identity acceptance

Chat 733 uses the native Anthropic adapter and a synthetic provider response with the wrong model identity.
Execution `cbc2fc2caca833b16086d83a7853fe2b` reports `MODEL_RESPONSE_INVALID` before it accepts answer content.
Worker logs report `anthropic_gateway.invalid_stream` at ERROR level.
The provider-body sentinel does not appear in the public response or inspected diagnostics.
Fresh headed Playwright verifies the typed event, readable guidance, support reference, and browser reload.
Browser page errors and response mocks remain absent.

The fixture adds `[[mock:wrong_model]]` to `deploy/mock-llm/server.py`.
It changes only synthetic Responses events. Production routing remains unchanged.
Evidence resides in `/private/tmp/elitea-wrong-model-result.json` and `/private/tmp/elitea-wrong-model-log-proof.json`.
The live and reloaded screenshots use the `elitea-wrong-model-native-20260928` prefix.

## Request-growth test correction

Chats 734 and 735 complete with manual continuation controls.
These direct chat executions do not establish automatic continuation request growth.
Chat 736 executes a pipeline LLM node and reports `OUTPUT_CONTINUATION_EXHAUSTED`.
Its seeded chat history does not enter the node's model request.
The observer timeout therefore does not establish a missing error message or request-byte failure.
Chat 737's oversized fixed input fails configuration admission before model dispatch.
This is not request-byte acceptance.
The next fixture explicitly maps graph messages into model history.
Each synthetic history message stays below the node's existing per-message bound.

Chat 738 explicitly maps `messages` and seeds 160 messages below the per-message bound.
The provider journal still contains only the current task and continuation content.
The run reports continuation exhaustion, not request-byte exhaustion.
This fixture does not prove the request-byte category.
A future acceptance fixture must verify actual provider-request growth before it asserts the byte-limit result.
No production limits or real model settings change for these tests.

## Serialized request-byte acceptance

Chat 740 enables the internal tool catalog and uses a synthetic Full-window model.
The model window is 4,000,000 tokens to isolate the byte limit from token admission.
This is fixture metadata, not a claim about a real model's capacity.
The first run admits 8,320,720 bytes of synthetic history and completes successfully.
Its provider journal records 21 history messages and 53 tool declarations.
The fixture then adds 2,500 bytes to each of the 20 synthetic history entries.
The new history contains 8,370,720 bytes. Production limits remain unchanged.
A fresh headed Playwright browser regenerates the completed response.

Execution `b65cbd1acb9ca509b420b0dbb3af9ff5` fails before model dispatch with `model_request_bytes_exceeded`.
The serialized request includes tool declarations and framing beyond the admitted history.
Worker logs report the failure at ERROR level with the same execution identity.
The UI receives `MODEL_REQUEST_TOO_LARGE`, retryable false, and specific transport-size guidance.
The support reference and error remain available after reload.
The reloaded screenshot is visually inspected. Browser errors and response mocks remain absent.
No partial response exists for this pre-dispatch failure.
Context analytics remain unavailable because no model call completes; zero values are not provider measurements.

Local evidence:

- `/private/tmp/elitea-request-bytes-catalog-result.json`
- `/private/tmp/elitea-request-bytes-catalog-regenerate.mjs`
- `/private/tmp/elitea-request-bytes-catalog-bytes-20260928-live.png`
- `/private/tmp/elitea-request-bytes-catalog-bytes-20260928-reload.png`

The unsuccessful child fixture in chat 739 reaches configuration loading limits before model dispatch.
It does not establish a request-byte failure. Its execution is `6f98a74bcddff80da3543fc43d027c28`.
